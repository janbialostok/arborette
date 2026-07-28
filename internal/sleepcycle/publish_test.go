package sleepcycle

import (
	"context"
	"errors"
	"math"
	"slices"
	"testing"

	"github.com/arborette/arborette/internal/domain"
	"github.com/arborette/arborette/internal/graph"
	"github.com/arborette/arborette/internal/objective"
)

// publicationsFrom runs the two stages a test exercises together — build the
// candidates, then select — so a case states a fixture and reads a ranking.
func publicationsFrom(t *testing.T, cfg Config, obj objective.Objective, baseline float64, findings []graph.CausalTriplet) []candidate {
	t.Helper()
	h := newHarness(t, cfg)
	return selectPublications(
		h.worker.candidatesFromFindings(context.Background(), "g1", findings, obj, baseline),
		cfg.MaxPublications,
	)
}

// TestShrunkScoreRanksEvidenceOverThinExtremes is the live pathology, pinned: a
// 54-row segment that hit 100% must not outrank an 800-row segment at 98.9%.
// Publication is what a consuming agent can act on, so ranking on the raw delta
// ships the thinner evidence as the stronger heuristic.
//
// The floor is raised to the production default deliberately. Shrinkage uses
// MinSupport as its constant, so at the test tuning's floor of 0 every weight is
// 1 and the mechanism under test is switched off — the second half of this case
// pins that, and would pass on any implementation that merely sorted by delta.
func TestShrunkScoreRanksEvidenceOverThinExtremes(t *testing.T) {
	findings := []graph.CausalTriplet{
		findingWithSupport("thin", 1.0, 54, "a"),
		findingWithSupport("broad", 0.989, 800, "b"),
	}

	cfg := testConfig()
	cfg.MinSupport = 30
	ranked := publicationsFrom(t, cfg, objectiveFor(t, domain.Maximize), 0.5, findings)
	if len(ranked) != 2 {
		t.Fatalf("both findings clear the floor and improve, got %d candidates", len(ranked))
	}
	if ranked[0].support != 800 {
		t.Fatalf("the 800-row segment must rank first, got the %d-row one", ranked[0].support)
	}

	flat := testConfig()
	flat.MinSupport = 0
	ranked = publicationsFrom(t, flat, objectiveFor(t, domain.Maximize), 0.5, findings)
	if ranked[0].support != 54 {
		t.Fatalf("with shrinkage disabled the raw delta must win, got the %d-row segment first", ranked[0].support)
	}
}

// TestPublicationGatesOnTheBaselineOnly: the gate is objective.Improves against
// the goal's global baseline and nothing else. There is deliberately no lift
// threshold here — a bar phrased relative to a strong Phase 1 gets harder to clear
// the better Phase 1 did, which must not decide whether a goal publishes.
func TestPublicationGatesOnTheBaselineOnly(t *testing.T) {
	cfg := testConfig()
	cfg.MinSupport = 30

	t.Run("maximize", func(t *testing.T) {
		got := publicationsFrom(t, cfg, objectiveFor(t, domain.Maximize), 10.0, []graph.CausalTriplet{
			findingWithSupport("above", 10.0001, 100, "a"), // barely improves — publishes
			findingWithSupport("equal", 10.0, 100, "b"),    // no movement
			findingWithSupport("below", 9.0, 100, "c"),     // wrong way
		})
		if len(got) != 1 || got[0].canonical != canonicalFor(t, "a") {
			t.Fatalf("only the improving segment may publish, got %+v", canonicals(got))
		}
	})

	t.Run("minimize", func(t *testing.T) {
		obj := objectiveFor(t, domain.Minimize)
		got := publicationsFrom(t, cfg, obj, 10.0, []graph.CausalTriplet{
			findingWithSupport("below", 9.0, 100, "a"), // improves under Minimize
			findingWithSupport("equal", 10.0, 100, "b"),
			findingWithSupport("above", 11.0, 100, "c"),
		})
		if len(got) != 1 || got[0].canonical != canonicalFor(t, "a") {
			t.Fatalf("under Minimize only the lower segment may publish, got %+v", canonicals(got))
		}
	})
}

// TestLegacyFindingIsNeverPublished: an outcome with no recorded support reads as
// 0, and a heuristic published off unknown evidence is exactly what the support
// floor exists to prevent. The floor-of-0 case is the sharper one — the skip is
// unconditional, which is also what keeps the shrinkage weight from computing
// 0/(0+0).
func TestLegacyFindingIsNeverPublished(t *testing.T) {
	legacy := []graph.CausalTriplet{findingWithSupport("legacy", 50.0, 0, "a")}

	for name, floor := range map[string]int{"at the production floor": 30, "with the floor disabled": 0} {
		t.Run(name, func(t *testing.T) {
			cfg := testConfig()
			cfg.MinSupport = floor
			if got := publicationsFrom(t, cfg, objectiveFor(t, domain.Maximize), 1.0, legacy); len(got) != 0 {
				t.Fatalf("a support-less finding must never publish, got %+v", canonicals(got))
			}
		})
	}
}

