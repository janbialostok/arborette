package sleepcycle

import (
	"context"
	"errors"
	"fmt"
	"log"
	"strings"

	"github.com/arborette/arborette/internal/domain"
	"github.com/arborette/arborette/internal/graph"
	"github.com/arborette/arborette/internal/llm"
	"github.com/arborette/arborette/internal/objective"
)

// maxAbstractionRepairs bounds how many times one macro-segment's definition is
// re-requested when it still names a real column. After the bound the segment is
// skipped rather than persisted, because a leaky definition is a heuristic bound
// to one dataset — the opposite of what abstraction is for.
const maxAbstractionRepairs = 1

// abstractAll turns each selected segment into a Meta-Heuristic, returning how
// many are published and searchable when it finishes. Failures are isolated per
// segment: one Claude error, exhausted repair, rejected abstractedFrom, or
// embedding fault never aborts the others.
//
// The count is of segments that came out published, not of segments attempted, so
// the run audit cannot report a healthy publication count for a run whose every
// Claude call failed. A segment a previous run already published counts too — it
// is reachable to a consumer either way.
func (w *Worker) abstractAll(ctx context.Context, target searchTarget, obj objective.Objective, goalText string, baseline float64, columns []string, selected []candidate) int {
	published := 0
	for _, cand := range selected {
		if err := w.abstractOne(ctx, target, obj, goalText, baseline, columns, cand); err != nil {
			log.Printf("sleepcycle: abstract macro-segment: %v", err)
			w.report(ctx, "sleepcycle_abstraction_failure", "failure", map[string]any{
				"optimization_function_id": target.goalID,
				"canonical_filter":         cand.canonical,
				"error":                    err.Error(),
			})
			continue
		}
		published++
	}
	return published
}

// abstractOne processes one selected segment, resuming rather than repeating work
// a prior run already did. Progress is keyed off embedding_pending being false,
// not off node existence: a crash between the graph write and the pgvector write
// leaves a node that exists but is not yet searchable, and that must be finished,
// not skipped.
//
// An absent node is the normal first-run case; a transport error is not, which is
// why the lookup branches three ways on the not-found sentinel.
func (w *Worker) abstractOne(ctx context.Context, target searchTarget, obj objective.Objective, goalText string, baseline float64, columns []string, cand candidate) error {
	mhID := derivedID(target.namespace, roleMetaHeuristic, cand.canonical)

	existing, err := w.repo.GetMetaHeuristic(ctx, mhID)
	switch {
	case err == nil:
		// A later run can select this segment carrying provenance the first run never
		// saw: a second Phase-1 finding reaching the same effective segment by a
		// different branch order, or a derived macro-segment landing on it.
		// ABSTRACTED_FROM is the only input to the staleness sweep, so a link never
		// written is evidence whose rejection can never retire this heuristic.
		// CreateMetaHeuristic MERGEs the node and each edge and leaves
		// embedding_pending alone, so re-issuing it adds what is missing without
		// disturbing a finished abstraction.
		relinkErr := w.repo.CreateMetaHeuristic(ctx, domain.MetaHeuristic{ID: mhID, Definition: existing.Definition, GoalID: target.goalID}, cand.abstractedFrom)
		if !existing.EmbeddingPending {
			// The heuristic is written and embedded, so it is reachable to a consumer
			// whether or not this run managed to widen its evidence. Reporting it as
			// unpublished would be the misleading answer — one flapping write window
			// would read as a run that published nothing while the whole corpus is
			// live — so the fault is surfaced on its own and the segment still counts.
			if relinkErr != nil {
				w.relinkFailure(ctx, target.goalID, mhID, relinkErr)
			}
			return nil
		}
		// Written but never embedded: finish the tail, skip the Claude call. Here the
		// fault is terminal — embedding a node whose evidence was just rejected would
		// publish it to similarity search on provenance the graph does not agree with.
		if relinkErr != nil {
			return fmt.Errorf("re-link meta-heuristic %q: %w", mhID, relinkErr)
		}
		return w.embed(ctx, target.goalID, mhID, existing.Definition, cand.abstractedFrom)
	case !errors.Is(err, graph.ErrNotFound):
		return fmt.Errorf("look up meta-heuristic %q: %w", mhID, err)
	}

	seg := llm.MacroSegment{
		ObjectiveLabel: obj.Label,
		Direction:      obj.Direction,
		Filters:        cand.filters,
		Baseline:       baseline,
		Value:          cand.value,
	}
	abstraction, err := w.abstractSegment(ctx, goalText, seg, columns)
	if err != nil {
		return err
	}

	if err := w.repo.CreateMetaHeuristic(ctx, domain.MetaHeuristic{ID: mhID, Definition: abstraction.Definition, GoalID: target.goalID}, cand.abstractedFrom); err != nil {
		return fmt.Errorf("create meta-heuristic %q: %w", mhID, err)
	}
	return w.embed(ctx, target.goalID, mhID, abstraction.Definition, cand.abstractedFrom)
}

