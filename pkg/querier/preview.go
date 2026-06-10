package querier

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"reflect"
	"slices"
	"strings"
	"sync"

	chproto "github.com/ClickHouse/ch-go/proto"
	"github.com/ClickHouse/clickhouse-go/v2"

	"github.com/SigNoz/signoz/pkg/errors"
	"github.com/SigNoz/signoz/pkg/querybuilder"
	qbtypes "github.com/SigNoz/signoz/pkg/types/querybuildertypes/querybuildertypesv5"
	"github.com/SigNoz/signoz/pkg/types/telemetrytypes"
	"github.com/SigNoz/signoz/pkg/valuer"
)

// statementProvider is implemented by query types that can render the
// underlying SQL/PromQL statement without executing it.
type statementProvider interface {
	Statement(ctx context.Context) (*qbtypes.Statement, error)
}

// missingMetricNames returns the distinct metric names referenced by a metric
// builder query, in order of first appearance. It is used to name the metric(s)
// in the warning attached to a fully-missing-metric query. Returns nil for any
// non-metric query.
func missingMetricNames(env qbtypes.QueryEnvelope) []string {
	spec, ok := env.Spec.(qbtypes.QueryBuilderQuery[qbtypes.MetricAggregation])
	if !ok {
		return nil
	}
	names := make([]string, 0, len(spec.Aggregations))
	for _, agg := range spec.Aggregations {
		if agg.MetricName != "" && !slices.Contains(names, agg.MetricName) {
			names = append(names, agg.MetricName)
		}
	}
	return names
}

// clickhouseExplainClause maps a variant to the EXPLAIN clause understood
// by ClickHouse (i.e. what comes between EXPLAIN and the SELECT).
func clickhouseExplainClause(v qbtypes.ExplainVariant) (string, bool) {
	switch v {
	case qbtypes.ExplainVariantPlan:
		return "PLAN", true
	case qbtypes.ExplainVariantEstimate:
		return "ESTIMATE", true
	default:
		return "", false
	}
}

// ParseExplainVariant parses the ?explain= query parameter. An empty value
// (or "false") returns ExplainVariantNone. The literal "true" maps to PLAN
// for back-compat with simple ?explain=true. Otherwise the value must
// match one of the named variants.
func ParseExplainVariant(value string) (qbtypes.ExplainVariant, error) {
	token := strings.ToLower(strings.TrimSpace(value))
	switch token {
	case "", "false":
		return qbtypes.ExplainVariantNone, nil
	case "true":
		return qbtypes.ExplainVariantPlan, nil
	}
	v := qbtypes.ExplainVariant(token)
	if _, ok := clickhouseExplainClause(v); !ok {
		return qbtypes.ExplainVariantNone, errors.NewInvalidInputf(errors.CodeInvalidInput, "unsupported explain variant %q (allowed: plan, estimate)", token)
	}
	return v, nil
}

// parseBoolQueryParam parses a true/false query parameter. An empty value (or
// "false"/"0") is false; "true"/"1" is true. name is used only in the error
// message so each caller reports the parameter the user actually sent.
func parseBoolQueryParam(value, name string) (bool, error) {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "", "false", "0":
		return false, nil
	case "true", "1":
		return true, nil
	}
	return false, errors.NewInvalidInputf(errors.CodeInvalidInput, "invalid %s value %q (allowed: true, false)", name, value)
}

// ParseVerbose parses the ?verbose= query parameter. When true the preview
// includes the rendered ClickHouse statement(s); the default is a lightweight
// verdict-only preview (valid/error/warnings per query).
func ParseVerbose(value string) (bool, error) {
	return parseBoolQueryParam(value, "verbose")
}

// ParseScore parses the ?score= query parameter. It defaults to TRUE: the
// top-level granuleSkipScore is computed unless explicitly disabled with
// score=false (which skips the granule-skip EXPLAIN round trips).
func ParseScore(value string) (bool, error) {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "", "true", "1":
		return true, nil
	case "false", "0":
		return false, nil
	}
	return false, errors.NewInvalidInputf(errors.CodeInvalidInput, "invalid score value %q (allowed: true, false)", value)
}

