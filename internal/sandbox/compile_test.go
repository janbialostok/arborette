package sandbox

import (
	"errors"
	"strings"
	"testing"

	"github.com/arborette/arborette/internal/datasource"
	"github.com/arborette/arborette/internal/domain"
)

// testTableFn is the server-built table-function expression compileQuery wraps its
// SQL around; its file path is already a single-quote-escaped literal, never a ?.
const testTableFn = "read_csv_auto('/tmp/arborette-sandbox-42/source.csv')"

func testCols() []datasource.Column {
	return []datasource.Column{
		{Name: "amount", Type: "DOUBLE"},
		{Name: "qty", Type: "BIGINT"},
		{Name: "price", Type: "DECIMAL(10,2)"},
		{Name: "name", Type: "VARCHAR"},
		{Name: "created", Type: "DATE"},
		{Name: "flag", Type: "BOOLEAN"},
	}
}

func ptrExpr(e domain.Expression) *domain.Expression { return &e }

func col(name string) domain.Expression {
	return domain.Expression{Kind: domain.ColumnRefKind, Column: name}
}

func strLit(s string) domain.Expression {
	return domain.Expression{Kind: domain.LiteralKind, Literal: &domain.LiteralValue{String: &s}}
}

func numLit(n float64) domain.Expression {
	return domain.Expression{Kind: domain.LiteralKind, Literal: &domain.LiteralValue{Number: &n}}
}

func cmp(op string, left, right domain.Expression) domain.Expression {
	return domain.Expression{Kind: domain.ComparisonKind, Op: op, Left: ptrExpr(left), Right: ptrExpr(right)}
}

func target(field string) domain.Target {
	return domain.Target{Field: field, Direction: domain.Maximize}
}

func boolConstraint(field string, op domain.ConstraintOp, v bool) domain.Constraint {
	return domain.Constraint{Field: field, Op: op, Operand: &domain.LiteralValue{Bool: &v}}
}

func strConstraint(field string, op domain.ConstraintOp, v string) domain.Constraint {
	return domain.Constraint{Field: field, Op: op, Operand: &domain.LiteralValue{String: &v}}
}

func numConstraint(field string, op domain.ConstraintOp, v float64) domain.Constraint {
	return domain.Constraint{Field: field, Op: op, Operand: &domain.LiteralValue{Number: &v}}
}

func strMembers(field string, op domain.ConstraintOp, vs ...string) domain.Constraint {
	m := make([]domain.LiteralValue, 0, len(vs))
	for _, v := range vs {
		v := v
		m = append(m, domain.LiteralValue{String: &v})
	}
	return domain.Constraint{Field: field, Op: op, Members: m}
}

func TestCompileQueryRejections(t *testing.T) {
	cases := []struct {
		name    string
		agg     string
		target  string
		filters []domain.Constraint
	}{
		{"injection field", "avg", `x"); DROP TABLE users; --`, nil},
		{"unknown target", "avg", "nonexistent", nil},
		{"aggregation off allowlist", "median", "amount", nil},
		{"avg over varchar", "avg", "name", nil},
		{"sum over date", "sum", "created", nil},
		{"min over varchar", "min", "name", nil},
		{"filter on varchar", "avg", "amount", []domain.Constraint{{Field: "name", Op: domain.GreaterThan, Value: 1}}},
		{"unknown filter field", "avg", "amount", []domain.Constraint{{Field: "ghost", Op: domain.LessThan, Value: 1}}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if _, _, err := compileQuery(testTableFn, testCols(), c.agg, target(c.target), c.filters); err == nil {
				t.Fatalf("expected rejection for %s, got nil error", c.name)
			}
		})
	}
}

func TestCompileQueryAcceptsValidAggregates(t *testing.T) {
	cases := []struct {
		name   string
		agg    string
		target string
	}{
		{"avg numeric", "avg", "amount"},
		{"sum integer", "sum", "qty"},
		{"min decimal", "min", "price"},
		{"max decimal", "max", "price"},
		{"count varchar", "count", "name"},
		{"count date", "count", "created"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			sql, args, err := compileQuery(testTableFn, testCols(), c.agg, target(c.target), nil)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if len(args) != 0 {
				t.Fatalf("expected no args without filters, got %v", args)
			}
			if !strings.Contains(sql, "CAST(") || !strings.Contains(sql, "AS DOUBLE)") {
				t.Fatalf("aggregate not wrapped in CAST(... AS DOUBLE): %q", sql)
			}
			if !strings.Contains(sql, `"`+c.target+`"`) {
				t.Fatalf("target column not double-quoted: %q", sql)
			}
			if !strings.Contains(sql, testTableFn) {
				t.Fatalf("table function missing from query: %q", sql)
			}
		})
	}
}

