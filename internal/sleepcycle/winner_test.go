package sleepcycle

import (
	"testing"

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

// TestMaxOrderOneIsDegenerate: with plenty of atoms but an order cap of 1 no
// conjunction can exist, so the atom count alone would not catch it.
func TestMaxOrderOneIsDegenerate(t *testing.T) {
	cfg := testConfig()
	cfg.MaxOrder = 1
	h := newHarness(t, cfg)
	h.repo.findings = findingsFor("a", "b", "c", "d", "e")

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
}