// TestSupportFloorExcludesThinButRecordedEvidence covers the half of the support
// guard the legacy case cannot reach. The condition fuses two rules — an
// unconditional skip at zero, and the configurable floor — so a fixture that only
// ever uses support 0 leaves the floor itself unpinned, and a 3-row segment would
// publish as a Meta-Heuristic at the production floor of 30.
func TestSupportFloorExcludesThinButRecordedEvidence(t *testing.T) {
	cfg := testConfig()
	cfg.MinSupport = 30
	got := publicationsFrom(t, cfg, objectiveFor(t, domain.Maximize), 1.0, []graph.CausalTriplet{
		findingWithSupport("thin", 50.0, 3, "a"),    // real rows, below the floor
		findingWithSupport("atfloor", 2.0, 30, "b"), // exactly at the floor — inclusive
	})

	if len(got) != 1 || got[0].canonical != canonicalFor(t, "b") {
		t.Fatalf("only the floor-clearing segment may publish, got %+v", canonicals(got))
	}
}

// TestMinimizeRanksByDirectionalDelta pins the sign of the Minimize normalization
// in a ranking rather than a gate. Every other Minimize case leaves exactly one
// survivor, so order is unobservable — yet a flipped sign would invert the
// published ranking for every Minimize goal AND make every score negative, which
// silently breaks the strictly-positive assumption the containment margin rests on.
func TestMinimizeRanksByDirectionalDelta(t *testing.T) {
	cfg := testConfig()
	cfg.MinSupport = 30
	got := publicationsFrom(t, cfg, objectiveFor(t, domain.Minimize), 10.0, []graph.CausalTriplet{
		findingWithSupport("near", 9.0, 500, "a"), // improves a little
		findingWithSupport("far", 2.0, 500, "b"),  // improves a lot — must rank first
	})

	if len(got) != 2 {
		t.Fatalf("both segments improve under Minimize, got %+v", canonicals(got))
	}
	if got[0].canonical != canonicalFor(t, "b") {
		t.Fatalf("the segment furthest below the baseline must rank first, got %+v", canonicals(got))
	}
	for _, c := range got {
		if c.score <= 0 {
			t.Fatalf("a gate-cleared candidate must score positive, got %v for %q", c.score, c.canonical)
		}
	}
}

// TestPublishesTheEffectiveSegmentNotTheNewPredicate: a deep finding's value was
// measured under its whole cumulative branch, so that is the segment the
// Meta-Heuristic must describe. Publishing under the newly-introduced predicate
// alone would name a broader population than the number backing it.
func TestPublishesTheEffectiveSegmentNotTheNewPredicate(t *testing.T) {
	cfg := testConfig()
	cfg.MinSupport = 30
	got := publicationsFrom(t, cfg, objectiveFor(t, domain.Maximize), 1.0, []graph.CausalTriplet{
		findingWithEffective("deep", 5.0, 200, []string{"b"}, []string{"a", "b"}),
	})

	if len(got) != 1 {
		t.Fatalf("expected one candidate, got %+v", canonicals(got))
	}
	if got[0].canonical != canonicalFor(t, "a", "b") {
		t.Fatalf("canonical = %q, want the cumulative segment, not the new predicate alone", got[0].canonical)
	}
	if len(got[0].filters) != 2 {
		t.Fatalf("the abstracted filters must be the whole segment, got %+v", got[0].filters)
	}
}

// TestUndecodableFiltersAreIsolated: one malformed filter property must cost its
// own candidate and nothing else, audited so the gap is visible.
func TestUndecodableFiltersAreIsolated(t *testing.T) {
	cfg := testConfig()
	cfg.MinSupport = 30
	h := newHarness(t, cfg)

	broken := findingWithSupport("broken", 5.0, 100, "a")
	broken.Intervention.Properties["effective_filters"] = "not a filter set"
	findings := []graph.CausalTriplet{broken, findingWithSupport("sound", 5.0, 100, "b")}

	got := h.worker.candidatesFromFindings(context.Background(), "g1", findings, objectiveFor(t, domain.Maximize), 1.0)
	if len(got) != 1 || got[0].canonical != canonicalFor(t, "b") {
		t.Fatalf("the sound finding must survive its neighbour's decode failure, got %+v", canonicals(got))
	}
	rec, ok := h.audits.find("sleepcycle_publication_failure")
	if !ok {
		t.Fatalf("expected a publication-failure audit: %+v", h.audits.records)
	}
	if rec.detail["intervention_id"] != "i-broken" {
		t.Fatalf("the audit must name the finding it lost: %+v", rec.detail)
	}
}

