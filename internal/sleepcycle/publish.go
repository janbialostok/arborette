package sleepcycle

import (
	"cmp"
	"context"
	"log"
	"slices"
	"strings"

	"github.com/arborette/arborette/internal/domain"
	"github.com/arborette/arborette/internal/graph"
	"github.com/arborette/arborette/internal/objective"
)

// containmentMargin is the relative edge one candidate must hold over another
// before their nesting is read as a restatement worth collapsing. A depth-4
// branch measures one relationship at four sliding resolutions and should
// publish only its strongest, but a broad segment that a narrow refinement
// outscores by two percent is not a restatement — it is the sturdier of two
// genuinely different reads, and evicting it would trade evidence for noise.
//
// Comparing scores multiplicatively is sound because every candidate cleared the
// Improves gate before selection sees it, which makes its directional delta, and
// therefore its score, strictly positive. Everything that ranks or evicts on
// score depends on that.
const containmentMargin = 0.10

// candidate is one segment eligible to become a Meta-Heuristic, normalized
// across the two sources that produce them: a verified Phase-1 finding, and a
// derived macro-segment this run wrote back. Everything downstream — dedupe,
// ranking, abstraction — reads only these fields, so once constructed the two
// sources are indistinguishable and compete on one ranking.
// value, support, and score are carried together as the whole measurement, not
// just the part selection ranks on: score alone cannot say what was measured or
// how much evidence stood behind it.
type candidate struct {
	canonical      string
	filters        []domain.Constraint
	value          float64
	support        int64
	score          float64
	abstractedFrom []string
}

// candidatesFromFindings turns the goal's eligible Phase-1 findings into
// publication candidates — the half of the union that does not depend on the
// search having found anything.
//
// The filter set read is the *effective* one, not the new one. Outcome.Value was
// measured under the finding's whole cumulative branch segment, so the effective
// set is the only predicate set that value describes. Publishing under the new
// filters alone would mint a Meta-Heuristic whose definition names a broader
// population than the number backing it.
func (w *Worker) candidatesFromFindings(ctx context.Context, goalID string, findings []graph.CausalTriplet, obj objective.Objective, baseline float64) []candidate {
	var cands []candidate
	for _, f := range findings {
		filters, err := domain.DecodeConstraints(f.Intervention.Properties[domain.PropEffectiveFilters])
		if err != nil {
			w.publicationFailure(ctx, goalID, f.Intervention.ID, err)
			continue
		}
		// Support of zero is never a measured row count — it is a legacy outcome or
		// an extraction one, and production's own measure path already treats a
		// zero-row segment as failed. Excluding it unconditionally rather than only
		// under a positive floor also keeps the shrinkage weight defined: with
		// MinSupport at 0 a support-0 candidate would compute 0/(0+0).
		if f.Outcome.Support <= 0 || f.Outcome.Support < int64(w.cfg.MinSupport) {
			continue
		}
		v, ok := objective.NumericValue(f.Outcome.Value, obj.Label)
		if !ok || !objective.Improves(baseline, v, obj.Direction) {
			continue
		}
		canonical, err := CanonicalFilters(filters)
		if err != nil {
			w.publicationFailure(ctx, goalID, f.Intervention.ID, err)
			continue
		}
		// A finding with no effective filters *is* the baseline measurement: there is
		// no segment to name, so there is nothing to abstract.
		if canonical == "" {
			continue
		}
		cands = append(cands, candidate{
			canonical:      canonical,
			filters:        filters,
			value:          v,
			support:        f.Outcome.Support,
			score:          w.shrunkScore(v, baseline, f.Outcome.Support, obj.Direction),
			abstractedFrom: appendMissing(nil, f.State.ID, f.Intervention.ID, f.Outcome.ID),
		})
	}
	return cands
}

