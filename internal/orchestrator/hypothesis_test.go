package orchestrator

import (
	"encoding/json"
	"testing"

	"github.com/arborette/arborette/internal/domain"
	"github.com/arborette/arborette/internal/llm"
)

func TestPinObjective(t *testing.T) {
	matrix := domain.EvaluationMatrix{Targets: []domain.Target{
		{Field: "cost", Direction: domain.Minimize},
		{Field: "revenue", Direction: domain.Maximize},
	}}

	cases := []struct {
		name string
		root llm.Proposal
		want objective
	}{
		{
			"model choice honored",
			llm.Proposal{ObjectiveAggregation: "sum", ObjectiveField: "revenue"},
			objective{aggregation: "sum", field: "revenue", direction: domain.Maximize},
		},
		{
			"empty field falls back to first target",
			llm.Proposal{ObjectiveAggregation: "avg", ObjectiveField: ""},
			objective{aggregation: "avg", field: "cost", direction: domain.Minimize},
		},
		{
			"disallowed aggregation falls back to count",
			llm.Proposal{ObjectiveAggregation: "median", ObjectiveField: "cost"},
			objective{aggregation: "count", field: "cost", direction: domain.Minimize},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := pinObjective(c.root, matrix); got != c.want {
				t.Fatalf("pinObjective = %+v, want %+v", got, c.want)
			}
		})
	}
}

func TestImproves(t *testing.T) {
	if !improves(10, 12, domain.Maximize) {
		t.Fatal("maximize: larger value should improve")
	}
	if improves(10, 8, domain.Maximize) {
		t.Fatal("maximize: smaller value should not improve")
	}
	if !improves(10, 8, domain.Minimize) {
		t.Fatal("minimize: smaller value should improve")
	}
	if improves(10, 12, domain.Minimize) {
		t.Fatal("minimize: larger value should not improve")
	}
	if improves(10, 10, domain.Maximize) || improves(10, 10, domain.Minimize) {
		t.Fatal("equal value should not improve in either direction")
	}
}

func TestConstraintsSatisfied(t *testing.T) {
	constraints := []domain.Constraint{
		{Field: "revenue", Op: domain.GreaterThanOrEqual, Value: 100},
		{Field: "other", Op: domain.LessThan, Value: 1}, // on a different field — unverifiable, treated as satisfied
	}
	if !constraintsSatisfied(constraints, "revenue", 100) {
		t.Fatal("value at the gte boundary should satisfy")
	}
	if constraintsSatisfied(constraints, "revenue", 99) {
		t.Fatal("value below the gte boundary should violate")
	}
	// A constraint only on another field cannot be checked from the objective
	// aggregate and must not cause a false violation.
	if !constraintsSatisfied([]domain.Constraint{{Field: "other", Op: domain.LessThan, Value: 1}}, "revenue", 5) {
		t.Fatal("constraint on a non-objective field should be treated as satisfied")
	}
	for _, tc := range []struct {
		op    domain.ConstraintOp
		value float64
		ok    bool
	}{
		{domain.LessThan, 4, true}, {domain.LessThan, 5, false},
		{domain.LessThanOrEqual, 5, true}, {domain.LessThanOrEqual, 6, false},
		{domain.GreaterThan, 6, true}, {domain.GreaterThan, 5, false},
		{domain.GreaterThanOrEqual, 5, true}, {domain.GreaterThanOrEqual, 4, false},
	} {
		got := constraintsSatisfied([]domain.Constraint{{Field: "f", Op: tc.op, Value: 5}}, "f", tc.value)
		if got != tc.ok {
			t.Fatalf("%s %v vs 5 = %v, want %v", tc.op, tc.value, got, tc.ok)
		}
	}
}

func TestNumericValue(t *testing.T) {
	if v, ok := numericValue(map[string]any{"f": 12.5}, "f"); !ok || v != 12.5 {
		t.Fatalf("float64 = (%v,%v), want (12.5,true)", v, ok)
	}
	if v, ok := numericValue(map[string]any{"f": json.Number("7")}, "f"); !ok || v != 7 {
		t.Fatalf("json.Number = (%v,%v), want (7,true)", v, ok)
	}
	if _, ok := numericValue(map[string]any{"f": nil}, "f"); ok {
		t.Fatal("nil value (empty aggregate) should be unusable")
	}
	if _, ok := numericValue(map[string]any{}, "f"); ok {
		t.Fatal("missing key should be unusable")
	}
	if _, ok := numericValue(map[string]any{"f": "12"}, "f"); ok {
		t.Fatal("string value should be unusable")
	}
}

func TestConcatFiltersDoesNotAliasParent(t *testing.T) {
	parent := []domain.Constraint{{Field: "region", Op: domain.GreaterThanOrEqual, Value: 1}}
	added := []domain.Constraint{{Field: "tier", Op: domain.LessThan, Value: 3}}

	out := concatFilters(parent, added)
	if len(out) != 2 {
		t.Fatalf("expected 2 filters, got %d", len(out))
	}
	// Mutating the result must not bleed into the parent's cumulative filters,
	// or sibling branches would cross-contaminate.
	out[0].Value = 999
	if parent[0].Value != 1 {
		t.Fatalf("parent filter mutated through the concatenated slice: %+v", parent[0])
	}
}
