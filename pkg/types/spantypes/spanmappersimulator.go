package spantypes

import (
	"strings"

	"github.com/SigNoz/signoz/pkg/errors"
	"go.opentelemetry.io/collector/pdata/ptrace"
)

var resourceSourcePrefix = FieldContextResource.StringValue() + "."

var ErrCodeMappingPreviewFailed = errors.MustNewCode("span_attribute_mapping_preview_failed")

type SpanMappingPreviewSpan struct {
	ResourceAttributes map[string]any `json:"resourceAttributes" nullable:"true"`
	SpanAttributes     map[string]any `json:"spanAttributes" nullable:"true"`
}

type SpanMappingPreviewRequest struct {
	Attributes *SpanMappingPreviewSpan `json:"attributes" nullable:"true"`
	OtlpTraces map[string]any          `json:"otlpTraces" nullable:"true"`
	GroupID    *string                 `json:"groupId" nullable:"true"`
}

type SpanMappingPreviewResponse struct {
	Attributes *SpanMappingPreviewSpan `json:"attributes,omitempty" nullable:"true"`
	OtlpTraces map[string]any          `json:"otlpTraces,omitempty" nullable:"true"`
}

func SimulateSpanMapping(groups []*SpanMapperGroupWithMappers, resourceAttrs, spanAttrs map[string]any) (outResource, outSpan map[string]any) {
	cfg := buildProcessorConfig(filterEnabledGroupsWithMappers(groups))

	outResource = cloneAttrs(resourceAttrs)
	outSpan = cloneAttrs(spanAttrs)

	applyEnabledGroups(cfg, outSpan, outResource)
	return outResource, outSpan
}

func SimulateSpanMappingOTLP(groups []*SpanMapperGroupWithMappers, otlp []byte) ([]byte, error) {
	td, err := (&ptrace.JSONUnmarshaler{}).UnmarshalTraces(otlp)
	if err != nil {
		return nil, errors.WrapInvalidInputf(err, ErrCodeMappingInvalidInput, "invalid OTLP traces payload")
	}

	cfg := buildProcessorConfig(filterEnabledGroupsWithMappers(groups))

	rss := td.ResourceSpans()
	for i := 0; i < rss.Len(); i++ {
		rs := rss.At(i)
		resourceAttrs := rs.Resource().Attributes().AsRaw()

		scopeSpans := rs.ScopeSpans()
		for j := 0; j < scopeSpans.Len(); j++ {
			spans := scopeSpans.At(j).Spans()
			for k := 0; k < spans.Len(); k++ {
				span := spans.At(k)
				spanAttrs := span.Attributes().AsRaw()
				applyEnabledGroups(cfg, spanAttrs, resourceAttrs)
				if err := span.Attributes().FromRaw(spanAttrs); err != nil {
					return nil, errors.WrapInternalf(err, ErrCodeMappingPreviewFailed, "could not write transformed span attributes")
				}
			}
		}

		if err := rs.Resource().Attributes().FromRaw(resourceAttrs); err != nil {
			return nil, errors.WrapInternalf(err, ErrCodeMappingPreviewFailed, "could not write transformed resource attributes")
		}
	}

	out, err := (&ptrace.JSONMarshaler{}).MarshalTraces(td)
	if err != nil {
		return nil, errors.WrapInternalf(err, ErrCodeMappingPreviewFailed, "could not marshal transformed traces")
	}
	return out, nil
}

// TODO(spanmapper-preview): the apply logic below is a temporary copy of the signozspanmapper processor — remove it and call the real processor once signoz-otel-collector#796 is merged.
func applyEnabledGroups(cfg *spanMapperProcessorConfig, spanAttrs, resourceAttrs map[string]any) {
	for i := range cfg.Groups {
		g := &cfg.Groups[i]
		if !spanMapperConditionMet(g.ExistsAny, spanAttrs, resourceAttrs) {
			continue
		}
		for j := range g.Attributes {
			applySpanMapperRule(&g.Attributes[j], spanAttrs, resourceAttrs)
		}
	}
}

// filterEnabledGroupsWithMappers keeps only enabled groups and their enabled
// mappers, dropping groups left with no enabled mappers.
func filterEnabledGroupsWithMappers(groups []*SpanMapperGroupWithMappers) []*SpanMapperGroupWithMappers {
	out := make([]*SpanMapperGroupWithMappers, 0, len(groups))
	for _, gm := range groups {
		if gm == nil || gm.Group == nil || !gm.Group.Enabled {
			continue
		}
		enabled := make([]*SpanMapper, 0, len(gm.Mappers))
		for _, m := range gm.Mappers {
			if m != nil && m.Enabled {
				enabled = append(enabled, m)
			}
		}
		if len(enabled) > 0 {
			out = append(out, &SpanMapperGroupWithMappers{Group: gm.Group, Mappers: enabled})
		}
	}
	return out
}

func spanMapperConditionMet(cond spanMapperProcessorExistsAny, spanAttrs, resourceAttrs map[string]any) bool {
	return anyKeyContains(spanAttrs, cond.Attributes) || anyKeyContains(resourceAttrs, cond.Resource)
}

func anyKeyContains(attrs map[string]any, patterns []string) bool {
	for k := range attrs {
		for _, p := range patterns {
			if strings.Contains(k, p) {
				return true
			}
		}
	}
	return false
}

func applySpanMapperRule(rule *spanMapperProcessorAttribute, spanAttrs, resourceAttrs map[string]any) {
	dst := spanAttrs
	if rule.Context == FieldContextResource.StringValue() {
		dst = resourceAttrs
	}

	for i := range rule.Sources {
		src := &rule.Sources[i]
		bare, isResource := strings.CutPrefix(src.Key, resourceSourcePrefix)

		from := spanAttrs
		if isResource {
			from = resourceAttrs
		}
		val, ok := from[bare]
		if !ok {
			continue
		}

		dst[rule.Target] = val
		if src.Action == SpanMapperOperationMove.StringValue() {
			delete(from, bare)
		}
		return
	}
}

func cloneAttrs(in map[string]any) map[string]any {
	out := make(map[string]any, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}
