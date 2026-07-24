package domain

import (
	"encoding/json"
	"reflect"
	"testing"
)

func ptr[T any](v T) *T { return &v }

// TestExpressionRoundTrip confirms every node kind survives a marshal/unmarshal
// cycle unchanged, which is what lets the AST live as jsonb inside the matrix.
func TestExpressionRoundTrip(t *testing.T) {
	cases := []struct {
		name string
		expr Expression
	}{
		{"column_ref", Expression{Kind: ColumnRefKind, Column: "revenue"}},
		{"literal_number", Expression{Kind: LiteralKind, Literal: &LiteralValue{Number: ptr(3.5)}}},
		{"literal_string", Expression{Kind: LiteralKind, Literal: &LiteralValue{String: ptr("gold")}}},
		{"literal_bool", Expression{Kind: LiteralKind, Literal: &LiteralValue{Bool: ptr(true)}}},
		{"cast", Expression{Kind: CastKind, CastType: "DOUBLE",
			Operand: &Expression{Kind: ColumnRefKind, Column: "flag"}}},
		{"comparison", Expression{Kind: ComparisonKind, Op: "=",
			Left:  &Expression{Kind: ColumnRefKind, Column: "Transported"},
			Right: &Expression{Kind: LiteralKind, Literal: &LiteralValue{Bool: ptr(true)}}}},
		{"arithmetic", Expression{Kind: ArithmeticKind, Op: "/",
			Left:  &Expression{Kind: ColumnRefKind, Column: "wins"},
			Right: &Expression{Kind: ColumnRefKind, Column: "games"}}},
		{"case", Expression{Kind: CaseKind,
			Cases: []CaseBranch{{
				When: &Expression{Kind: ComparisonKind, Op: ">",
					Left:  &Expression{Kind: ColumnRefKind, Column: "age"},
					Right: &Expression{Kind: LiteralKind, Literal: &LiteralValue{Number: ptr(18.0)}}},
				Then: &Expression{Kind: LiteralKind, Literal: &LiteralValue{Number: ptr(1.0)}},
			}},
			Else: &Expression{Kind: LiteralKind, Literal: &LiteralValue{Number: ptr(0.0)}}}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			b, err := json.Marshal(c.expr)
			if err != nil {
				t.Fatalf("marshal: %v", err)
			}
			var got Expression
			if err := json.Unmarshal(b, &got); err != nil {
				t.Fatalf("unmarshal: %v", err)
			}
			if !reflect.DeepEqual(got, c.expr) {
				t.Fatalf("round-trip mismatch:\n got %+v\nwant %+v", got, c.expr)
			}
		})
	}
}

// TestLegacyTargetRoundTrip guards the additive contract: a stored field-only
// target deserializes without error, resolves to a degenerate ColumnRef, and
// re-marshals byte-identically (no aggregation/value keys leak in).
func TestLegacyTargetRoundTrip(t *testing.T) {
	legacy := `{"field":"revenue","direction":"maximize"}`

	var got Target
	if err := json.Unmarshal([]byte(legacy), &got); err != nil {
		t.Fatalf("unmarshal legacy target: %v", err)
	}

	want := Expression{Kind: ColumnRefKind, Column: "revenue"}
	if ve := got.ValueExpression(); !reflect.DeepEqual(ve, want) {
		t.Fatalf("legacy ValueExpression = %+v, want %+v", ve, want)
	}

	b, err := json.Marshal(got)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if string(b) != legacy {
		t.Fatalf("legacy target not byte-identical:\n got %s\nwant %s", b, legacy)
	}
}

// TestTargetWithExpressionRoundTrip confirms a new expression-carrying target
// (aggregation + value) round-trips and resolves to its explicit expression.
func TestTargetWithExpressionRoundTrip(t *testing.T) {
	expr := Expression{Kind: ComparisonKind, Op: "=",
		Left:  &Expression{Kind: ColumnRefKind, Column: "Transported"},
		Right: &Expression{Kind: LiteralKind, Literal: &LiteralValue{Bool: ptr(true)}}}
	target := Target{Field: "Transported", Direction: Maximize, Aggregation: "avg", Value: &expr}

	b, err := json.Marshal(target)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var got Target
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if !reflect.DeepEqual(got, target) {
		t.Fatalf("round-trip mismatch:\n got %+v\nwant %+v", got, target)
	}
	if ve := got.ValueExpression(); !reflect.DeepEqual(ve, expr) {
		t.Fatalf("ValueExpression = %+v, want %+v", ve, expr)
	}
}

