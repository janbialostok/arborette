package sandbox

import (
	"errors"
	"fmt"
	"strings"

	"github.com/arborette/arborette/internal/datasource"
	"github.com/arborette/arborette/internal/domain"
)

// Sentinel errors for query-compilation failures, all mapped to 400 by the HTTP
// layer: the request named an aggregation, field, operator, cast, or type
// combination the deterministic compiler will not emit SQL for, or the measured
// value came back non-finite.
var (
	errUnknownAggregation = errors.New("unsupported aggregation")
	errUnknownField       = errors.New("field not present in schema")
	errAmbiguousField     = errors.New("field matches multiple columns")
	errUnknownOperator    = errors.New("unsupported constraint operator")
	errNonNumeric         = errors.New("numeric aggregation over non-numeric column")
	errTypeIncompatible   = errors.New("type-incompatible objective expression")
	errUnknownCast        = errors.New("unsupported cast target")
	errNonFiniteValue     = errors.New("non-finite objective value")
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

// allowedCompareOp is the Comparison node's operator allowlist, keyed by the raw
// SQL symbol the AST carries. It is deliberately separate from allowedOp (the
// hard-constraint map keyed by domain.ConstraintOp): the AST comparison operators
// are SQL symbols, so an operator never reaches SQL uninterpreted here either.
var allowedCompareOp = map[string]bool{
	"=":  true,
	"!=": true,
	"<":  true,
	"<=": true,
	">":  true,
	">=": true,
}

var allowedArithOp = map[string]bool{
	"+": true,
	"-": true,
	"*": true,
	"/": true,
}

// allowedCast is the user-specifiable set of Cast targets, mapped to the coarse
// type of the result. INTEGER is deliberately absent: a narrowing numeric->INTEGER
// cast of an out-of-range value raises a DuckDB conversion error mid-scan (masked
// to 500), so the only INTEGER cast is the compiler-emitted boolean->INTEGER at
// the aggregate boundary, over a 0/1 value that cannot overflow.
var allowedCast = map[string]coarseType{
	"DOUBLE":  typeNumeric,
	"VARCHAR": typeString,
	"BOOLEAN": typeBoolean,
}

// coarseType is the compiler's approximation of a DuckDB value's type: enough to
// decide casts and indicator wrapping and to reject clearly-invalid combinations
// up front, without a full type system. Every objective expression is ultimately
// measured as a DOUBLE.
type coarseType int

const (
	typeNumeric coarseType = iota
	typeBoolean
	typeString
	typeTemporal
)

func (t coarseType) String() string {
	switch t {
	case typeNumeric:
		return "numeric"
	case typeBoolean:
		return "boolean"
	case typeString:
		return "string"
	case typeTemporal:
		return "temporal"
	default:
		return "unknown"
	}
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

	predicates, args, err := compileFilters(cols, filters)
	if err != nil {
		return "", nil, err
	}

	return aggregateSelect(sqlAgg, quoteIdent(targetCol.Name), tableFn, predicates), args, nil
}

// aggregateSelect assembles the read-only aggregate SELECT both execute paths
// emit: the aggregated operand cast to DOUBLE for a uniform scalar scan, over the
// server-built table function, under the compiled hard-constraint predicates.
func aggregateSelect(sqlAgg, operand, tableFn string, predicates []string) string {
	query := "SELECT CAST(" + sqlAgg + "(" + operand + ") AS DOUBLE) FROM " + tableFn
	if len(predicates) > 0 {
		query += " WHERE " + strings.Join(predicates, " AND ")
	}
	return query
}

// compileFilters compiles the hard-constraint filters shared by the legacy and
// expression execute paths into quoted, bound predicates. Each filter column is
// schema-validated and numeric-guarded (a Constraint.Value is a float64, so a
// comparison against a non-numeric column would reach DuckDB as a runtime cast
// error); each threshold is a bound ? parameter, never interpolated.
func compileFilters(cols []datasource.Column, filters []domain.Constraint) ([]string, []any, error) {
	var predicates []string
	var args []any
	for _, f := range filters {
		filterCol, err := resolveColumn(cols, f.Field)
		if err != nil {
			return nil, nil, err
		}
		if !isNumeric(filterCol.Type) {
			return nil, nil, fmt.Errorf("%w: filter %q (%s)", errNonNumeric, filterCol.Name, filterCol.Type)
		}
		op, ok := allowedOp[f.Op]
		if !ok {
			return nil, nil, fmt.Errorf("%w: %q", errUnknownOperator, f.Op)
		}
		predicates = append(predicates, quoteIdent(filterCol.Name)+" "+op+" ?")
		args = append(args, f.Value)
	}
	return predicates, args, nil
}

// compileObjective produces the read-only aggregate SELECT that measures an
// objective value expression under the intervention filters, keyed for a single
// DOUBLE scan. It mirrors compileQuery's injection contract -- allowlisted
// aggregation, schema-validated columns, bound ? values -- but measures a compiled
// expression rather than a bare column, so boolean/categorical objectives that
// compileQuery rejects as non-numeric succeed here (a boolean expression is cast
// to INTEGER; a comparison is a 0/1 indicator). Expression args precede filter
// args because the expression is emitted before the WHERE clause and DuckDB binds
// ? positionally.
func compileObjective(tableFn string, cols []datasource.Column, agg string, expr domain.Expression, filters []domain.Constraint) (string, []any, error) {
	sqlAgg, ok := allowedAgg[strings.ToLower(agg)]
	if !ok {
		return "", nil, fmt.Errorf("%w: %q", errUnknownAggregation, agg)
	}

	exprSQL, exprType, args, err := compileExpr(cols, expr)
	if err != nil {
		return "", nil, err
	}
	measured, err := aggregateOperand(sqlAgg, exprSQL, exprType)
	if err != nil {
		return "", nil, err
	}

	predicates, filterArgs, err := compileFilters(cols, filters)
	if err != nil {
		return "", nil, err
	}
	args = append(args, filterArgs...)

	return aggregateSelect(sqlAgg, measured, tableFn, predicates), args, nil
}

// aggregateOperand adapts a compiled objective expression to its aggregation at
// the aggregate boundary. A boolean expression is cast to INTEGER so avg/sum
// measure it as 0/1 (the boolean-rate case). A string/temporal expression is
// rejected for every aggregation except COUNT (which counts rows of any type). A
// numeric expression is measured as-is.
func aggregateOperand(sqlAgg, exprSQL string, exprType coarseType) (string, error) {
	if sqlAgg == "COUNT" {
		return exprSQL, nil
	}
	switch exprType {
	case typeNumeric:
		return exprSQL, nil
	case typeBoolean:
		return "CAST(" + exprSQL + " AS INTEGER)", nil
	default:
		return "", fmt.Errorf("%w: %s objective under %s", errTypeIncompatible, exprType, sqlAgg)
	}
}

// compileExpr compiles one objective value-expression node to a DuckDB SQL
// fragment, its coarse type, and the ordered ? args it binds. It validates every
// ColumnRef against the introspected schema and rejects type-incompatible
// combinations up front, so a compiled fragment cannot fail at scan time for a
// reason the compiler could have seen. Args are appended in emission
// (left-to-right) order to match positional binding.
func compileExpr(cols []datasource.Column, e domain.Expression) (string, coarseType, []any, error) {
	switch e.Kind {
	case domain.ColumnRefKind:
		col, err := resolveColumn(cols, e.Column)
		if err != nil {
			return "", 0, nil, err
		}
		return quoteIdent(col.Name), columnCoarseType(col.Type), nil, nil

	case domain.LiteralKind:
		val, typ, err := literalValue(e.Literal)
		if err != nil {
			return "", 0, nil, err
		}
		return "?", typ, []any{val}, nil

	case domain.CastKind:
		castType := strings.ToUpper(strings.TrimSpace(e.CastType))
		target, ok := allowedCast[castType]
		if !ok {
			return "", 0, nil, fmt.Errorf("%w: %q", errUnknownCast, e.CastType)
		}
		operandSQL, operandType, args, err := compileExpr(cols, child(e.Operand))
		if err != nil {
			return "", 0, nil, err
		}
		if err := checkCastable(operandType, target); err != nil {
			return "", 0, nil, err
		}
		return "CAST(" + operandSQL + " AS " + castType + ")", target, args, nil

	case domain.ComparisonKind:
		if !allowedCompareOp[e.Op] {
			return "", 0, nil, fmt.Errorf("%w: %q", errUnknownOperator, e.Op)
		}
		leftSQL, leftType, leftArgs, err := compileExpr(cols, child(e.Left))
		if err != nil {
			return "", 0, nil, err
		}
		rightSQL, rightType, rightArgs, err := compileExpr(cols, child(e.Right))
		if err != nil {
			return "", 0, nil, err
		}
		if leftType != rightType {
			return "", 0, nil, fmt.Errorf("%w: comparison of %s and %s", errTypeIncompatible, leftType, rightType)
		}
		sql := "CASE WHEN (" + leftSQL + " " + e.Op + " " + rightSQL + ") THEN 1 ELSE 0 END"
		return sql, typeNumeric, append(leftArgs, rightArgs...), nil

	case domain.ArithmeticKind:
		if !allowedArithOp[e.Op] {
			return "", 0, nil, fmt.Errorf("%w: %q", errUnknownOperator, e.Op)
		}
		leftSQL, leftType, leftArgs, err := compileExpr(cols, child(e.Left))
		if err != nil {
			return "", 0, nil, err
		}
		rightSQL, rightType, rightArgs, err := compileExpr(cols, child(e.Right))
		if err != nil {
			return "", 0, nil, err
		}
		if leftType != typeNumeric || rightType != typeNumeric {
			return "", 0, nil, fmt.Errorf("%w: arithmetic over non-numeric operand", errTypeIncompatible)
		}
		// Compile in floating-point space: an integer overflow then yields +Inf,
		// which the non-finite scan guard catches as a 400, rather than a mid-scan
		// Out-of-Range error that would mask to a 500.
		sql := "(CAST(" + leftSQL + " AS DOUBLE) " + e.Op + " CAST(" + rightSQL + " AS DOUBLE))"
		return sql, typeNumeric, append(leftArgs, rightArgs...), nil

	case domain.CaseKind:
		return compileCase(cols, e)

	default:
		return "", 0, nil, fmt.Errorf("%w: unknown expression kind %q", errTypeIncompatible, e.Kind)
	}
}

// compileCase compiles a CASE/bucket node. A Comparison WHEN compiles to a 0/1
// indicator, which DuckDB accepts as a WHEN condition (nonzero is true); every
// THEN/ELSE must be numeric so the bucket maps to a measurable number.
func compileCase(cols []datasource.Column, e domain.Expression) (string, coarseType, []any, error) {
	if len(e.Cases) == 0 {
		return "", 0, nil, fmt.Errorf("%w: case with no branches", errTypeIncompatible)
	}
	var b strings.Builder
	var args []any
	b.WriteString("CASE")
	for _, br := range e.Cases {
		whenSQL, whenType, whenArgs, err := compileExpr(cols, child(br.When))
		if err != nil {
			return "", 0, nil, err
		}
		// A WHEN condition must be boolean-valued; a Comparison compiles to a 0/1
		// numeric indicator and a bare boolean column is boolean, both of which
		// DuckDB accepts. A string/temporal WHEN would raise a mid-scan conversion
		// error, so reject it here for the same reason every other node validates
		// its operands.
		if whenType != typeNumeric && whenType != typeBoolean {
			return "", 0, nil, fmt.Errorf("%w: case condition must be boolean, got %s", errTypeIncompatible, whenType)
		}
		thenSQL, thenType, thenArgs, err := compileExpr(cols, child(br.Then))
		if err != nil {
			return "", 0, nil, err
		}
		if thenType != typeNumeric {
			return "", 0, nil, fmt.Errorf("%w: case result must be numeric, got %s", errTypeIncompatible, thenType)
		}
		b.WriteString(" WHEN " + whenSQL + " THEN " + thenSQL)
		args = append(args, whenArgs...)
		args = append(args, thenArgs...)
	}
	if e.Else != nil {
		elseSQL, elseType, elseArgs, err := compileExpr(cols, child(e.Else))
		if err != nil {
			return "", 0, nil, err
		}
		if elseType != typeNumeric {
			return "", 0, nil, fmt.Errorf("%w: case else must be numeric, got %s", errTypeIncompatible, elseType)
		}
		b.WriteString(" ELSE " + elseSQL)
		args = append(args, elseArgs...)
	}
	b.WriteString(" END")
	return b.String(), typeNumeric, args, nil
}

// child dereferences an AST child, returning a zero Expression (which compiles to
// a clean type error, not a panic) when a node is missing a required operand.
func child(e *domain.Expression) domain.Expression {
	if e == nil {
		return domain.Expression{}
	}
	return *e
}

// literalValue extracts the bound value and coarse type of a Literal node; exactly
// one typed field must be set.
func literalValue(l *domain.LiteralValue) (any, coarseType, error) {
	switch {
	case l == nil:
		return nil, 0, fmt.Errorf("%w: empty literal", errTypeIncompatible)
	case l.Number != nil:
		return *l.Number, typeNumeric, nil
	case l.String != nil:
		return *l.String, typeString, nil
	case l.Bool != nil:
		return *l.Bool, typeBoolean, nil
	default:
		return nil, 0, fmt.Errorf("%w: empty literal", errTypeIncompatible)
	}
}

// checkCastable rejects a Cast whose operand cannot convert to the target without
// a possible mid-scan runtime error: string/temporal data to a numeric or boolean
// target fails per-row on dirty values, so it is rejected at compile time. Any
// operand casts to VARCHAR; numeric and boolean operands cast to DOUBLE/BOOLEAN.
func checkCastable(operand, target coarseType) error {
	if target == typeString {
		return nil
	}
	switch operand {
	case typeNumeric, typeBoolean:
		return nil
	default:
		return fmt.Errorf("%w: cannot cast %s to %s", errTypeIncompatible, operand, target)
	}
}

// columnCoarseType maps a DuckDB column type string to its coarse type. isNumeric
// already recognizes the numeric set (including DECIMAL by prefix); the rest are
// matched by prefix so DuckDB's parameterized spellings (VARCHAR(n), TIMESTAMP
// WITH TIME ZONE) still classify. An unrecognized type falls to string, which the
// aggregate boundary rejects for everything but COUNT.
func columnCoarseType(t string) coarseType {
	u := strings.ToUpper(strings.TrimSpace(t))
	switch {
	case isNumeric(u):
		return typeNumeric
	case u == "BOOLEAN" || u == "BOOL":
		return typeBoolean
	case strings.HasPrefix(u, "VARCHAR"), strings.HasPrefix(u, "CHAR"),
		strings.HasPrefix(u, "TEXT"), u == "BPCHAR", u == "STRING":
		return typeString
	case strings.HasPrefix(u, "DATE"), strings.HasPrefix(u, "TIME"),
		strings.HasPrefix(u, "TIMESTAMP"):
		return typeTemporal
	default:
		return typeString
	}
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
