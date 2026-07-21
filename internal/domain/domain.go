// Package domain holds the shared data-model types every arborette service
// imports. It is intentionally dependency-free so any package or service can
// use it without pulling in graph, storage, or embedding concerns.
package domain

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

// Relationship names for the graph edges. Kept as constants so both the graph
// implementation and its consumers reference one spelling.
const (
	PreConditionFor = "PRE_CONDITION_FOR"
	Produced        = "PRODUCED"
	AbstractedFrom  = "ABSTRACTED_FROM"
)

// State is a snapshot/telemetry point in time.
type State struct {
	ID         string
	Properties map[string]any
}

// Intervention is reified action metadata (configuration change, execution
// metadata, confidence bounds).
type Intervention struct {
	ID         string
	Type       InterventionType
	Properties map[string]any
}

// ProvenanceLocator pins an extract-type outcome's value to the exact source
// excerpt it came from. Nil when no exact match is found in the source text.
type ProvenanceLocator struct {
	Page      int
	CharStart int
	CharEnd   int
}

// Outcome is a measured delta from an intervention, evaluated against the
// analyst's Evaluation Matrix.
type Outcome struct {
	ID                 string
	VerificationStatus VerificationStatus
	Value              map[string]any
	// Provenance is set only for extract-type outcomes; nil for query-type or
	// when no exact source match was located.
	Provenance *ProvenanceLocator
}

// MetaHeuristic is a semantic abstraction produced during the Sleep Cycle. Its
// embedding lives in pgvector keyed by ID; EmbeddingPending is true from node
// creation until the pgvector write succeeds.
type MetaHeuristic struct {
	ID               string
	Definition       string
	EmbeddingPending bool
}

// ProducedEdge carries the measured effect size and a self-reported confidence
// weight (0.0–1.0) on an Intervention→Outcome edge.
type ProducedEdge struct {
	EffectSize float64
	Confidence float64
}

// EvaluationMatrix is the structured form of an analyst's plain-English goal:
// targets to maximize/minimize plus hard-constraint boundaries. Stored as jsonb
// in the goal registry.
type EvaluationMatrix struct {
	Targets     []Target     `json:"targets"`
	Constraints []Constraint `json:"constraints"`
}

// TargetDirection is whether a target should be maximized or minimized.
type TargetDirection string

const (
	Maximize TargetDirection = "maximize"
	Minimize TargetDirection = "minimize"
)

// Target is a single optimization objective bound to a data-source field.
type Target struct {
	Field     string          `json:"field"`
	Direction TargetDirection `json:"direction"`
}

// ConstraintOp expresses a hard-constraint boundary comparison.
type ConstraintOp string

const (
	LessThan           ConstraintOp = "lt"
	LessThanOrEqual    ConstraintOp = "lte"
	GreaterThan        ConstraintOp = "gt"
	GreaterThanOrEqual ConstraintOp = "gte"
)

// Constraint is a hard boundary a candidate intervention must respect.
type Constraint struct {
	Field string       `json:"field"`
	Op    ConstraintOp `json:"op"`
	Value float64      `json:"value"`
}
