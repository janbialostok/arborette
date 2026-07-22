package llm

import (
	"testing"

	"github.com/arborette/arborette/internal/domain"
)

func requiredKeys(t *testing.T, schema map[string]any) map[string]bool {
	t.Helper()
	raw, ok := schema["required"].([]any)
	if !ok {
		t.Fatalf("schema has no []any required set: %v", schema["required"])
	}
	out := map[string]bool{}
	for _, k := range raw {
		out[k.(string)] = true
	}
	return out
}

func properties(t *testing.T, schema map[string]any) map[string]any {
	t.Helper()
	props, ok := schema["properties"].(map[string]any)
	if !ok {
		t.Fatalf("schema has no properties map: %v", schema)
	}
	return props
}

func enumValues(t *testing.T, prop any) []any {
	t.Helper()
	m, ok := prop.(map[string]any)
	if !ok {
		t.Fatalf("property is not a schema object: %v", prop)
	}
	vals, _ := m["enum"].([]any)
	return vals
}

func TestEvaluationMatrixSchema(t *testing.T) {
	schema := evaluationMatrixSchema()
	if schema["additionalProperties"] != false {
		t.Fatal("structured-output objects must set additionalProperties:false")
	}
	req := requiredKeys(t, schema)
	if !req["targets"] || !req["constraints"] {
		t.Fatalf("required must include targets and constraints: %v", req)
	}
	targetItems := properties(t, schema)["targets"].(map[string]any)["items"].(map[string]any)
	dir := enumValues(t, properties(t, targetItems)["direction"])
	if len(dir) != 2 || dir[0] != string(domain.Maximize) || dir[1] != string(domain.Minimize) {
		t.Fatalf("direction enum = %v, want [maximize minimize]", dir)
	}
}

func TestInterventionTreeSchemaRoot(t *testing.T) {
	schema := SandboxSchema{Columns: []SandboxColumn{{Name: "revenue"}, {Name: "region"}}}
	root := interventionTreeSchema(schema, true, []string{"revenue"})

	req := requiredKeys(t, root)
	if !req["objective_aggregation"] || !req["objective_field"] || !req["candidates"] {
		t.Fatalf("root required must include the objective fields and candidates: %v", req)
	}
	props := properties(t, root)

	// Objective field is constrained to the matrix targets, not all columns —
	// this is what guarantees the run pins a target with a known direction.
	objField := enumValues(t, props["objective_field"])
	if len(objField) != 1 || objField[0] != "revenue" {
		t.Fatalf("objective_field enum = %v, want [revenue]", objField)
	}
	agg := enumValues(t, props["objective_aggregation"])
	if len(agg) != len(domain.Aggregations) {
		t.Fatalf("objective_aggregation enum = %v, want the domain aggregation set", agg)
	}

	// Candidate filter fields are constrained to the introspected columns.
	filterItems := props["candidates"].(map[string]any)["items"].(map[string]any)
	filterField := properties(t, filterItems)["filters"].(map[string]any)["items"].(map[string]any)
	fEnum := enumValues(t, properties(t, filterField)["field"])
	if len(fEnum) != 2 {
		t.Fatalf("candidate filter field enum = %v, want the 2 columns", fEnum)
	}
}

func TestInterventionTreeSchemaDeepNodeOmitsObjective(t *testing.T) {
	schema := SandboxSchema{Columns: []SandboxColumn{{Name: "revenue"}}}
	deep := interventionTreeSchema(schema, false, nil)

	req := requiredKeys(t, deep)
	if req["objective_aggregation"] || req["objective_field"] {
		t.Fatalf("deep node must not require objective fields: %v", req)
	}
	if !req["candidates"] {
		t.Fatalf("deep node must require candidates: %v", req)
	}
	if _, present := properties(t, deep)["objective_field"]; present {
		t.Fatal("deep node must not expose objective_field at all")
	}
}

func TestColumnEnumUnconstrainedWhenEmpty(t *testing.T) {
	// With no introspected columns, filter fields stay unconstrained (no enum),
	// rather than emitting an empty enum the model could never satisfy.
	item := constraintItem(columnEnum(SandboxSchema{}))
	field := properties(t, item)["field"].(map[string]any)
	if _, hasEnum := field["enum"]; hasEnum {
		t.Fatalf("empty schema should leave the filter field unconstrained: %v", field)
	}
}
