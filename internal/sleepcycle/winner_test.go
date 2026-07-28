package sleepcycle

import (
	"testing"

	"github.com/arborette/arborette/internal/domain"
	"github.com/arborette/arborette/internal/graph"
)

// TestOrderOneNodeIsNeverAWinner: the output is defined as conjunctions, so an
// order-1 node is only ever a route to order 2. This is not hypothetical — S* is
// computed over the eligible findings' measured values, so a predicate that only
// ever appeared alongside another in Phase 1 was never measured in isolation, and
// measuring it alone can genuinely beat S* by more than MinLift.
func TestOrderOneNodeIsNeverAWinner(t *testing.T) {
	cfg := testConfig()
	cfg.MinLift = 0.05
	h := newHarness(t, cfg)
	// One finding introduces both atoms at once, so S* is its modest value and
	// neither atom has a standalone measurement in the graph.
	h.repo.findings = []graph.CausalTriplet{finding("1", 1.0, testObjectiveLabel, "a", "b")}
	h.sandbox.baseline = 1.0
	h.sandbox.measurements = map[string]sandboxMeasurement{
		"a":   {value: 10.0, support: 100}, // clears MinLift over S*=1.0, but order 1
		"b":   {value: 1.0, support: 100},
		"a+b": {value: 1.0, support: 100}, // no lift
	}

	if err := h.run(t); err != nil {
		t.Fatalf("run: %v", err)
	}
	if len(h.repo.interventions) != 0 {
		t.Fatalf("an order-1 node must never be written back as a macro-segment: %+v", h.repo.interventions)
	}
	if len(h.repo.heuristics) != 0 {
		t.Fatal("an order-1 node must never be abstracted")
	}
}

// TestOrderTwoNodeWithTheSameValueWins is the counterpart: the identical measured
// value at order 2, with sufficient support, is a macro-segment.
func TestOrderTwoNodeWithTheSameValueWins(t *testing.T) {
	cfg := testConfig()
	cfg.MinLift = 0.05
	cfg.MinSupport = 30
	h := newHarness(t, cfg)
	h.repo.findings = []graph.CausalTriplet{finding("1", 1.0, testObjectiveLabel, "a", "b")}
	h.sandbox.baseline = 1.0
	h.sandbox.measurements = map[string]sandboxMeasurement{
		"a":   {value: 1.0, support: 100},
		"b":   {value: 1.0, support: 100},
		"a+b": {value: 10.0, support: 100},
	}

	if err := h.run(t); err != nil {
		t.Fatalf("run: %v", err)
	}
	if len(h.repo.interventions) != 1 {
		t.Fatalf("expected one macro-segment written back, got %d", len(h.repo.interventions))
	}
	if !h.repo.interventions[0].SleepDerived {
		t.Fatal("a derived macro-segment must be flagged sleep-derived")
	}
}

// TestBestSingleSegmentHoldsTheSupportFloor: S* is held to the same floor as the
// candidates it gates, so the bar can never be set by evidence the gate itself
// would reject. A sub-floor finding — however good its value — must not set S*,
// and a legacy outcome with no recorded support reads as 0 and self-excludes.
func TestBestSingleSegmentHoldsTheSupportFloor(t *testing.T) {
	obj := objectiveFor(t, domain.Maximize)
	// The at-floor finding carries the best floor-clearing value, so the
	// inclusive boundary is discriminated: a <= regression would exclude it and
	// hand S* to the dominated 4.0.
	findings := []graph.CausalTriplet{
		findingWithSupport("1", 10.0, 5, "a"),  // below the floor
		findingWithSupport("2", 6.0, 30, "b"),  // exactly at the floor — sets S*
		findingWithSupport("3", 4.0, 100, "c"), // above the floor, dominated
		findingWithSupport("4", 50.0, 0, "d"),  // legacy: no recorded support
	}

	best := bestSingleSegment(findings, obj, 30)
	if best == nil {
		t.Fatal("S* must be defined: two findings meet the floor")
	}
	if *best != 6.0 {
		t.Fatalf("S* = %v, want 6.0 — set by the at-floor finding, never by sub-floor or legacy ones", *best)
	}

	legacy := []graph.CausalTriplet{
		findingWithSupport("1", 10.0, 0, "a"),
		findingWithSupport("2", 20.0, 0, "b"),
	}
	if got := bestSingleSegment(legacy, obj, 30); got != nil {
		t.Fatalf("S* must be undefined when no finding meets the floor, got %v", *got)
	}
	// The exclusion holds at a floor of 0 too. A configured floor of zero means "no
	// minimum", not "unrecorded support counts as evidence" — the rest of the
	// package reads a zero row count as no measurement at all, and S* is the bar
	// deciding what gets written back.
	if got := bestSingleSegment(legacy, obj, 0); got != nil {
		t.Fatalf("a support-less finding must not set S* with the floor disabled, got %v", *got)
	}
}

