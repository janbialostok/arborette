package llm

import (
	"strings"
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

func anyOfVariants(t *testing.T, schema any) []map[string]any {
	t.Helper()
	m, ok := schema.(map[string]any)
	if !ok {
		t.Fatalf("schema is not an object: %v", schema)
	}
	raw, ok := m["anyOf"].([]any)
	if !ok {
		t.Fatalf("schema has no anyOf: %v", schema)
	}
	out := make([]map[string]any, 0, len(raw))
	for _, v := range raw {
		out = append(out, v.(map[string]any))
	}
	return out
}

func variantByKind(t *testing.T, variants []map[string]any, kind string) map[string]any {
	t.Helper()
	for _, v := range variants {
		if k, ok := properties(t, v)["kind"].(map[string]any); ok && k["const"] == kind {
			return v
		}
	}
	t.Fatalf("no anyOf variant with kind %q", kind)
	return nil
}

// resolveRef follows a {$ref: "#/$defs/name"} node to its definition; a non-ref
// schema is returned unchanged.
func resolveRef(t *testing.T, defs map[string]any, node any) map[string]any {
	t.Helper()
	m, ok := node.(map[string]any)
	if !ok {
		t.Fatalf("node is not a schema: %v", node)
	}
	if ref, ok := m["$ref"].(string); ok {
		def, ok := defs[strings.TrimPrefix(ref, "#/$defs/")].(map[string]any)
		if !ok {
			t.Fatalf("dangling $ref: %s", ref)
		}
		return def
	}
	return m
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

	// The value expression is a $ref into $defs; the resolved depth-N schema is an
	// anyOf of per-kind variants. At the top depth all six kinds are offered; the
	// column_ref variant is a closed object requiring kind+column, with column
	// enum-constrained to the schema.
	defs := s["$defs"].(map[string]any)
	value := resolveRef(t, defs, tprops["value"])
	variants := anyOfVariants(t, value)
	if len(variants) != 6 {
		t.Fatalf("value anyOf = %d variants, want 6 kinds", len(variants))
	}
	colRef := variantByKind(t, variants, string(domain.ColumnRefKind))
	if colRef["additionalProperties"] != false {
		t.Fatal("expression variant must set additionalProperties:false")
	}
	creq := requiredKeys(t, colRef)
	if !creq["kind"] || !creq["column"] {
		t.Fatalf("column_ref variant must require kind and column: %v", creq)
	}
	if len(enumValues(t, properties(t, colRef)["column"])) != 2 {
		t.Fatalf("column enum = %v, want the 2 columns", enumValues(t, properties(t, colRef)["column"]))
	}
	// The literal variant models "exactly one of number/string/bool" as its own
	// three-way anyOf.
	lit := variantByKind(t, variants, string(domain.LiteralKind))
	if len(anyOfVariants(t, properties(t, lit)["literal"])) != 3 {
		t.Fatalf("literal must be a 3-way anyOf: %v", properties(t, lit)["literal"])
	}

	// Composite variants recurse (via $ref) down to maxExprDepth; the leaf offers
	// only the two leaf kinds.
	node := tprops["value"]
	for depth := maxExprDepth; depth > 0; depth-- {
		vs := anyOfVariants(t, resolveRef(t, defs, node))
		if len(vs) != 6 {
			t.Fatalf("depth %d anyOf = %d variants, want 6", depth, len(vs))
		}
		node = properties(t, variantByKind(t, vs, string(domain.ComparisonKind)))["left"]
	}
	if leaf := anyOfVariants(t, resolveRef(t, defs, node)); len(leaf) != 2 {
		t.Fatalf("leaf anyOf = %d variants, want 2 leaf kinds", len(leaf))
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

	// Candidate filter fields are constrained to the introspected columns.
	filterItems := props["candidates"].(map[string]any)["items"].(map[string]any)
	filterField := properties(t, filterItems)["filters"].(map[string]any)["items"].(map[string]any)
	if len(enumValues(t, properties(t, filterField)["field"])) != 2 {
		t.Fatalf("candidate filter field enum = %v, want the 2 columns", enumValues(t, properties(t, filterField)["field"]))
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
