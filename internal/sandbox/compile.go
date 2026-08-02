package sandbox

import (
	"errors"
	"fmt"
	"math"
	"strconv"
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

// allowedWindowAgg maps a trailing-aggregate window's function name to the SQL
// function emitted, following allowedAgg's allowlist convention -- a window
// aggregate name never reaches the SQL string uninterpreted.
var allowedWindowAgg = map[string]string{
	"avg":   "AVG",
	"sum":   "SUM",
	"min":   "MIN",
	"max":   "MAX",
	"count": "COUNT",
}

// allowedOp maps a numeric-threshold filter operator to a fixed SQL comparison.
// Only these four operators can appear in a compiled threshold predicate.
var allowedOp = map[domain.ConstraintOp]string{
	domain.LessThan:           "<",
	domain.LessThanOrEqual:    "<=",
	domain.GreaterThan:        ">",
	domain.GreaterThanOrEqual: ">=",
}

// allowedEqOp maps a scalar-equality filter operator to its SQL comparison; the
// operand is a bound ? parameter, never interpolated.
var allowedEqOp = map[domain.ConstraintOp]string{
	domain.Equal:    "=",
	domain.NotEqual: "!=",
}

// allowedMembershipOp maps a set-membership filter operator to its SQL form; each
// member is a bound ? parameter inside the IN/NOT IN list.
var allowedMembershipOp = map[domain.ConstraintOp]string{
	domain.In:    "IN",
	domain.NotIn: "NOT IN",
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

// integerTypes is the DuckDB integer set (numericTypes minus the floating-point
// and fixed-point kinds). Distinct-value probing is restricted to these plus
// text and boolean columns, so a FLOAT/DOUBLE/DECIMAL column -- effectively
// continuous, never under a low-cardinality cap -- is not probed.
var integerTypes = map[string]bool{
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
}

// isProbeableForDistinctValues reports whether a column's type is in the
// low-cardinality categorical surface value grounding probes: text, boolean, or
// integer. Floating-point, fixed-point, and temporal columns are effectively
// continuous, so they are never probed for distinct values.
func isProbeableForDistinctValues(t string) bool {
	u := strings.ToUpper(strings.TrimSpace(t))
	if integerTypes[u] {
		return true
	}
	switch columnCoarseType(u) {
	case typeBoolean, typeString:
		return true
	default:
		return false
	}
}

// staticValidate rejects a request whose aggregation, operators, cast targets, or
// literals the deterministic compiler will not emit SQL for -- everything checkable
// without the introspected schema. It lets handleExecute return a 400 before any
// object-store Get, so a malformed request never pays a staging download. Column
// name/type checks intrinsically need the schema and stay post-staging in
// compileQueryCounted; this is deliberately a subset, reusing the same sentinels so
// the HTTP status mapping needs no new arms.
func staticValidate(agg string, expr *domain.Expression, filters []domain.Constraint) error {
	if _, ok := allowedAgg[strings.ToLower(agg)]; !ok {
		return fmt.Errorf("%w: %q", errUnknownAggregation, agg)
	}
	if expr != nil {
		if err := staticValidateExpr(*expr); err != nil {
			return err
		}
		// The window shape guard (no window nested under a window; offset/size in
		// range) is CGO-free domain logic reused here so the sandbox does not trust
		// the orchestrator to have run it. Its sentinels map to no HTTP status, so the
		// rejection is re-wrapped as a compile error the 400 mapping already covers.
		if err := domain.ValidateWindowShape(*expr); err != nil {
			return fmt.Errorf("%w: %v", errTypeIncompatible, err)
		}
	}
	for _, f := range filters {
		if err := staticValidateFilter(f); err != nil {
			return err
		}
	}
	return nil
}

// staticValidateExpr walks an objective value expression for the schema-independent
// violations -- unallowed comparison/arithmetic operators, unallowed cast targets,
// malformed or non-finite literals -- mirroring compileExpr's structure but without
// resolving columns or checking types (both of which need the schema).
func staticValidateExpr(e domain.Expression) error {
	switch e.Kind {
	case domain.ColumnRefKind:
		return nil
	case domain.LiteralKind:
		return staticValidateLiteral(e.Literal)
	case domain.CastKind:
		castType := strings.ToUpper(strings.TrimSpace(e.CastType))
		if _, ok := allowedCast[castType]; !ok {
			return fmt.Errorf("%w: %q", errUnknownCast, e.CastType)
		}
		return staticValidateExpr(child(e.Operand))
	case domain.ComparisonKind:
		if !allowedCompareOp[e.Op] {
			return fmt.Errorf("%w: %q", errUnknownOperator, e.Op)
		}
		if err := staticValidateExpr(child(e.Left)); err != nil {
			return err
		}
		return staticValidateExpr(child(e.Right))
	case domain.ArithmeticKind:
		if !allowedArithOp[e.Op] {
			return fmt.Errorf("%w: %q", errUnknownOperator, e.Op)
		}
		if err := staticValidateExpr(child(e.Left)); err != nil {
			return err
		}
		return staticValidateExpr(child(e.Right))
	case domain.CaseKind:
		if len(e.Cases) == 0 {
			return fmt.Errorf("%w: case with no branches", errTypeIncompatible)
		}
		for _, br := range e.Cases {
			if err := staticValidateExpr(child(br.When)); err != nil {
				return err
			}
			if err := staticValidateExpr(child(br.Then)); err != nil {
				return err
			}
		}
		if e.Else != nil {
			return staticValidateExpr(child(e.Else))
		}
		return nil
	case domain.LagKind:
		return staticValidateExpr(child(e.Inner))
	case domain.TrailingAggregateKind:
		if _, ok := allowedWindowAgg[strings.ToLower(e.WindowAgg)]; !ok {
			return fmt.Errorf("%w: window aggregate %q", errUnknownAggregation, e.WindowAgg)
		}
		return staticValidateExpr(child(e.Inner))
	default:
		return fmt.Errorf("%w: unknown expression kind %q", errTypeIncompatible, e.Kind)
	}
}

// staticValidateFilter checks one intervention filter for the schema-independent
// violations: an unknown operator, an empty membership set, or a non-finite
// threshold/operand/member literal. The column-vs-operand type coherence check
// needs the schema and stays in compileFilter.
func staticValidateFilter(f domain.Constraint) error {
	switch {
	case f.IsNumericThresholdOp():
		if math.IsInf(f.Value, 0) || math.IsNaN(f.Value) {
			return fmt.Errorf("%w: filter %q threshold %v", errNonFiniteValue, f.Field, f.Value)
		}
		return nil
	case f.IsEqualityOp():
		return staticValidateLiteral(f.Operand)
	case f.IsMembershipOp():
		if len(f.Members) == 0 {
			return fmt.Errorf("%w: filter %q has an empty %s set", errTypeIncompatible, f.Field, f.Op)
		}
		for i := range f.Members {
			if err := staticValidateLiteral(&f.Members[i]); err != nil {
				return err
			}
		}
		return nil
	default:
		return fmt.Errorf("%w: %q", errUnknownOperator, f.Op)
	}
}

// staticValidateLiteral rejects a malformed literal (per literalValue's one-field
// rule) or a non-finite numeric literal, which json.Marshal could not encode and
// which the non-finite scan guard would otherwise only catch after staging.
func staticValidateLiteral(l *domain.LiteralValue) error {
	val, _, err := literalValue(l)
	if err != nil {
		return err
	}
	if n, ok := val.(float64); ok && (math.IsInf(n, 0) || math.IsNaN(n)) {
		return fmt.Errorf("%w: literal %v", errNonFiniteValue, n)
	}
	return nil
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
	return compileQueryCounted(tableFn, cols, agg, target, filters, false)
}

// compileQueryCounted is compileQuery with an opt-in matched-row count measured
// in the same scan. With withCount false it emits byte-identical SQL to
// compileQuery, so the legacy single-column path is unaffected.
func compileQueryCounted(tableFn string, cols []datasource.Column, agg string, target domain.Target, filters []domain.Constraint, withCount bool) (string, []any, error) {
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

	return aggregateSelect(sqlAgg, quoteIdent(targetCol.Name), tableFn, predicates, withCount), args, nil
}

// aggregateSelect assembles the read-only aggregate SELECT both execute paths
// emit: the aggregated operand cast to DOUBLE for a uniform scalar scan, over the
// server-built table function, under the compiled hard-constraint predicates.
// withCount appends count(*) as a second column over those same predicates, so a
// caller measuring support pays one scan rather than a second round-trip.
func aggregateSelect(sqlAgg, operand, tableFn string, predicates []string, withCount bool) string {
	query := "SELECT CAST(" + sqlAgg + "(" + operand + ") AS DOUBLE)"
	if withCount {
		query += ", CAST(count(*) AS BIGINT)"
	}
	query += " FROM " + tableFn
	if len(predicates) > 0 {
		query += " WHERE " + strings.Join(predicates, " AND ")
	}
	return query
}

// compileFilters compiles the intervention filters shared by the legacy and
// expression execute paths into quoted, bound predicates. Each filter column is
// schema-validated; each filter compiles by operator class (numeric threshold,
// scalar equality, or set membership) with every value bound as a ? parameter,
// never interpolated. Shared by compileQuery (legacy) and compileObjective, so both
// execute paths gain the boolean/categorical/set capability.
func compileFilters(cols []datasource.Column, filters []domain.Constraint) ([]string, []any, error) {
	var predicates []string
	var args []any
	for _, f := range filters {
		filterCol, err := resolveColumn(cols, f.Field)
		if err != nil {
			return nil, nil, err
		}
		pred, filterArgs, err := compileFilter(filterCol, f)
		if err != nil {
			return nil, nil, err
		}
		predicates = append(predicates, pred)
		args = append(args, filterArgs...)
	}
	return predicates, args, nil
}

// compileFilter compiles one filter into a bound predicate, coercing by operator
// class the way compileExpr's comparison node does. A numeric threshold requires a
// numeric column (as before) and binds Value; an eq/neq equality binds the typed
// Operand after a coarse-type-equality check against the column; an in/not_in
// membership binds each typed Member after the same check, rejecting an empty set.
// The coarse-type reject (not checkCastable, which would permit e.g. numeric→boolean)
// is the compile-time guard that keeps a mismatched operand — Age = 'young' or
// HomePlanet < 5 — from reaching DuckDB as a mid-scan cast error.
func compileFilter(col datasource.Column, f domain.Constraint) (string, []any, error) {
	switch {
	case f.IsNumericThresholdOp():
		if !isNumeric(col.Type) {
			return "", nil, fmt.Errorf("%w: filter %q (%s)", errNonNumeric, col.Name, col.Type)
		}
		return quoteIdent(col.Name) + " " + allowedOp[f.Op] + " ?", []any{f.Value}, nil

	case f.IsEqualityOp():
		val, valType, err := literalValue(f.Operand)
		if err != nil {
			return "", nil, err
		}
		colType := columnCoarseType(col.Type)
		if valType != colType {
			return "", nil, fmt.Errorf("%w: filter %q (%s) %s %s operand", errTypeIncompatible, col.Name, colType, f.Op, valType)
		}
		return quoteIdent(col.Name) + " " + allowedEqOp[f.Op] + " ?", []any{val}, nil

	case f.IsMembershipOp():
		if len(f.Members) == 0 {
			return "", nil, fmt.Errorf("%w: filter %q has an empty %s set", errTypeIncompatible, col.Name, f.Op)
		}
		colType := columnCoarseType(col.Type)
		placeholders := make([]string, 0, len(f.Members))
		args := make([]any, 0, len(f.Members))
		for i := range f.Members {
			val, valType, err := literalValue(&f.Members[i])
			if err != nil {
				return "", nil, err
			}
			if valType != colType {
				return "", nil, fmt.Errorf("%w: filter %q (%s) %s %s member", errTypeIncompatible, col.Name, colType, f.Op, valType)
			}
			placeholders = append(placeholders, "?")
			args = append(args, val)
		}
		return quoteIdent(col.Name) + " " + allowedMembershipOp[f.Op] + " (" + strings.Join(placeholders, ", ") + ")", args, nil

	default:
		return "", nil, fmt.Errorf("%w: %q", errUnknownOperator, f.Op)
	}
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
	return compileObjectiveCounted(tableFn, cols, agg, expr, filters, false, "", "")
}

// compileObjectiveCounted is compileObjective with an opt-in matched-row count
// measured in the same scan. With withCount false and no window kind it emits
// byte-identical SQL to compileObjective, so the legacy single-column path is
// unaffected. entityKey/timeColumn are the window bindings a windowed value
// expression compiles against; they are empty (and unused) for a plain objective.
func compileObjectiveCounted(tableFn string, cols []datasource.Column, agg string, expr domain.Expression, filters []domain.Constraint, withCount bool, entityKey, timeColumn string) (string, []any, error) {
	sqlAgg, ok := allowedAgg[strings.ToLower(agg)]
	if !ok {
		return "", nil, fmt.Errorf("%w: %q", errUnknownAggregation, agg)
	}

	cc := compileCtx{cols: cols, entityKey: entityKey, timeColumn: timeColumn}
	exprSQL, exprType, exprArgs, err := compileExpr(cc, expr)
	if err != nil {
		return "", nil, err
	}

	predicates, filterArgs, err := compileFilters(cols, filters)
	if err != nil {
		return "", nil, err
	}

	// A window function cannot nest inside the objective's aggregate call, so a
	// windowed value expression is measured in a two-level SELECT: the window over
	// full history inside, the aggregate and intervention filters outside.
	if domain.HasWindowKind(expr) {
		return compileWindowedObjective(sqlAgg, exprSQL, exprType, exprArgs, tableFn, cols, predicates, filterArgs, withCount)
	}

	measured, err := aggregateOperand(sqlAgg, exprSQL, exprType)
	if err != nil {
		return "", nil, err
	}
	args := append(exprArgs, filterArgs...)
	return aggregateSelect(sqlAgg, measured, tableFn, predicates, withCount), args, nil
}

// compileWindowedObjective assembles the two-level SELECT a windowed objective
// requires: the value expression (carrying its window fragments) is computed in an
// inner select over each entity's full ordered history and aliased to a
// collision-proof column; the objective aggregate, intervention predicates, and
// optional support count are applied in the outer select over that alias. DuckDB
// rejects a window function nested inside an aggregate call, which is why a single
// SELECT cannot measure it. Window-over-history (not over the filtered rows) is
// deliberate: a row's entity-relative value is intrinsic to its history, so the same
// row carries the same value in every segment, keeping parent/child effect sizes
// comparable. The inner select's ? args precede the outer predicates' args, matching
// their emission order.
func compileWindowedObjective(sqlAgg, exprSQL string, exprType coarseType, exprArgs []any, tableFn string, cols []datasource.Column, predicates []string, filterArgs []any, withCount bool) (string, []any, error) {
	alias := windowAlias(cols)
	measured, err := aggregateOperand(sqlAgg, quoteIdent(alias), exprType)
	if err != nil {
		return "", nil, err
	}
	inner := "(SELECT *, " + exprSQL + " AS " + quoteIdent(alias) + " FROM " + tableFn + ")"
	args := append(exprArgs, filterArgs...)
	return aggregateSelect(sqlAgg, measured, inner, predicates, withCount), args, nil
}

// windowAlias returns a column alias for the windowed inner select that cannot
// collide with a real column: user datasets are arbitrary, so a fixed literal could
// shadow an actual column and make the outer aggregate's reference ambiguous. It
// starts from a sentinel and suffixes an index until the name is absent from cols
// (case-insensitive).
func windowAlias(cols []datasource.Column) string {
	const base = "__arborette_window"
	candidate := base
	for i := 0; columnExists(cols, candidate); i++ {
		candidate = base + "_" + strconv.Itoa(i)
	}
	return candidate
}

func columnExists(cols []datasource.Column, name string) bool {
	for _, c := range cols {
		if strings.EqualFold(c.Name, name) {
			return true
		}
	}
	return false
}

// compileCtx carries the schema columns and the window bindings through the
// expression compiler, so the recursive descent does not grow two extra parameters
// at every node. entityKey and timeColumn are the raw request fields, resolved and
// quoted only where a window node needs them.
type compileCtx struct {
	cols       []datasource.Column
	entityKey  string
	timeColumn string
}

// windowClause resolves and quotes the entity/time bindings into the PARTITION BY
// and ORDER BY clauses every window fragment shares. Absent bindings are rejected
// defensively -- the execute handler already refuses a windowed request without them
// pre-staging, but the compiler must never emit a partition-less window.
//
// The time column alone is not a total order: tied time values leave peer rows in an
// unspecified order that, under DuckDB's parallel file scan, can vary between the
// independent per-segment re-executions -- so the same row could carry different
// lag/frame values in a parent vs a child measurement, breaking the comparability
// invariant. Every other column is appended as a deterministic tiebreaker, making
// the order total up to fully-identical rows (which carry identical window values
// regardless of order). It uses only real, quoted schema columns, so it works over
// both CSV and Parquet table functions, neither of which exposes a stable synthetic
// row id.
func (cc compileCtx) windowClause() (partition, order string, err error) {
	if cc.entityKey == "" || cc.timeColumn == "" {
		return "", "", fmt.Errorf("%w: windowed expression requires entity and time bindings", errTypeIncompatible)
	}
	entityCol, err := resolveColumn(cc.cols, cc.entityKey)
	if err != nil {
		return "", "", err
	}
	timeCol, err := resolveColumn(cc.cols, cc.timeColumn)
	if err != nil {
		return "", "", err
	}
	orderCols := []string{quoteIdent(timeCol.Name)}
	for _, c := range cc.cols {
		if strings.EqualFold(c.Name, timeCol.Name) {
			continue
		}
		orderCols = append(orderCols, quoteIdent(c.Name))
	}
	return "PARTITION BY " + quoteIdent(entityCol.Name), "ORDER BY " + strings.Join(orderCols, ", "), nil
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
func compileExpr(cc compileCtx, e domain.Expression) (string, coarseType, []any, error) {
	switch e.Kind {
	case domain.ColumnRefKind:
		col, err := resolveColumn(cc.cols, e.Column)
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
		operandSQL, operandType, args, err := compileExpr(cc, child(e.Operand))
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
		leftSQL, leftType, leftArgs, err := compileExpr(cc, child(e.Left))
		if err != nil {
			return "", 0, nil, err
		}
		rightSQL, rightType, rightArgs, err := compileExpr(cc, child(e.Right))
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
		leftSQL, leftType, leftArgs, err := compileExpr(cc, child(e.Left))
		if err != nil {
			return "", 0, nil, err
		}
		rightSQL, rightType, rightArgs, err := compileExpr(cc, child(e.Right))
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
		return compileCase(cc, e)

	case domain.LagKind:
		return compileLag(cc, e)

	case domain.TrailingAggregateKind:
		return compileTrailingAggregate(cc, e)

	default:
		return "", 0, nil, fmt.Errorf("%w: unknown expression kind %q", errTypeIncompatible, e.Kind)
	}
}

// compileLag compiles a lag window: the inner expression's value Offset rows back
// within the entity's ordered history. The offset is a function argument, bound as
// a ? param; the entity/time columns are schema-resolved and quoted, never request
// text. lag preserves the inner expression's coarse type. The inner ? args precede
// the offset arg, matching their emission order.
func compileLag(cc compileCtx, e domain.Expression) (string, coarseType, []any, error) {
	partition, order, err := cc.windowClause()
	if err != nil {
		return "", 0, nil, err
	}
	innerSQL, innerType, innerArgs, err := compileExpr(cc, child(e.Inner))
	if err != nil {
		return "", 0, nil, err
	}
	sql := "lag(" + innerSQL + ", ?) OVER (" + partition + " " + order + ")"
	return sql, innerType, append(innerArgs, e.Offset), nil
}

// compileTrailingAggregate compiles a trailing-aggregate window: an allowlisted
// aggregate of the inner expression over the WindowSize rows preceding the current
// one within the entity's ordered history. The aggregate name comes from the
// allowlist, never request text; avg/sum/min/max require a numeric inner (count
// admits any). The frame bound is emitted as a range-checked integer literal
// (validated in [1, MaxWindowSize] before compile), not a bound parameter, because
// frame-bound parameter binding is driver-dependent while a bounded integer is
// injection-safe. The result is always measured as numeric.
func compileTrailingAggregate(cc compileCtx, e domain.Expression) (string, coarseType, []any, error) {
	sqlAgg, ok := allowedWindowAgg[strings.ToLower(e.WindowAgg)]
	if !ok {
		return "", 0, nil, fmt.Errorf("%w: window aggregate %q", errUnknownAggregation, e.WindowAgg)
	}
	if e.WindowSize < 1 || e.WindowSize > domain.MaxWindowSize {
		return "", 0, nil, fmt.Errorf("%w: trailing window size %d not in [1, %d]", errTypeIncompatible, e.WindowSize, domain.MaxWindowSize)
	}
	partition, order, err := cc.windowClause()
	if err != nil {
		return "", 0, nil, err
	}
	innerSQL, innerType, innerArgs, err := compileExpr(cc, child(e.Inner))
	if err != nil {
		return "", 0, nil, err
	}
	if sqlAgg != "COUNT" && innerType != typeNumeric {
		return "", 0, nil, fmt.Errorf("%w: trailing %s over %s operand", errTypeIncompatible, sqlAgg, innerType)
	}
	frame := "ROWS BETWEEN " + strconv.Itoa(e.WindowSize) + " PRECEDING AND 1 PRECEDING"
	sql := sqlAgg + "(" + innerSQL + ") OVER (" + partition + " " + order + " " + frame + ")"
	return sql, typeNumeric, innerArgs, nil
}

// compileCase compiles a CASE/bucket node. A Comparison WHEN compiles to a 0/1
// indicator, which DuckDB accepts as a WHEN condition (nonzero is true); every
// THEN/ELSE must be numeric so the bucket maps to a measurable number.
func compileCase(cc compileCtx, e domain.Expression) (string, coarseType, []any, error) {
	if len(e.Cases) == 0 {
		return "", 0, nil, fmt.Errorf("%w: case with no branches", errTypeIncompatible)
	}
	var b strings.Builder
	var args []any
	b.WriteString("CASE")
	for _, br := range e.Cases {
		whenSQL, whenType, whenArgs, err := compileExpr(cc, child(br.When))
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
		thenSQL, thenType, thenArgs, err := compileExpr(cc, child(br.Then))
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
		elseSQL, elseType, elseArgs, err := compileExpr(cc, child(e.Else))
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
