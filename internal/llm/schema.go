package llm

import (
	"strconv"

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

// maxExprDepth bounds the objective value-expression schema. Objective
// expressions are shallow (a Cast over a Case over a Comparison over a ColumnRef
// is depth 3), so a bounded, non-self-referential schema covers them without a
// recursive schema the structured-output contract does not accept.
const maxExprDepth = 3

// evaluationMatrixSchema is the structured-output schema for GenerateEvaluationMatrix,
// matching domain.EvaluationMatrix's json tags. Each target carries its own
// aggregation, optimization direction, and value expression so the fitted
// objective is fully described by the matrix; there is no separate field, because
// a compiled expression has no single column name and a plain-numeric objective
// is a bare column_ref value expression. Column references and constraint fields
// are left as plain strings — the model is grounded to the real columns via the
// prompt and validated against the schema after generation — so this schema is
// independent of SandboxSchema.Columns. Only the fixed-cardinality enums remain
// (direction, aggregation, constraint op), keeping the compiled decoding grammar
// small regardless of dataset width and making the schema statically cacheable.
// The schema argument is unused, retained for symmetry with the caller.
func evaluationMatrixSchema(_ SandboxSchema) map[string]any {
	defs := map[string]any{}
	value := expressionRef(maxExprDepth, defs)
	root := object(props{
		"targets": arrayOf(object(props{
			"direction":   enumSchema(directionEnum()),
			"aggregation": enumSchema(aggregationEnum()),
			"value":       value,
		}, "direction", "aggregation", "value")),
		"constraints": arrayOf(constraintItem()),
	}, "targets", "constraints")
	root["$defs"] = defs
	return root
}

// expressionRef defines the depth-N objective value-expression schema in defs (if
// not already present) and returns a $ref to it. Each depth's composite variants
// $ref the next-shallower depth, so the definitions form a bounded DAG — never
// self-referential, which the structured-output contract rejects, and never
// inlined per use, which would blow the schema size up exponentially.
func expressionRef(depth int, defs map[string]any) map[string]any {
	name := "expr_d" + strconv.Itoa(depth)
	if _, ok := defs[name]; !ok {
		defs[name] = expressionDef(depth, defs)
	}
	return map[string]any{"$ref": "#/$defs/" + name}
}

// expressionDef is the depth-N schema for a domain.Expression: an anyOf of
// per-kind variants, each a closed object whose kind is a const discriminator and
// whose every property is required, so the schema needs no optional properties. At
// depth 0 only the leaf variants (column_ref, literal) are offered; each deeper
// level also offers the composite variants, whose child expressions $ref the
// next-shallower depth. Every variant unmarshals into the flat domain.Expression
// fat node (unset kinds' fields stay zero). The Sandbox's dry-run enforces the
// coarse-type rules the schema does not, so operators and cast targets stay
// unconstrained strings here.
func expressionDef(depth int, defs map[string]any) map[string]any {
	variants := []any{
		object(props{
			"kind":   constProp(string(domain.ColumnRefKind)),
			"column": stringProp(),
		}, "kind", "column"),
		object(props{
			"kind":    constProp(string(domain.LiteralKind)),
			"literal": literalSchema(),
		}, "kind", "literal"),
	}
	if depth > 0 {
		child := expressionRef(depth-1, defs)
		// Comparison and Arithmetic share the same binary shape, differing only by kind.
		binaryVariant := func(kind string) map[string]any {
			return object(props{
				"kind":  constProp(kind),
				"op":    stringProp(),
				"left":  child,
				"right": child,
			}, "kind", "op", "left", "right")
		}
		variants = append(variants,
			object(props{
				"kind":      constProp(string(domain.CastKind)),
				"operand":   child,
				"cast_type": stringProp(),
			}, "kind", "operand", "cast_type"),
			binaryVariant(string(domain.ComparisonKind)),
			binaryVariant(string(domain.ArithmeticKind)),
			object(props{
				"kind": constProp(string(domain.CaseKind)),
				"cases": arrayOf(object(props{
					"when": child,
					"then": child,
				}, "when", "then")),
				"else": child,
			}, "kind", "cases", "else"),
		)
	}
	return map[string]any{"anyOf": variants}
}

// literalSchema is the schema for a domain.LiteralValue: an anyOf of single-field
// variants, so "exactly one of number/string/bool is set" is expressed without any
// optional property. The Sandbox rejects an empty literal.
func literalSchema() map[string]any {
	return map[string]any{"anyOf": []any{
		object(props{"number": map[string]any{"type": "number"}}, "number"),
		object(props{"string": stringProp()}, "string"),
		object(props{"bool": map[string]any{"type": "boolean"}}, "bool"),
	}}
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

// constProp is a string property fixed to a single value, discriminating an anyOf
// variant.
func constProp(value string) map[string]any {
	return map[string]any{"type": "string", "const": value}
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
