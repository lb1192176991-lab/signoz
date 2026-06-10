package implspanmapper

import (
	"context"
	"encoding/json"

	"github.com/SigNoz/signoz/pkg/errors"
	"github.com/SigNoz/signoz/pkg/modules/spanmapper"
	"github.com/SigNoz/signoz/pkg/query-service/agentConf"
	"github.com/SigNoz/signoz/pkg/types/opamptypes"
	"github.com/SigNoz/signoz/pkg/types/spantypes"
	"github.com/SigNoz/signoz/pkg/valuer"
)

type module struct {
	store spantypes.SpanMapperStore
}

func NewModule(store spantypes.SpanMapperStore) spanmapper.Module {
	return &module{store: store}
}

func (module *module) ListGroups(ctx context.Context, orgID valuer.UUID, q *spantypes.ListSpanMapperGroupsQuery) ([]*spantypes.SpanMapperGroup, error) {
	return module.store.ListGroups(ctx, orgID, q)
}

func (module *module) GetGroup(ctx context.Context, orgID, id valuer.UUID) (*spantypes.SpanMapperGroup, error) {
	return module.store.GetGroup(ctx, orgID, id)
}

func (module *module) CreateGroup(ctx context.Context, orgID valuer.UUID, group *spantypes.SpanMapperGroup) error {
	return module.store.CreateGroup(ctx, group)
}

func (module *module) UpdateGroup(ctx context.Context, orgID, id valuer.UUID, name *string, condition *spantypes.SpanMapperGroupCondition, enabled *bool, updatedBy string) error {
	group, err := module.store.GetGroup(ctx, orgID, id)
	if err != nil {
		return err
	}
	group.Update(name, condition, enabled, updatedBy)

	err = module.store.UpdateGroup(ctx, group)
	if err != nil {
		return err
	}
	agentConf.NotifyConfigUpdate(ctx)
	return nil
}

func (module *module) DeleteGroup(ctx context.Context, orgID, id valuer.UUID) error {
	err := module.store.DeleteGroup(ctx, orgID, id)
	if err != nil {
		return err
	}
	agentConf.NotifyConfigUpdate(ctx)
	return nil
}

func (module *module) ListMappers(ctx context.Context, orgID, groupID valuer.UUID) ([]*spantypes.SpanMapper, error) {
	return module.store.ListMappers(ctx, orgID, groupID)
}

func (module *module) GetMapper(ctx context.Context, orgID, groupID, id valuer.UUID) (*spantypes.SpanMapper, error) {
	return module.store.GetMapper(ctx, orgID, groupID, id)
}

func (module *module) CreateMapper(ctx context.Context, orgID, groupID valuer.UUID, mapper *spantypes.SpanMapper) error {
	// Ensure the group belongs to the org before inserting the child row.
	if _, err := module.store.GetGroup(ctx, orgID, groupID); err != nil {
		return err
	}
	err := module.store.CreateMapper(ctx, mapper)
	if err != nil {
		return err
	}
	agentConf.NotifyConfigUpdate(ctx)
	return nil
}

func (module *module) UpdateMapper(ctx context.Context, orgID, groupID, id valuer.UUID, fieldContext spantypes.FieldContext, config *spantypes.SpanMapperConfig, enabled *bool, updatedBy string) error {
	if _, err := module.store.GetGroup(ctx, orgID, groupID); err != nil {
		return err
	}
	mapper, err := module.store.GetMapper(ctx, orgID, groupID, id)
	if err != nil {
		return err
	}
	mapper.Update(fieldContext, config, enabled, updatedBy)
	err = module.store.UpdateMapper(ctx, mapper)
	if err != nil {
		return err
	}
	agentConf.NotifyConfigUpdate(ctx)
	return nil
}

func (module *module) DeleteMapper(ctx context.Context, orgID, groupID, id valuer.UUID) error {
	err := module.store.DeleteMapper(ctx, orgID, groupID, id)
	if err != nil {
		return err
	}
	agentConf.NotifyConfigUpdate(ctx)
	return nil
}