// candidatesFromWinners turns the macro-segments this run persisted into
// publication candidates, so the search's output competes with Phase 1's rather
// than bypassing it.
//
// The improvement check is against the global baseline, not S*. materiallyBetter
// gates on S*, and S* can itself sit below the baseline — a macro-segment that
// beats a bad S* while still moving the objective the wrong way is a legitimate
// search result worth persisting, but publishing it would ship a heuristic its
// own measurement contradicts. It is also what upholds the positive-score
// invariant documented on containmentMargin.
func (w *Worker) candidatesFromWinners(written []winner, atoms []atom, obj objective.Objective, baseline float64) []candidate {
	sources := map[string][]string{}
	for _, a := range atoms {
		sources[a.key] = a.sourceIDs
	}
	var cands []candidate
	for _, won := range written {
		if !objective.Improves(baseline, won.value, obj.Direction) {
			continue
		}
		cands = append(cands, candidate{
			canonical:      won.node.canonical,
			filters:        won.node.filters,
			value:          won.value,
			support:        won.support,
			score:          w.shrunkScore(won.value, baseline, won.support, obj.Direction),
			abstractedFrom: abstractedFromIDs(won, sources),
		})
	}
	return cands
}

// shrunkScore ranks a candidate by how far it moves the objective, discounted by
// how much evidence backs the move. The weight support/(support+k) is the
// empirical-Bayes shrinkage of the measured value toward the baseline, and
// multiplying the delta by it is algebraically identical to shrinking the value
// first and then taking the delta.
//
// Ranking on the raw delta instead puts a 54-row segment that hit 100% above an
// 800-row segment at 98.9% — publishing the thinner evidence as the stronger
// heuristic, which is precisely backwards for a consumer that can only act on
// what was published. k is MinSupport rather than a knob of its own: the floor
// already states how many rows the operator considers sufficient evidence, and
// that is exactly where the weight reaches one half.
func (w *Worker) shrunkScore(value, baseline float64, support int64, direction domain.TargetDirection) float64 {
	k := float64(w.cfg.MinSupport)
	return directionalDelta(value, baseline, direction) * float64(support) / (float64(support) + k)
}

// directionalDelta is the value's movement in the objective's desired direction,
// so a plain descending sort ranks both directions. Every ranked stage shares it:
// a sign error here silently inverts a whole ranking, so it gets one definition
// rather than one per stage.
func directionalDelta(value, baseline float64, direction domain.TargetDirection) float64 {
	d := value - baseline
	if direction == domain.Minimize {
		return -d
	}
	return d
}

// publicationFailure records one candidate that could not be built. Isolated per
// item like every other stage: a single undecodable filter property must not
// cost the run every other candidate.
func (w *Worker) publicationFailure(ctx context.Context, goalID, interventionID string, err error) {
	log.Printf("sleepcycle: build publication candidate for %q: %v", goalID, err)
	w.report(ctx, "sleepcycle_publication_failure", "failure", map[string]any{
		"optimization_function_id": goalID,
		"intervention_id":          interventionID,
		"error":                    err.Error(),
	})
}

// selectPublications cuts the union of candidates down to what is worth
// publishing: one entry per distinct relationship, ranked by shrunk score, capped
// at maxN.
//
// The cap is the point of the stage. A consuming agent reaches this knowledge
// only through similarity search over Meta-Heuristic embeddings, so publishing
// every improving segment of a deep tree buries the strong ones among near-
// duplicate restatements of the same relationship.
//
// Ranking happens before both later stages, not just before the cap: the
// containment pass needs to know which candidate of a nested pair is the stronger
// one, and the cap is only meaningful over an already-ranked set.
func selectPublications(cands []candidate, maxN int) []candidate {
	ranked := dedupeByCanonical(cands)
	// Ties break on the canonical filter so a re-run publishes the same set --
	// slices.SortFunc is not stable, so without it a capped run over tied
	// candidates would publish a different subset each time.
	slices.SortFunc(ranked, func(a, b candidate) int {
		if d := cmp.Compare(b.score, a.score); d != 0 {
			return d
		}
		return strings.Compare(a.canonical, b.canonical)
	})
	kept := dropContainedRestatements(ranked)
	if len(kept) > maxN {
		kept = kept[:maxN]
	}
	return kept
}

