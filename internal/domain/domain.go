// Package domain holds the shared data-model types every arborette service
// imports. It is intentionally dependency-free so any package or service can
// use it without pulling in graph, storage, or embedding concerns.
package domain

import (
	"strconv"
	"strings"
)

// InterventionType discriminates how an intervention is evaluated against a
// data source: a deterministic tabular query or a document extraction task.
type InterventionType string

const (
	InterventionQuery   InterventionType = "query"
	InterventionExtract InterventionType = "extract"
)

// VerificationStatus gates Sleep-Cycle clustering. query-type outcomes are
// always verified; extract-type outcomes start unverified and move to
// confirmed/corrected/rejected once a human resolves them. This status, not the
// PRODUCED-edge confidence weight, is what makes an outcome cluster-eligible.
type VerificationStatus string

const (
	VerificationVerified   VerificationStatus = "verified"
	VerificationUnverified VerificationStatus = "unverified"
	VerificationConfirmed  VerificationStatus = "confirmed"
	VerificationCorrected  VerificationStatus = "corrected"
	VerificationRejected   VerificationStatus = "rejected"
)

// SearchEligibleStatuses is the canonical set of verification statuses whose
// outcomes may feed the Sleep-Cycle search: query outcomes (always verified) and
// HITL-resolved extract outcomes an analyst affirmed. This status, never the
// PRODUCED-edge confidence weight, is the gate — confidence is a model
// self-report and is never proof of verification. It is a slice because the
// eligible-finding Cypher binds the set as a query parameter.
var SearchEligibleStatuses = []VerificationStatus{VerificationVerified, VerificationConfirmed, VerificationCorrected}

// EpistemicSource records how a PRODUCED edge's effect was established. V1 writes
// only observational — a measured correlation P(Outcome | Segment), not do-calculus
// causation. interventional is reserved for a future V2 interventional layer
// (physically-executed interventions, do-calculus edges) and is never written in
// the MVP; a physically-executed intervention likewise reuses the existing
// InterventionType field rather than adding a node-level field. Any consumer that
// reads this property treats an absent/empty value as observational and must not
// assume interventional exists.
type EpistemicSource string

const (
	EpistemicObservational  EpistemicSource = "observational"
	EpistemicInterventional EpistemicSource = "interventional"
)

// Relationship names for the graph edges. Kept as constants so both the graph
// implementation and its consumers reference one spelling.
const (
	PreConditionFor = "PRE_CONDITION_FOR"
	Produced        = "PRODUCED"
	AbstractedFrom  = "ABSTRACTED_FROM"
)

// Node property keys that cross a service boundary. PropNewFilters in particular
// is written by the hypothesis loop and read back by the Sleep-Cycle search to
// build its atom set; a divergence between the two spellings does not fail — the
// read yields no filters, so the search finds nothing to conjoin and every run
// reports a successful degenerate result. Naming them here makes that divergence
// a compile error instead.
const (
	PropNewFilters           = "new_filters"
	PropEffectiveFilters     = "effective_filters"
	PropObjectiveLabel       = "objective_label"
	PropObjectiveAggregation = "objective_aggregation"
	PropDataSourceRef        = "data_source_ref"
	PropSupport              = "support"
)

// State is a snapshot/telemetry point in time. GoalID scopes it to the
// optimization function it was measured for.
type State struct {
	ID         string
	GoalID     string
	Properties map[string]any
}

// Intervention is reified action metadata (configuration change, execution
// metadata, confidence bounds). GoalID scopes it to its optimization function;
// SleepDerived marks a macro-segment the Sleep-Cycle search produced, which is a
// search output rather than an atomic input and is therefore excluded from a
// later run's search space so conjunctions are never double-counted.
type Intervention struct {
	ID           string
	GoalID       string
	Type         InterventionType
	SleepDerived bool
	Properties   map[string]any
}

// ProvenanceLocator pins an extract-type outcome's value to the exact source
// excerpt it came from: a 0-based page index and the byte offsets (not rune
// offsets) of the value within that page's extracted text. Nil when no exact
// match is found in the source text.
type ProvenanceLocator struct {
	Page      int
	CharStart int
	CharEnd   int
}

// Outcome is a measured delta from an intervention, evaluated against the
// analyst's Evaluation Matrix.
type Outcome struct {
	ID                 string
	GoalID             string
	VerificationStatus VerificationStatus
	Value              map[string]any
	// Support is the matched-row count of the measurement that produced this
	// outcome. 0 means unrecorded (legacy or extraction outcomes); consumers
	// must treat absent/zero as below any positive support floor.
	Support int64
	// Provenance is set only for extract-type outcomes; nil for query-type or
	// when no exact source match was located.
	Provenance *ProvenanceLocator
}

