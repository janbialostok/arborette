package sandbox

import (
	"strings"
	"testing"

	"github.com/arborette/arborette/internal/domain"
)

// TestRowCountColumnIsOptIn pins the seam's whole point: with the count off, the
// emitted SQL is byte-identical to the legacy single-column path. A difference
// here means the count leaked into the default path.
func TestRowCountColumnIsOptIn(t *testing.T) {
	filters := []domain.Constraint{{Field: "qty", Op: domain.GreaterThan, Value: 1}}

	t.Run("query path", func(t *testing.T) {
		legacy, _, err := compileQuery(testTableFn, testCols(), "avg", target("amount"), filters)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		uncounted, _, err := compileQueryCounted(testTableFn, testCols(), "avg", target("amount"), filters, false)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if uncounted != legacy {
			t.Fatalf("uncounted SQL drifted from the legacy path:\n got %q\nwant %q", uncounted, legacy)
		}

		counted, _, err := compileQueryCounted(testTableFn, testCols(), "avg", target("amount"), filters, true)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		assertCountedShape(t, counted, legacy)
	})

	t.Run("objective path", func(t *testing.T) {
		expr := cmp("=", col("name"), strLit("gold"))
		legacy, _, err := compileObjective(testTableFn, testCols(), "avg", expr, filters)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		uncounted, _, err := compileObjectiveCounted(testTableFn, testCols(), "avg", expr, filters, false)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if uncounted != legacy {
			t.Fatalf("uncounted SQL drifted from the legacy path:\n got %q\nwant %q", uncounted, legacy)
		}

		counted, args, err := compileObjectiveCounted(testTableFn, testCols(), "avg", expr, filters, true)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		assertCountedShape(t, counted, legacy)
		// The count rides the same scan, so the bound args are unchanged.
		if len(args) != 2 || args[0].(string) != "gold" || args[1].(float64) != 1 {
			t.Fatalf("counting must not disturb the bound args, got %v", args)
		}
	})
}

// assertCountedShape checks the counted SQL is the legacy SQL with exactly one
// extra column: same table function, same WHERE clause, one scan.
func assertCountedShape(t *testing.T, counted, legacy string) {
	t.Helper()
	if !strings.Contains(counted, ", CAST(count(*) AS BIGINT) FROM ") {
		t.Fatalf("expected a second count column before FROM: %q", counted)
	}
	if strings.Count(counted, " FROM ") != 1 {
		t.Fatalf("the count must ride the same scan, not a second one: %q", counted)
	}
	legacyWhere := whereClause(legacy)
	if whereClause(counted) != legacyWhere {
		t.Fatalf("the count must be measured under the same predicates:\n got %q\nwant %q",
			whereClause(counted), legacyWhere)
	}
	if uncount := strings.Replace(counted, ", CAST(count(*) AS BIGINT)", "", 1); uncount != legacy {
		t.Fatalf("counted SQL differs from legacy beyond the count column:\n got %q\nwant %q", uncount, legacy)
	}
}

func whereClause(sql string) string {
	i := strings.Index(sql, " WHERE ")
	if i < 0 {
		return ""
	}
	return sql[i:]
}
