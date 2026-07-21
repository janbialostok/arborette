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
	}
}

func target(field string) domain.Target {
	return domain.Target{Field: field, Direction: domain.Maximize}
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