// TestContainmentDedupeHoldsAMargin: a segment and a refinement of it restate one
// relationship at two resolutions, and publishing both spends two of a small
// number of slots on one insight. But a broad segment is not noise merely because
// a narrower one edges past it, so the collapse only fires on a decisive win.
func TestContainmentDedupeHoldsAMargin(t *testing.T) {
	broad := canonicalFor(t, "a")
	refined := canonicalFor(t, "a", "b")
	disjoint := canonicalFor(t, "c")

	t.Run("a decisive refinement evicts the broad segment", func(t *testing.T) {
		got := selectPublications([]candidate{
			{canonical: broad, score: 1.0},
			{canonical: refined, score: 2.0},
		}, 20)
		if len(got) != 1 || got[0].canonical != refined {
			t.Fatalf("only the stronger resolution should survive, got %+v", canonicals(got))
		}
	})

	t.Run("a near-tie keeps both", func(t *testing.T) {
		// A literal 2% edge, not one expressed in terms of containmentMargin: a
		// margin-relative fixture moves with the constant and would still pass if the
		// margin were deleted outright, which is the regression worth catching.
		got := selectPublications([]candidate{
			{canonical: broad, score: 1.0},
			{canonical: refined, score: 1.02},
		}, 20)
		if len(got) != 2 {
			t.Fatalf("a segment must not be evicted by a refinement it nearly matches, got %+v", canonicals(got))
		}
	})

	t.Run("a weak refinement is evicted by an already-kept broad segment", func(t *testing.T) {
		// The mirror of the decisive case: here the KEPT candidate is the broader one
		// and the weaker candidate is the refinement. Nesting is a relation, not an
		// ordering, so a pass that only ever compared one way would republish a deep
		// branch at every resolution. The disjoint {c} sits between them by score, so
		// the comparison must also reach past the most recently kept candidate.
		got := selectPublications([]candidate{
			{canonical: broad, score: 5.0},
			{canonical: disjoint, score: 4.0},
			{canonical: refined, score: 1.0},
		}, 20)
		if len(got) != 2 {
			t.Fatalf("the weak refinement restates the kept broad segment, got %+v", canonicals(got))
		}
		if got[0].canonical != broad || got[1].canonical != disjoint {
			t.Fatalf("expected the broad segment and the unrelated one, got %+v", canonicals(got))
		}
	})

	t.Run("disjoint segments both survive", func(t *testing.T) {
		got := selectPublications([]candidate{
			{canonical: broad, score: 1.0},
			{canonical: disjoint, score: 5.0},
		}, 20)
		if len(got) != 2 {
			t.Fatalf("unrelated segments are two insights, not one, got %+v", canonicals(got))
		}
	})
}

// TestEvictedCandidateCannotEvictAnother: drops are decided against surviving
// candidates only, so no segment is ever removed as a restatement of something
// that is not itself published.
func TestEvictedCandidateCannotEvictAnother(t *testing.T) {
	got := selectPublications([]candidate{
		{canonical: canonicalFor(t, "a"), score: 1.0},
		{canonical: canonicalFor(t, "a", "b"), score: 1.2},
		{canonical: canonicalFor(t, "a", "c"), score: 0.85},
	}, 20)

	if len(got) != 2 {
		t.Fatalf("only {a} is restated by a surviving segment, got %+v", canonicals(got))
	}
	for _, want := range []string{canonicalFor(t, "a", "b"), canonicalFor(t, "a", "c")} {
		if !slices.ContainsFunc(got, func(c candidate) bool { return c.canonical == want }) {
			t.Fatalf("%q must survive — nothing published contains it: %+v", want, canonicals(got))
		}
	}
}

// TestSelectionRanksAndDedupesBeforeCapping pins the cap as the LAST stage, which
// both earlier stages depend on. Capping before ranking publishes an arbitrary N
// and drops the best-evidenced segments; capping before the containment pass
// spends a slot on a restatement that is then dropped, so a run publishes fewer
// segments than the cap allows while real ones were available.
//
// The input is deliberately in worst-first order, and {a} is a restatement of
// {a,b}: with the cap at 2 the answer is {a,b} and {c}, and each mis-ordering
// yields a different set.
func TestSelectionRanksAndDedupesBeforeCapping(t *testing.T) {
	got := selectPublications([]candidate{
		{canonical: canonicalFor(t, "d"), score: 0.5},
		{canonical: canonicalFor(t, "c"), score: 1.0},
		{canonical: canonicalFor(t, "a", "b"), score: 9.0},
		{canonical: canonicalFor(t, "a"), score: 8.0}, // restated by {a,b}
	}, 2)

	if len(got) != 2 {
		t.Fatalf("the cap must be filled with publishable segments, got %+v", canonicals(got))
	}
	if got[0].canonical != canonicalFor(t, "a", "b") || got[1].canonical != canonicalFor(t, "c") {
		t.Fatalf("expected the strongest segment and the strongest non-restatement, got %+v", canonicals(got))
	}
}