// MetaHeuristic is a semantic abstraction produced during the Sleep Cycle. Its
// embedding lives in pgvector keyed by ID; EmbeddingPending is true from node
// creation until the pgvector write succeeds. Stale marks a heuristic whose
// supporting evidence was since rejected by an analyst.
type MetaHeuristic struct {
	ID               string
	Definition       string
	EmbeddingPending bool
	Stale            bool
}

// ProducedEdge carries the measured effect size and a self-reported confidence
// weight (0.0–1.0) on an Intervention→Outcome edge, plus the epistemic provenance
// of that effect (see EpistemicSource).
type ProducedEdge struct {
	EffectSize      float64
	Confidence      float64
	EpistemicSource EpistemicSource
}

// EvaluationMatrix is the structured form of an analyst's plain-English goal:
// targets to maximize/minimize plus hard-constraint boundaries. Stored as jsonb
// in the goal registry.
type EvaluationMatrix struct {
	Targets     []Target     `json:"targets"`
	Constraints []Constraint `json:"constraints"`
}

// TargetField is one field a document goal extracts. A document goal has no
// aggregation or direction to optimize -- its objective is a set of fields to
// extract accurately -- so target fields are the document analog of an
// EvaluationMatrix's targets, and a document goal persists these instead of a
// matrix. Stored as jsonb in the goal registry.
type TargetField struct {
	Name        string `json:"name"`
	Description string `json:"description"`
}

// TargetDirection is whether a target should be maximized or minimized.
type TargetDirection string

const (
	Maximize TargetDirection = "maximize"
	Minimize TargetDirection = "minimize"
)

// Target is a single optimization objective. Aggregation and Value let an
// objective measure a structured expression rather than a bare column; both are
// omitempty so a field-only target deserializes unchanged and re-marshals
// identically (rows persisted as {field, direction} round-trip).
type Target struct {
	Field       string          `json:"field"`
	Direction   TargetDirection `json:"direction"`
	Aggregation string          `json:"aggregation,omitempty"`
	Value       *Expression     `json:"value,omitempty"`
}

// ValueExpression resolves the target's measured value expression. A target
// carrying an explicit Value uses it; a legacy field-only target degenerates to a
// bare ColumnRef over Field. Routing both forms through one accessor keeps the
// legacy row untouched on disk (no custom UnmarshalJSON rewriting it) while giving
// the compiler a uniform Expression to consume.
func (t Target) ValueExpression() Expression {
	if t.Value != nil {
		return *t.Value
	}
	return Expression{Kind: ColumnRefKind, Column: t.Field}
}

// ExpressionKind discriminates the node type of an objective value Expression.
type ExpressionKind string

const (
	ColumnRefKind  ExpressionKind = "column_ref"
	LiteralKind    ExpressionKind = "literal"
	CastKind       ExpressionKind = "cast"
	ComparisonKind ExpressionKind = "comparison"
	ArithmeticKind ExpressionKind = "arithmetic"
	CaseKind       ExpressionKind = "case"
)

// LiteralValue is the typed scalar carried by a LiteralKind Expression. Exactly
// one field is set; the set field is both the compiler's coarse-type signal and
// the value bound as a parameter, so a literal never reaches SQL uninterpreted.
type LiteralValue struct {
	Number *float64 `json:"number,omitempty"`
	String *string  `json:"string,omitempty"`
	Bool   *bool    `json:"bool,omitempty"`
}

// CaseBranch is one WHEN/THEN arm of a CaseKind Expression.
type CaseBranch struct {
	When *Expression `json:"when"`
	Then *Expression `json:"then"`
}

// Expression is the objective value-expression AST: a discriminated union over
// ExpressionKind, represented as one flat "fat node" so stdlib encoding/json
// (un)marshals it without a custom marshaler -- Kind selects which omitempty
// fields are populated. A bare ColumnRef is the degenerate "measure this column"
// objective; the other kinds express a computed value (a boolean/categorical
// indicator, a rate, a bucket) that no single column name describes. The Sandbox
// compiles it to a DuckDB SQL expression; the model never emits raw SQL.
type Expression struct {
	Kind ExpressionKind `json:"kind"`

	// ColumnRef
	Column string `json:"column,omitempty"`
	// Literal
	Literal *LiteralValue `json:"literal,omitempty"`
	// Cast
	Operand  *Expression `json:"operand,omitempty"`
	CastType string      `json:"cast_type,omitempty"`
	// Comparison / Arithmetic
	Op    string      `json:"op,omitempty"`
	Left  *Expression `json:"left,omitempty"`
	Right *Expression `json:"right,omitempty"`
	// Case
	Cases []CaseBranch `json:"cases,omitempty"`
	Else  *Expression  `json:"else,omitempty"`
}

