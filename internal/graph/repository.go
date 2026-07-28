// Package graph is the single seam every graph-touching service shares for
// Neo4j access. Nothing outside this package touches the driver: callers depend
// on Repository or on a narrower interface they declare themselves, so a future
// swap to Amazon Neptune is a new implementation behind those interfaces rather
// than a rewrite across services. All Cypher is restricted to a
// Neptune-portable openCypher subset (MERGE/MATCH/CREATE/SET, parameterized
// maps) with no APOC or db.* procedures, and every node is keyed on an
// application-assigned UUID id property (never the driver's internal element id).
package graph

import (
	"context"

	"github.com/arborette/arborette/internal/domain"
)

// CausalTriplet is the linearized State→Intervention→Outcome chain underlying a
// Meta-Heuristic, returned by TraceCausalChain for trace_causal_chain.
type CausalTriplet struct {
	State        domain.State
	Intervention domain.Intervention
	Outcome      domain.Outcome
}

// Repository is the whole Cypher surface, wider than any single caller uses:
// only the Orchestrator's Server depends on it as a unit, and it exercises the
// triplet writes alone. Every method added here has to be stubbed by that
// Server's fakes whether or not the Server calls it, so prefer a narrow
// contract at a new call site over widening this.
type Repository interface {
	// Node upserts, keyed on the application-assigned id.
	CreateState(ctx context.Context, s domain.State) error
	CreateIntervention(ctx context.Context, i domain.Intervention) error
	CreateOutcome(ctx context.Context, o domain.Outcome) error

	GetState(ctx context.Context, id string) (domain.State, error)
	GetIntervention(ctx context.Context, id string) (domain.Intervention, error)
	GetOutcome(ctx context.Context, id string) (domain.Outcome, error)
	GetMetaHeuristic(ctx context.Context, id string) (domain.MetaHeuristic, error)

	// Edge writes.
	CreatePreConditionFor(ctx context.Context, stateID, interventionID string) error
	CreateProduced(ctx context.Context, interventionID, outcomeID string, edge domain.ProducedEdge) error

	// CreateMetaHeuristic writes the node, its ABSTRACTED_FROM edges, and
	// embedding_pending:true within one transaction. It first validates that
	// every abstractedFrom id resolves to exactly one existing node, erroring
	// (and committing nothing) on any missing or duplicate reference, so a
	// Meta-Heuristic is never abstracted from incomplete evidence.
	CreateMetaHeuristic(ctx context.Context, mh domain.MetaHeuristic, abstractedFrom []string) error

	// ClearEmbeddingPending marks a Meta-Heuristic's embedding as written to
	// pgvector; ListEmbeddingPending surfaces nodes still awaiting that write
	// (including any left flagged by a mid-write crash) for retry.
	ClearEmbeddingPending(ctx context.Context, id string) error
	ListEmbeddingPending(ctx context.Context) ([]domain.MetaHeuristic, error)

	// UpdateOutcomeVerification applies an HITL resolution to an Outcome and its
	// inbound PRODUCED-edge confidence.
	UpdateOutcomeVerification(ctx context.Context, outcomeID string, status domain.VerificationStatus, confidence float64) error

	// TraceCausalChain walks ABSTRACTED_FROM from a Meta-Heuristic back to the
	// State/Intervention/Outcome triplet(s) that support it.
	TraceCausalChain(ctx context.Context, metaHeuristicID string) ([]CausalTriplet, error)

	// ListEligibleFindings returns the Sleep-Cycle search's input set for one
	// goal: complete State→Intervention→Outcome paths that are not sleep-derived
	// (a prior run's macro-segments are search outputs, not atomic inputs) and
	// whose outcome status is search-eligible (domain.SearchEligibleStatuses).
	ListEligibleFindings(ctx context.Context, goalID string) ([]CausalTriplet, error)

	// MarkStaleMetaHeuristics flags every Meta-Heuristic for a goal whose
	// ABSTRACTED_FROM components include a rejected Outcome, returning how many
	// were marked. It triggers on rejected only -- deliberately not corrected,
	// which domain.SearchEligibleStatuses admits, so a corrected-triggering sweep
	// would flag the worker's own fresh output on the very next run.
	MarkStaleMetaHeuristics(ctx context.Context, goalID string) (int, error)
}