// TestRunHoldsBestSingleToTheConfiguredFloor pins the S* floor's config wiring
// end to end, which the direct bestSingleSegment test cannot: Run must pass
// cfg.MinSupport, so a wiring regression (a literal 0, the wrong knob) would
// leave the sub-floor 50.0 as the bar and produce zero winners here.
func TestRunHoldsBestSingleToTheConfiguredFloor(t *testing.T) {
	cfg := testConfig()
	cfg.MinSupport = 30
	cfg.MinLift = 0.05
	h := newHarness(t, cfg)
	h.repo.findings = []graph.CausalTriplet{
		findingWithSupport("1", 50.0, 5, "a"),  // best value on record, below the floor
		findingWithSupport("2", 2.0, 100, "b"), // sets S* = 2.0
	}
	h.sandbox.baseline = 1.0
	h.sandbox.measurements = map[string]sandboxMeasurement{
		"a":   {value: 2.0, support: 100},
		"b":   {value: 2.0, support: 100},
		"a+b": {value: 10.0, support: 100}, // beats S* = 2.0, loses to a floorless 50.0
	}

	if err := h.run(t); err != nil {
		t.Fatalf("run: %v", err)
	}
	if len(h.repo.interventions) != 1 {
		t.Fatalf("the sub-floor finding must not set S*, so a+b must win: got %d interventions", len(h.repo.interventions))
	}
}

// TestMaxOrderOneIsDegenerate: with plenty of atoms but an order cap of 1 no
// conjunction can exist, so the atom count alone would not catch it.
//
// The baseline is pinned at the findings' own value so nothing is publishable
// either — the subject here is the search's degeneracy, and a run that published
// its Phase-1 findings would still be correct but would stop exercising it.
// Reaching publication is not evidence the search ran, so search_skipped is what
// keeps a skipped search visible.
func TestMaxOrderOneIsDegenerate(t *testing.T) {
	cfg := testConfig()
	cfg.MaxOrder = 1
	h := newHarness(t, cfg)
	h.repo.findings = findingsFor("a", "b", "c", "d", "e")
	h.sandbox.baseline = 1.0

	if err := h.run(t); err != nil {
		t.Fatalf("a degenerate run is a successful run, got %v", err)
	}
	if h.sandbox.execCalls != 1 {
		t.Fatalf("no search should run when no conjunction is formable, got %d executes (want just the baseline)", h.sandbox.execCalls)
	}
	rec, ok := h.audits.find("sleepcycle_no_abstraction")
	if !ok {
		t.Fatalf("expected a no-abstraction audit: %+v", h.audits.records)
	}
	if rec.detail["max_order"] != 1 || rec.detail["atoms"] != 5 {
		t.Fatalf("the audit must distinguish this case: %+v", rec.detail)
	}
	complete, ok := h.audits.find("sleepcycle_run_complete")
	if !ok {
		t.Fatalf("expected a run-complete audit: %+v", h.audits.records)
	}
	if complete.detail["search_skipped"] == nil {
		t.Fatalf("a run that never searched must say so: %+v", complete.detail)
	}
}

// TestNoConjunctionStillPublishesPhaseOne is the counterpart, and the reason an
// order cap must not end the run: an unconjoinable goal still knows something
// worth publishing, and skipping abstraction would leave every one of its
// measured segments unreachable to a consuming agent.
func TestNoConjunctionStillPublishesPhaseOne(t *testing.T) {
	cfg := testConfig()
	cfg.MaxOrder = 1
	cfg.MinSupport = 30
	h := newHarness(t, cfg)
	h.repo.findings = findingsFor("a", "b")
	h.sandbox.baseline = 0.5

	if err := h.run(t); err != nil {
		t.Fatalf("run: %v", err)
	}
	if h.sandbox.execCalls != 1 {
		t.Fatalf("no search should run when no conjunction is formable, got %d executes", h.sandbox.execCalls)
	}
	if len(h.repo.heuristics) != 2 {
		t.Fatalf("both improving findings must publish, got %d heuristics", len(h.repo.heuristics))
	}
	complete, _ := h.audits.find("sleepcycle_run_complete")
	if complete.detail["published"] != 2 {
		t.Fatalf("published = %v, want 2", complete.detail["published"])
	}
}