// QueryRangePreview validates each query in the composite query without
// executing it. By default it returns a lightweight per-query verdict
// (valid/error/warnings) plus the headline GranuleSkipScore (unless disabled via
// opts.IncludeGranuleSkipScore=false). When opts.Verbose (or Explain, which
// implies it) is set, it also renders the underlying ClickHouse statement(s)
// each query would run, with opts.Explain attaching EXPLAIN output and the
// per-statement GranuleSkipScore alongside each.
func (q *querier) QueryRangePreview(
	ctx context.Context,
	_ valuer.UUID,
	req *qbtypes.QueryRangeRequest,
	opts qbtypes.QueryRangePreviewOptions,
) (*qbtypes.QueryRangePreviewResponse, error) {

	// The preview must transform the payload exactly as QueryRange does so the
	// rendered SQL matches what the same payload will actually execute. Coerce
	// the window to epoch milliseconds up front, just like QueryRange.
	req.Start = querybuilder.ToMilliSecs(req.Start)
	req.End = querybuilder.ToMilliSecs(req.End)

	tmplVars := req.Variables
	if tmplVars == nil {
		tmplVars = make(map[string]qbtypes.VariableItem)
	}

	// Validate request-level invariants (time range, request type, unique
	// names, …) up front — these are request-wide, so there is nothing per
	// query to preview if they fail. Per-query spec validation is deliberately
	// NOT done here: it runs per query below so each query's structural error is
	// reported in its own QueryPreview instead of aborting the whole
	// preview on the first one. validationOpts carries the request-type-specific
	// options into that per-query validation.
	validationOpts, err := req.ValidateRequestScope()
	if err != nil {
		return nil, err
	}

	// A query that only exists as a dependency of a trace operator (e.g. A and
	// B in C := A => B) is not executed standalone, so it gets no statement of
	// its own — matching QueryRange.
	dependencyQueries, err := q.constructTraceOperatorDependencyMap(req.CompositeQuery.Queries)
	if err != nil {
		return nil, err
	}

	results := make(map[string]qbtypes.QueryPreview, len(req.CompositeQuery.Queries))

	// Phase 1: normalize every query's spec (step interval + metric metadata)
	// and capture the per-query warnings/errors. This runs for ALL queries —
	// including trace-operator dependencies — before any statement is rendered,
	// because a trace-operator query reads its siblings' specs at render time
	// and they must already be normalized. adjustStepInterval and
	// resolveMetricMetadata both patch the spec in place, so feed each a
	// single-element slice and write the patched envelope back into the
	// composite query. Doing it per-query (rather than once over all queries
	// like QueryRange) lets us attribute each warning/error to the query that
	// produced it, which is the whole point of a per-query preview report; the
	// extra metadata lookups are acceptable on this low-volume dry-run path.
	prepared := make(map[string]qbtypes.QueryPreview, len(req.CompositeQuery.Queries))
	missingMetricQuerySet := make(map[string]bool)
	for idx := range req.CompositeQuery.Queries {
		name := req.CompositeQuery.Queries[idx].GetQueryName()
		ps := qbtypes.QueryPreview{}

		// Validate this query's spec on its own and attribute any structural
		// error to it, instead of aborting the whole preview on the first bad
		// query (the request-level invariants were already checked above). An
		// invalid spec gets no step/metadata normalization or rendering.
		if vErr := req.CompositeQuery.Queries[idx].Validate(validationOpts...); vErr != nil {
			ps.Error = vErr
			prepared[name] = ps
			continue
		}

		env := []qbtypes.QueryEnvelope{req.CompositeQuery.Queries[idx]}
		ps.Warnings = q.adjustStepInterval(env, req.Start, req.End)

		missingMetricQueries, dormantMetricsWarningMsg, mErr := q.resolveMetricMetadata(ctx, env, req.Start, req.End)
		if mErr != nil {
			// Don't abort the whole preview: report this query's error and keep
			// going so the agent sees every problem in one round trip.
			ps.Error = mErr
		} else {
			if dormantMetricsWarningMsg != "" {
				ps.Warnings = append(ps.Warnings, dormantMetricsWarningMsg)
			}
			if len(missingMetricQueries) > 0 {
				missingMetricQuerySet[name] = true
				// A fully-missing-metric query renders no SQL and returns an empty
				// result, so flag it explicitly. resolveMetricMetadata only emits a
				// (dormant) warning for external metrics it has seen before; when it
				// stays silent — e.g. internal signoz.* metrics — the empty result
				// would otherwise be unexplained, so attach a clear note naming the
				// metric(s) the agent referenced.
				if dormantMetricsWarningMsg == "" {
					if metricNames := missingMetricNames(env[0]); len(metricNames) > 0 {
						ps.Warnings = append(ps.Warnings, fmt.Sprintf(
							"query %q references metric(s) %s with no data available; it will return an empty result",
							name, strings.Join(metricNames, ", ")))
					}
				}
			}
		}

		req.CompositeQuery.Queries[idx] = env[0]
		prepared[name] = ps
	}

	// Phase 2: render the statement for each query that actually executes, and
	// collect the ClickHouse-bound work (granuleSkipScore/EXPLAIN) to run concurrently.
	var previewTasks []previewTask
	for _, query := range req.CompositeQuery.Queries {
		name := query.GetQueryName()

		if query.GetType() != qbtypes.QueryTypeTraceOperator && dependencyQueries[name] {
			continue
		}

		ps := prepared[name]

		// Surface a phase-1 error (e.g. a not-found metric) without rendering.
		if ps.Error != nil {
			results[name] = ps
			continue
		}
		// Every aggregation resolved to a missing metric: QueryRange returns an
		// empty result for this query and renders no SQL. Mirror that.
		if missingMetricQuerySet[name] {
			results[name] = ps
			continue
		}

		var provider qbtypes.Query
		switch query.Type {
		case qbtypes.QueryTypePromQL:
			promQuery, ok := query.Spec.(qbtypes.PromQuery)
			if !ok {
				ps.Error = errors.NewInvalidInputf(errors.CodeInvalidInput, "invalid promql query spec %T", query.Spec)
				results[name] = ps
				continue
			}
			provider = newPromqlQuery(q.logger, q.promEngine, promQuery, qbtypes.TimeRange{From: req.Start, To: req.End}, req.RequestType, tmplVars)
		case qbtypes.QueryTypeClickHouseSQL:
			chQuery, ok := query.Spec.(qbtypes.ClickHouseQuery)
			if !ok {
				ps.Error = errors.NewInvalidInputf(errors.CodeInvalidInput, "invalid clickhouse query spec %T", query.Spec)
				results[name] = ps
				continue
			}
			provider = newchSQLQuery(q.logger, q.telemetryStore, chQuery, nil, qbtypes.TimeRange{From: req.Start, To: req.End}, req.RequestType, tmplVars)
		case qbtypes.QueryTypeTraceOperator:
			traceOpQuery, ok := query.Spec.(qbtypes.QueryBuilderTraceOperator)
			if !ok {
				ps.Error = errors.NewInvalidInputf(errors.CodeInvalidInput, "invalid trace operator query spec %T", query.Spec)
				results[name] = ps
				continue
			}
			provider = &traceOperatorQuery{
				telemetryStore: q.telemetryStore,
				stmtBuilder:    q.traceOperatorStmtBuilder,
				spec:           traceOpQuery,
				compositeQuery: &req.CompositeQuery,
				fromMS:         uint64(req.Start),
				toMS:           uint64(req.End),
				kind:           req.RequestType,
			}
		case qbtypes.QueryTypeBuilder:
			switch spec := query.Spec.(type) {
			case qbtypes.QueryBuilderQuery[qbtypes.TraceAggregation]:
				spec.ShiftBy = extractShiftFromBuilderQuery(spec)
				timeRange := adjustTimeRangeForShift(spec, qbtypes.TimeRange{From: req.Start, To: req.End}, req.RequestType)
				provider = newBuilderQuery(q.logger, q.telemetryStore, q.traceStmtBuilder, spec, timeRange, req.RequestType, tmplVars)
			case qbtypes.QueryBuilderQuery[qbtypes.LogAggregation]:
				spec.ShiftBy = extractShiftFromBuilderQuery(spec)
				timeRange := adjustTimeRangeForShift(spec, qbtypes.TimeRange{From: req.Start, To: req.End}, req.RequestType)
				stmtBuilder := q.logStmtBuilder
				if spec.Source == telemetrytypes.SourceAudit {
					stmtBuilder = q.auditStmtBuilder
				}
				provider = newBuilderQuery(q.logger, q.telemetryStore, stmtBuilder, spec, timeRange, req.RequestType, tmplVars)
			case qbtypes.QueryBuilderQuery[qbtypes.MetricAggregation]:
				spec.ShiftBy = extractShiftFromBuilderQuery(spec)
				timeRange := adjustTimeRangeForShift(spec, qbtypes.TimeRange{From: req.Start, To: req.End}, req.RequestType)
				if spec.Source == telemetrytypes.SourceMeter {
					provider = newBuilderQuery(q.logger, q.telemetryStore, q.meterStmtBuilder, spec, timeRange, req.RequestType, tmplVars)
				} else {
					provider = newBuilderQuery(q.logger, q.telemetryStore, q.metricStmtBuilder, spec, timeRange, req.RequestType, tmplVars)
				}
			default:
				ps.Error = errors.NewInvalidInputf(errors.CodeInvalidInput, "unsupported builder spec type %T", query.Spec)
				results[name] = ps
				continue
			}
		default:
			ps.Error = errors.NewInvalidInputf(errors.CodeInvalidInput, "unsupported query type %q", query.Type)
			results[name] = ps
			continue
		}

		stmtProvider, ok := provider.(statementProvider)
		if !ok {
			ps.Error = errors.NewInternalf(errors.CodeInternal, "query does not support preview")
			results[name] = ps
			continue
		}

		// Build the statement even in validate-only mode: a successful build is
		// the strongest validation we can do (it parses the filter/group-by and
		// resolves fields against the schema), and a build error is exactly the
		// per-query verdict a validation caller wants.
		stmt, sErr := stmtProvider.Statement(ctx)
		if sErr != nil {
			ps.Error = sErr
			results[name] = ps
			continue
		}

		ps.Warnings = append(ps.Warnings, stmt.Warnings...)

		// clickhouse_sql is user-authored raw SQL; rendering only substitutes
		// variables, so by itself it doesn't prove the SQL is valid. Verify it
		// parses and binds (tables/columns/types resolve) via EXPLAIN PLAN —
		// without executing. Builder/PromQL/trace-operator SQL is engine-generated
		// and well-formed by construction, so this is scoped to clickhouse_sql.
		if query.Type == qbtypes.QueryTypeClickHouseSQL {
			if invalidErr, infraErr := q.explainBindCheck(ctx, stmt.Query, stmt.Args); invalidErr != nil {
				ps.Error = invalidErr
				results[name] = ps
				continue
			} else if infraErr != nil {
				ps.Warnings = append(ps.Warnings, "could not validate ClickHouse SQL: "+infraErr.Error())
			}
		}

		// The query is fully validated by this point (statement built, plus the
		// clickhouse_sql bind check). Render the underlying statement(s) when the
		// caller wants them (verbose/explain) or when we need them to compute the
		// top-level granuleSkipScore (on by default). If none of those apply
		// (score disabled and not verbose), return just the verdict.
		needScore := opts.IncludeGranuleSkipScore
		needExplain := opts.Explain != qbtypes.ExplainVariantNone
		if !opts.Verbose && !needExplain && !needScore {
			results[name] = ps
			continue
		}

		// Every query exposes its underlying ClickHouse statement(s) uniformly in
		// Statements. Builder/ClickHouse/trace-operator render exactly one; PromQL
		// is not SQL — the Prometheus engine issues one statement per metric
		// selector, captured (without executing) via PreviewStatements.
		if query.Type == qbtypes.QueryTypePromQL {
			if pq, ok := provider.(*promqlQuery); ok {
				sqlStmts, pErr := pq.PreviewStatements(ctx)
				if pErr != nil {
					ps.Warnings = append(ps.Warnings, "could not render underlying ClickHouse SQL: "+pErr.Error())
				} else {
					for _, s := range sqlStmts {
						ps.Statements = append(ps.Statements, qbtypes.PreviewStatement{Query: s.Query, Args: s.Args})
					}
				}
			}
		} else {
			ps.Statements = []qbtypes.PreviewStatement{{Query: stmt.Query, Args: stmt.Args}}
		}

		results[name] = ps

		// granuleSkipScore and EXPLAIN both hit ClickHouse. Queue one task per
		// statement; runPreviewTasks executes them concurrently across queries
		// after rendering, rather than serializing one query's round trips behind
		// the next.
		if needScore || needExplain {
			for j := range ps.Statements {
				previewTasks = append(previewTasks, previewTask{name: name, stmtIdx: j, query: ps.Statements[j].Query, args: ps.Statements[j].Args})
			}
		}
	}

	q.runPreviewTasks(ctx, previewTasks, opts, results)

	// granuleSkipScore is on by default, but the rendered statements are only
	// returned when the caller asked (verbose/explain). So derive the headline
	// per-query score from the statements (the minimum — the least-selective,
	// worst-skipping statement, which dominates cost), then drop the statements
	// from the response unless they were requested.
	includeStatements := opts.Verbose || opts.Explain != qbtypes.ExplainVariantNone
	for name, ps := range results {
		var minScore *float64
		for i := range ps.Statements {
			s := ps.Statements[i].GranuleSkipScore
			if s != nil && (minScore == nil || *s < *minScore) {
				minScore = s
			}
		}
		if minScore != nil {
			v := *minScore // copy so the top-level field doesn't alias a statement entry
			ps.Score = &v
		}
		if !includeStatements {
			ps.Statements = nil
		}
		results[name] = ps
	}

	return &qbtypes.QueryRangePreviewResponse{
		Queries: results,
	}, nil
}