// PreviewMapping resolves the org's saved mappings for a sample input
// and returns the transformed result.
func (module *module) PreviewMapping(ctx context.Context, orgID valuer.UUID, req *spantypes.SpanMappingPreviewRequest) (*spantypes.SpanMappingPreviewResponse, error) {
	groups, err := module.resolvePreviewGroups(ctx, orgID, req)
	if err != nil {
		return nil, err
	}

	hasAttrs := req.Attributes != nil
	hasOTLP := len(req.OtlpTraces) > 0
	if hasAttrs == hasOTLP {
		return nil, errors.New(errors.TypeInvalidInput, spantypes.ErrCodeMappingInvalidInput, "exactly one of 'attributes' or 'otlpTraces' must be provided")
	}

	if hasAttrs {
		outResource, outSpan := spantypes.SimulateSpanMapping(groups, req.Attributes.ResourceAttributes, req.Attributes.SpanAttributes)
		return &spantypes.SpanMappingPreviewResponse{
			Attributes: &spantypes.SpanMappingPreviewSpan{ResourceAttributes: outResource, SpanAttributes: outSpan},
		}, nil
	}

	in, err := json.Marshal(req.OtlpTraces)
	if err != nil {
		return nil, errors.Wrapf(err, errors.TypeInvalidInput, spantypes.ErrCodeMappingInvalidInput, "could not serialize otlpTraces payload")
	}
	out, err := spantypes.SimulateSpanMappingOTLP(groups, in)
	if err != nil {
		return nil, err
	}
	var transformed map[string]any
	if err := json.Unmarshal(out, &transformed); err != nil {
		return nil, errors.WrapInternalf(err, spantypes.ErrCodeMappingPreviewFailed, "could not deserialize transformed traces")
	}
	return &spantypes.SpanMappingPreviewResponse{OtlpTraces: transformed}, nil
}

// resolvePreviewGroups resolves the config to preview against a specific saved
// group when GroupID is set, otherwise all the org's enabled saved mappings.
func (module *module) resolvePreviewGroups(ctx context.Context, orgID valuer.UUID, req *spantypes.SpanMappingPreviewRequest) ([]*spantypes.SpanMapperGroupWithMappers, error) {
	if req.GroupID != nil && *req.GroupID != "" {
		id, err := valuer.NewUUID(*req.GroupID)
		if err != nil {
			return nil, errors.Wrapf(err, errors.TypeInvalidInput, spantypes.ErrCodeMappingInvalidInput, "group id is not a valid uuid")
		}
		group, err := module.store.GetGroup(ctx, orgID, id)
		if err != nil {
			return nil, err
		}
		mappers, err := module.store.ListMappers(ctx, orgID, id)
		if err != nil {
			return nil, err
		}
		return []*spantypes.SpanMapperGroupWithMappers{{Group: group, Mappers: mappers}}, nil
	}

	return module.listEnabledGroupsWithMappers(ctx, orgID)
}

func (module *module) AgentFeatureType() agentConf.AgentFeatureType {
	return spantypes.SpanAttrMappingFeatureType
}

func (module *module) RecommendAgentConfig(orgID valuer.UUID, currentConfYaml []byte, configVersion *opamptypes.AgentConfigVersion) ([]byte, string, error) {
	ctx := context.Background()

	enabled, err := module.listEnabledGroupsWithMappers(ctx, orgID)
	if err != nil {
		return nil, "", err
	}

	updatedConf, err := spantypes.GenerateCollectorConfigWithSpanMapperProcessor(currentConfYaml, enabled)
	if err != nil {
		return nil, "", err
	}

	serialized, err := json.Marshal(enabled)
	if err != nil {
		return nil, "", err
	}

	return updatedConf, string(serialized), nil
}

// listEnabledGroupsWithMappers returns groups with their mappers.
func (module *module) listEnabledGroupsWithMappers(ctx context.Context, orgID valuer.UUID) ([]*spantypes.SpanMapperGroupWithMappers, error) {
	enabled := true
	groups, err := module.store.ListGroups(ctx, orgID, &spantypes.ListSpanMapperGroupsQuery{Enabled: &enabled})
	if err != nil {
		return nil, err
	}

	out := make([]*spantypes.SpanMapperGroupWithMappers, 0, len(groups))
	for _, g := range groups {
		mappers, err := module.store.ListMappers(ctx, orgID, g.ID)
		if err != nil {
			return nil, err
		}
		enabledMappers := make([]*spantypes.SpanMapper, 0, len(mappers))
		for _, m := range mappers {
			if m.Enabled {
				enabledMappers = append(enabledMappers, m)
			}
		}
		if len(enabledMappers) == 0 {
			continue
		}
		out = append(out, &spantypes.SpanMapperGroupWithMappers{Group: g, Mappers: enabledMappers})
	}
	return out, nil
}
