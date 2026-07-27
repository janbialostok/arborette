package sleepcycle

import (
	"slices"
	"sort"
	"strings"
	"testing"

	"github.com/arborette/arborette/internal/domain"

	"github.com/arborette/arborette/internal/graph"
)

// findingsFor builds one eligible finding per atom, all measuring the same value
// so the atom set is the only thing the case varies.
func findingsFor(fields ...string) []graph.CausalTriplet {
	out := make([]graph.CausalTriplet, 0, len(fields))
	for i, f := range fields {
		out = append(out, finding(string(rune('a'+i)), 1.0, testObjectiveLabel, f))
	}
	return out
}

// TestBeamGeneratesEachSubsetOnce guards the rank-ordered construction: a child
// only ever adds a higher-ranked atom, so every subset has exactly one generator
// and the walk is of the subset lattice, not the permutation tree.
func TestBeamGeneratesEachSubsetOnce(t *testing.T) {
	h := newHarness(t, testConfig())
	h.repo.findings = findingsFor("a", "b", "c")
	h.sandbox.defaultValue = 1.0

	if err := h.run(t); err != nil {
		t.Fatalf("run: %v", err)
	}

	measured := h.sandbox.segmentsMeasured()
	seen := map[string]int{}
	for _, m := range measured {
		seen[m]++
	}
	for key, n := range seen {
		if n > 1 {
			t.Fatalf("subset %q was generated %d times, want exactly once", key, n)
		}
	}
	// The full lattice over 3 atoms up to order 3: 3 + 3 + 1 = 7.
	want := []string{"a", "a+b", "a+b+c", "a+c", "b", "b+c", "c"}
	got := append([]string{}, measured...)
	sort.Strings(got)
	if len(got) != len(want) {
		t.Fatalf("measured %v, want the 7-node lattice %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("measured %v, want %v", got, want)
		}
	}
}

// TestBeamRespectsMaxOrder pins the level cap: no candidate exceeds MaxOrder
// atoms even when the lattice is wider.
func TestBeamRespectsMaxOrder(t *testing.T) {
	cfg := testConfig()
	cfg.MaxOrder = 2
	h := newHarness(t, cfg)
	h.repo.findings = findingsFor("a", "b", "c")
	h.sandbox.defaultValue = 1.0

	if err := h.run(t); err != nil {
		t.Fatalf("run: %v", err)
	}
	for _, key := range h.sandbox.segmentsMeasured() {
		if order := len(strings.Split(key, "+")); order > cfg.MaxOrder {
			t.Fatalf("measured order-%d candidate %q past MaxOrder %d", order, key, cfg.MaxOrder)
		}
	}
}

// TestBeamWidthBoundsTheFrontier drives four atoms through a width-2 beam: only
// the two best level-1 nodes may seed level 2, so the level-2 candidates are
// exactly the pairs those two generate.
func TestBeamWidthBoundsTheFrontier(t *testing.T) {
	cfg := testConfig()
	cfg.BeamWidth = 2
	h := newHarness(t, cfg)
	h.repo.findings = findingsFor("a", "b", "c", "d")
	h.sandbox.baseline = 1.0
	// c and d rank highest at level 1, so they alone form the frontier.
	h.sandbox.measurements = map[string]sandboxMeasurement{
		"a": {value: 1.1, support: 100},
		"b": {value: 1.2, support: 100},
		"c": {value: 5.0, support: 100},
		"d": {value: 4.0, support: 100},
	}
	h.sandbox.defaultValue = 1.0

	if err := h.run(t); err != nil {
		t.Fatalf("run: %v", err)
	}

	// Only c and d survive width-2, and expansion only adds higher-ranked atoms,
	// so c+d is the single level-2 candidate they can generate.
	for _, unexpected := range []string{"a+b", "a+c", "a+d", "b+c", "b+d"} {
		if h.sandbox.didMeasure(unexpected) {
			t.Fatalf("%q was measured, but its parent fell outside the width-2 frontier", unexpected)
		}
	}
	if !h.sandbox.didMeasure("c+d") {
		t.Fatal("c+d should be measured: both parents are in the frontier")
	}
}

// TestBeamExpandsNonImprovingNodes is the interaction-effect guard: a predicate
// that underperforms the baseline alone must still be expanded, because the
// conjunction it forms may improve. Only support and width prune; delta ranks.
func TestBeamExpandsNonImprovingNodes(t *testing.T) {
	h := newHarness(t, testConfig())
	h.repo.findings = findingsFor("a", "b")
	h.sandbox.baseline = 10.0
	h.sandbox.measurements = map[string]sandboxMeasurement{
		"a":   {value: 2.0, support: 100}, // far below baseline
		"b":   {value: 3.0, support: 100}, // also below baseline
		"a+b": {value: 50.0, support: 100},
	}

	if err := h.run(t); err != nil {
		t.Fatalf("run: %v", err)
	}
	if !h.sandbox.didMeasure("a+b") {
		t.Fatal("a non-improving node inside the beam must still be expanded — interaction effects are the point")
	}
}

