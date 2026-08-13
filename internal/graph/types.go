// Package graph is the single seam every graph-touching service shares for
// Neo4j access. Nothing outside this package touches the driver: each caller
// depends on a narrow interface it declares at its own call site, listing only
// the methods it uses, so a future swap to Amazon Neptune is a new implementation
// behind those interfaces rather than a rewrite across services. All Cypher is
// restricted to a Neptune-portable openCypher subset (MERGE/MATCH/CREATE/SET,
// parameterized maps) with no APOC or db.* procedures, and every node is keyed on
// an application-assigned UUID id property (never the driver's internal element id).
package graph

import (
	"github.com/arborette/arborette/internal/domain"
)

// CausalTriplet is the linearized State→Intervention→Outcome chain underlying a
// Meta-Heuristic, returned by TraceCausalChain for trace_causal_chain. EpistemicSource
// labels the PRODUCED edge each triplet was read through (observational or
// causal_inferred), so a trace that intentionally returns both kinds distinguishes
// them; it defaults to observational for an edge with no recorded value.
type CausalTriplet struct {
	State           domain.State
	Intervention    domain.Intervention
	Outcome         domain.Outcome
	EpistemicSource domain.EpistemicSource
}

// CausalEvidence is the latest non-superseded causal_inferred PRODUCED edge for one
// intervention: the backdoor-adjusted effect, the refutation-derived confidence, and
// the causal-graph version it was computed against. It is read through the dedicated
// causal-evidence lookup, which is exempt from the observational-only collection
// filter, so a consumer that weights causal knowledge (search value backprop, the MCP
// evidence output) can reach it without seeing observational edges.
type CausalEvidence struct {
	InterventionID string
	EffectSize     float64
	Confidence     float64
	GraphVersion   int
}

// ExtractionOutcome is an extract-type Outcome joined to the two things a bare
// node read cannot supply: the field and method from its producing Intervention,
// and the self-reported Confidence carried on the PRODUCED edge. The human
// verification surface needs all three -- the field keys the outcome's value
// map, and the confidence is what review is judging.
type ExtractionOutcome struct {
	OutcomeID          string
	DatasetID          string
	Field              string
	Method             string
	Value              map[string]any
	Provenance         *domain.ProvenanceLocator
	VerificationStatus domain.VerificationStatus
	Confidence         float64
}