// previewTask is one rendered ClickHouse statement queued for ClickHouse-bound
// preview work (granuleSkipScore and/or EXPLAIN). stmtIdx is the index into the
// query's Statements list that this task's results merge back into.
type previewTask struct {
	name    string
	stmtIdx int
	query   string
	args    []any
}

// runPreviewTasks computes the granuleSkipScore and/or EXPLAIN output for each task
// concurrently — every query's ClickHouse round trips are in flight at once
// instead of serialized — and merges the outcomes back into previews. A
// composite query holds only a handful of queries, so a goroutine per task is
// fine without an explicit concurrency bound. Each goroutine writes to its own
// slot; the merge into the previews map happens after the wait, single-
// threaded, so there are no map races.
func (q *querier) runPreviewTasks(ctx context.Context, tasks []previewTask, opts qbtypes.QueryRangePreviewOptions, previews map[string]qbtypes.QueryPreview) {
	if len(tasks) == 0 {
		return
	}

	type outcome struct {
		score    *float64
		explain  string
		warnings []string
	}
	outcomes := make([]outcome, len(tasks))

	var wg sync.WaitGroup
	for i := range tasks {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			t := tasks[i]
			var out outcome
			if opts.IncludeGranuleSkipScore {
				if score, scErr := q.computeGranuleSkipScore(ctx, t.query, t.args); scErr != nil {
					// Surface the failure instead of silently dropping the score.
					out.warnings = append(out.warnings, "could not compute query score: "+scErr.Error())
				} else if score != nil {
					out.score = score
				}
			}
			if opts.Explain != qbtypes.ExplainVariantNone {
				if explained, eErr := q.runExplain(ctx, opts.Explain, t.query, t.args); eErr != nil {
					// Surface the failure instead of silently dropping the output.
					out.warnings = append(out.warnings, "could not run EXPLAIN: "+eErr.Error())
				} else {
					out.explain = explained
				}
			}
			outcomes[i] = out
		}(i)
	}
	wg.Wait()

	for i := range tasks {
		ps := previews[tasks[i].name]
		if idx := tasks[i].stmtIdx; idx >= 0 && idx < len(ps.Statements) {
			if outcomes[i].score != nil {
				ps.Statements[idx].GranuleSkipScore = outcomes[i].score
			}
			if outcomes[i].explain != "" {
				ps.Statements[idx].Explain = outcomes[i].explain
			}
		}
		ps.Warnings = append(ps.Warnings, outcomes[i].warnings...)
		previews[tasks[i].name] = ps
	}
}

