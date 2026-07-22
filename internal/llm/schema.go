package llm

import "github.com/arborette/arborette/internal/domain"

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
// state into a proposal call. At the root (IsRoot) the model chooses the
// objective; at deeper nodes the pinned objective is passed back so refinement
// re-proposes filters against the same measurement.
type TreeContext struct {
	IsRoot               bool
	Breadth              int
	ObjectiveField       string
	ObjectiveAggregation string
	Direction            domain.TargetDirection
	ParentFilters        []domain.Constraint
	PriorValue           *float64
}

// Proposal is one node's model output: the run's fixed objective (set only at
// the root) plus candidate interventions that vary filters only.
type Proposal struct {
	ObjectiveField       string
	ObjectiveAggregation string
	Candidates           []CandidateIntervention
}

// CandidateIntervention is one proposed refinement, carrying filters only: the
// aggregation and target are the run's fixed objective, never re-proposed.
type CandidateIntervention struct {
	Filters []domain.Constraint
}

// evaluationMatrixSchema is the structured-output schema for GenerateEvaluationMatrix,
// matching domain.EvaluationMatrix's json tags.
func evaluationMatrixSchema() map[string]any {
	return object(props{
		"targets": arrayOf(object(props{
			"field":     stringProp(),
			"direction": enumSchema(directionEnum()),
		}, "field", "direction")),
		"constraints": arrayOf(constraintItem(nil)),
	}, "targets", "constraints")
}

// interventionTreeSchema is the structured-output schema for ProposeInterventionTree.
// At the root it also asks for the objective aggregation and target field; at
// deeper nodes those fields are omitted and only candidates are returned. Filter
// fields are constrained to the introspected column names, and the root's
// objective field to the Evaluation Matrix's targets so the run always pins a
// target with a known optimization direction. The aggregation is constrained to
// the allowlist the Sandbox accepts.
func interventionTreeSchema(schema SandboxSchema, isRoot bool, objectiveFields []string) map[string]any {
	cols := columnEnum(schema)
	properties := props{
		"candidates": arrayOf(object(props{
			"filters": arrayOf(constraintItem(cols)),
		}, "filters")),
	}
	required := []string{"candidates"}
	if isRoot {
		properties["objective_aggregation"] = enumSchema(aggregationEnum())
		properties["objective_field"] = fieldProp(toAny(objectiveFields))
		required = []string{"objective_aggregation", "objective_field", "candidates"}
	}
	return object(properties, required...)
}

// constraintItem is the schema for one hard-constraint filter. cols, when
// non-nil, restricts the field to the introspected column names.
func constraintItem(cols []any) map[string]any {
	return object(props{
		"field": fieldProp(cols),
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

// fieldProp is a string property optionally constrained to an enum of values.
func fieldProp(values []any) map[string]any {
	if len(values) == 0 {
		return stringProp()
	}
	return map[string]any{"type": "string", "enum": values}
}

func enumSchema(values []any) map[string]any {
	return map[string]any{"type": "string", "enum": values}
}

// columnEnum returns the introspected column names as an enum value list, or nil
// when the schema carries no columns (leaving fields unconstrained).
func columnEnum(schema SandboxSchema) []any {
	if len(schema.Columns) == 0 {
		return nil
	}
	names := make([]any, 0, len(schema.Columns))
	for _, c := range schema.Columns {
		names = append(names, c.Name)
	}
	return names
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
