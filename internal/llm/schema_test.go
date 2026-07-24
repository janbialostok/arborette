package llm

import (
	"encoding/json"
	"strconv"
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
	schema := SandboxSchema{Columns: []SandboxColumn{{Name: "revenue"}, {Name: "cost"}}}
	s := evaluationMatrixSchema(schema)
	if s["additionalProperties"] != false {
		t.Fatal("structured-output objects must set additionalProperties:false")
	}
	req := requiredKeys(t, s)
	if !req["targets"] || !req["constraints"] {
		t.Fatalf("required must include targets and constraints: %v", req)
	}

	targetItems := properties(t, s)["targets"].(map[string]any)["items"].(map[string]any)
	treq := requiredKeys(t, targetItems)
	if !treq["direction"] || !treq["aggregation"] || !treq["value"] {
		t.Fatalf("target required must include direction/aggregation/value: %v", treq)
	}
	tprops := properties(t, targetItems)
	// The strict-safe schema drops the separate field: a plain-numeric objective
	// is a bare column_ref value expression.
	if _, ok := tprops["field"]; ok {
		t.Fatal("target must not offer a separate field")
	}

	dir := enumValues(t, tprops["direction"])
	if len(dir) != 2 || dir[0] != string(domain.Maximize) || dir[1] != string(domain.Minimize) {
		t.Fatalf("direction enum = %v, want [maximize minimize]", dir)
	}
	agg := enumValues(t, tprops["aggregation"])
	if len(agg) != len(domain.Aggregations) {
		t.Fatalf("aggregation enum = %v, want the domain aggregation set", agg)
	}

	// The value expression is a plain string holding a JSON-encoded domain.Expression
	// (grounded via the prompt, parsed + depth-guarded + validated after generation),
	// not a strict AST — so the compiled grammar is independent of expression depth
	// and no $defs are emitted.
	value := tprops["value"].(map[string]any)
	if value["type"] != "string" {
		t.Fatalf("value must be a plain string schema: %v", value)
	}
	if _, hasEnum := value["enum"]; hasEnum {
		t.Fatalf("value must not be enum-constrained: %v", value)
	}
	if _, hasAnyOf := value["anyOf"]; hasAnyOf {
		t.Fatalf("value must not carry a strict AST anyOf: %v", value)
	}
	if _, hasRef := value["$ref"]; hasRef {
		t.Fatalf("value must not $ref an expression definition: %v", value)
	}
	if _, hasDefs := s["$defs"]; hasDefs {
		t.Fatalf("schema must not emit $defs once the AST is a plain string: %v", s["$defs"])
	}
}

func TestInterventionTreeSchema(t *testing.T) {
	schema := SandboxSchema{Columns: []SandboxColumn{{Name: "revenue"}, {Name: "region"}}}
	s := interventionTreeSchema(schema)

	req := requiredKeys(t, s)
	if !req["candidates"] {
		t.Fatalf("required must include candidates: %v", req)
	}
	props := properties(t, s)
	// The objective is pinned from the matrix, never proposed — no node exposes it.
	if _, present := props["objective_aggregation"]; present {
		t.Fatal("no node must expose objective_aggregation")
	}
	if _, present := props["objective_field"]; present {
		t.Fatal("no node must expose objective_field")
	}

	// Candidate filter fields are plain strings (grounded via the prompt, validated
	// after generation), while the op keeps its fixed-cardinality enum over the richer
	// FilterOps and the value is a bounded scalar-union-plus-array anyOf.
	filterItems := props["candidates"].(map[string]any)["items"].(map[string]any)
	filterField := properties(t, filterItems)["filters"].(map[string]any)["items"].(map[string]any)
	field := properties(t, filterField)["field"].(map[string]any)
	if field["type"] != "string" {
		t.Fatalf("candidate filter field must be a plain string schema: %v", field)
	}
	if _, hasEnum := field["enum"]; hasEnum {
		t.Fatalf("candidate filter field must not be enum-constrained: %v", field)
	}
	op := enumValues(t, properties(t, filterField)["op"])
	if len(op) != len(domain.FilterOps) {
		t.Fatalf("op enum = %v, want the domain filter-op set (%d entries)", op, len(domain.FilterOps))
	}

	// The value is a bounded anyOf: number/string/boolean scalar, or a flat array of
	// those scalars — non-recursive, no per-column enum, so the grammar stays small.
	value := properties(t, filterField)["value"].(map[string]any)
	variants, ok := value["anyOf"].([]any)
	if !ok || len(variants) != 4 {
		t.Fatalf("filter value must be a 4-variant anyOf (scalar union + array), got %v", value)
	}
	if _, hasEnum := value["enum"]; hasEnum {
		t.Fatalf("filter value must not be enum-constrained: %v", value)
	}
	arrayVariant := variants[3].(map[string]any)
	if arrayVariant["type"] != "array" {
		t.Fatalf("filter value's fourth variant must be the flat array: %v", arrayVariant)
	}
}

// TestConstraintItemStaysNumeric guards the schema split: the matrix hard-constraint
// item (shared with evaluationMatrixSchema) keeps the numeric op enum and a plain
// number value, so decodeMatrix never has to decode a richer op or value.
func TestConstraintItemStaysNumeric(t *testing.T) {
	item := constraintItem()
	op := enumValues(t, properties(t, item)["op"])
	if len(op) != len(domain.ConstraintOps) {
		t.Fatalf("matrix constraint op enum = %v, want the numeric constraint-op set (%d entries)", op, len(domain.ConstraintOps))
	}
	value := properties(t, item)["value"].(map[string]any)
	if value["type"] != "number" {
		t.Fatalf("matrix constraint value must stay a plain number: %v", value)
	}
}

// TestSchemaWidthIndependent pins the load-bearing property of this change: both
// structured-output schemas are byte-identical regardless of column count, so the
// compiled decoding grammar never grows with dataset width and the schema stays
// eligible for the structured-output compile cache.
func TestSchemaWidthIndependent(t *testing.T) {
	narrow := SandboxSchema{Columns: []SandboxColumn{{Name: "a"}, {Name: "b"}}}
	wide := SandboxSchema{Columns: make([]SandboxColumn, 200)}
	for i := range wide.Columns {
		wide.Columns[i] = SandboxColumn{Name: "col" + strconv.Itoa(i)}
	}

	for _, tc := range []struct {
		name  string
		build func(SandboxSchema) map[string]any
	}{
		{"evaluation matrix", evaluationMatrixSchema},
		{"intervention tree", interventionTreeSchema},
	} {
		t.Run(tc.name, func(t *testing.T) {
			n, err := json.Marshal(tc.build(narrow))
			if err != nil {
				t.Fatalf("marshal narrow: %v", err)
			}
			w, err := json.Marshal(tc.build(wide))
			if err != nil {
				t.Fatalf("marshal wide: %v", err)
			}
			if string(n) != string(w) {
				t.Fatalf("schema is not width-independent:\n narrow %s\n wide   %s", n, w)
			}
		})
	}
}