// abstractSegment generates a definition and repairs it while it still leaks a
// concrete column name, bounded so a persistently leaky model cannot stall the
// run. The leak check is deterministic — the output schema leaves the definition
// a free string, so nothing else grounds "actually generalized".
func (w *Worker) abstractSegment(ctx context.Context, goalText string, seg llm.MacroSegment, columns []string) (llm.Abstraction, error) {
	abstraction, err := w.claude.AbstractMetaHeuristic(ctx, goalText, seg)
	if err != nil {
		return llm.Abstraction{}, fmt.Errorf("abstract meta-heuristic: %w", err)
	}
	leaked := llm.LeakedConcreteTerms(abstraction.Definition, columns)
	for attempts := 0; len(leaked) > 0 && attempts < maxAbstractionRepairs; attempts++ {
		repaired, rerr := w.claude.RepairMetaHeuristic(ctx, goalText, seg, abstraction, leakedColumnsMessage(leaked))
		if rerr != nil {
			return llm.Abstraction{}, fmt.Errorf("repair meta-heuristic: %w", rerr)
		}
		abstraction = repaired
		leaked = llm.LeakedConcreteTerms(abstraction.Definition, columns)
	}
	if len(leaked) > 0 {
		return llm.Abstraction{}, fmt.Errorf("definition still names concrete columns after %d repair(s): %s",
			maxAbstractionRepairs, strings.Join(leaked, ", "))
	}
	return abstraction, nil
}

// relinkFailure records evidence that could not be attached to an
// already-published heuristic. It is its own action rather than an abstraction
// failure so an operator can tell "never abstracted" from "abstracted, and one
// run's evidence did not land" — the second leaves a live heuristic whose
// provenance is narrower than the graph knows.
func (w *Worker) relinkFailure(ctx context.Context, goalID, mhID string, err error) {
	log.Printf("sleepcycle: re-link meta-heuristic %q: %v", mhID, err)
	w.report(ctx, "sleepcycle_relink_failure", "failure", map[string]any{
		"optimization_function_id": goalID,
		"meta_heuristic_id":        mhID,
		"error":                    err.Error(),
	})
}

func leakedColumnsMessage(leaked []string) string {
	return "these dataset column names are still present in the definition: " + strings.Join(leaked, ", ") +
		"; replace each with a bracketed structural ontology term"
}