// dedupeByCanonical collapses candidates naming the identical predicate set,
// keeping the higher-scoring one. A derived macro-segment and a Phase-1 finding
// can land on the same segment, and both mint the same Meta-Heuristic id — so
// without this the second would occupy a publication slot only to be skipped by
// the abstraction stage's resume check.
//
// The evidence links are unioned rather than taken from the survivor alone. Two
// Phase-1 findings can reach the same effective segment by adding their
// predicates in opposite orders, in which case they tie on value, support, and
// score; keeping one arbitrarily would drop the other's triplet from the
// Meta-Heuristic's provenance, and ListEligibleFindings has no ORDER BY, so which
// one was dropped would not even be reproducible across runs.
func dedupeByCanonical(cands []candidate) []candidate {
	at := make(map[string]int, len(cands))
	out := make([]candidate, 0, len(cands))
	for _, c := range cands {
		i, seen := at[c.canonical]
		if !seen {
			at[c.canonical] = len(out)
			out = append(out, c)
			continue
		}
		// Built fresh rather than appended onto either slice, so the merge cannot
		// write through a backing array the other candidate still shares. The union
		// is order-insensitive, so it is computed before the score comparison picks
		// which candidate's remaining fields survive.
		merged := appendMissing(appendMissing(nil, out[i].abstractedFrom...), c.abstractedFrom...)
		if c.score > out[i].score {
			out[i] = c
		}
		out[i].abstractedFrom = merged
	}
	return out
}

// dropContainedRestatements drops a candidate when one already kept nests with it
// and outscores it by more than containmentMargin — the pair is one relationship
// measured at two resolutions, and only the stronger reading earns a slot.
//
// It requires ranked to be sorted descending and walks it in that order, which is
// what guarantees an evicting candidate is itself published. Deciding every pair
// against the full input instead lets a candidate that was evicted take another
// one down with it: with {A} at 1.0, {A,B} at 1.2 and {A,C} at 0.85, {A,B} evicts
// {A}, and the already-evicted {A} then evicts {A,C} — leaving the C predicate
// described by nothing at all, silently and with no audit record.
//
// Order is preserved so the caller's cap still keeps the highest-scoring
// survivors. The quadratic pass is fine on a set already bounded by the eligible
// findings and the beam's winners.
func dropContainedRestatements(ranked []candidate) []candidate {
	kept := make([]candidate, 0, len(ranked))
	keptComponents := make([]map[string]bool, 0, len(ranked))
	for _, c := range ranked {
		components := componentSet(c.canonical)
		restated := false
		for i, k := range kept {
			if nests(components, keptComponents[i]) && k.score > c.score*(1+containmentMargin) {
				restated = true
				break
			}
		}
		if restated {
			continue
		}
		kept = append(kept, c)
		keptComponents = append(keptComponents, components)
	}
	return kept
}

// componentSet reads a canonical key as the set of predicates it encodes, so two
// segments can be compared for containment.
func componentSet(canonical string) map[string]bool {
	parts := canonicalComponents(canonical)
	set := make(map[string]bool, len(parts))
	for _, p := range parts {
		set[p] = true
	}
	return set
}

// nests reports whether one predicate set is a strict subset of the other, in
// either direction — the shape that makes two segments a restatement rather than
// two findings.
func nests(a, b map[string]bool) bool {
	return strictSubset(a, b) || strictSubset(b, a)
}

func strictSubset(a, b map[string]bool) bool {
	if len(a) >= len(b) {
		return false
	}
	for k := range a {
		if !b[k] {
			return false
		}
	}
	return true
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