// RenderObjectiveLabel derives the human-readable key an objective is carried
// under end to end. A compiled expression has no single column name, so the
// aggregation and expression render into one stable string (e.g.
// "avg(Transported = True)") that keys the execute request and its response
// value. Deterministic and dependency-free so the orchestrator (CGO-free) and the
// sandbox derive the same label.
func RenderObjectiveLabel(agg string, expr Expression) string {
	return agg + "(" + renderExpr(expr) + ")"
}

func renderExpr(e Expression) string {
	switch e.Kind {
	case ColumnRefKind:
		return e.Column
	case LiteralKind:
		return renderLiteral(e.Literal)
	case CastKind:
		return renderChild(e.Operand) + "::" + e.CastType
	case ComparisonKind, ArithmeticKind:
		return renderChild(e.Left) + " " + e.Op + " " + renderChild(e.Right)
	case CaseKind:
		var b strings.Builder
		b.WriteString("CASE")
		for _, br := range e.Cases {
			b.WriteString(" WHEN " + renderChild(br.When) + " THEN " + renderChild(br.Then))
		}
		if e.Else != nil {
			b.WriteString(" ELSE " + renderChild(e.Else))
		}
		b.WriteString(" END")
		return b.String()
	default:
		return ""
	}
}

func renderChild(e *Expression) string {
	if e == nil {
		return ""
	}
	return renderExpr(*e)
}

func renderLiteral(l *LiteralValue) string {
	switch {
	case l == nil:
		return ""
	case l.Number != nil:
		return strconv.FormatFloat(*l.Number, 'g', -1, 64)
	case l.String != nil:
		return *l.String
	case l.Bool != nil:
		if *l.Bool {
			return "True"
		}
		return "False"
	default:
		return ""
	}
}

// RenderConstraint renders a constraint as a human-readable predicate chip (e.g.
// "Age <= 100", "HomePlanet = Europa", "HomePlanet IN {Europa, Mars}").
// Deterministic and dependency-free, mirroring renderExpr/renderLiteral, so the
// orchestrator's SSE payload and the llm package's prompt summaries render a filter
// identically.
func RenderConstraint(c Constraint) string {
	op := renderConstraintOp(c.Op)
	switch {
	case c.IsMembershipOp():
		return c.Field + " " + op + " {" + renderMembers(c.Members) + "}"
	case c.IsEqualityOp():
		return c.Field + " " + op + " " + renderLiteral(c.Operand)
	default:
		return c.Field + " " + op + " " + strconv.FormatFloat(c.Value, 'g', -1, 64)
	}
}

func renderConstraintOp(op ConstraintOp) string {
	switch op {
	case LessThan:
		return "<"
	case LessThanOrEqual:
		return "<="
	case GreaterThan:
		return ">"
	case GreaterThanOrEqual:
		return ">="
	case Equal:
		return "="
	case NotEqual:
		return "!="
	case In:
		return "IN"
	case NotIn:
		return "NOT IN"
	default:
		return string(op)
	}
}

func renderMembers(members []LiteralValue) string {
	parts := make([]string, 0, len(members))
	for i := range members {
		parts = append(parts, renderLiteral(&members[i]))
	}
	return strings.Join(parts, ", ")
}

// MaxObjectiveExpressionDepth caps how deeply an objective value expression may
// nest. Real objectives are shallow — the deepest expected shape is avg over a
// case-bucket over a comparison over a column_ref, which ExpressionDepth counts as
// 3 — so 6 is generous headroom while still rejecting a pathological tree the
// output schema does not bound (it carries the expression as a plain string).
const MaxObjectiveExpressionDepth = 6

// ExpressionDepth returns the maximum nesting depth of the value-expression AST,
// counting a leaf (column_ref, literal) as 1 and each composite as one more than
// its deepest child. It is CGO-free so the orchestrator can depth-guard a decoded
// objective before dispatching it to the sandbox — the deterministic check that
// grounds the loosened JSON-string value.
func ExpressionDepth(e Expression) int {
	switch e.Kind {
	case CastKind:
		return 1 + childDepth(e.Operand)
	case ComparisonKind, ArithmeticKind:
		return 1 + max(childDepth(e.Left), childDepth(e.Right))
	case CaseKind:
		deepest := childDepth(e.Else)
		for _, br := range e.Cases {
			deepest = max(deepest, max(childDepth(br.When), childDepth(br.Then)))
		}
		return 1 + deepest
	default:
		return 1
	}
}

