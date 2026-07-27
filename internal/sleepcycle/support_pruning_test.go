package sleepcycle

import (
	"testing"

	"github.com/arborette/arborette/internal/sandboxclient"
)

// TestBelowFloorNodePrunesItsSupersets is the anti-monotone guarantee: support
// cannot grow by conjoining, so a node under the floor takes every superset with
// it — unmeasured, not merely unranked.
func TestBelowFloorNodePrunesItsSupersets(t *testing.T) {
	cfg := testConfig()
	cfg.MinSupport = 50
	h := newHarness(t, cfg)
	h.repo.findings = findingsFor("a", "b", "c")
	h.sandbox.baseline = 1.0
	h.sandbox.defaultValue = 1.0
	h.sandbox.measurements = map[string]sandboxMeasurement{
		"a": {value: 1.0, support: 10}, // below the floor
		"b": {value: 1.0, support: 100},
		"c": {value: 1.0, support: 100},
	}

	if err := h.run(t); err != nil {
		t.Fatalf("run: %v", err)
	}
	for _, superset := range []string{"a+b", "a+c", "a+b+c"} {
		if h.sandbox.didMeasure(superset) {
			t.Fatalf("%q was measured, but its subset {a} fell below the support floor", superset)
		}
	}
	// b+c is unaffected: neither of its subsets was below floor.
	if !h.sandbox.didMeasure("b+c") {
		t.Fatal("b+c should still be measured — neither subset was below floor")
	}
}

// TestNonPrefixSubsetPrunes is the case prefix-only generation alone would miss:
// {a,b,c} is generated from the frequent prefix {a,b}, but it also contains the
// below-floor {a,c}, so the memo-evidence subset check must drop it unmeasured.
func TestNonPrefixSubsetPrunes(t *testing.T) {
	cfg := testConfig()
	cfg.MinSupport = 50
	h := newHarness(t, cfg)
	h.repo.findings = findingsFor("a", "b", "c")
	h.sandbox.baseline = 1.0
	h.sandbox.defaultValue = 1.0
	h.sandbox.measurements = map[string]sandboxMeasurement{
		"a":   {value: 1.0, support: 100},
		"b":   {value: 1.0, support: 100},
		"c":   {value: 1.0, support: 100},
		"a+b": {value: 1.0, support: 100}, // frequent prefix
		"a+c": {value: 1.0, support: 10},  // below floor, and NOT the prefix of a+b+c
		"b+c": {value: 1.0, support: 100},
	}

	if err := h.run(t); err != nil {
		t.Fatalf("run: %v", err)
	}
	if h.sandbox.didMeasure("a+b+c") {
		t.Fatal("a+b+c must be dropped by the subset check: {a,c} was below floor even though the generating prefix {a,b} was frequent")
	}
}

// TestWidthDiscardedSubsetDoesNotPrune keeps width-pruning from masquerading as
// support-pruning: an unmeasured subset says nothing about support, so it must
// not prune anything.
//
// Reaching the real case takes some staging. A pair is only ever generated when
// its lower-ranked atom is in the level-1 frontier, so with 5 atoms and width 2
// the frontier {a,b} generates every a+X and b+X pair but never c+d. Ranking
// a+b and a+c top at level 2 then produces the level-3 candidate a+c+d, whose
// subset c+d was never measured at all. If prunedBySubset treated an unmeasured
// subset as pruning, a+c+d would silently vanish — width-pruning masquerading as
// support-pruning, which would make the beam drop reachable macro-segments.
func TestWidthDiscardedSubsetDoesNotPrune(t *testing.T) {
	cfg := testConfig()
	cfg.BeamWidth = 2
	cfg.MinSupport = 50
	h := newHarness(t, cfg)
	h.repo.findings = findingsFor("a", "b", "c", "d", "e")
	h.sandbox.baseline = 1.0
	h.sandbox.defaultValue = 2.0
	h.sandbox.measurements = map[string]sandboxMeasurement{
		// Level 1 ranks a and b top, so c, d, e never seed a pair of their own.
		"a": {value: 10.0, support: 100},
		"b": {value: 9.0, support: 100},
		"c": {value: 5.0, support: 100},
		"d": {value: 4.0, support: 100},
		"e": {value: 3.0, support: 100},
		// Level 2 ranks a+b and a+c top; everything else falls outside the width.
		"a+b": {value: 20.0, support: 100},
		"a+c": {value: 19.0, support: 100},
	}

	if err := h.run(t); err != nil {
		t.Fatalf("run: %v", err)
	}

	// Premise: c+d is never generated, so nothing ever measured it.
	if h.sandbox.didMeasure("c+d") {
		t.Fatalf("premise broken — c+d was measured, so this run proves nothing: %v",
			h.sandbox.segmentsMeasured())
	}
	// The guarantee: a+c+d is generated from the frequent prefix a+c and must be
	// measured, because its unmeasured subset c+d says nothing about support.
	if !h.sandbox.didMeasure("a+c+d") {
		t.Fatalf("a+c+d must be measured: its subset c+d was never measured, and an "+
			"unmeasured subset must not prune. measured=%v", h.sandbox.segmentsMeasured())
	}
}

// TestSupportArrivesWithTheObjective pins the one-scan contract: the row count
// rides in the same response as the objective, so there is never a second
// sandbox call just to count rows.
func TestSupportArrivesWithTheObjective(t *testing.T) {
	h := newHarness(t, testConfig())
	h.repo.findings = findingsFor("a", "b")
	h.sandbox.defaultValue = 1.0

	if err := h.run(t); err != nil {
		t.Fatalf("run: %v", err)
	}
	// One baseline plus the full 2-atom lattice (a, b, a+b) — no extra count calls.
	if h.sandbox.execCalls != 4 {
		t.Fatalf("execCalls = %d, want 4 (baseline + 3 candidates, each measured once)", h.sandbox.execCalls)
	}
}

func TestRowCountAccessorToleratesJSONShapes(t *testing.T) {
	resp := sandboxclient.ExecuteResponse{Value: map[string]any{sandboxclient.RowCountKey: 42.0}}
	if n, ok := sandboxclient.RowCount(resp); !ok || n != 42 {
		t.Fatalf("float64 row count = (%d,%v), want (42,true)", n, ok)
	}
	if _, ok := sandboxclient.RowCount(sandboxclient.ExecuteResponse{Value: map[string]any{}}); ok {
		t.Fatal("an absent row count must not report present")
	}
}

// TestBelowFloorNodeCannotWin pins the winner-side half of the floor: lift alone
// never promotes a thin-support conjunction.
func TestBelowFloorNodeCannotWin(t *testing.T) {
	cfg := testConfig()
	cfg.MinSupport = 50
	cfg.MinLift = 0.05
	h := newHarness(t, cfg)
	h.repo.findings = findingsFor("a", "b")
	h.sandbox.baseline = 1.0
	h.sandbox.measurements = map[string]sandboxMeasurement{
		"a":   {value: 1.0, support: 100},
		"b":   {value: 1.0, support: 100},
		"a+b": {value: 100.0, support: 5}, // huge lift, thin support
	}

	if err := h.run(t); err != nil {
		t.Fatalf("run: %v", err)
	}
	if len(h.repo.interventions) != 0 {
		t.Fatalf("a below-floor node must not be written back despite its lift: %+v", h.repo.interventions)
	}
	if _, ok := h.audits.find("sleepcycle_no_abstraction"); !ok {
		t.Fatalf("expected a no-abstraction audit: %+v", h.audits.records)
	}
}