// runExplain runs `EXPLAIN <variant> <stmt>` against the telemetry store and
// returns the formatted output as a single string with one row per line.
//
// The column shape differs by variant: most variants (plan, ast, syntax,
// pipeline, query_tree) return a single `explain` String column, but ESTIMATE
// returns five columns (database, table, parts, rows, marks). So the scan is
// driven by the result's column types — each row's columns are scanned into
// destinations of the driver-reported types and tab-joined — rather than
// assuming a single string (which silently dropped ESTIMATE output before).
func (q *querier) runExplain(ctx context.Context, variant qbtypes.ExplainVariant, stmt string, args []any) (string, error) {
	clause, ok := clickhouseExplainClause(variant)
	if !ok {
		return "", errors.NewInvalidInputf(errors.CodeInvalidInput, "unsupported explain variant %q", string(variant))
	}
	explainQuery := "EXPLAIN " + clause + " " + stmt
	rows, err := q.telemetryStore.ClickhouseDB().Query(ctx, explainQuery, args...)
	if err != nil {
		return "", errors.WrapInternalf(err, errors.CodeInternal, "failed to run EXPLAIN %s", clause)
	}
	defer rows.Close()

	colTypes := rows.ColumnTypes()
	multiColumn := len(colTypes) > 1

	var lines []string
	// For a multi-column variant (ESTIMATE), lead with a header row so the
	// tab-separated values are readable; single-column variants stay verbatim.
	if multiColumn {
		header := make([]string, len(colTypes))
		for i, ct := range colTypes {
			header[i] = ct.Name()
		}
		lines = append(lines, strings.Join(header, "\t"))
	}

	for rows.Next() {
		dest := make([]any, len(colTypes))
		for i, ct := range colTypes {
			dest[i] = reflect.New(ct.ScanType()).Interface()
		}
		if err := rows.Scan(dest...); err != nil {
			return "", errors.WrapInternalf(err, errors.CodeInternal, "failed to scan EXPLAIN row")
		}
		fields := make([]string, len(dest))
		for i := range dest {
			fields[i] = fmt.Sprintf("%v", reflect.ValueOf(dest[i]).Elem().Interface())
		}
		lines = append(lines, strings.Join(fields, "\t"))
	}
	if err := rows.Err(); err != nil {
		return "", errors.WrapInternalf(err, errors.CodeInternal, "EXPLAIN row iteration failed")
	}
	return strings.Join(lines, "\n"), nil
}