// TestBeamDegeneratesToFullEnumeration pins the small-lattice case: when a level
// has no more survivors than the beam is wide, the beam keeps them all.
func TestBeamDegeneratesToFullEnumeration(t *testing.T) {
	cfg := testConfig()
	cfg.BeamWidth = 10
	h := newHarness(t, cfg)
	h.repo.findings = findingsFor("a", "b", "c")
	h.sandbox.defaultValue = 1.0

	if err := h.run(t); err != nil {
		t.Fatalf("run: %v", err)
	}
	if got := len(h.sandbox.segmentsMeasured()); got != 7 {
		t.Fatalf("measured %d candidates, want the full 7-node lattice", got)
	}
}

// TestMinimizeDirectionRanksTheFrontier: direction is per-goal configuration, not
// a corner case. Ranking is by movement in the objective's desired direction, so
// under Minimize the LOWEST measured values form the frontier. A sign error here
// would keep the worst nodes and quietly expand the wrong part of the lattice.
//
// It drives the policy directly rather than a whole run, because the frontier
// constrains only a candidate's prefix — higher-ranked atoms are conjoined onto
// it either way — so the generated candidate set is the sharpest observable.
func TestMinimizeDirectionRanksTheFrontier(t *testing.T) {
	cfg := testConfig()
	cfg.BeamWidth = 2
	atoms, err := buildAtoms(findingsFor("a", "b", "c"))
	if err != nil {
		t.Fatalf("build atoms: %v", err)
	}
	// Baseline 10: under Minimize, a (1.0) and b (2.0) improve on it and c (99.0)
	// is by far the worst, so the width-2 frontier must be {a, b}.
	values := map[string]float64{"a": 1.0, "b": 2.0, "c": 99.0}
	p := newBeamPolicy(atoms, cfg, 10.0, domain.Minimize)

	for {
		node, ok := p.Select()
		if !ok {
			break
		}
		field := node.filters[0].Field
		p.Update(node, Measurement{Value: values[field], Support: 100})
		if p.level > 1 {
			break
		}
	}

	// Only a prefix in the frontier can generate a candidate, so a+b appears iff
	// a survived the width cut — which it does only when Minimize ranks ascending.
	var generated []string
	for _, n := range p.pending {
		generated = append(generated, segmentKey(n.filters))
	}
	if !slices.Contains(generated, "a+b") {
		t.Fatalf("a and b are the two best nodes under Minimize, so a+b must be generated; got %v", generated)
	}
	// Under a maximize-style ranking the frontier would be {c, b}, and c is the
	// highest-ranked atom so it can prefix nothing — a+b could not appear.
	if len(generated) != 3 {
		t.Fatalf("frontier {a,b} should generate a+b, a+c, b+c; got %v", generated)
	}
}

// TestMinimizeDirectionGatesWinners: a macro-segment wins under Minimize by
// measuring LOWER than the best single segment, not higher.
func TestMinimizeDirectionGatesWinners(t *testing.T) {
	cfg := testConfig()
	cfg.MinLift = 0.05
	cfg.MinSupport = 30
	h := newHarness(t, cfg)
	h.goals.goal = minimizeGoal()
	// Two findings, so S* has to pick an END of the range: under Minimize the best
	// single segment is 2.0, not the worst at 90.0. With one finding the two
	// coincide and a direction-blind max would pass unnoticed.
	h.repo.findings = []graph.CausalTriplet{
		finding("1", 2.0, testObjectiveLabel, "a"),
		finding("2", 90.0, testObjectiveLabel, "b"),
	}
	h.sandbox.baseline = 10.0
	h.sandbox.measurements = map[string]sandboxMeasurement{
		"a":   {value: 8.0, support: 100},
		"b":   {value: 9.0, support: 100},
		"a+b": {value: 1.0, support: 100}, // below S*=2.0 — a real Minimize win
	}

	if err := h.run(t); err != nil {
		t.Fatalf("run: %v", err)
	}
	if len(h.repo.interventions) != 1 {
		t.Fatalf("a conjunction measuring below S* must win under Minimize, got %d", len(h.repo.interventions))
	}
	// Effect size stays value - baseline, which is negative here: the sign is the
	// direction of movement, not a judgment about it.
	if got := h.repo.produced[0].EffectSize; got != 1.0-10.0 {
		t.Fatalf("effect size = %v, want value - global baseline (-9)", got)
	}
}
