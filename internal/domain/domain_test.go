package domain

import (
	"encoding/json"
	"errors"
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

func TestExpressionDepth(t *testing.T) {
	// avg(CASE WHEN age > 18 THEN 1 ELSE 0 END): case(3) over comparison(2) over
	// column_ref(1) — the deepest real objective shape, well under the cap.
	caseBucket := Expression{Kind: CaseKind,
		Cases: []CaseBranch{{
			When: &Expression{Kind: ComparisonKind, Op: ">",
				Left:  &Expression{Kind: ColumnRefKind, Column: "age"},
				Right: &Expression{Kind: LiteralKind, Literal: &LiteralValue{Number: ptr(18.0)}}},
			Then: &Expression{Kind: LiteralKind, Literal: &LiteralValue{Number: ptr(1.0)}}}},
		Else: &Expression{Kind: LiteralKind, Literal: &LiteralValue{Number: ptr(0.0)}}}

	// A CASE whose deepest nesting lives in the else arm, so the else seed — not a
	// WHEN/THEN branch — must drive the result; a dropped else seed would undercount.
	caseElseDeepest := Expression{Kind: CaseKind,
		Cases: []CaseBranch{{
			When: &Expression{Kind: ColumnRefKind, Column: "flag"},
			Then: &Expression{Kind: LiteralKind, Literal: &LiteralValue{Number: ptr(1.0)}}}},
		Else: &Expression{Kind: ComparisonKind, Op: ">",
			Left:  &Expression{Kind: ColumnRefKind, Column: "age"},
			Right: &Expression{Kind: LiteralKind, Literal: &LiteralValue{Number: ptr(18.0)}}}}

	// A pathological tree the plain-string output schema does not bound: casts
	// nested one past the cap.
	overCap := Expression{Kind: ColumnRefKind, Column: "revenue"}
	for i := 0; i <= MaxObjectiveExpressionDepth; i++ {
		inner := overCap
		overCap = Expression{Kind: CastKind, CastType: "DOUBLE", Operand: &inner}
	}

	cases := []struct {
		name string
		expr Expression
		want int
	}{
		{"leaf column_ref", Expression{Kind: ColumnRefKind, Column: "revenue"}, 1},
		{"boolean-rate comparison indicator", Expression{Kind: ComparisonKind, Op: "=",
			Left:  &Expression{Kind: ColumnRefKind, Column: "Transported"},
			Right: &Expression{Kind: LiteralKind, Literal: &LiteralValue{Bool: ptr(true)}}}, 2},
		{"case bucket", caseBucket, 3},
		{"case with deepest else arm", caseElseDeepest, 3},
		{"nil children are depth zero", Expression{Kind: ComparisonKind, Op: "="}, 1},
		{"lag counts through its inner expression", Expression{Kind: LagKind, Offset: 1,
			Inner: &Expression{Kind: ColumnRefKind, Column: "amount"}}, 2},
		{"trailing aggregate counts through its inner expression", Expression{Kind: TrailingAggregateKind, WindowAgg: "avg", WindowSize: 5,
			Inner: &Expression{Kind: ComparisonKind, Op: "=",
				Left:  &Expression{Kind: ColumnRefKind, Column: "flag"},
				Right: &Expression{Kind: LiteralKind, Literal: &LiteralValue{Bool: ptr(true)}}}}, 3},
		{"over-cap cast chain", overCap, MaxObjectiveExpressionDepth + 2},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := ExpressionDepth(c.expr); got != c.want {
				t.Fatalf("ExpressionDepth = %d, want %d", got, c.want)
			}
		})
	}
	if ExpressionDepth(overCap) <= MaxObjectiveExpressionDepth {
		t.Fatalf("over-cap tree must exceed the cap: depth %d, cap %d", ExpressionDepth(overCap), MaxObjectiveExpressionDepth)
	}
	if ExpressionDepth(caseBucket) > MaxObjectiveExpressionDepth {
		t.Fatalf("the case-bucket shape must be under the cap: depth %d, cap %d", ExpressionDepth(caseBucket), MaxObjectiveExpressionDepth)
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

// TestLegacyConstraintRoundTrip guards the additive contract on Constraint: a
// stored numeric-threshold constraint deserializes without error and re-marshals
// byte-identically (no operand/members keys leak in, and value:0 is preserved).
func TestLegacyConstraintRoundTrip(t *testing.T) {
	legacy := `{"field":"revenue","op":"lte","value":100}`

	var got Constraint
	if err := json.Unmarshal([]byte(legacy), &got); err != nil {
		t.Fatalf("unmarshal legacy constraint: %v", err)
	}
	if !got.IsNumericThresholdOp() {
		t.Fatalf("lte should be a numeric threshold op")
	}
	b, err := json.Marshal(got)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if string(b) != legacy {
		t.Fatalf("legacy constraint not byte-identical:\n got %s\nwant %s", b, legacy)
	}
}

// TestFilterConstraintRoundTrip confirms the new equality/membership constraints
// round-trip through their typed operand/members fields.
func TestFilterConstraintRoundTrip(t *testing.T) {
	cases := []struct {
		name string
		c    Constraint
	}{
		{"bool eq", Constraint{Field: "CryoSleep", Op: Equal, Operand: &LiteralValue{Bool: ptr(true)}}},
		{"string neq", Constraint{Field: "HomePlanet", Op: NotEqual, Operand: &LiteralValue{String: ptr("Europa")}}},
		{"string in set", Constraint{Field: "HomePlanet", Op: In, Members: []LiteralValue{
			{String: ptr("Europa")}, {String: ptr("Mars")}}}},
		{"string not_in set", Constraint{Field: "HomePlanet", Op: NotIn, Members: []LiteralValue{
			{String: ptr("Earth")}}}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			b, err := json.Marshal(c.c)
			if err != nil {
				t.Fatalf("marshal: %v", err)
			}
			var got Constraint
			if err := json.Unmarshal(b, &got); err != nil {
				t.Fatalf("unmarshal: %v", err)
			}
			if !reflect.DeepEqual(got, c.c) {
				t.Fatalf("round-trip mismatch:\n got %+v\nwant %+v", got, c.c)
			}
		})
	}
}