// userFacingClickHouseErrorCodes mirrors PR #10679's userFacingCHCodes: the
// ClickHouse error codes that indicate a problem with the query itself (bad SQL,
// unknown table/column, …) rather than a server-side/infra failure — i.e. the
// ones that should map to invalid input (400) instead of internal (500).
//
// TODO(#10679): once that PR lands, delete this and have explainBindCheck call
// the shared querier.mapClickHouseError so there's a single source of truth.
var userFacingClickHouseErrorCodes = map[chproto.Error]bool{
	chproto.ErrSyntaxError:                  true,
	chproto.ErrUnknownTable:                 true,
	chproto.ErrUnknownDatabase:              true,
	chproto.ErrUnknownIdentifier:            true,
	chproto.ErrUnknownFunction:              true,
	chproto.ErrUnknownAggregateFunction:     true,
	chproto.ErrUnknownType:                  true,
	chproto.ErrUnknownStorage:               true,
	chproto.ErrUnknownElementInAst:          true,
	chproto.ErrUnknownTypeOfQuery:           true,
	chproto.ErrIllegalTypeOfArgument:        true,
	chproto.ErrIllegalColumn:                true,
	chproto.ErrNumberOfArgumentsDoesntMatch: true,
	chproto.ErrTooManyArgumentsForFunction:  true,
	chproto.ErrTooLessArgumentsForFunction:  true,
}

