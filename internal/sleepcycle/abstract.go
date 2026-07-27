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

// abstractAll turns each persisted macro-segment into a Meta-Heuristic. Failures
// are isolated per segment: one Claude error, exhausted repair, rejected
// abstractedFrom, or embedding fault never aborts the others.
func (w *Worker) abstractAll(ctx context.Context, target searchTarget, obj objective.Objective, goalText string, baseline float64, columns []string, atoms []atom, winners []winner) {
	sources := map[string][]string{}
	for _, a := range atoms {
		sources[a.key] = a.sourceIDs
	}
	for _, won := range winners {
		if err := w.abstractOne(ctx, target, obj, goalText, baseline, columns, sources, won); err != nil {
			log.Printf("sleepcycle: abstract macro-segment: %v", err)
			w.report(ctx, "sleepcycle_abstraction_failure", "failure", map[string]any{
				"optimization_function_id": target.goalID,
				"canonical_filter":         won.node.canonical,
				"error":                    err.Error(),
			})
		}
	}
}

// abstractOne processes one macro-segment, resuming rather than repeating work a
// prior run already did. Progress is keyed off embedding_pending being false, not
// off node existence: a crash between the graph write and the pgvector write
// leaves a node that exists but is not yet searchable, and that must be finished,
// not skipped.
//
// An absent node is the normal first-run case; a transport error is not, which is
// why the lookup branches three ways on the not-found sentinel.
func (w *Worker) abstractOne(ctx context.Context, target searchTarget, obj objective.Objective, goalText string, baseline float64, columns []string, sources map[string][]string, won winner) error {
	mhID := derivedID(target.namespace, roleMetaHeuristic, won.node.canonical)

	existing, err := w.repo.GetMetaHeuristic(ctx, mhID)
	switch {
	case err == nil && !existing.EmbeddingPending:
		return nil
	case err == nil:
		// Written but never embedded: finish the tail, skip the Claude call.
		return w.embed(ctx, target.goalID, mhID, existing.Definition, nil)
	case !errors.Is(err, graph.ErrNotFound):
		return fmt.Errorf("look up meta-heuristic %q: %w", mhID, err)
	}

	seg := llm.MacroSegment{
		ObjectiveLabel: obj.Label,
		Direction:      obj.Direction,
		Filters:        won.node.filters,
		Baseline:       baseline,
		Value:          won.value,
	}
	abstraction, err := w.abstractSegment(ctx, goalText, seg, columns)
	if err != nil {
		return err
	}

	abstractedFrom := abstractedFromIDs(won, sources)
	if err := w.repo.CreateMetaHeuristic(ctx, domain.MetaHeuristic{ID: mhID, Definition: abstraction.Definition}, abstractedFrom); err != nil {
		return fmt.Errorf("create meta-heuristic %q: %w", mhID, err)
	}
	return w.embed(ctx, target.goalID, mhID, abstraction.Definition, abstractedFrom)
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

func leakedColumnsMessage(leaked []string) string {
	return "these dataset column names are still present in the definition: " + strings.Join(leaked, ", ") +
		"; replace each with a bracketed structural ontology term"
}

// abstractedFromIDs links a Meta-Heuristic to its evidence: the component
// single-feature triplets every atom of the macro-segment came from, plus the
// derived macro-segment's own Intervention and Outcome.
//
// The shared baseline State is deliberately excluded. trace_causal_chain's second
// MATCH is unbound — it enumerates every complete path in the graph and keeps the
// rows whose target is the State, Intervention, or Outcome. Since the baseline
// State is one node per goal wired as the State of *every* derived triplet,
// linking it would make each Meta-Heuristic's trace return all of them, silently
// attributing other segments' evidence to it. Linking only the derived
// Intervention and Outcome loses nothing: matching the Intervention already pulls
// the complete path, which returns that State anyway. Component triplet ids are
// safe because Phase 1 mints a unique State per triplet — the rule is about
// shared nodes, not about the State label.
func abstractedFromIDs(won winner, sources map[string][]string) []string {
	var ids []string
	for _, key := range won.node.keys {
		ids = appendMissing(ids, sources[key]...)
	}
	return appendMissing(ids, won.interventionID, won.outcomeID)
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
	if err := w.embeddings.Upsert(ctx, mhID, vec); err != nil {
		return fmt.Errorf("upsert embedding %q: %w", mhID, err)
	}
	if err := w.repo.ClearEmbeddingPending(ctx, mhID); err != nil {
		return fmt.Errorf("clear embedding_pending %q: %w", mhID, err)
	}
	// The resume pass settles nodes from any goal, so it has no goal to name here.
	// Record that as nil rather than "": an empty string is a valid-looking id, and
	// a consumer grouping injections by goal would file resumed ones under a
	// phantom goal instead of skipping them.
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

// resumeEmbeddings re-runs the embed tail for every Meta-Heuristic still flagged
// pending, so a node left behind by a mid-write crash is finished rather than
// silently unsearchable.
//
// The listing is global by design, not goal-scoped: any crashed run should be
// settled by the next run whichever goal triggered it. That makes this pass
// non-terminal — a wedged embedding provider here must not abort a run for a goal
// it has nothing to do with — and means the audits it emits can reference a
// different goal than the run's own.
func (w *Worker) resumeEmbeddings(ctx context.Context) {
	pending, err := w.repo.ListEmbeddingPending(ctx)
	if err != nil {
		w.resumeFailure(ctx, "", err)
		return
	}
	for _, mh := range pending {
		if err := w.embed(ctx, "", mh.ID, mh.Definition, nil); err != nil {
			w.resumeFailure(ctx, mh.ID, err)
		}
	}
}

func (w *Worker) resumeFailure(ctx context.Context, mhID string, err error) {
	log.Printf("sleepcycle: resume embedding %q: %v", mhID, err)
	// Both ids are nil rather than "": an empty string is a valid-looking id, the
	// resume pass names no goal, and a failure listing the set names no node.
	detail := map[string]any{"optimization_function_id": nil, "meta_heuristic_id": nil, "error": err.Error()}
	if mhID != "" {
		detail["meta_heuristic_id"] = mhID
	}
	w.report(ctx, "sleepcycle_resume_failure", "failure", detail)
}
