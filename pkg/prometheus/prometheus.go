package prometheus

import (
	"github.com/prometheus/prometheus/promql"
	"github.com/prometheus/prometheus/promql/parser"
	"github.com/prometheus/prometheus/storage"
)

type Engine = promql.Engine

type Parser = parser.Parser

type Prometheus interface {
	Engine() *Engine
	Storage() storage.Queryable
	Parser() Parser
}

// CapturedStatement is one underlying datastore statement that a PromQL query would
// run, captured without executing it.
type CapturedStatement struct {
	Query string
	Args  []any
}

// StatementRecorder collects the Statements captured while a PromQL query is
// evaluated against a capturing Storage (see StatementCapturer).
type StatementRecorder interface {
	Statements() []CapturedStatement
}

// StatementCapturer is an optional capability of a Prometheus provider: it
// returns a Storage that records the datastore statement(s) each Select would
// run — without executing them — together with a recorder to read them back.
// The query dry-run path discovers it via a type assertion, so providers that
// do not implement it simply expose no underlying SQL.
type StatementCapturer interface {
	CapturingStorage() (storage.Queryable, StatementRecorder)
}