// TestTiedCandidatesPublishDeterministically: slices.SortFunc is not stable, so
// without the canonical tie-break a capped run over equally-scored candidates
// would publish a different subset each time it ran.
func TestTiedCandidatesPublishDeterministically(t *testing.T) {
	tied := []candidate{
		{canonical: canonicalFor(t, "d"), score: 1.0},
		{canonical: canonicalFor(t, "a"), score: 1.0},
		{canonical: canonicalFor(t, "c"), score: 1.0},
		{canonical: canonicalFor(t, "b"), score: 1.0},
	}

	// Distinct orderings, not rotations: rotations preserve relative order and so
	// reach only 4 of the 24 permutations, while the graph read that feeds this has
	// no ORDER BY and can hand over any of them.
	first := canonicals(selectPublications(slices.Clone(tied), 2))
	for _, order := range [][]int{{0, 1, 2, 3}, {3, 2, 1, 0}, {1, 3, 0, 2}, {2, 0, 3, 1}, {0, 3, 1, 2}} {
		shuffled := make([]candidate, 0, len(order))
		for _, i := range order {
			shuffled = append(shuffled, tied[i])
		}
		if got := canonicals(selectPublications(shuffled, 2)); !slices.Equal(got, first) {
			t.Fatalf("tied candidates published a different set for order %v: %+v vs %+v", order, got, first)
		}
	}
}

// TestExactDedupeKeepsTheHigherScore: a derived macro-segment and a Phase-1
// finding can name the same predicate set, and both mint the same Meta-Heuristic
// id — so a duplicate would occupy a slot only to be skipped by the abstraction
// stage's resume check.
func TestExactDedupeKeepsTheHigherScore(t *testing.T) {
	shared := canonicalFor(t, "a", "b")
	got := selectPublications([]candidate{
		{canonical: shared, score: 1.0, support: 100},
		{canonical: shared, score: 3.0, support: 800},
	}, 20)

	if len(got) != 1 {
		t.Fatalf("one segment is one publication, got %d", len(got))
	}
	if got[0].support != 800 {
		t.Fatalf("the better-evidenced duplicate must win, got support %d", got[0].support)
	}
}

// TestExactDedupeUnionsEvidence: two Phase-1 findings can reach the same effective
// segment by adding their predicates in opposite orders, tying on score. Keeping
// one arbitrarily would drop the other's triplet from the heuristic's provenance —
// and since the graph read has no ORDER BY, which one was dropped would not even
// be reproducible.
func TestExactDedupeUnionsEvidence(t *testing.T) {
	shared := canonicalFor(t, "a", "b")
	got := selectPublications([]candidate{
		{canonical: shared, score: 1.0, abstractedFrom: []string{"s-1", "i-1", "o-1"}},
		{canonical: shared, score: 1.0, abstractedFrom: []string{"s-2", "i-2", "o-2"}},
	}, 20)

	if len(got) != 1 {
		t.Fatalf("one segment is one publication, got %d", len(got))
	}
	for _, want := range []string{"s-1", "i-1", "o-1", "s-2", "i-2", "o-2"} {
		if !contains(got[0].abstractedFrom, want) {
			t.Fatalf("both findings' evidence must survive the merge, missing %q: %+v", want, got[0].abstractedFrom)
		}
	}
	// The later duplicate outscoring the earlier is the production-ordinary shape —
	// a strong derived macro-segment landing on the canonical a weaker Phase-1
	// finding already produced — and the displaced candidate's evidence must survive
	// the swap, not be replaced by the winner's own.
	swapped := selectPublications([]candidate{
		{canonical: shared, score: 1.0, abstractedFrom: []string{"s-1", "i-1", "o-1"}},
		{canonical: shared, score: 9.0, abstractedFrom: []string{"s-2", "i-2", "o-2"}},
	}, 20)
	if len(swapped) != 1 || swapped[0].score != 9.0 {
		t.Fatalf("the higher-scoring duplicate must win, got %+v", swapped)
	}
	for _, want := range []string{"s-1", "i-1", "o-1", "s-2", "i-2", "o-2"} {
		if !contains(swapped[0].abstractedFrom, want) {
			t.Fatalf("the displaced candidate's evidence must survive, missing %q: %+v", want, swapped[0].abstractedFrom)
		}
	}

	// Overlap is the production shape — a depth-2 finding contributes the same
	// triplet ids the derived macro-segment already carries as atom sources — and
	// CreateMetaHeuristic rejects a duplicate reference outright.
	overlapping := selectPublications([]candidate{
		{canonical: shared, score: 2.0, abstractedFrom: []string{"s-1", "i-1", "o-1"}},
		{canonical: shared, score: 1.0, abstractedFrom: []string{"i-1", "o-1", "o-9"}},
	}, 20)
	seen := map[string]int{}
	for _, id := range overlapping[0].abstractedFrom {
		seen[id]++
	}
	if len(seen) != 4 {
		t.Fatalf("the union must be a set of 4 distinct ids, got %+v", overlapping[0].abstractedFrom)
	}
	for id, n := range seen {
		if n > 1 {
			t.Fatalf("id %q appears %d times; CreateMetaHeuristic rejects duplicate references", id, n)
		}
	}
}

