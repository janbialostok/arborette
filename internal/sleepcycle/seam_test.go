package sleepcycle

import (
	"context"
	"testing"
	"time"

	"github.com/arborette/arborette/internal/domain"
)

// stubPolicy is a deliberately ill-behaved SearchPolicy: it re-selects the same
// node and then contradicts its own Done by selecting nothing. The beam cannot
// do either, but the seam exists for a V2 policy that re-selects by design, so
// the driver's two defenses need a policy that actually exercises them.
type stubPolicy struct {
	node       *Node
	selects    int
	maxSelects int
	endless    bool
	updates    []Measurement
}

func (p *stubPolicy) Select() (*Node, bool) {
	if !p.endless && p.selects >= p.maxSelects {
		return nil, false // contradicts Done, which stays false
	}
	p.selects++
	return p.node, true
}

func (p *stubPolicy) Update(_ *Node, m Measurement) { p.updates = append(p.updates, m) }

func (p *stubPolicy) Done() bool { return false }

func singleNode(t *testing.T, field string) *Node {
	t.Helper()
	c := atomOn(field)
	key := mustCanonical(t, []domain.Constraint{c})
	return &Node{keys: []string{key}, ranks: []int{0}, filters: []domain.Constraint{c}, canonical: key}
}

// TestDriverMemoizesAReSelectedNode pins the budget semantics the seam promises:
// a policy that re-selects an already-measured node costs no second sandbox call
// and does not inflate the measurement count — which is what keeps MaxMeasurements
// a count of *distinct* measurements for a re-selecting V2 policy.
func TestDriverMemoizesAReSelectedNode(t *testing.T) {
	h := newHarness(t, testConfig())
	h.sandbox.defaultValue = 5.0
	obj := objectiveFor(t, domain.Maximize)
	policy := &stubPolicy{node: singleNode(t, "a"), maxSelects: 3}

	outcome := h.worker.runSearch(t.Context(), searchTarget{goalID: "g1", dataSourceRef: "ref.csv"}, obj, policy)

	if h.sandbox.execCalls != 1 {
		t.Fatalf("the same node selected 3 times must reach the sandbox once, got %d calls", h.sandbox.execCalls)
	}
	if len(outcome.measured) != 1 {
		t.Fatalf("a re-select must not inflate the measured set, got %d entries", len(outcome.measured))
	}
	// The policy still sees an Update per selection — the memo serves the value.
	if len(policy.updates) != 3 {
		t.Fatalf("expected one Update per selection, got %d", len(policy.updates))
	}
	for _, m := range policy.updates {
		if m.Value != 5.0 {
			t.Fatalf("the memoized measurement must be handed back verbatim, got %+v", m)
		}
	}
}

// TestDriverStopsWhenSelectContradictsDone: a policy that reports not-Done and
// then selects nothing must terminate the run under its own stop reason, not be
// logged as a clean exhaustion and not spin forever.
func TestDriverStopsWhenSelectContradictsDone(t *testing.T) {
	h := newHarness(t, testConfig())
	h.sandbox.defaultValue = 5.0
	obj := objectiveFor(t, domain.Maximize)
	policy := &stubPolicy{node: singleNode(t, "a"), maxSelects: 1}

	outcome := h.worker.runSearch(t.Context(), searchTarget{goalID: "g1", dataSourceRef: "ref.csv"}, obj, policy)

	if outcome.stopReason != stopPolicyExhausted {
		t.Fatalf("stop reason = %q, want %q — a seam violation must not read as a clean exhaustion",
			outcome.stopReason, stopPolicyExhausted)
	}
}

// TestBeamUpdateIsIdempotent guards the level-advance accounting. The driver
// memoizes measurements but still calls Update on a re-selected node, so a
// non-idempotent Update would double-count outcomes and advance the level before
// its candidates had all been measured.
func TestBeamUpdateIsIdempotent(t *testing.T) {
	atoms, err := buildAtoms(findingsFor("a", "b", "c"))
	if err != nil {
		t.Fatalf("build atoms: %v", err)
	}
	p := newBeamPolicy(atoms, testConfig(), 0, domain.Maximize)

	node, ok := p.Select()
	if !ok {
		t.Fatal("expected a level-1 candidate")
	}
	m := Measurement{Value: 1, Support: 100}
	p.Update(node, m)
	before := len(p.results)
	p.Update(node, m) // repeat, as a re-selecting policy would produce
	if len(p.results) != before {
		t.Fatalf("a repeated Update recorded a duplicate outcome: %d -> %d", before, len(p.results))
	}
	if p.level != 1 {
		t.Fatalf("the level advanced early, at level %d with candidates outstanding", p.level)
	}
}

// TestCancelledContextBoundsAReSelectingPolicy pins the driver's only liveness
// bound against a policy that re-selects forever. The budget cannot stop it: it
// advances on distinct measurements only, and a re-selected node is served from
// the memo without touching the sandbox. Without the context check this loop does
// not terminate, so the guard is what keeps a wedged V2 policy from burning the
// whole run timeout.
func TestCancelledContextBoundsAReSelectingPolicy(t *testing.T) {
	h := newHarness(t, testConfig())
	h.sandbox.defaultValue = 1.0
	obj := objectiveFor(t, domain.Maximize)
	policy := &stubPolicy{node: singleNode(t, "a"), endless: true}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	done := make(chan searchOutcome, 1)
	go func() {
		done <- h.worker.runSearch(ctx, searchTarget{goalID: "g1", dataSourceRef: "ref.csv", namespace: goalNamespace("g1")}, obj, policy)
	}()

	select {
	case outcome := <-done:
		if outcome.stopReason != stopContextDone {
			t.Fatalf("stop reason = %q, want %q", outcome.stopReason, stopContextDone)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("an endlessly re-selecting policy was not bounded by the cancelled context")
	}
}
