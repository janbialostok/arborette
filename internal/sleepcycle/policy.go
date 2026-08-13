package sleepcycle

import (
	"context"
	"fmt"
	"log"
	"strings"

	"github.com/arborette/arborette/internal/graph"
	"github.com/arborette/arborette/internal/objective"
	"github.com/arborette/arborette/internal/sandboxclient"
)

// Why a run never searched, recorded so a run reporting zero measurements is never
// ambiguous about which degenerate case it hit.
const (
	skipNoConjunction = "no conjunction formable"
	skipNoBestSingle  = "no eligible finding clears the support floor, so no macro-segment could be written back"
)

// atomVocabulary is the predicate set one run searches, plus the critic's advisory
// ordering of the columns behind it. The ranking is empty whenever no critic ran or
// it offered no opinion, which is simply an unbiased search.
type atomVocabulary struct {
	atoms   []atom
	ranking []string
}

// vocabulary assembles the run's atom set. Without the schema-derived vocabulary
// enabled it is exactly the findings-derived one — every predicate the hypothesis
// loop measured — and the whole critic path is skipped.
//
// With it enabled the schema's own value sets and quantile cuts are enumerated
// into predicates, which is what stops the search being bounded by whatever
// vocabulary Phase 1 happened to surface. That enumeration is indiscriminate, so it
// is filtered by the critic first: an identifier column, a column recorded after
// the outcome, or a restatement of the objective yields segments that are true and
// useless, and enumerating them would spend the whole budget there.
//
// A critic failure therefore degrades to the findings-derived vocabulary rather
// than to an unfiltered enumeration: an unreviewed schema vocabulary is the one
// outcome worse than not having one.
func (w *Worker) vocabulary(ctx context.Context, goalID, goalText string, schema sandboxclient.Schema, findingAtoms []atom) atomVocabulary {
	if !w.cfg.SchemaAtoms {
		return atomVocabulary{atoms: findingAtoms}
	}
	schemaAtoms, err := buildSchemaAtoms(schema, w.cfg.QuantileBins)
	if err != nil {
		w.vocabularyFailure(ctx, goalID, fmt.Errorf("build schema atoms: %w", err))
		return atomVocabulary{atoms: findingAtoms}
	}
	critique, err := w.claude.CritiqueAtoms(ctx, goalText, llmSchema(schema))
	if err != nil {
		w.vocabularyFailure(ctx, goalID, fmt.Errorf("critique atoms: %w", err))
		return atomVocabulary{atoms: findingAtoms}
	}

	// The dropped count comes back from the merge rather than being recomputed: it is
	// the number that says whether a critique narrowed the search or gutted it, and
	// two implementations of the same predicate could disagree about it.
	merged, dropped := mergeAtoms(findingAtoms, schemaAtoms, critique.ExcludedColumns)
	w.report(ctx, "sleepcycle_atom_vocabulary", "job", map[string]any{
		"optimization_function_id": goalID,
		"finding_atoms":            len(findingAtoms),
		"schema_atoms":             len(schemaAtoms),
		"excluded_columns":         critique.ExcludedColumns,
		"excluded_atoms":           dropped,
		"ranked_columns":           critique.RankedColumns,
		"rationale":                critique.Rationale,
		"atoms":                    len(merged),
	})
	return atomVocabulary{atoms: merged, ranking: critique.RankedColumns}
}

// selectPolicy builds the run's traversal strategy over the vocabulary it will
// search, or names why the search cannot run at all. It returns the vocabulary
// either way, because the publication stage links a winner back through the atoms
// it was built from. A nil policy always comes with a reason, and a reason always
// with a nil policy.
//
// The beam keeps its original guard: fewer than two predicates, or an order cap
// that forbids conjoining, means no conjunction exists to measure.
//
// The knowledge-guided policy nests its decision so the cheap clauses short-circuit
// before any model call is paid for — the vocabulary's critic call included, which
// is why widening happens here rather than at the caller. The order cap is free to
// check. So is the absence of a qualifying finding: with no best single segment
// nothing can clear the materially-better gate, so a search there would spend
// sandbox budget and knowledge calls producing winners that are structurally
// unwritable. Only the last clause needs knowledge to decide: a goal with too few
// of its own predicates can still search, because a validated grounded proposal is
// a whole conjunction and needs no second atom to be formable — and that is exactly
// the sparse-findings goal reuse exists for.
func (w *Worker) selectPolicy(
	ctx context.Context,
	target searchTarget,
	goalText string,
	obj objective.Objective,
	schema sandboxclient.Schema,
	findingAtoms []atom,
	findings []graph.CausalTriplet,
	baseline float64,
	bestSingle *float64,
) (SearchPolicy, []atom, string) {
	if w.cfg.MaxOrder < 2 {
		return nil, findingAtoms, skipNoConjunction
	}
	if w.cfg.Policy != policyUCT {
		vocab := w.vocabulary(ctx, target.goalID, goalText, schema, findingAtoms)
		if len(vocab.atoms) < 2 {
			return nil, vocab.atoms, skipNoConjunction
		}
		return newBeamPolicy(vocab.atoms, w.cfg, baseline, obj.Direction), vocab.atoms, ""
	}

	if bestSingle == nil {
		return nil, findingAtoms, skipNoBestSingle
	}
	vocab := w.vocabulary(ctx, target.goalID, goalText, schema, findingAtoms)
	proposals := w.groundProposals(ctx, target, goalText, schema)
	if len(vocab.atoms) < 2 && len(proposals) == 0 {
		return nil, vocab.atoms, skipNoConjunction
	}
	evidence := w.causalEvidence(ctx, target.goalID, findings, proposals)
	policy := newUCTPolicy(vocab.atoms, proposals, atomPriors(vocab), evidence, w.cfg, baseline, obj.Direction)
	return policy, vocab.atoms, ""
}

