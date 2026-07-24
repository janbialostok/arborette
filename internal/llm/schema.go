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
			"filters": arrayOf(constraintItem()),
		}, "filters")),
	}, "candidates")
}

// constraintItem is the schema for one hard-constraint filter. The field is a plain
// string (grounded via the prompt, validated after generation); only the op keeps
// its fixed-cardinality enum.
func constraintItem() map[string]any {
	return object(props{
		"field": stringProp(),
		"op":    enumSchema(constraintOpEnum()),
		"value": map[string]any{"type": "number"},
	}, "field", "op", "value")
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

func constraintOpEnum() []any {
	out := make([]any, 0, len(domain.ConstraintOps))
	for _, op := range domain.ConstraintOps {
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
