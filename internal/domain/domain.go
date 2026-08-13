// Package domain holds the shared data-model types every arborette service
// imports. It is intentionally dependency-free so any package or service can
// use it without pulling in graph, storage, or embedding concerns.
//
// Terminology (2026 refactor): "Dataset" is the top-level entity (formerly
// "Goal"/"OptimizationFunction"), "Question" specifies what to ask of a
// dataset (formerly "Objective"), and "Insight" is the abstraction produced
// during the Sleep Cycle (formerly "MetaHeuristic"/"Heuristic").
package domain

import (
	"errors"
	"fmt"
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

// EpistemicSource records how a PRODUCED edge's effect was established, ordered by
// strength of causal evidence: observational (a measured correlation P(Outcome |
// Segment)), causal_inferred (a backdoor-adjusted effect that survived the refutation
// battery over the discovered graph — supported given that model, but not a
// physically-executed intervention), and the still-reserved interventional (physical
// execution, do-calculus edges, never written in the MVP; a physically-executed
// intervention reuses the InterventionType field rather than a node-level field). Any
// consumer that reads this property treats an absent/empty value as observational and
// must not assume interventional exists.
type EpistemicSource string

const (
	EpistemicObservational  EpistemicSource = "observational"
	EpistemicCausalInferred EpistemicSource = "causal_inferred"
	EpistemicInterventional EpistemicSource = "interventional"
)

// CausalInferredCaveat is the honest analyst-facing qualifier a causal_inferred edge
// is rendered with: its effect is supported by backdoor adjustment and refutation
// over the discovered causal model, not proven by a physical intervention, so it is
// never presented as unconditional causation.
const CausalInferredCaveat = "causally supported (given the discovered model)"

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
//
// Terminology note: objective_label is retained as the wire name (it was never
// a user-visible concept; it is the label pinned from the evaluation matrix).
const (
	PropNewFilters           = "new_filters"
	PropEffectiveFilters     = "effective_filters"
	PropObjectiveLabel       = "objective_label"
	PropObjectiveAggregation = "objective_aggregation"
	PropDataSourceRef        = "data_source_ref"
	PropSupport              = "support"
	PropClaimDerived         = "claim_derived"
	PropProposedBy           = "proposed_by_insight_id"
)

// State is a snapshot/telemetry point in time. DatasetID scopes it to the
// dataset it was measured for.
type State struct {
	ID         string
	DatasetID  string
	Properties map[string]any
}

// Intervention is reified action metadata (configuration change, execution
// metadata, confidence bounds). DatasetID scopes it to its dataset;
// SleepDerived marks a macro-segment the Sleep-Cycle search produced, which is a
// search output rather than an atomic input and is therefore excluded from a
// later run's search space so conjunctions are never double-counted.
type Intervention struct {
	ID           string
	DatasetID    string
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

// Outcome is a measured delta from an intervention, expressed as a question
// answer evaluated against the analyst's dataset.
type Outcome struct {
	ID                 string
	DatasetID          string
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

// OntologyTerm maps one concrete, dataset-bound variable to the universal
// structural term an abstraction replaced it with. Persisting the mapping beside
// the definition is what lets a later run re-instantiate a heuristic against real
// columns: the definition alone names only bracketed terms, so without the pairs
// the only route back to a filter is to re-derive it from scratch.
type OntologyTerm struct {
	Concrete    string `json:"concrete"`
	Ontological string `json:"ontological"`
}

// Insight is an abstraction produced during the Sleep Cycle. Analogous to the
// former MetaHeuristic: it captures a learned pattern from a dataset. Its
// embedding lives in pgvector keyed by ID; EmbeddingPending is true from node
// creation until the pgvector write succeeds. Stale marks an insight whose
// supporting evidence was since rejected by an analyst. DatasetID is the
// dataset whose abstraction run wrote it, scoping the node's embedding to that
// dataset; a legacy node created before dataset scoping carries an empty
// DatasetID until a later run relinks it.
//
// OntologyTerms, OriginDatasetID, and OriginDataSourceRef are the abstraction's
// provenance, recorded so a reuse path can tell same-dataset re-instantiation
// (the origin ref matches, and the term map resolves every bracketed term) from
// cross-dataset grounding. All three are empty on a node written before they were
// persisted, which is exactly the signal to route it through grounding.
type Insight struct {
	ID                  string
	Definition          string
	DatasetID           string
	EmbeddingPending    bool
	Stale               bool
	OntologyTerms       []OntologyTerm
	OriginDatasetID     string
	OriginDataSourceRef string
}

// MetaHeuristic is a deprecated alias for Insight, kept for backward
// compatibility during the transition. New code must use Insight.
//
// Deprecated: Use Insight instead.
type MetaHeuristic = Insight

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
	ColumnRefKind         ExpressionKind = "column_ref"
	LiteralKind           ExpressionKind = "literal"
	CastKind              ExpressionKind = "cast"
	ComparisonKind        ExpressionKind = "comparison"
	ArithmeticKind        ExpressionKind = "arithmetic"
	CaseKind              ExpressionKind = "case"
	LagKind               ExpressionKind = "lag"
	TrailingAggregateKind ExpressionKind = "trailing_aggregate"
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
	// Window (Lag / TrailingAggregate): Inner is the per-row expression the window
	// is computed over its entity's ordered history. Lag reads the value Offset rows
	// back; TrailingAggregate applies WindowAgg over the WindowSize rows preceding
	// the current one. A window kind may not nest inside another (see ValidateWindowShape).
	Inner      *Expression `json:"inner,omitempty"`
	Offset     int         `json:"offset,omitempty"`
	WindowSize int         `json:"window_size,omitempty"`
	WindowAgg  string      `json:"window_agg,omitempty"`
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
	case LagKind:
		return "lag(" + renderChild(e.Inner) + ", " + strconv.Itoa(e.Offset) + ")"
	case TrailingAggregateKind:
		return e.WindowAgg + "(" + renderChild(e.Inner) + ") over trailing " + strconv.Itoa(e.WindowSize)
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
	case LagKind, TrailingAggregateKind:
		return 1 + childDepth(e.Inner)
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

// MaxWindowSize caps a trailing-aggregate window's row span and a lag's offset, so
// a pathological window the plain-string output schema does not bound cannot be
// requested. Real entity-relative signals span a handful to a few hundred rows;
// 10_000 is generous headroom while keeping the frame bound small enough to format
// or bind safely.
const MaxWindowSize = 10_000

// Sentinel errors for a structurally-invalid windowed expression, so the
// orchestrator can classify a shape-guard rejection as a repairable objective-fit
// failure (like the depth guard) rather than a fault.
var (
	ErrWindowNested = errors.New("a window expression may not nest inside another window expression")
	ErrWindowBounds = errors.New("window offset or size out of range")
)

// ValidateWindowShape rejects a structurally-invalid windowed expression: a window
// kind nested anywhere under another window kind (a row's window value is an
// intrinsic property of its entity's history, so windows do not compose), or a lag
// offset / trailing-window size outside [1, MaxWindowSize]. It is CGO-free so the
// orchestrator can guard a decoded objective before dispatch, alongside the depth
// guard. A non-windowed expression passes trivially; operand type/aggregate
// validity is the sandbox compiler's concern, not this guard's.
func ValidateWindowShape(e Expression) error {
	return validateWindowShape(e, false)
}

func validateWindowShape(e Expression, underWindow bool) error {
	switch e.Kind {
	case LagKind:
		if underWindow {
			return ErrWindowNested
		}
		if e.Offset < 1 || e.Offset > MaxWindowSize {
			return fmt.Errorf("%w: lag offset %d not in [1, %d]", ErrWindowBounds, e.Offset, MaxWindowSize)
		}
		return validateWindowShape(derefExpr(e.Inner), true)
	case TrailingAggregateKind:
		if underWindow {
			return ErrWindowNested
		}
		if e.WindowSize < 1 || e.WindowSize > MaxWindowSize {
			return fmt.Errorf("%w: trailing window size %d not in [1, %d]", ErrWindowBounds, e.WindowSize, MaxWindowSize)
		}
		return validateWindowShape(derefExpr(e.Inner), true)
	case CastKind:
		return validateWindowShape(derefExpr(e.Operand), underWindow)
	case ComparisonKind, ArithmeticKind:
		if err := validateWindowShape(derefExpr(e.Left), underWindow); err != nil {
			return err
		}
		return validateWindowShape(derefExpr(e.Right), underWindow)
	case CaseKind:
		for _, br := range e.Cases {
			if err := validateWindowShape(derefExpr(br.When), underWindow); err != nil {
				return err
			}
			if err := validateWindowShape(derefExpr(br.Then), underWindow); err != nil {
				return err
			}
		}
		return validateWindowShape(derefExpr(e.Else), underWindow)
	default:
		return nil
	}
}

// HasWindowKind reports whether an expression tree contains any windowed construct
// (lag or trailing aggregate). It is the cheap "this objective is windowed"
// detector used to require the entity/time bindings and to route windowed findings
// to their own verification outcome.
func HasWindowKind(e Expression) bool {
	switch e.Kind {
	case LagKind, TrailingAggregateKind:
		return true
	case CastKind:
		return hasWindowChild(e.Operand)
	case ComparisonKind, ArithmeticKind:
		return hasWindowChild(e.Left) || hasWindowChild(e.Right)
	case CaseKind:
		for _, br := range e.Cases {
			if hasWindowChild(br.When) || hasWindowChild(br.Then) {
				return true
			}
		}
		return hasWindowChild(e.Else)
	default:
		return false
	}
}

func hasWindowChild(e *Expression) bool {
	if e == nil {
		return false
	}
	return HasWindowKind(*e)
}

// derefExpr dereferences an AST child, returning a zero Expression (which the
// window walkers treat as a leaf) when the child is nil.
func derefExpr(e *Expression) Expression {
	if e == nil {
		return Expression{}
	}
	return *e
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

// UnknownFilterValues returns the equality/membership filter operands that name a
// value absent from their column's known distinct-value set, rendered as
// "column=value" offenders and deduplicated in first-seen order. It is the value
// analogue of UnknownFilterColumns: only eq/neq/in/not_in constraints are checked,
// and only against columns present in values -- a column with no listed values
// (high-cardinality or continuous) is not value-constrained and is skipped, not an
// error. Comparison is case-insensitive on both the column key and the value,
// mirroring the compiler's case-insensitive column matching and absorbing the
// boolean rendering difference between a proposal's True and DuckDB's cast true.
// Operands are rendered to the string form the distinct-value probe stored (a
// CAST(... AS VARCHAR)) -- integer literals without a trailing decimal -- so a
// valid proposal is never falsely rejected. CGO-free so the orchestrator can
// pre-check a proposal's filter values without importing the sandbox compiler.
func UnknownFilterValues(filters []Constraint, values map[string][]string) []string {
	seen := map[string]bool{}
	var unknown []string
	for _, f := range filters {
		known, ok := knownValues(f.Field, values)
		if !ok {
			continue
		}
		for _, operand := range filterOperands(f) {
			if valueKnown(operand, known) {
				continue
			}
			offender := f.Field + "=" + operand
			key := strings.ToLower(offender)
			if seen[key] {
				continue
			}
			seen[key] = true
			unknown = append(unknown, offender)
		}
	}
	return unknown
}

// knownValues finds a column's distinct-value set under case-insensitive column
// matching, reporting whether the column is value-constrained at all.
func knownValues(field string, values map[string][]string) ([]string, bool) {
	for col, vs := range values {
		if strings.EqualFold(field, col) {
			return vs, true
		}
	}
	return nil, false
}

// valueKnown reports whether operand matches any known value case-insensitively.
func valueKnown(operand string, known []string) bool {
	for _, v := range known {
		if strings.EqualFold(operand, v) {
			return true
		}
	}
	return false
}

// filterOperands renders a constraint's equality/membership operands to the
// strings a distinct-value set is matched against. A numeric-threshold constraint
// carries no categorical operand and yields nothing.
func filterOperands(c Constraint) []string {
	switch {
	case c.IsEqualityOp():
		if c.Operand == nil {
			return nil
		}
		return []string{renderOperandValue(c.Operand)}
	case c.IsMembershipOp():
		out := make([]string, 0, len(c.Members))
		for i := range c.Members {
			out = append(out, renderOperandValue(&c.Members[i]))
		}
		return out
	default:
		return nil
	}
}

// renderOperandValue renders a filter operand to the string form the distinct-
// value probe stored (a DuckDB CAST(... AS VARCHAR)): a string verbatim, a boolean
// as true/false (matched case-insensitively), and a number without a trailing
// decimal for an integral value, so an integer column's 5 is not compared as 5.0.
// The 'f' format avoids the scientific notation 'g' would emit for large integers,
// which DuckDB's integer cast never produces.
func renderOperandValue(l *LiteralValue) string {
	switch {
	case l == nil:
		return ""
	case l.String != nil:
		return *l.String
	case l.Bool != nil:
		if *l.Bool {
			return "true"
		}
		return "false"
	case l.Number != nil:
		return strconv.FormatFloat(*l.Number, 'f', -1, 64)
	default:
		return ""
	}
}
