package llm

import (
	"github.com/arborette/arborette/internal/domain"
)

// SandboxSchema is the column set the sandbox introspected for a data source,
// dependency-free so the llm package does not import the sandbox client. The
// orchestrator maps its introspection response into this before proposing a
// tree, so candidate filters and the objective target can be constrained to real
// columns.
type SandboxSchema struct {
	Columns []SandboxColumn
}

// SandboxColumn is one introspected column name and its type.
type SandboxColumn struct {
	Name string
	Type string
}

// TreeContext carries the fixed objective and the parent's cumulative filter
// state into a proposal call. The objective is pinned from the matrix before any
// proposal, so both root and deeper nodes re-propose filters against the same
// measurement; ObjectiveLabel is the rendered objective the deep-node prompt
// references.
type TreeContext struct {
	IsRoot         bool
	Breadth        int
	ObjectiveLabel string
	Direction      domain.TargetDirection
	ParentFilters  []domain.Constraint
	PriorValue     *float64
}

// Proposal is one node's model output: candidate interventions that vary filters
// only. The objective is pinned from the matrix, never proposed here.
type Proposal struct {
	Candidates []CandidateIntervention
}

// CandidateIntervention is one proposed refinement, carrying filters only: the
// aggregation and target are the run's fixed objective, never re-proposed.
type CandidateIntervention struct {
	Filters []domain.Constraint
}

// evaluationMatrixSchema is the structured-output schema for GenerateEvaluationMatrix,
// matching domain.EvaluationMatrix's json tags. Each target carries its own
// aggregation, optimization direction, and value expression so the fitted
// objective is fully described by the matrix; there is no separate field, because
// a compiled expression has no single column name and a plain-numeric objective
// is a bare column_ref value expression. The value expression is a plain string
// holding a JSON-encoded domain.Expression — the AST shape is grounded via the
// prompt and parsed + depth-guarded + validated deterministically after
// generation — because the structured-output contract rejects the recursive AST
// schema outright and a bounded-depth workaround compiles to a grammar past the
// size ceiling. Column references and constraint fields are likewise plain strings.
// So this schema is independent of SandboxSchema.Columns and of expression depth:
// only the fixed-cardinality enums remain (direction, aggregation, constraint op),
// keeping the compiled decoding grammar small regardless of dataset width or
// objective structure and making the schema statically cacheable. The schema
// argument is unused, retained for symmetry with the caller.
func evaluationMatrixSchema(_ SandboxSchema) map[string]any {
	return object(props{
		"targets": arrayOf(object(props{
			"direction":   enumSchema(directionEnum()),
			"aggregation": enumSchema(aggregationEnum()),
			"value":       stringProp(),
		}, "direction", "aggregation", "value")),
		"constraints": arrayOf(constraintItem()),
	}, "targets", "constraints")
}

// interventionTreeSchema is the structured-output schema for ProposeInterventionTree.
// The objective is pinned from the matrix before any proposal, so root and deep
// nodes share one candidates-only schema. Filter fields are plain strings, grounded
// to the real columns via the prompt and validated after generation, so the schema
// is independent of SandboxSchema.Columns (static per code version). The schema
// argument is unused, retained for symmetry with the caller.
func interventionTreeSchema(_ SandboxSchema) map[string]any {
	return object(props{
		"candidates": arrayOf(object(props{
			"filters": arrayOf(filterItem()),
		}, "filters")),
	}, "candidates")
}

// documentFieldsSchema is the structured-output schema for IntrospectDocumentFields:
// an array of candidate extractable fields, each a name plus a one-line
// description. Both are plain strings grounded via the prompt, so the schema is
// static (no enums, no recursion) and statically cacheable.
func documentFieldsSchema() map[string]any {
	return object(props{
		"fields": arrayOf(object(props{
			"name":        stringProp(),
			"description": stringProp(),
		}, "name", "description")),
	}, "fields")
}

// extractionSchema is the structured-output schema for Extract: the extracted
// value as a plain string plus a confidence in [0,1]. It deliberately carries no
// source locator -- Citations and structured outputs are mutually exclusive on
// the Claude API, so provenance is computed separately by a deterministic text
// search. Keeping value a plain string (not a per-field typed union) keeps the
// grammar small and lets the deterministic provenance pass match it verbatim
// against the source text.
func extractionSchema() map[string]any {
	return object(props{
		"value":      stringProp(),
		"confidence": map[string]any{"type": "number"},
	}, "value", "confidence")
}

// constraintItem is the schema for one matrix hard-constraint. The field is a plain
// string (grounded via the prompt, validated after generation); only the op keeps
// its fixed-cardinality enum. It stays numeric-only because it is shared with
// evaluationMatrixSchema, whose evaluationMatrixWire decodes the value straight into
// a float64 — growing it would break decodeMatrix and leak the richer ops into the
// matrix hard constraints. The intervention filters use filterItem instead.
func constraintItem() map[string]any {
	return object(props{
		"field": stringProp(),
		"op":    enumSchema(opEnum(domain.ConstraintOps)),
		"value": map[string]any{"type": "number"},
	}, "field", "op", "value")
}

// filterItem is the schema for one intervention-tree filter. The field is a plain
// string (grounded via the prompt, validated after generation); the op is a bounded
// enum over the richer FilterOps; the value is a bounded anyOf — a scalar
// (number/string/boolean) or a flat typed array — so a boolean/categorical/set
// filter is expressible without a per-column enum or recursion, keeping the compiled
// grammar small. filterWire (client.go) decodes the polymorphic value into a typed
// domain.Constraint by sniffing the raw JSON token.
func filterItem() map[string]any {
	return object(props{
		"field": stringProp(),
		"op":    enumSchema(opEnum(domain.FilterOps)),
		"value": map[string]any{"anyOf": append(scalarValueSchemas(),
			map[string]any{"type": "array", "items": map[string]any{"anyOf": scalarValueSchemas()}})},
	}, "field", "op", "value")
}

// scalarValueSchemas is the number/string/boolean union reused by filterItem's value
// (as the leading bare-scalar variants and as the array element type). Each call
// returns a fresh slice, so appending the array variant does not mutate the reuse.
func scalarValueSchemas() []any {
	return []any{
		map[string]any{"type": "number"},
		map[string]any{"type": "string"},
		map[string]any{"type": "boolean"},
	}
}

// props is a JSON-schema property map.
type props map[string]any

// object builds an object schema with additionalProperties:false and the given
// required keys, as structured outputs demand.
func object(properties props, required ...string) map[string]any {
	return map[string]any{
		"type":                 "object",
		"properties":           map[string]any(properties),
		"required":             toAny(required),
		"additionalProperties": false,
	}
}

func arrayOf(items map[string]any) map[string]any {
	return map[string]any{"type": "array", "items": items}
}

func stringProp() map[string]any { return map[string]any{"type": "string"} }

func enumSchema(values []any) map[string]any {
	return map[string]any{"type": "string", "enum": values}
}

func directionEnum() []any {
	return []any{string(domain.Maximize), string(domain.Minimize)}
}

func aggregationEnum() []any { return toAny(domain.Aggregations) }

// opEnum boxes a constraint-op slice into the []any enum the structured-output
// schema wants, so the matrix (ConstraintOps) and filter (FilterOps) enums derive
// from one helper rather than duplicating the map-and-box loop.
func opEnum(ops []domain.ConstraintOp) []any {
	out := make([]any, 0, len(ops))
	for _, op := range ops {
		out = append(out, string(op))
	}
	return out
}

func toAny(ss []string) []any {
	out := make([]any, 0, len(ss))
	for _, s := range ss {
		out = append(out, s)
	}
	return out
}