// embed completes an abstraction: generate the vector, write it to pgvector, and
// only then clear the pending flag. That order is what makes the crash window
// recoverable — a node left flagged is retried, never silently treated as done.
// EmbedDocument, never EmbedQuery: the read path embeds queries, and the two
// sides must stay consistent for similarity search to work.
func (w *Worker) embed(ctx context.Context, goalID, mhID, definition string, abstractedFrom []string) error {
	vec, err := w.provider.EmbedDocument(ctx, definition)
	if err != nil {
		return fmt.Errorf("embed meta-heuristic %q: %w", mhID, err)
	}
	if err := w.embeddings.Upsert(ctx, mhID, goalID, vec); err != nil {
		return fmt.Errorf("upsert embedding %q: %w", mhID, err)
	}
	if err := w.repo.ClearEmbeddingPending(ctx, mhID); err != nil {
		return fmt.Errorf("clear embedding_pending %q: %w", mhID, err)
	}
	// A goal-less caller (a reconcile re-embed of a legacy node the graph never
	// scoped) names no goal here. Record that as nil rather than "": an empty
	// string is a valid-looking id, and a consumer grouping injections by goal
	// would file such a node under a phantom goal instead of skipping it.
	detail := map[string]any{
		"optimization_function_id": nil,
		"meta_heuristic_id":        mhID,
		"abstracted_from":          abstractedFrom,
	}
	if goalID != "" {
		detail["optimization_function_id"] = goalID
	}
	w.report(ctx, "sleepcycle_heuristic_injection", "heuristic", detail)
	return nil
}

// reconcileEmbeddings settles the graph and pgvector back into agreement, so a
// node left behind by a mid-write crash is finished and a legacy row that lost
// its goal scope is repaired, rather than either staying silently unsearchable
// or invisible to goal-scoped retrieval.
//
// It diffs every Meta-Heuristic node against every embedding row and walks the
// graph refs with three arms per node: re-embed when the row is missing or the
// node is still flagged pending (the crash case), passing the node's goal so the
// healed row is goal-visible; goal-repair via SetGoalID when the row exists but
// lost the goal the graph node now carries; nothing otherwise.
//
// The pass is global by design, not goal-scoped: any crashed or legacy node
// should be settled by the next run whichever goal triggered it. That makes it
// non-terminal — a wedged embedding provider here must not abort a run for a goal
// it has nothing to do with — and means the audits it emits can reference a
// different goal than the run's own.
func (w *Worker) reconcileEmbeddings(ctx context.Context) {
	nodes, err := w.repo.ListMetaHeuristics(ctx)
	if err != nil {
		w.reconcileFailure(ctx, "", err)
		return
	}
	refs, err := w.embeddings.ListNodeRefs(ctx)
	if err != nil {
		w.reconcileFailure(ctx, "", err)
		return
	}
	rowGoal := make(map[string]string, len(refs))
	for _, ref := range refs {
		rowGoal[ref.NodeID] = ref.GoalID
	}

	var reembedded, repaired []string
	for _, mh := range nodes {
		goalID, hasRow := rowGoal[mh.ID]
		switch {
		case !hasRow || mh.EmbeddingPending:
			if err := w.embed(ctx, mh.GoalID, mh.ID, mh.Definition, nil); err != nil {
				w.reconcileFailure(ctx, mh.ID, err)
				continue
			}
			reembedded = append(reembedded, mh.ID)
		case mh.GoalID != "" && goalID == "":
			if err := w.embeddings.SetGoalID(ctx, mh.ID, mh.GoalID); err != nil {
				w.reconcileFailure(ctx, mh.ID, err)
				continue
			}
			repaired = append(repaired, mh.ID)
		}
	}

	// One summary event when the pass actually changed something: the embedding
	// drift this pass exists to close (rows re-embedded, legacy rows made
	// goal-visible) is exactly what an operator needs recorded.
	if len(reembedded) > 0 || len(repaired) > 0 {
		w.report(ctx, "sleepcycle_reconcile", "job", map[string]any{
			"optimization_function_id": nil,
			"reembedded":               reembedded,
			"goal_repaired":            repaired,
		})
	}
}

func (w *Worker) reconcileFailure(ctx context.Context, mhID string, err error) {
	log.Printf("sleepcycle: reconcile embedding %q: %v", mhID, err)
	// Both ids are nil rather than "": an empty string is a valid-looking id, the
	// reconcile pass names no goal, and a failure listing the set names no node.
	detail := map[string]any{"optimization_function_id": nil, "meta_heuristic_id": nil, "error": err.Error()}
	if mhID != "" {
		detail["meta_heuristic_id"] = mhID
	}
	w.report(ctx, "sleepcycle_reconcile_failure", "failure", detail)
}
