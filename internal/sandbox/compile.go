package sandbox

import (
	"errors"
	"fmt"
	"strings"

	"github.com/arborette/arborette/internal/datasource"
	"github.com/arborette/arborette/internal/domain"
)

// Sentinel errors for query-compilation failures, all mapped to 400 by the HTTP
// layer: the request named an aggregation, field, operator, or column type the
// deterministic compiler will not emit SQL for.
var (
	errUnknownAggregation = errors.New("unsupported aggregation")
	errUnknownField       = errors.New("field not present in schema")
	errAmbiguousField     = errors.New("field matches multiple columns")
	errUnknownOperator    = errors.New("unsupported constraint operator")
	errNonNumeric         = errors.New("numeric aggregation over non-numeric column")
)

// allowedAgg maps a request aggregation to the SQL function emitted. Anything
// outside this allowlist is rejected -- an aggregation name never reaches the SQL
// string uninterpreted.
var allowedAgg = map[string]string{
	"avg":   "AVG",
	"sum":   "SUM",
	"min":   "MIN",
	"max":   "MAX",
	"count": "COUNT",
}

// allowedOp maps a hard-constraint operator to a fixed SQL comparison. Only these
// four operators can appear in a compiled predicate.
var allowedOp = map[domain.ConstraintOp]string{
	domain.LessThan:           "<",
	domain.LessThanOrEqual:    "<=",
	domain.GreaterThan:        ">",
	domain.GreaterThanOrEqual: ">=",
}

// numericTypes is the canonical DuckDB numeric set. DECIMAL is handled by prefix
// (DESCRIBE reports fixed-point columns as DECIMAL(p,s), common in Parquet) so an
// exact-string match would spuriously reject them.
var numericTypes = map[string]bool{
	"TINYINT":   true,
	"SMALLINT":  true,
	"INTEGER":   true,
	"BIGINT":    true,
	"HUGEINT":   true,
	"UTINYINT":  true,
	"USMALLINT": true,
	"UINTEGER":  true,
	"UBIGINT":   true,
	"UHUGEINT":  true,
	"FLOAT":     true,
	"DOUBLE":    true,
}

// compileQuery produces a single read-only aggregate SELECT and its bound args.
// Injection safety is the crux: SQL cannot parameterize identifiers, so the field
// names are validated against the introspected schema and only the schema-present
// column name is quoted into the SQL; the aggregation and operators come from
// allowlists; and every threshold value -- the only upstream-influenced input --
// is bound as a ? parameter, never interpolated. tableFn is the server-built
// read_csv_auto/read_parquet expression whose file path is already a literal (a
// server-generated temp path, not a bound parameter, since DuckDB resolves it at
// bind time). The aggregate is CAST(... AS DOUBLE) so every allowed aggregation
// scans back as one uniform DOUBLE/NULL.
func compileQuery(tableFn string, cols []datasource.Column, agg string, target domain.Target, filters []domain.Constraint) (string, []any, error) {
	sqlAgg, ok := allowedAgg[strings.ToLower(agg)]
	if !ok {
		return "", nil, fmt.Errorf("%w: %q", errUnknownAggregation, agg)
	}

	targetCol, err := resolveColumn(cols, target.Field)
	if err != nil {
		return "", nil, err
	}
	// count is defined over any column type; the numeric aggregations are guarded
	// because min/max over VARCHAR/DATE return that type and the CAST AS DOUBLE
	// would reject it as an opaque error.
	if sqlAgg != "COUNT" && !isNumeric(targetCol.Type) {
		return "", nil, fmt.Errorf("%w: %q over %q (%s)", errNonNumeric, agg, targetCol.Name, targetCol.Type)
	}

	var predicates []string
	var args []any
	for _, f := range filters {
		filterCol, err := resolveColumn(cols, f.Field)
		if err != nil {
			return "", nil, err
		}
		// Each Constraint.Value is a float64; a comparison against a non-numeric
		// column would reach DuckDB as a runtime cast error, so reject it up front.
		if !isNumeric(filterCol.Type) {
			return "", nil, fmt.Errorf("%w: filter %q (%s)", errNonNumeric, filterCol.Name, filterCol.Type)
		}
		op, ok := allowedOp[f.Op]
		if !ok {
			return "", nil, fmt.Errorf("%w: %q", errUnknownOperator, f.Op)
		}
		predicates = append(predicates, quoteIdent(filterCol.Name)+" "+op+" ?")
		args = append(args, f.Value)
	}

	query := "SELECT CAST(" + sqlAgg + "(" + quoteIdent(targetCol.Name) + ") AS DOUBLE) FROM " + tableFn
	if len(predicates) > 0 {
		query += " WHERE " + strings.Join(predicates, " AND ")
	}
	return query, args, nil
}

// matchColumns returns every schema column whose name case-insensitively equals
// field. Both the introspect target binding and the execute field resolution
// share this one rule so a field resolves identically at both endpoints; the
// callers differ only in how they treat the resulting match count.
func matchColumns(cols []datasource.Column, field string) []datasource.Column {
	var matches []datasource.Column
	for _, c := range cols {
		if strings.EqualFold(c.Name, field) {
			matches = append(matches, c)
		}
	}
	return matches
}

// resolveColumn resolves a request field to its single schema column. No match is
// an unknown-field error; a field matching more than one column (a case-colliding
// schema) is ambiguous. The resolved column's actual name is what gets quoted
// into SQL.
func resolveColumn(cols []datasource.Column, field string) (datasource.Column, error) {
	matches := matchColumns(cols, field)
	switch len(matches) {
	case 0:
		return datasource.Column{}, fmt.Errorf("%w: %q", errUnknownField, field)
	case 1:
		return matches[0], nil
	default:
		return datasource.Column{}, fmt.Errorf("%w: %q", errAmbiguousField, field)
	}
}

// isNumeric reports whether a DuckDB column type is aggregatable as a number,
// including any DECIMAL(p,s).
func isNumeric(t string) bool {
	u := strings.ToUpper(strings.TrimSpace(t))
	if numericTypes[u] {
		return true
	}
	return strings.HasPrefix(u, "DECIMAL")
}

// quoteIdent double-quotes a validated identifier, escaping embedded quotes.
func quoteIdent(name string) string {
	return `"` + strings.ReplaceAll(name, `"`, `""`) + `"`
}