// TestWrittenWinnerCarriesTheMeasuredSupport pins the support the winner struct
// exists to carry. The publication score weights a segment by its evidence, so a
// winner that lost its row count on the way out of write-back would be ranked as
// if it had almost none — or, at a floor of 0, score NaN from 0/(0+0) and poison
// the sort.
func TestWrittenWinnerCarriesTheMeasuredSupport(t *testing.T) {
	cfg := testConfig()
	cfg.MinSupport = 30
	h := newHarness(t, cfg)
	target := searchTarget{goalID: "g1", dataSourceRef: "ref.csv", namespace: goalNamespace("g1")}
	seg := measuredNode{node: singleNode(t, "a"), measurement: Measurement{Value: 7.0, Support: 640}}

	written := h.worker.writeWinners(context.Background(), target, objectiveFor(t, domain.Maximize), 1.0, []measuredNode{seg})
	if len(written) != 1 {
		t.Fatalf("expected one written winner, got %d", len(written))
	}
	if written[0].support != 640 {
		t.Fatalf("support = %d, want the measured row count 640", written[0].support)
	}
	// The objective persisted alongside it is the pinned one, not a hand-built
	// stand-in — which is what makes deriving the test fixture from the goal
	// load-bearing rather than decorative.
	if got := h.repo.interventions[0].Properties[domain.PropObjectiveAggregation]; got != "avg" {
		t.Fatalf("persisted aggregation = %v, want the pinned \"avg\"", got)
	}

	// And it reaches the score: 640/(640+30) of the delta, not 1/(1+30).
	cands := h.worker.candidatesFromWinners(written, nil, objectiveFor(t, domain.Maximize), 1.0)
	if len(cands) != 1 {
		t.Fatalf("the winner improves on the baseline, so it must be a candidate: %+v", cands)
	}
	if want := 6.0 * 640 / 670; math.Abs(cands[0].score-want) > 1e-9 {
		t.Fatalf("score = %v, want %v — the measured support must weight the delta", cands[0].score, want)
	}
}

// TestPhaseOnePublicationLinksItsEvidence: a Meta-Heuristic published straight
// from Phase 1 must carry its finding's triplet ids. Without them
// trace_causal_chain returns nothing for it and MarkStaleMetaHeuristics can never
// retire it when an analyst rejects the underlying finding — and on a strong-Phase-1
// goal these are the majority of what gets published.
func TestPhaseOnePublicationLinksItsEvidence(t *testing.T) {
	cfg := testConfig()
	cfg.MaxOrder = 1 // skip the search; everything published comes from Phase 1
	cfg.MinSupport = 30
	h := newHarness(t, cfg)
	h.repo.findings = []graph.CausalTriplet{findingWithSupport("1", 5.0, 100, "a")}
	h.sandbox.baseline = 1.0

	if err := h.run(t); err != nil {
		t.Fatalf("run: %v", err)
	}
	mhID := derivedID(goalNamespace("g1"), roleMetaHeuristic, canonicalFor(t, "a"))
	refs, ok := h.repo.abstractedFrom[mhID]
	if !ok {
		t.Fatalf("no abstractedFrom recorded for the published finding: %+v", h.repo.abstractedFrom)
	}
	for _, want := range []string{"s-1", "i-1", "o-1"} {
		if !contains(refs, want) {
			t.Fatalf("the heuristic must link its finding's %q: %+v", want, refs)
		}
	}
}