// causalEvidence collects the verified causal effects behind everything the search
// might build a candidate from: this goal's own eligible findings, and the
// heuristics that proposed conjunctions for it. One batched read each rather than a
// lookup per candidate, because the search weights every value estimate and a
// per-node read would put that many round trips inside the measurement loop.
//
// A failed read degrades to no evidence, which multiplies every estimate by exactly
// one — the search still runs, unweighted. Losing the weighting is a worse search;
// losing the run is a worse outcome.
func (w *Worker) causalEvidence(ctx context.Context, goalID string, findings []graph.CausalTriplet, proposals []groundedProposal) map[string]graph.CausalEvidence {
	evidence := map[string]graph.CausalEvidence{}

	interventionIDs := make([]string, 0, len(findings))
	for _, f := range findings {
		interventionIDs = appendMissing(interventionIDs, f.Intervention.ID)
	}
	if len(interventionIDs) > 0 {
		found, err := w.repo.CausalEvidenceForInterventions(ctx, interventionIDs)
		if err != nil {
			w.evidenceFailure(ctx, goalID, err)
		}
		for id, e := range found {
			evidence[id] = e
		}
	}

	var heuristicIDs []string
	for _, prop := range proposals {
		heuristicIDs = appendMissing(heuristicIDs, prop.heuristicID)
	}
	if len(heuristicIDs) > 0 {
		found, err := w.repo.CausalEvidenceForHeuristics(ctx, heuristicIDs)
		if err != nil {
			w.evidenceFailure(ctx, goalID, err)
		}
		for id, e := range found {
			evidence[id] = e
		}
	}
	return evidence
}

// atomPriors turns the critic's column ranking into per-atom prior mass, strongest
// first and falling to the last ranked column's share. An atom on an unranked
// column is simply absent, which the policy reads as its uniform floor — so a
// critic that ranked nothing leaves every move equally weighted.
func atomPriors(vocab atomVocabulary) map[string]float64 {
	if len(vocab.ranking) == 0 {
		return nil
	}
	at := make(map[string]int, len(vocab.ranking))
	for i, col := range vocab.ranking {
		at[strings.ToLower(col)] = i
	}
	total := float64(len(vocab.ranking))
	priors := make(map[string]float64, len(vocab.atoms))
	for _, a := range vocab.atoms {
		i, ranked := at[strings.ToLower(a.constraint.Field)]
		if !ranked {
			continue
		}
		priors[a.key] = (total - float64(i)) / total
	}
	return priors
}

// vocabularyFailure records a vocabulary that could not be widened. The run
// proceeds on the predicates Phase 1 measured, and the event is filed as a failure
// like every other *_failure action in the package, because that is the class an
// operator enumerates to find what degraded.
func (w *Worker) vocabularyFailure(ctx context.Context, goalID string, err error) {
	log.Printf("sleepcycle: schema atom vocabulary for %q: %v", goalID, err)
	w.report(ctx, "sleepcycle_atom_vocabulary_failure", "failure", map[string]any{
		"optimization_function_id": goalID,
		"error":                    err.Error(),
	})
}

// evidenceFailure records causal evidence that could not be read; the run continues
// unweighted, and the event is filed as a failure so the degradation is enumerable.
func (w *Worker) evidenceFailure(ctx context.Context, goalID string, err error) {
	log.Printf("sleepcycle: load causal evidence for %q: %v", goalID, err)
	w.report(ctx, "sleepcycle_causal_evidence_failure", "failure", map[string]any{
		"optimization_function_id": goalID,
		"error":                    err.Error(),
	})
}