// explainBindCheck validates that a rendered ClickHouse statement parses and
// binds (its tables, columns, and types resolve) by running EXPLAIN PLAN
// against it without executing it. It distinguishes two failure modes:
//
//   - invalidErr (non-nil): ClickHouse rejected the statement with a user-facing
//     error code — it's genuinely invalid input (syntax, unknown table/column,
//     type mismatch). The caller marks the query invalid.
//   - infraErr (non-nil): the check couldn't run, or ClickHouse failed with a
//     non-user-facing code (e.g. unreachable, timeout, server-side). The caller
//     warns rather than falsely marking the query invalid, since validity is
//     unknown.
//
// Both nil means the statement is valid.
func (q *querier) explainBindCheck(ctx context.Context, stmt string, args []any) (invalidErr error, infraErr error) {
	rows, err := q.telemetryStore.ClickhouseDB().Query(ctx, "EXPLAIN PLAN "+stmt, args...)
	if err != nil {
		var ex *clickhouse.Exception
		if errors.As(err, &ex) && userFacingClickHouseErrorCodes[chproto.Error(ex.Code)] {
			return errors.NewInvalidInputf(errors.CodeInvalidInput, "invalid ClickHouse SQL: %s", ex.Message), nil
		}
		return nil, err
	}
	rows.Close()
	return nil, nil
}