// TestResumedPublicationRelinksItsEvidence pins the resume branch's three
// obligations: link this run's evidence to an already-published heuristic, spend
// no Claude call doing it, and leave the published definition untouched. See
// abstractOne for why an un-linked finding is evidence that can never retire it.
func TestResumedPublicationRelinksItsEvidence(t *testing.T) {
	cfg := testConfig()
	cfg.MaxOrder = 1 // skip the search; the candidate comes from Phase 1
	cfg.MinSupport = 30
	h := newHarness(t, cfg)
	h.repo.findings = []graph.CausalTriplet{findingWithSupport("2", 5.0, 100, "a")}
	h.sandbox.baseline = 1.0

	mhID := derivedID(goalNamespace("g1"), roleMetaHeuristic, canonicalFor(t, "a"))
	h.repo.existing[mhID] = domain.MetaHeuristic{ID: mhID, Definition: "published by an earlier run"}

	if err := h.run(t); err != nil {
		t.Fatalf("run: %v", err)
	}
	if h.claude.abstractCalls != 0 {
		t.Fatalf("an already-published segment must not be re-abstracted, got %d calls", h.claude.abstractCalls)
	}
	refs, ok := h.repo.abstractedFrom[mhID]
	if !ok {
		t.Fatalf("the resumed heuristic must still be linked to this run's evidence: %+v", h.repo.abstractedFrom)
	}
	for _, want := range []string{"s-2", "i-2", "o-2"} {
		if !contains(refs, want) {
			t.Fatalf("missing evidence link %q: %+v", want, refs)
		}
	}
	if len(h.repo.heuristics) != 1 || h.repo.heuristics[0].Definition != "published by an earlier run" {
		t.Fatalf("the existing definition must be re-issued verbatim, got %+v", h.repo.heuristics)
	}
}

// TestResumeFaultsAreScopedToWhatIsActuallyUnreachable: the re-link write is the
// one part of a resume that can fail, and the two resume shapes must dispose of
// that failure differently.
func TestResumeFaultsAreScopedToWhatIsActuallyUnreachable(t *testing.T) {
	// resumeHarness stages a goal whose single segment a previous run published.
	resumeHarness := func(t *testing.T, pending bool) (*harness, string) {
		t.Helper()
		cfg := testConfig()
		cfg.MaxOrder = 1
		cfg.MinSupport = 30
		h := newHarness(t, cfg)
		h.repo.findings = []graph.CausalTriplet{findingWithSupport("2", 5.0, 100, "a")}
		h.sandbox.baseline = 1.0
		mhID := derivedID(goalNamespace("g1"), roleMetaHeuristic, canonicalFor(t, "a"))
		h.repo.existing[mhID] = domain.MetaHeuristic{ID: mhID, Definition: "published earlier", EmbeddingPending: pending}
		return h, mhID
	}

	t.Run("an embedded heuristic stays published when its re-link fails", func(t *testing.T) {
		h, _ := resumeHarness(t, false)
		h.repo.createErrs["metaheuristic"] = errors.New("neo4j down")

		if err := h.run(t); err != nil {
			t.Fatalf("run: %v", err)
		}
		rec, _ := h.audits.find("sleepcycle_run_complete")
		if rec.detail["published"] != 1 {
			t.Fatalf("published = %v, want 1 — the heuristic is embedded and reachable regardless",
				rec.detail["published"])
		}
		if _, ok := h.audits.find("sleepcycle_relink_failure"); !ok {
			t.Fatalf("the un-attached evidence must still be surfaced: %+v", h.audits.records)
		}
		if h.audits.count("sleepcycle_abstraction_failure") != 0 {
			t.Fatal("a re-link fault is not a failure to abstract, and must not read as one")
		}
	})

	t.Run("a pending heuristic is not embedded when its re-link fails", func(t *testing.T) {
		h, _ := resumeHarness(t, true)
		h.repo.createErrs["metaheuristic"] = errors.New("neo4j down")

		if err := h.run(t); err != nil {
			t.Fatalf("run: %v", err)
		}
		if h.provider.calls != 0 {
			t.Fatal("embedding a node whose evidence write was rejected would publish it on provenance the graph does not hold")
		}
		if h.audits.count("sleepcycle_abstraction_failure") != 1 {
			t.Fatalf("expected the segment to be audited as failed, got %+v", h.audits.records)
		}
		rec, _ := h.audits.find("sleepcycle_run_complete")
		if rec.detail["published"] != 0 {
			t.Fatalf("published = %v, want 0 — the segment was never made searchable", rec.detail["published"])
		}
	})

	t.Run("a pending heuristic re-links and embeds with this run's evidence", func(t *testing.T) {
		h, mhID := resumeHarness(t, true)

		if err := h.run(t); err != nil {
			t.Fatalf("run: %v", err)
		}
		if h.claude.abstractCalls != 0 {
			t.Fatalf("a written node must not be re-abstracted, got %d calls", h.claude.abstractCalls)
		}
		if h.provider.calls == 0 || len(h.repo.cleared) == 0 {
			t.Fatal("the embed tail must still run and clear the pending flag")
		}
		for _, want := range []string{"s-2", "i-2", "o-2"} {
			if !contains(h.repo.abstractedFrom[mhID], want) {
				t.Fatalf("the re-link must carry this run's evidence, missing %q: %+v", want, h.repo.abstractedFrom[mhID])
			}
		}
		rec, ok := h.audits.find("sleepcycle_heuristic_injection")
		if !ok {
			t.Fatalf("expected an injection audit: %+v", h.audits.records)
		}
		// Asserted on content, not against nil: a nil []string inside an any is not
		// == nil, so a nil-check here would pass on the very regression it guards.
		refs, _ := rec.detail["abstracted_from"].([]string)
		if len(refs) == 0 {
			t.Fatalf("a resumed injection knows its evidence and must record it: %+v", rec.detail)
		}
	})
}