func TestCompileQueryFieldResolution(t *testing.T) {
	// A differently-cased target/filter resolves to the actual column name, which
	// is what gets quoted into SQL.
	sql, _, err := compileQuery(testTableFn, testCols(), "avg", target("AMOUNT"),
		[]domain.Constraint{{Field: "QtY", Op: domain.GreaterThan, Value: 1}})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(sql, `"amount"`) || !strings.Contains(sql, `"qty"`) {
		t.Fatalf("case-insensitive fields not resolved to actual columns: %q", sql)
	}

	// A case-colliding schema is ambiguous, not a silent pick.
	dup := []datasource.Column{{Name: "Amount", Type: "DOUBLE"}, {Name: "amount", Type: "DOUBLE"}}
	if _, _, err := compileQuery(testTableFn, dup, "avg", target("AMOUNT"), nil); !errors.Is(err, errAmbiguousField) {
		t.Fatalf("expected errAmbiguousField, got %v", err)
	}
}

func TestCompileQueryOperatorMapping(t *testing.T) {
	cases := []struct {
		op   domain.ConstraintOp
		want string
	}{
		{domain.LessThan, `"qty" < ?`},
		{domain.LessThanOrEqual, `"qty" <= ?`},
		{domain.GreaterThan, `"qty" > ?`},
		{domain.GreaterThanOrEqual, `"qty" >= ?`},
	}
	for _, c := range cases {
		t.Run(string(c.op), func(t *testing.T) {
			sql, args, err := compileQuery(testTableFn, testCols(), "avg", target("amount"),
				[]domain.Constraint{{Field: "qty", Op: c.op, Value: 5}})
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if !strings.Contains(sql, c.want) {
				t.Fatalf("operator %q not mapped: %q", c.op, sql)
			}
			if len(args) != 1 || args[0].(float64) != 5 {
				t.Fatalf("threshold value not bound as arg: %v", args)
			}
		})
	}
}

// TestCompileObjectiveAcceptance covers the expression paths that retire the
// boolean/categorical hard failure: a boolean column is measured via an INTEGER
// cast, and an equality comparison becomes a 0/1 indicator with its literal bound.
func TestCompileObjectiveAcceptance(t *testing.T) {
	t.Run("boolean column avg", func(t *testing.T) {
		sql, args, err := compileObjective(testTableFn, testCols(), "avg", col("flag"), nil)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if !strings.Contains(sql, `AVG(CAST("flag" AS INTEGER))`) {
			t.Fatalf("boolean not cast to INTEGER under the aggregate: %q", sql)
		}
		if !strings.Contains(sql, "AS DOUBLE)") {
			t.Fatalf("aggregate not wrapped in CAST(... AS DOUBLE): %q", sql)
		}
		if len(args) != 0 {
			t.Fatalf("expected no args, got %v", args)
		}
	})

	t.Run("categorical equals indicator", func(t *testing.T) {
		expr := cmp("=", col("name"), strLit("gold"))
		sql, args, err := compileObjective(testTableFn, testCols(), "avg", expr, nil)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if !strings.Contains(sql, `CASE WHEN ("name" = ?) THEN 1 ELSE 0 END`) {
			t.Fatalf("comparison not compiled to a 0/1 indicator: %q", sql)
		}
		if len(args) != 1 || args[0].(string) != "gold" {
			t.Fatalf("literal not bound as arg: %v", args)
		}
	})

	t.Run("expression args precede filter args", func(t *testing.T) {
		expr := cmp("=", col("name"), strLit("gold"))
		filters := []domain.Constraint{{Field: "qty", Op: domain.GreaterThan, Value: 5}}
		_, args, err := compileObjective(testTableFn, testCols(), "avg", expr, filters)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if len(args) != 2 || args[0].(string) != "gold" || args[1].(float64) != 5 {
			t.Fatalf("expected [expression arg, filter arg] in order, got %v", args)
		}
	})

	t.Run("arithmetic compiled in double space", func(t *testing.T) {
		expr := domain.Expression{Kind: domain.ArithmeticKind, Op: "/", Left: ptrExpr(col("qty")), Right: ptrExpr(col("amount"))}
		sql, _, err := compileObjective(testTableFn, testCols(), "sum", expr, nil)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if !strings.Contains(sql, `(CAST("qty" AS DOUBLE) / CAST("amount" AS DOUBLE))`) {
			t.Fatalf("arithmetic operands not cast to DOUBLE: %q", sql)
		}
	})
}