func childDepth(e *Expression) int {
	if e == nil {
		return 0
	}
	return ExpressionDepth(*e)
}

// ConstraintOp expresses a comparison used by both the matrix's numeric
// hard-constraint boundaries (lt/lte/gt/gte only) and the intervention filters,
// which additionally segment boolean/categorical columns via equality (eq/neq)
// and set membership (in/not_in).
type ConstraintOp string

const (
	LessThan           ConstraintOp = "lt"
	LessThanOrEqual    ConstraintOp = "lte"
	GreaterThan        ConstraintOp = "gt"
	GreaterThanOrEqual ConstraintOp = "gte"
	Equal              ConstraintOp = "eq"
	NotEqual           ConstraintOp = "neq"
	In                 ConstraintOp = "in"
	NotIn              ConstraintOp = "not_in"
)

// Constraint is a hard boundary a candidate intervention must respect. A numeric
// threshold uses Value (Op ∈ lt/lte/gt/gte); a boolean/categorical equality uses
// Operand (Op ∈ eq/neq); a set membership uses Members (Op ∈ in/not_in). Value
// stays present (not omitempty) so a legacy numeric-threshold constraint round-trips
// byte-identical; Operand/Members are omitempty so they are absent unless set.
type Constraint struct {
	Field   string         `json:"field"`
	Op      ConstraintOp   `json:"op"`
	Value   float64        `json:"value"`
	Operand *LiteralValue  `json:"operand,omitempty"`
	Members []LiteralValue `json:"members,omitempty"`
}

// IsNumericThresholdOp reports whether the operator is a numeric threshold
// (lt/lte/gt/gte).
func (c Constraint) IsNumericThresholdOp() bool {
	return c.Op == LessThan || c.Op == LessThanOrEqual || c.Op == GreaterThan || c.Op == GreaterThanOrEqual
}

// IsEqualityOp reports whether the operator is a scalar equality (eq/neq).
func (c Constraint) IsEqualityOp() bool {
	return c.Op == Equal || c.Op == NotEqual
}

// IsMembershipOp reports whether the operator is set membership (in/not_in).
func (c Constraint) IsMembershipOp() bool {
	return c.Op == In || c.Op == NotIn
}

// Aggregations is the canonical set of aggregate functions a query intervention
// may measure its objective with; the Sandbox accepts exactly these. It is the
// single source both the structured-output schema enum and the objective
// validator derive from, so the two cannot drift.
var Aggregations = []string{"count", "sum", "avg", "min", "max"}

// IsAggregation reports whether agg is a supported aggregation.
func IsAggregation(agg string) bool {
	for _, a := range Aggregations {
		if a == agg {
			return true
		}
	}
	return false
}

// ConstraintOps is the canonical set of matrix hard-constraint comparison
// operators, so the structured-output schema enum derives from the same values the
// typed ConstraintOp constants define rather than re-hardcoding them. It stays
// numeric-only because the matrix hard constraints are numeric boundaries on the
// objective aggregate; the richer intervention-filter ops live in FilterOps.
var ConstraintOps = []ConstraintOp{LessThan, LessThanOrEqual, GreaterThan, GreaterThanOrEqual}

// FilterOps is the canonical set of intervention-filter operators: the numeric
// threshold ops plus equality and set membership for boolean/categorical columns.
// It is the single source the intervention-filter structured-output schema enum
// derives from, kept separate from ConstraintOps so the richer ops never leak into
// the matrix hard-constraint schema.
var FilterOps = []ConstraintOp{LessThan, LessThanOrEqual, GreaterThan, GreaterThanOrEqual, Equal, NotEqual, In, NotIn}

// UnknownFilterColumns returns the constraint fields that match no column in the
// provided set under case-insensitive comparison (mirroring the sandbox compiler's
// own strings.EqualFold column matching), deduplicated in first-seen order. It is
// CGO-free so the orchestrator can pre-check a proposal's filter columns before
// dispatching it to the sandbox, without importing the CGO-locked sandbox compiler.
// The structured-output schema leaves filter fields as free strings, so this is the
// deterministic check that grounds a proposal to the real columns.
func UnknownFilterColumns(filters []Constraint, columns []string) []string {
	seen := map[string]bool{}
	var unknown []string
	for _, f := range filters {
		if columnKnown(f.Field, columns) {
			continue
		}
		key := strings.ToLower(f.Field)
		if seen[key] {
			continue
		}
		seen[key] = true
		unknown = append(unknown, f.Field)
	}
	return unknown
}

func columnKnown(field string, columns []string) bool {
	for _, c := range columns {
		if strings.EqualFold(field, c) {
			return true
		}
	}
	return false
}