// TestConstraintOpClass confirms the op-class accessors partition the operators so
// consumers never hardcode the op sets.
func TestConstraintOpClass(t *testing.T) {
	cases := []struct {
		op        ConstraintOp
		threshold bool
		equality  bool
		member    bool
	}{
		{LessThan, true, false, false},
		{GreaterThanOrEqual, true, false, false},
		{Equal, false, true, false},
		{NotEqual, false, true, false},
		{In, false, false, true},
		{NotIn, false, false, true},
	}
	for _, c := range cases {
		t.Run(string(c.op), func(t *testing.T) {
			con := Constraint{Op: c.op}
			if con.IsNumericThresholdOp() != c.threshold ||
				con.IsEqualityOp() != c.equality ||
				con.IsMembershipOp() != c.member {
				t.Fatalf("%s classes = (threshold %v, equality %v, member %v), want (%v, %v, %v)",
					c.op, con.IsNumericThresholdOp(), con.IsEqualityOp(), con.IsMembershipOp(),
					c.threshold, c.equality, c.member)
			}
		})
	}
}

func TestRenderConstraint(t *testing.T) {
	cases := []struct {
		name string
		c    Constraint
		want string
	}{
		{"numeric threshold", Constraint{Field: "Age", Op: LessThanOrEqual, Value: 100}, "Age <= 100"},
		{"bool eq", Constraint{Field: "CryoSleep", Op: Equal, Operand: &LiteralValue{Bool: ptr(true)}}, "CryoSleep = True"},
		{"string neq", Constraint{Field: "HomePlanet", Op: NotEqual, Operand: &LiteralValue{String: ptr("Europa")}}, "HomePlanet != Europa"},
		{"string in set", Constraint{Field: "HomePlanet", Op: In, Members: []LiteralValue{
			{String: ptr("Europa")}, {String: ptr("Mars")}}}, "HomePlanet IN {Europa, Mars}"},
		{"string not_in set", Constraint{Field: "HomePlanet", Op: NotIn, Members: []LiteralValue{
			{String: ptr("Earth")}}}, "HomePlanet NOT IN {Earth}"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := RenderConstraint(c.c); got != c.want {
				t.Fatalf("RenderConstraint = %q, want %q", got, c.want)
			}
		})
	}
}