// TestCompileObjectiveCase covers the Case/bucket node: the emitted CASE shape,
// the when/then/else arg accumulation order, and the guards that keep an invalid
// branch from reaching DuckDB as a masked 500.
func TestCompileObjectiveCase(t *testing.T) {
	t.Run("bucket compiles with ordered args", func(t *testing.T) {
		bucket := domain.Expression{Kind: domain.CaseKind,
			Cases: []domain.CaseBranch{{
				When: ptrExpr(cmp(">", col("amount"), numLit(100))),
				Then: ptrExpr(numLit(1)),
			}},
			Else: ptrExpr(numLit(0))}
		sql, args, err := compileObjective(testTableFn, testCols(), "avg", bucket, nil)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if !strings.Contains(sql, "CASE WHEN") || !strings.Contains(sql, " THEN ") || !strings.Contains(sql, " ELSE ") || !strings.Contains(sql, " END") {
			t.Fatalf("case not compiled to CASE WHEN...THEN...ELSE...END: %q", sql)
		}
		if len(args) != 3 || args[0].(float64) != 100 || args[1].(float64) != 1 || args[2].(float64) != 0 {
			t.Fatalf("expected [when, then, else] args in order, got %v", args)
		}
	})

	t.Run("boolean column when", func(t *testing.T) {
		bucket := domain.Expression{Kind: domain.CaseKind,
			Cases: []domain.CaseBranch{{When: ptrExpr(col("flag")), Then: ptrExpr(numLit(1))}},
			Else:  ptrExpr(numLit(0))}
		if _, _, err := compileObjective(testTableFn, testCols(), "avg", bucket, nil); err != nil {
			t.Fatalf("boolean-column WHEN should compile, got %v", err)
		}
	})

	cases := []struct {
		name string
		expr domain.Expression
		want error
	}{
		{"empty branches", domain.Expression{Kind: domain.CaseKind}, errTypeIncompatible},
		{"non-numeric then", domain.Expression{Kind: domain.CaseKind,
			Cases: []domain.CaseBranch{{When: ptrExpr(col("flag")), Then: ptrExpr(strLit("hi"))}}}, errTypeIncompatible},
		{"string when", domain.Expression{Kind: domain.CaseKind,
			Cases: []domain.CaseBranch{{When: ptrExpr(col("name")), Then: ptrExpr(numLit(1))}}}, errTypeIncompatible},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if _, _, err := compileObjective(testTableFn, testCols(), "avg", c.expr, nil); !errors.Is(err, c.want) {
				t.Fatalf("expected %v, got %v", c.want, err)
			}
		})
	}
}

// TestCompileObjectiveCast covers the Cast acceptance paths: a boolean cast to
// DOUBLE is measurable under a numeric aggregation, and any operand casts to
// VARCHAR (measurable only under count).
func TestCompileObjectiveCast(t *testing.T) {
	boolToDouble := domain.Expression{Kind: domain.CastKind, CastType: "DOUBLE", Operand: ptrExpr(col("flag"))}
	sql, _, err := compileObjective(testTableFn, testCols(), "avg", boolToDouble, nil)
	if err != nil {
		t.Fatalf("boolean->DOUBLE cast: %v", err)
	}
	if !strings.Contains(sql, `AVG(CAST("flag" AS DOUBLE))`) {
		t.Fatalf("boolean not cast to DOUBLE under the aggregate: %q", sql)
	}

	numToVarchar := domain.Expression{Kind: domain.CastKind, CastType: "VARCHAR", Operand: ptrExpr(col("amount"))}
	sql, _, err = compileObjective(testTableFn, testCols(), "count", numToVarchar, nil)
	if err != nil {
		t.Fatalf("numeric->VARCHAR cast under count: %v", err)
	}
	if !strings.Contains(sql, `COUNT(CAST("amount" AS VARCHAR))`) {
		t.Fatalf("cast to VARCHAR not emitted under count: %q", sql)
	}

	// A numeric->BOOLEAN cast yields a boolean expression, so the aggregate
	// boundary double-wraps it back to INTEGER for a 0/1 measure.
	numToBool := domain.Expression{Kind: domain.CastKind, CastType: "BOOLEAN", Operand: ptrExpr(col("amount"))}
	sql, _, err = compileObjective(testTableFn, testCols(), "avg", numToBool, nil)
	if err != nil {
		t.Fatalf("numeric->BOOLEAN cast under avg: %v", err)
	}
	if !strings.Contains(sql, `AVG(CAST(CAST("amount" AS BOOLEAN) AS INTEGER))`) {
		t.Fatalf("numeric->BOOLEAN not double-wrapped to INTEGER under the aggregate: %q", sql)
	}
}