func TestUnknownFilterColumns(t *testing.T) {
	columns := []string{"Revenue", "Region", "HomePlanet"}

	t.Run("matches case-insensitively", func(t *testing.T) {
		filters := []Constraint{
			{Field: "revenue", Op: GreaterThan, Value: 1},
			{Field: "REGION", Op: LessThan, Value: 2},
		}
		if got := UnknownFilterColumns(filters, columns); got != nil {
			t.Fatalf("case-insensitive matches should be known, got unknown %v", got)
		}
	})

	t.Run("reports unknown fields in first-seen order", func(t *testing.T) {
		filters := []Constraint{
			{Field: "Revenue", Op: GreaterThan, Value: 1},
			{Field: "Cabin", Op: LessThan, Value: 2},
			{Field: "Age", Op: GreaterThan, Value: 3},
		}
		got := UnknownFilterColumns(filters, columns)
		if len(got) != 2 || got[0] != "Cabin" || got[1] != "Age" {
			t.Fatalf("unknown = %v, want [Cabin Age]", got)
		}
	})

	t.Run("deduplicates unknown fields case-insensitively", func(t *testing.T) {
		filters := []Constraint{
			{Field: "Cabin", Op: GreaterThan, Value: 1},
			{Field: "cabin", Op: LessThan, Value: 2},
		}
		if got := UnknownFilterColumns(filters, columns); len(got) != 1 || got[0] != "Cabin" {
			t.Fatalf("unknown = %v, want [Cabin] (deduplicated)", got)
		}
	})

	t.Run("empty schema makes every filtered field unknown", func(t *testing.T) {
		filters := []Constraint{{Field: "Revenue", Op: GreaterThan, Value: 1}}
		if got := UnknownFilterColumns(filters, nil); len(got) != 1 || got[0] != "Revenue" {
			t.Fatalf("unknown = %v, want [Revenue]", got)
		}
	})

	t.Run("no filters yields no unknowns", func(t *testing.T) {
		if got := UnknownFilterColumns(nil, columns); got != nil {
			t.Fatalf("no filters should yield no unknowns, got %v", got)
		}
	})
}

func TestRenderObjectiveLabel(t *testing.T) {
	cases := []struct {
		name string
		agg  string
		expr Expression
		want string
	}{
		{"bare column", "sum", Expression{Kind: ColumnRefKind, Column: "revenue"}, "sum(revenue)"},
		{"comparison bool", "avg", Expression{Kind: ComparisonKind, Op: "=",
			Left:  &Expression{Kind: ColumnRefKind, Column: "Transported"},
			Right: &Expression{Kind: LiteralKind, Literal: &LiteralValue{Bool: ptr(true)}}},
			"avg(Transported = True)"},
		{"comparison string", "avg", Expression{Kind: ComparisonKind, Op: "=",
			Left:  &Expression{Kind: ColumnRefKind, Column: "tier"},
			Right: &Expression{Kind: LiteralKind, Literal: &LiteralValue{String: ptr("gold")}}},
			"avg(tier = gold)"},
		{"cast", "avg", Expression{Kind: CastKind, CastType: "DOUBLE",
			Operand: &Expression{Kind: ColumnRefKind, Column: "flag"}},
			"avg(flag::DOUBLE)"},
		{"arithmetic number literal", "sum", Expression{Kind: ArithmeticKind, Op: "/",
			Left:  &Expression{Kind: ColumnRefKind, Column: "wins"},
			Right: &Expression{Kind: LiteralKind, Literal: &LiteralValue{Number: ptr(3.5)}}},
			"sum(wins / 3.5)"},
		{"case bucket", "avg", Expression{Kind: CaseKind,
			Cases: []CaseBranch{{
				When: &Expression{Kind: ComparisonKind, Op: ">",
					Left:  &Expression{Kind: ColumnRefKind, Column: "age"},
					Right: &Expression{Kind: LiteralKind, Literal: &LiteralValue{Number: ptr(18.0)}}},
				Then: &Expression{Kind: LiteralKind, Literal: &LiteralValue{Number: ptr(1.0)}}}},
			Else: &Expression{Kind: LiteralKind, Literal: &LiteralValue{Number: ptr(0.0)}}},
			"avg(CASE WHEN age > 18 THEN 1 ELSE 0 END)"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := RenderObjectiveLabel(c.agg, c.expr); got != c.want {
				t.Fatalf("RenderObjectiveLabel = %q, want %q", got, c.want)
			}
		})
	}
}