// explainPlanNode is the subset of a ClickHouse `EXPLAIN json = 1, indexes = 1`
// plan node that granuleSkipScore needs: the node type, its per-index granule funnel,
// and its children.
type explainPlanNode struct {
	NodeType string             `json:"Node Type"`
	Indexes  []explainPlanIndex `json:"Indexes"`
	Plans    []explainPlanNode  `json:"Plans"`
}

// explainPlanIndex is one index step under a ReadFromMergeTree node. The index
// steps run in sequence, so the first step's Initial Granules is the candidate
// total and the last step's Selected Granules is what survives all pruning.
type explainPlanIndex struct {
	InitialGranules  *int64 `json:"Initial Granules"`
	SelectedGranules *int64 `json:"Selected Granules"`
}

// computeGranuleSkipScore runs `EXPLAIN json = 1, indexes = 1` against the telemetry
// store and returns a 0-100 score: the percentage of candidate granules
// eliminated by partition, primary-key, and skip-index pruning before any data
// is read (higher = more selective, reads less). Granules are summed across
// every ReadFromMergeTree node so multi-read queries (e.g. a resource-filter
// subquery plus the main read) are scored as a whole. Returns nil — not an
// error — when the plan exposes no MergeTree index analysis, so the caller
// simply omits the score.
func (q *querier) computeGranuleSkipScore(ctx context.Context, stmt string, args []any) (*float64, error) {
	rows, err := q.telemetryStore.ClickhouseDB().Query(ctx, "EXPLAIN json = 1, indexes = 1 "+stmt, args...)
	if err != nil {
		return nil, errors.WrapInternalf(err, errors.CodeInternal, "failed to run EXPLAIN for query score")
	}
	defer rows.Close()

	// json=1 emits the plan as a single JSON document; read every row and join
	// so we are robust to the driver splitting it across rows.
	var sb strings.Builder
	for rows.Next() {
		var line string
		if err := rows.Scan(&line); err != nil {
			return nil, errors.WrapInternalf(err, errors.CodeInternal, "failed to scan EXPLAIN json row")
		}
		sb.WriteString(line)
		sb.WriteByte('\n')
	}
	if err := rows.Err(); err != nil {
		return nil, errors.WrapInternalf(err, errors.CodeInternal, "EXPLAIN json row iteration failed")
	}

	var plans []struct {
		Plan explainPlanNode `json:"Plan"`
	}
	if err := json.Unmarshal([]byte(sb.String()), &plans); err != nil {
		return nil, errors.WrapInternalf(err, errors.CodeInternal, "failed to parse EXPLAIN json")
	}

	var totalInitial, totalSelected int64
	for i := range plans {
		accumulateGranules(&plans[i].Plan, &totalInitial, &totalSelected)
	}
	if totalInitial <= 0 {
		// No MergeTree index analysis in the plan — nothing to score.
		return nil, nil
	}
	if totalSelected < 0 {
		totalSelected = 0
	}
	skipped := float64(totalInitial-totalSelected) / float64(totalInitial)
	if skipped < 0 {
		skipped = 0
	}
	score := math.Round(skipped*100*100) / 100 // percentage, 2 decimal places
	return &score, nil
}

// accumulateGranules walks the plan tree and, for every ReadFromMergeTree node,
// adds its candidate-granule total (first index step's Initial Granules) and
// surviving granules (last index step's Selected Granules) to the running sums.
func accumulateGranules(node *explainPlanNode, totalInitial, totalSelected *int64) {
	if node.NodeType == "ReadFromMergeTree" && len(node.Indexes) > 0 {
		var initial, selected *int64
		for i := range node.Indexes {
			if node.Indexes[i].InitialGranules != nil && initial == nil {
				initial = node.Indexes[i].InitialGranules
			}
			if node.Indexes[i].SelectedGranules != nil {
				selected = node.Indexes[i].SelectedGranules
			}
		}
		if initial != nil && selected != nil {
			*totalInitial += *initial
			*totalSelected += *selected
		}
	}
	for i := range node.Plans {
		accumulateGranules(&node.Plans[i], totalInitial, totalSelected)
	}
}