// TestCompileObjectiveAggregateBoundary covers the aggregate-boundary branches:
// count accepts a string expression that every numeric aggregation rejects, and
// min/max measure a numeric expression.
func TestCompileObjectiveAggregateBoundary(t *testing.T) {
	if _, _, err := compileObjective(testTableFn, testCols(), "count", col("name"), nil); err != nil {
		t.Fatalf("count over string column: %v", err)
	}
	div := domain.Expression{Kind: domain.ArithmeticKind, Op: "/", Left: ptrExpr(col("qty")), Right: ptrExpr(col("amount"))}
	for _, agg := range []string{"min", "max"} {
		if _, _, err := compileObjective(testTableFn, testCols(), agg, div, nil); err != nil {
			t.Fatalf("%s over numeric expression: %v", agg, err)
		}
	}
}

// TestCompileObjectiveMalformedNodes covers the defensive guards for a malformed
// AST: an empty Literal and a node missing a required operand.
func TestCompileObjectiveMalformedNodes(t *testing.T) {
	emptyLit := domain.Expression{Kind: domain.LiteralKind}
	if _, _, err := compileObjective(testTableFn, testCols(), "avg", emptyLit, nil); !errors.Is(err, errTypeIncompatible) {
		t.Fatalf("expected errTypeIncompatible for empty literal, got %v", err)
	}
	missingLeft := domain.Expression{Kind: domain.ComparisonKind, Op: "=", Right: ptrExpr(numLit(1))}
	if _, _, err := compileObjective(testTableFn, testCols(), "avg", missingLeft, nil); !errors.Is(err, errTypeIncompatible) {
		t.Fatalf("expected errTypeIncompatible for missing operand, got %v", err)
	}
}

// TestCompileObjectiveRejections asserts every clearly-invalid combination is
// caught at compile time with its sentinel, so it surfaces as a descriptive 400
// rather than an opaque DuckDB runtime error.
func TestCompileObjectiveRejections(t *testing.T) {
	cases := []struct {
		name string
		expr domain.Expression
		want error
	}{
		{"arithmetic over varchar", domain.Expression{Kind: domain.ArithmeticKind, Op: "+", Left: ptrExpr(col("amount")), Right: ptrExpr(col("name"))}, errTypeIncompatible},
		{"comparison numeric vs string", cmp("=", col("amount"), strLit("x")), errTypeIncompatible},
		{"unknown comparison operator", cmp("LIKE", col("name"), strLit("gold")), errUnknownOperator},
		{"unknown arithmetic operator", domain.Expression{Kind: domain.ArithmeticKind, Op: "%", Left: ptrExpr(col("amount")), Right: ptrExpr(col("qty"))}, errUnknownOperator},
		{"cast varchar to double", domain.Expression{Kind: domain.CastKind, CastType: "DOUBLE", Operand: ptrExpr(col("name"))}, errTypeIncompatible},
		{"cast to integer target", domain.Expression{Kind: domain.CastKind, CastType: "INTEGER", Operand: ptrExpr(col("amount"))}, errUnknownCast},
		{"unknown column", col("ghost"), errUnknownField},
		{"avg over bare varchar", col("name"), errTypeIncompatible},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if _, _, err := compileObjective(testTableFn, testCols(), "avg", c.expr, nil); !errors.Is(err, c.want) {
				t.Fatalf("expected %v, got %v", c.want, err)
			}
		})
	}
}