func TestUnknownFilterValues(t *testing.T) {
	values := map[string][]string{
		"HomePlanet": {"Europa", "Mars"},
		"flag":       {"true", "false"},
		"rating":     {"1", "2", "3"},
	}

	t.Run("known value passes case-insensitively", func(t *testing.T) {
		filters := []Constraint{{Field: "homeplanet", Op: Equal, Operand: &LiteralValue{String: ptr("europa")}}}
		if got := UnknownFilterValues(filters, values); got != nil {
			t.Fatalf("case-insensitive value match should pass, got %v", got)
		}
	})

	t.Run("unknown value is reported as column=value", func(t *testing.T) {
		filters := []Constraint{{Field: "HomePlanet", Op: Equal, Operand: &LiteralValue{String: ptr("Pluto")}}}
		got := UnknownFilterValues(filters, values)
		if len(got) != 1 || got[0] != "HomePlanet=Pluto" {
			t.Fatalf("unknown = %v, want [HomePlanet=Pluto]", got)
		}
	})

	t.Run("membership reports only the absent members", func(t *testing.T) {
		filters := []Constraint{{Field: "HomePlanet", Op: In, Members: []LiteralValue{{String: ptr("Europa")}, {String: ptr("Xyz")}}}}
		got := UnknownFilterValues(filters, values)
		if len(got) != 1 || got[0] != "HomePlanet=Xyz" {
			t.Fatalf("unknown = %v, want [HomePlanet=Xyz]", got)
		}
	})

	t.Run("column without a value list is skipped", func(t *testing.T) {
		filters := []Constraint{{Field: "Age", Op: Equal, Operand: &LiteralValue{String: ptr("anything")}}}
		if got := UnknownFilterValues(filters, values); got != nil {
			t.Fatalf("high-cardinality column must be skipped, got %v", got)
		}
	})

	t.Run("boolean operand matches the cast true/false", func(t *testing.T) {
		filters := []Constraint{{Field: "flag", Op: Equal, Operand: &LiteralValue{Bool: ptr(true)}}}
		if got := UnknownFilterValues(filters, values); got != nil {
			t.Fatalf("boolean true should match cast 'true', got %v", got)
		}
	})

	t.Run("integer operand renders without a decimal", func(t *testing.T) {
		known := []Constraint{{Field: "rating", Op: Equal, Operand: &LiteralValue{Number: ptr(2.0)}}}
		if got := UnknownFilterValues(known, values); got != nil {
			t.Fatalf("integral 2 should match '2', got %v", got)
		}
		unknown := []Constraint{{Field: "rating", Op: Equal, Operand: &LiteralValue{Number: ptr(5.0)}}}
		if got := UnknownFilterValues(unknown, values); len(got) != 1 || got[0] != "rating=5" {
			t.Fatalf("unknown = %v, want [rating=5]", got)
		}
	})

	t.Run("numeric threshold ops carry no categorical operand", func(t *testing.T) {
		filters := []Constraint{{Field: "rating", Op: GreaterThan, Value: 99}}
		if got := UnknownFilterValues(filters, values); got != nil {
			t.Fatalf("threshold ops are not value-checked, got %v", got)
		}
	})

	t.Run("repeated offenders dedupe case-insensitively", func(t *testing.T) {
		filters := []Constraint{
			{Field: "HomePlanet", Op: Equal, Operand: &LiteralValue{String: ptr("Pluto")}},
			{Field: "homeplanet", Op: NotEqual, Operand: &LiteralValue{String: ptr("pluto")}},
		}
		if got := UnknownFilterValues(filters, values); len(got) != 1 {
			t.Fatalf("repeated offender must dedupe, got %v", got)
		}
	})
}

func TestValidateWindowShape(t *testing.T) {
	col := Expression{Kind: ColumnRefKind, Column: "amount"}
	lag := func(inner Expression, offset int) Expression {
		return Expression{Kind: LagKind, Inner: &inner, Offset: offset}
	}
	trailing := func(agg string, inner Expression, size int) Expression {
		return Expression{Kind: TrailingAggregateKind, WindowAgg: agg, Inner: &inner, WindowSize: size}
	}

	t.Run("valid windows pass", func(t *testing.T) {
		if err := ValidateWindowShape(lag(col, 1)); err != nil {
			t.Fatalf("valid lag should pass: %v", err)
		}
		if err := ValidateWindowShape(trailing("avg", col, 10)); err != nil {
			t.Fatalf("valid trailing aggregate should pass: %v", err)
		}
	})

	t.Run("non-windowed expression passes", func(t *testing.T) {
		if err := ValidateWindowShape(col); err != nil {
			t.Fatalf("non-windowed expression should pass: %v", err)
		}
	})

	t.Run("directly nested window rejected", func(t *testing.T) {
		if err := ValidateWindowShape(trailing("avg", lag(col, 1), 5)); !errors.Is(err, ErrWindowNested) {
			t.Fatalf("nested window must be rejected: %v", err)
		}
	})

	t.Run("window nested under a non-window is still detected", func(t *testing.T) {
		nested := Expression{Kind: ArithmeticKind, Op: "/", Left: &col, Right: ptr(trailing("avg", lag(col, 1), 5))}
		if err := ValidateWindowShape(nested); !errors.Is(err, ErrWindowNested) {
			t.Fatalf("nested window under arithmetic must be detected: %v", err)
		}
	})

	t.Run("sibling windows under a non-window are allowed", func(t *testing.T) {
		siblings := Expression{Kind: ArithmeticKind, Op: "/", Left: ptr(lag(col, 1)), Right: ptr(trailing("avg", col, 5))}
		if err := ValidateWindowShape(siblings); err != nil {
			t.Fatalf("sibling (non-nested) windows should pass: %v", err)
		}
	})

	t.Run("out-of-range offsets and sizes rejected", func(t *testing.T) {
		for _, e := range []Expression{lag(col, 0), lag(col, MaxWindowSize+1), trailing("avg", col, 0), trailing("avg", col, MaxWindowSize+1)} {
			if err := ValidateWindowShape(e); !errors.Is(err, ErrWindowBounds) {
				t.Fatalf("out-of-range window must be rejected: %v", err)
			}
		}
	})
}

func TestHasWindowKind(t *testing.T) {
	col := Expression{Kind: ColumnRefKind, Column: "amount"}
	if HasWindowKind(col) {
		t.Fatal("a bare column is not windowed")
	}
	windowed := Expression{Kind: ArithmeticKind, Op: "/", Left: &col,
		Right: &Expression{Kind: TrailingAggregateKind, WindowAgg: "avg", WindowSize: 5, Inner: &col}}
	if !HasWindowKind(windowed) {
		t.Fatal("a window nested under arithmetic must be detected as windowed")
	}
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
