package sleepcycle

import (
	"testing"

	"github.com/arborette/arborette/internal/graph"
)

// TestAbstractedFromExcludesTheSharedBaselineState is the trace-isolation guard.
// trace_causal_chain's second MATCH is unbound: it enumerates every complete path
// in the graph and keeps the rows whose target is the State, Intervention, or
// Outcome. The baseline State is one node per goal wired as the State of *every*
// derived triplet, so linking it would make each Meta-Heuristic's trace return
// all of them — silently attributing other segments' evidence to it.
//
// The single-winner version of this check cannot detect that, so this test runs
// two winning macro-segments on one goal.
func TestAbstractedFromExcludesTheSharedBaselineState(t *testing.T) {
	cfg := testConfig()
	cfg.MinLift = 0.05
	cfg.MinSupport = 30
	h := newHarness(t, cfg)
	// Distinct component triplets per atom, so each segment has its own evidence.
	h.repo.findings = []graph.CausalTriplet{
		finding("1", 1.0, testObjectiveLabel, "a"),
		finding("2", 1.0, testObjectiveLabel, "b"),
		finding("3", 1.0, testObjectiveLabel, "c"),
	}
	h.sandbox.baseline = 1.0
	h.sandbox.defaultValue = 1.0
	h.sandbox.measurements = map[string]sandboxMeasurement{
		"a":   {value: 1.0, support: 100},
		"b":   {value: 1.0, support: 100},
		"c":   {value: 1.0, support: 100},
		"a+b": {value: 10.0, support: 100},
		"a+c": {value: 11.0, support: 100},
	}

	if err := h.run(t); err != nil {
		t.Fatalf("run: %v", err)
	}
	if len(h.repo.heuristics) != 2 {
		t.Fatalf("expected two meta-heuristics, got %d", len(h.repo.heuristics))
	}

	baselineID := baselineStateID(goalNamespace("g1"))
	abID := derivedID(goalNamespace("g1"), roleMetaHeuristic, canonicalFor(t, "a", "b"))
	acID := derivedID(goalNamespace("g1"), roleMetaHeuristic, canonicalFor(t, "a", "c"))

	for _, mhID := range []string{abID, acID} {
		refs, ok := h.repo.abstractedFrom[mhID]
		if !ok {
			t.Fatalf("no abstractedFrom recorded for %q", mhID)
		}
		if contains(refs, baselineID) {
			t.Fatalf("%q links the shared baseline state, which would return every segment's evidence in its trace", mhID)
		}
	}

	// Each links its own derived Intervention and Outcome plus its component
	// triplets' ids, and nothing belonging to the other segment.
	assertLinks(t, h.repo.abstractedFrom[abID], "a+b",
		[]string{"s-1", "i-1", "o-1", "s-2", "i-2", "o-2"},
		[]string{"s-3", "i-3", "o-3"})
	assertLinks(t, h.repo.abstractedFrom[acID], "a+c",
		[]string{"s-1", "i-1", "o-1", "s-3", "i-3", "o-3"},
		[]string{"s-2", "i-2", "o-2"})

	abIntervention := derivedID(goalNamespace("g1"), roleIntervention, canonicalFor(t, "a", "b"))
	abOutcome := derivedID(goalNamespace("g1"), roleOutcome, canonicalFor(t, "a", "b"))
	if !contains(h.repo.abstractedFrom[abID], abIntervention) || !contains(h.repo.abstractedFrom[abID], abOutcome) {
		t.Fatalf("a+b must link its own derived intervention and outcome: %+v", h.repo.abstractedFrom[abID])
	}
	if contains(h.repo.abstractedFrom[abID], derivedID(goalNamespace("g1"), roleIntervention, canonicalFor(t, "a", "c"))) {
		t.Fatal("a+b must not link the other segment's derived intervention")
	}
}

// TestAbstractedFromDeduplicatesSharedComponents: one finding can contribute
// several atoms, so its ids must appear once, not once per atom.
func TestAbstractedFromDeduplicatesSharedComponents(t *testing.T) {
	cfg := testConfig()
	cfg.MinLift = 0.05
	cfg.MinSupport = 30
	h := winningHarness(t, cfg, map[string]float64{"a+b": 10.0}, "a", "b")

	if err := h.run(t); err != nil {
		t.Fatalf("run: %v", err)
	}
	mhID := derivedID(goalNamespace("g1"), roleMetaHeuristic, canonicalFor(t, "a", "b"))
	refs := h.repo.abstractedFrom[mhID]

	seen := map[string]int{}
	for _, r := range refs {
		seen[r]++
	}
	for id, n := range seen {
		if n > 1 {
			t.Fatalf("id %q appears %d times; CreateMetaHeuristic rejects a duplicate reference", id, n)
		}
	}
	// The one finding introduced both atoms, so its three ids plus the derived
	// pair is the whole set.
	if len(refs) != 5 {
		t.Fatalf("expected 3 component ids + 2 derived ids, got %d: %+v", len(refs), refs)
	}
}

func contains(haystack []string, needle string) bool {
	for _, h := range haystack {
		if h == needle {
			return true
		}
	}
	return false
}

func assertLinks(t *testing.T, refs []string, name string, want, unwanted []string) {
	t.Helper()
	for _, id := range want {
		if !contains(refs, id) {
			t.Fatalf("%s must link its component id %q: %+v", name, id, refs)
		}
	}
	for _, id := range unwanted {
		if contains(refs, id) {
			t.Fatalf("%s must not link the other segment's component id %q: %+v", name, id, refs)
		}
	}
}