// TestCompileFiltersCoercedPredicates proves boolean/categorical/set filters compile
// to bound predicates (values as ? params, never interpolated) and that a numeric
// threshold still compiles unchanged.
func TestCompileFiltersCoercedPredicates(t *testing.T) {
	cases := []struct {
		name     string
		filter   domain.Constraint
		wantSQL  string
		wantArgs []any
	}{
		{"boolean eq", boolConstraint("flag", domain.Equal, true), `"flag" = ?`, []any{true}},
		{"string eq", strConstraint("name", domain.Equal, "Europa"), `"name" = ?`, []any{"Europa"}},
		{"string neq", strConstraint("name", domain.NotEqual, "Europa"), `"name" != ?`, []any{"Europa"}},
		{"string in set", strMembers("name", domain.In, "Europa", "Mars"), `"name" IN (?, ?)`, []any{"Europa", "Mars"}},
		{"string not_in set", strMembers("name", domain.NotIn, "Earth"), `"name" NOT IN (?)`, []any{"Earth"}},
		{"numeric threshold", domain.Constraint{Field: "amount", Op: domain.LessThanOrEqual, Value: 5}, `"amount" <= ?`, []any{float64(5)}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			preds, args, err := compileFilters(testCols(), []domain.Constraint{c.filter})
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if len(preds) != 1 || preds[0] != c.wantSQL {
				t.Fatalf("predicate = %v, want %q", preds, c.wantSQL)
			}
			if len(args) != len(c.wantArgs) {
				t.Fatalf("args = %v, want %v", args, c.wantArgs)
			}
			for i := range c.wantArgs {
				if args[i] != c.wantArgs[i] {
					t.Fatalf("arg %d = %v, want %v", i, args[i], c.wantArgs[i])
				}
			}
			// Every value is a bound ?, never interpolated into the predicate.
			for _, s := range []string{"Europa", "Mars", "Earth", "true"} {
				if strings.Contains(preds[0], s) {
					t.Fatalf("value %q interpolated into predicate: %q", s, preds[0])
				}
			}
		})
	}
}

// TestCompileFiltersRejections proves type-mismatched and malformed filters are
// rejected at compile time (400), not left to fail mid-scan.
func TestCompileFiltersRejections(t *testing.T) {
	cases := []struct {
		name   string
		filter domain.Constraint
		want   error
	}{
		{"string operand on numeric column", strConstraint("amount", domain.Equal, "young"), errTypeIncompatible},
		{"number operand on string column", numConstraint("name", domain.Equal, 5), errTypeIncompatible},
		{"numeric threshold on string column", domain.Constraint{Field: "name", Op: domain.LessThan, Value: 5}, errNonNumeric},
		{"empty in set", domain.Constraint{Field: "name", Op: domain.In}, errTypeIncompatible},
		{"member type mismatch", strMembers("amount", domain.In, "x"), errTypeIncompatible},
		{"unknown filter column", strConstraint("ghost", domain.Equal, "x"), errUnknownField},
		{"operator in no class", domain.Constraint{Field: "amount", Op: domain.ConstraintOp("bogus"), Value: 1}, errUnknownOperator},
		{"equality with nil operand", domain.Constraint{Field: "name", Op: domain.Equal}, errTypeIncompatible},
		{"membership with empty-literal member", domain.Constraint{Field: "name", Op: domain.In, Members: []domain.LiteralValue{{}}}, errTypeIncompatible},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if _, _, err := compileFilters(testCols(), []domain.Constraint{c.filter}); !errors.Is(err, c.want) {
				t.Fatalf("error = %v, want %v", err, c.want)
			}
		})
	}
}

// TestCompileQueryBindsThresholdsAndLiteralPath guards the injection contract:
// every threshold value is a bound ?, none is interpolated into the SQL, and the
// file path appears only as the escaped literal inside the table function.
func TestCompileQueryBindsThresholdsAndLiteralPath(t *testing.T) {
	filters := []domain.Constraint{
		{Field: "qty", Op: domain.GreaterThan, Value: 10},
		{Field: "amount", Op: domain.LessThanOrEqual, Value: 999.5},
	}
	sql, args, err := compileQuery(testTableFn, testCols(), "avg", target("amount"), filters)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(args) != 2 || args[0].(float64) != 10 || args[1].(float64) != 999.5 {
		t.Fatalf("thresholds not bound in order: %v", args)
	}
	if strings.Contains(sql, "10") || strings.Contains(sql, "999.5") {
		t.Fatalf("threshold value interpolated into SQL: %q", sql)
	}
	if strings.Count(sql, "?") != 2 {
		t.Fatalf("expected two bound placeholders, got %q", sql)
	}
	if !strings.Contains(sql, testTableFn) {
		t.Fatalf("file path not embedded as table-function literal: %q", sql)
	}
	// The staged path must not appear as a bound parameter.
	for _, a := range args {
		if s, ok := a.(string); ok && strings.Contains(s, "source.csv") {
			t.Fatalf("file path leaked into bound args: %v", args)
		}
	}
}