// TestFindingWithNoEffectiveSegmentIsNotPublished: a finding carrying no effective
// filters *is* the baseline measurement. Publishing it would mint a heuristic
// describing the unfiltered population, under the id derived from an empty
// canonical, with an empty filter set sent to Claude.
func TestFindingWithNoEffectiveSegmentIsNotPublished(t *testing.T) {
	cfg := testConfig()
	cfg.MinSupport = 30
	unfiltered := findingWithSupport("1", 5.0, 100, "a")
	delete(unfiltered.Intervention.Properties, "effective_filters")

	got := publicationsFrom(t, cfg, objectiveFor(t, domain.Maximize), 1.0, []graph.CausalTriplet{unfiltered})
	if len(got) != 0 {
		t.Fatalf("a finding with no effective segment must not publish, got %+v", canonicals(got))
	}
}

// TestPublicationIsCappedAtMaxPublications: the cap is what keeps similarity
// search useful — a deep tree yields dozens of improving segments, and publishing
// all of them buries the strong ones.
func TestPublicationIsCappedAtMaxPublications(t *testing.T) {
	cfg := testConfig()
	cfg.MaxOrder = 1 // no conjunction to form; every candidate comes from Phase 1
	cfg.MinSupport = 30
	cfg.MaxPublications = 2
	h := newHarness(t, cfg)
	h.repo.findings = findingsFor("a", "b", "c", "d", "e")
	h.sandbox.baseline = 0.5

	if err := h.run(t); err != nil {
		t.Fatalf("run: %v", err)
	}
	if len(h.repo.heuristics) != cfg.MaxPublications {
		t.Fatalf("abstracted %d segments, want exactly the cap %d", len(h.repo.heuristics), cfg.MaxPublications)
	}
}

// TestStrongPhaseOnePublishesWithoutAnyWinner is the regression this whole stage
// exists for. When Phase 1 is strong, no conjunction clears a bar set relative to
// it — which correctly means the lattice has nothing to write back, and must not
// mean the goal publishes nothing: its measured segments would then be unreachable
// to the only tool an agent can find them with.
func TestStrongPhaseOnePublishesWithoutAnyWinner(t *testing.T) {
	cfg := testConfig()
	cfg.MinLift = 0.05
	cfg.MinSupport = 30
	h := newHarness(t, cfg)
	h.repo.findings = []graph.CausalTriplet{
		findingWithSupport("broad", 0.989, 800, "a"),
		findingWithSupport("narrow", 0.9, 500, "b"),
	}
	h.sandbox.baseline = 0.5
	h.sandbox.measurements = map[string]sandboxMeasurement{
		"a": {value: 0.989, support: 800},
		"b": {value: 0.9, support: 500},
		// Real, well-supported, and still short of S* = 0.989: nothing the search
		// found improves on what Phase 1 already measured.
		"a+b": {value: 0.98, support: 450},
	}

	if err := h.run(t); err != nil {
		t.Fatalf("run: %v", err)
	}
	if len(h.repo.interventions) != 0 {
		t.Fatalf("nothing cleared the materially-better gate, so nothing may be written back: %+v", h.repo.interventions)
	}
	if len(h.repo.heuristics) != 2 {
		t.Fatalf("both Phase-1 findings must still publish, got %d heuristics", len(h.repo.heuristics))
	}
	rec, ok := h.audits.find("sleepcycle_run_complete")
	if !ok {
		t.Fatalf("expected a run-complete audit: %+v", h.audits.records)
	}
	if rec.detail["winners"] != 0 || rec.detail["published"] != 2 {
		t.Fatalf("the audit must separate winners from publications: %+v", rec.detail)
	}
	// The other half of search_skipped: this run DID search, so the key must be
	// absent. Always emitting it would make it useless as a disambiguator.
	if _, skipped := rec.detail["search_skipped"]; skipped {
		t.Fatalf("a run that searched must not report a skipped search: %+v", rec.detail)
	}
	if rec.detail["measurements"] == 0 {
		t.Fatalf("the search ran, so measurements must be recorded: %+v", rec.detail)
	}
}

