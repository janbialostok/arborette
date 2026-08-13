package sleepcycle

import (
	"context"
	"log"

	"github.com/arborette/arborette/internal/domain"
	"github.com/arborette/arborette/internal/objective"
)

// winner is one macro-segment whose derived triplet is fully persisted, carrying
// the ids the abstraction stage links back to and the row count backing its
// value. The support is carried rather than re-read off the persisted Outcome
// because publication ranks candidates by evidence weight, and a second graph
// read for a number already in hand would be pure cost.
type winner struct {
	node           *Node
	value          float64
	support        int64
	interventionID string
	outcomeID      string
}

// writeWinners persists each materially-better macro-segment as a full
// State→Intervention→Outcome observational triplet, and returns only those whose
// triplet was written end to end.
//
// Every id is a deterministic function of the goal and the canonicalized
// conjoined filter, and every written value is a function of that filter, so the
// graph's MERGE-on-id makes a re-run after a mid-write crash re-write identical
// nodes rather than append duplicates.
//
// The returned slice — not the search's original winner list — is what the
// abstraction stage iterates. That distinction is load-bearing: a half-written
// segment left in the list would send abstraction down its not-found happy path,
// spend a full Claude call plus repair round on it, and then hand
// CreateMetaHeuristic an abstractedFrom id that was never written, which it
// rejects after the cost was already paid.
//
// Note the effect size here is relative to the goal's global baseline, not to a
// tree parent as Phase 1's marginal effect is. A macro-segment {A,B,C} is
// reachable through several parents, so no unique parent exists; measuring
// against the fixed global baseline is what makes the written value a
// deterministic function of the filter alone, which the deterministic id requires.
func (w *Worker) writeWinners(ctx context.Context, target searchTarget, obj objective.Objective, baseline float64, segments []measuredNode) []winner {
	written := make([]winner, 0, len(segments))
	for _, seg := range segments {
		won, err := w.writeSegment(ctx, target, obj, baseline, seg)
		if err != nil {
			log.Printf("sleepcycle: write back macro-segment: %v", err)
			w.report(ctx, "sleepcycle_writeback_failure", "failure", map[string]any{
				"optimization_function_id": target.datasetID,
				"canonical_filter":         seg.node.canonical,
				"error":                    err.Error(),
			})
			continue
		}
		written = append(written, won)
	}
	return written
}

// writeSegment persists one macro-segment's five graph writes, each error-checked
// before the next so a failed node write never reaches the edge write that would
// silently no-op on it — a zero-row MATCH is not an error in Cypher, so an edge
// write against a missing node succeeds while linking nothing.
//
// The shared baseline State is written here, as the first of the five, rather
// than once per run. It is a MERGE on a deterministic id, so repeating it per
// segment costs one no-op upsert and still yields exactly one shared node — and
// it brings the State under this function's per-segment failure isolation. A
// once-per-run write that failed would otherwise leave every later segment
// silently edge-less: all its other writes succeed, no failure fires, and the
// result is exactly the Intervention→Outcome fragment that trace_causal_chain's
// complete-path match drops.
func (w *Worker) writeSegment(ctx context.Context, target searchTarget, obj objective.Objective, baseline float64, seg measuredNode) (winner, error) {
	canonical := seg.node.canonical
	stateID := baselineStateID(target.namespace)
	interventionID := derivedID(target.namespace, roleIntervention, canonical)
	outcomeID := derivedID(target.namespace, roleOutcome, canonical)

	state := domain.State{ID: stateID, DatasetID: target.datasetID, Properties: map[string]any{
		domain.PropDataSourceRef:        target.dataSourceRef,
		domain.PropObjectiveAggregation: obj.Aggregation,
		domain.PropObjectiveLabel:       obj.Label,
		domain.PropEffectiveFilters:     nil,
		"value":                         baseline,
	}}
	if err := w.repo.CreateState(ctx, state); err != nil {
		return winner{}, err
	}

	intervention := domain.Intervention{
		ID:           interventionID,
		DatasetID:       target.datasetID,
		Type:         domain.InterventionQuery,
		SleepDerived: true,
		Properties: map[string]any{
			domain.PropObjectiveAggregation: obj.Aggregation,
			domain.PropObjectiveLabel:       obj.Label,
			domain.PropNewFilters:           seg.node.filters,
			domain.PropEffectiveFilters:     seg.node.filters,
			"canonical_filter":              canonical,
			domain.PropSupport:              seg.measurement.Support,
		},
	}
	// The properties map is open, so recording the proposer needs no graph-schema
	// change, and its absence stays the honest answer for a segment the search built
	// from this goal's own predicates.
	if seg.node.proposedBy != "" {
		intervention.Properties[domain.PropProposedBy] = seg.node.proposedBy
	}
	if err := w.repo.CreateIntervention(ctx, intervention); err != nil {
		return winner{}, err
	}

	outcome := domain.Outcome{
		ID:                 outcomeID,
		DatasetID:             target.datasetID,
		VerificationStatus: domain.VerificationVerified,
		Value:              map[string]any{obj.Label: seg.measurement.Value},
		Support:            seg.measurement.Support,
	}
	if err := w.repo.CreateOutcome(ctx, outcome); err != nil {
		return winner{}, err
	}

	// The State and this edge are mandatory, not decorative: trace_causal_chain
	// matches only complete paths, so an Intervention→Outcome fragment would be
	// invisible to the very trace a Meta-Heuristic's evidence drill-down uses.
	if err := w.repo.CreatePreConditionFor(ctx, stateID, interventionID); err != nil {
		return winner{}, err
	}
	if err := w.repo.CreateProduced(ctx, interventionID, outcomeID, domain.ProducedEdge{
		EffectSize:      seg.measurement.Value - baseline,
		Confidence:      1.0,
		EpistemicSource: domain.EpistemicObservational,
	}); err != nil {
		return winner{}, err
	}

	// The proposing heuristic is recorded as nil rather than "" when the search built
	// the segment itself: an empty string is a valid-looking id, and a consumer
	// counting knowledge reuse would file such a winner under a phantom heuristic.
	interventionDetail := map[string]any{
		"optimization_function_id": target.datasetID,
		"intervention_id":          interventionID,
		"canonical_filter":         canonical,
		"support":                  seg.measurement.Support,
		// A literal, not the graph property constant: the audit log's schema is its
		// own, and renaming a node property must not silently rename a field a
		// consumer queries on.
		"proposed_by_meta_heuristic_id": nil,
	}
	if seg.node.proposedBy != "" {
		interventionDetail["proposed_by_meta_heuristic_id"] = seg.node.proposedBy
	}
	w.report(ctx, "sleepcycle_intervention", "intervention", interventionDetail)
	w.report(ctx, "sleepcycle_outcome", "outcome", map[string]any{
		"optimization_function_id": target.datasetID,
		"outcome_id":               outcomeID,
		"value":                    seg.measurement.Value,
	})

	return winner{
		node:           seg.node,
		value:          seg.measurement.Value,
		support:        seg.measurement.Support,
		interventionID: interventionID,
		outcomeID:      outcomeID,
	}, nil
}