// TestPublishedCountsOnlyWhatCameOutPublished: the audited count is the one number
// an operator reads to judge whether a run produced reachable knowledge, so it
// must track completions. Counting selections instead reports a healthy run when
// every abstraction failed — the failure mode this whole stage exists to surface.
func TestPublishedCountsOnlyWhatCameOutPublished(t *testing.T) {
	cfg := testConfig()
	cfg.MaxOrder = 1 // skip the search; both candidates come from Phase 1
	cfg.MinSupport = 30
	h := newHarness(t, cfg)
	h.repo.findings = findingsFor("a", "b")
	h.sandbox.baseline = 0.5
	h.provider.err = errors.New("ollama down") // both embeds fail, after Claude succeeds

	if err := h.run(t); err != nil {
		t.Fatalf("a per-segment failure must never abort the run, got %v", err)
	}
	if h.claude.abstractCalls != 2 {
		t.Fatalf("both segments must be attempted, got %d", h.claude.abstractCalls)
	}
	if got := h.audits.count("sleepcycle_abstraction_failure"); got != 2 {
		t.Fatalf("expected one failure audit per segment, got %d", got)
	}
	rec, ok := h.audits.find("sleepcycle_run_complete")
	if !ok {
		t.Fatalf("expected a run-complete audit: %+v", h.audits.records)
	}
	if rec.detail["published"] != 0 {
		t.Fatalf("published = %v, want 0 — nothing came out published", rec.detail["published"])
	}
}

// TestBelowBaselineWinnerIsWrittenButNotPublished: S* can sit below the baseline,
// so a macro-segment can beat it while still moving the objective the wrong way.
// That is a real search result worth persisting — but publishing it would ship a
// heuristic its own measurement contradicts, and its negative score would break
// the positive-score assumption the containment margin rests on.
func TestBelowBaselineWinnerIsWrittenButNotPublished(t *testing.T) {
	cfg := testConfig()
	cfg.MinLift = 0.05
	cfg.MinSupport = 30
	h := newHarness(t, cfg)
	h.repo.findings = []graph.CausalTriplet{finding("1", 5.0, testObjectiveLabel, "a", "b")}
	h.sandbox.baseline = 10.0
	h.sandbox.measurements = map[string]sandboxMeasurement{
		"a": {value: 5.0, support: 100},
		"b": {value: 5.0, support: 100},
		// Comfortably past S* = 5.0, comfortably short of the baseline 10.0.
		"a+b": {value: 7.0, support: 100},
	}

	if err := h.run(t); err != nil {
		t.Fatalf("run: %v", err)
	}
	if len(h.repo.interventions) != 1 {
		t.Fatalf("a segment beating S* must still be written back, got %d", len(h.repo.interventions))
	}
	if len(h.repo.heuristics) != 0 {
		t.Fatalf("a segment below the baseline must not be published: %+v", h.repo.heuristics)
	}
}

// TestEmptySelectionIsADegenerateRun: producing no publication is a successful
// run that records why, never an error — the same convention every other
// degenerate path follows. Both fixtures are ways a real goal reaches it: nothing
// improving on the baseline, and a goal whose findings all predate support being
// recorded at all.
func TestEmptySelectionIsADegenerateRun(t *testing.T) {
	cases := map[string][]graph.CausalTriplet{
		"nothing improves on the baseline": findingsFor("a", "b"),
		"every finding is support-less": {
			findingWithSupport("legacy-a", 50.0, 0, "a"),
			findingWithSupport("legacy-b", 60.0, 0, "b"),
		},
	}

	for name, findings := range cases {
		t.Run(name, func(t *testing.T) {
			cfg := testConfig()
			cfg.MinSupport = 30
			h := newHarness(t, cfg)
			h.repo.findings = findings
			h.sandbox.baseline = 1.0
			h.sandbox.defaultValue = 1.0

			if err := h.run(t); err != nil {
				t.Fatalf("a degenerate run is a successful run, got %v", err)
			}
			if len(h.repo.heuristics) != 0 {
				t.Fatalf("expected no publications, got %+v", h.repo.heuristics)
			}
			rec, ok := h.audits.find("sleepcycle_no_abstraction")
			if !ok {
				t.Fatalf("expected a no-abstraction audit: %+v", h.audits.records)
			}
			if rec.detail["reason"] != "no candidate cleared publication selection" {
				t.Fatalf("the audit must distinguish this case from the search's own gates: %+v", rec.detail)
			}
		})
	}
}

// canonicals renders a selection for failure messages.
func canonicals(cands []candidate) []string {
	out := make([]string, 0, len(cands))
	for _, c := range cands {
		out = append(out, c.canonical)
	}
	return out
}
