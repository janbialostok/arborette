package sleepcycle

import (
	"errors"
	"testing"

	"github.com/arborette/arborette/internal/domain"
	"github.com/arborette/arborette/internal/objective"
	"github.com/arborette/arborette/internal/store"
)

// searchOnly drives just the search stage, so a termination test can assert the
// stop reason and measurement count directly rather than inferring them. The
// tuning comes from the harness's own worker: passing it separately would let a
// test build the policy from one Config while the driver enforced another's
// budget, silently exercising a bound it appeared to pin.
func (h *harness) searchOnly(t *testing.T) searchOutcome {
	t.Helper()
	obj, err := objective.Pin(h.goals.goal.EvaluationMatrix)
	if err != nil {
		t.Fatalf("pin objective: %v", err)
	}
	atoms, err := buildAtoms(h.repo.findings)
	if err != nil {
		t.Fatalf("build atoms: %v", err)
	}
	target := searchTarget{goalID: "g1", dataSourceRef: "ref.csv", namespace: goalNamespace("g1")}
	policy := newBeamPolicy(atoms, h.worker.cfg, h.sandbox.baseline, obj.Direction)
	return h.worker.runSearch(t.Context(), target, obj, policy)
}

// TestSearchTerminatesByPolicyDone walks a 4-atom, order-3 lattice —
// 4 + 6 + 4 = 14 nodes, far under the budget — and asserts the run ends because
// the policy exhausted it, at exactly that measurement count.
func TestSearchTerminatesByPolicyDone(t *testing.T) {
	h := newHarness(t, testConfig())
	h.repo.findings = findingsFor("a", "b", "c", "d")
	h.sandbox.defaultValue = 1.0

	outcome := h.searchOnly(t)

	if outcome.stopReason != stopPolicyDone {
		t.Fatalf("stop reason = %q, want %q", outcome.stopReason, stopPolicyDone)
	}
	if len(outcome.measured) != 14 {
		t.Fatalf("measurements = %d, want the full 14-node lattice", len(outcome.measured))
	}
}

// TestSearchStopsOnBudgetExhaustion pins the other bound: a budget smaller than
// a level's candidate count stops the search immediately, mid-level, and the
// winner gate still runs over whatever was measured.
func TestSearchStopsOnBudgetExhaustion(t *testing.T) {
	cfg := testConfig()
	cfg.MaxMeasurements = 2
	h := newHarness(t, cfg)
	h.repo.findings = findingsFor("a", "b", "c", "d")
	h.sandbox.defaultValue = 1.0

	outcome := h.searchOnly(t)

	if outcome.stopReason != stopBudgetExhausted {
		t.Fatalf("stop reason = %q, want %q", outcome.stopReason, stopBudgetExhausted)
	}
	// The budget bounds distinct measurements, and the winner gate still sees
	// every one of them — a mid-level stop does not discard what was measured.
	if len(outcome.measured) != cfg.MaxMeasurements {
		t.Fatalf("measured = %d entries, want exactly the budget %d", len(outcome.measured), cfg.MaxMeasurements)
	}
}

// TestAllFailingSearchEndsCleanly is the degraded-sandbox path: a sandbox that
// fails after a successful baseline turns every candidate into a recorded miss,
// which empties the frontier at level 1 and ends the run by policy-Done rather
// than by error.
func TestAllFailingSearchEndsCleanly(t *testing.T) {
	cfg := testConfig()
	h := newHarness(t, cfg)
	h.repo.findings = findingsFor("a", "b", "c")
	h.sandbox.failAll = true

	outcome := h.searchOnly(t)

	if outcome.stopReason != stopPolicyDone {
		t.Fatalf("stop reason = %q, want %q", outcome.stopReason, stopPolicyDone)
	}
	if len(outcome.measured) != 3 {
		t.Fatalf("measurements = %d, want exactly one per atom (%d)", len(outcome.measured), 3)
	}
	for _, m := range outcome.measured {
		if !m.measurement.Failed {
			t.Fatalf("every measurement should be a recorded miss: %+v", m.measurement)
		}
	}
	if len(outcome.winners(0)) != 0 {
		t.Fatal("a failed measurement must never be a winner")
	}
}

// TestSandboxFailureAfterBaselineExitsCleanly is the same degradation seen end to
// end: the run reaches degenerate detection and returns nil rather than failing.
func TestSandboxFailureAfterBaselineExitsCleanly(t *testing.T) {
	h := newHarness(t, testConfig())
	h.repo.findings = findingsFor("a", "b")
	h.sandbox.failAll = true

	if err := h.run(t); err != nil {
		t.Fatalf("a sandbox that fails only after the baseline must exit cleanly, got %v", err)
	}
	if _, ok := h.audits.find("sleepcycle_no_abstraction"); !ok {
		t.Fatalf("expected a no-abstraction audit: %+v", h.audits.records)
	}
	// Both atoms fail, so the level-1 frontier is empty and no order-2 candidate
	// is ever generated: exactly two failures, not three.
	if h.audits.count("sleepcycle_measurement_failure") != 2 {
		t.Fatalf("expected one failure audit per level-1 atom, got %d", h.audits.count("sleepcycle_measurement_failure"))
	}
}

// TestBaselineFailureIsTerminal pins the one degraded-sandbox case that is NOT a
// clean exit: without a baseline there is no effect size to write and no delta to
// rank a frontier by, so there is nothing to degrade to.
func TestBaselineFailureIsTerminal(t *testing.T) {
	h := newHarness(t, testConfig())
	h.repo.findings = findingsFor("a", "b")
	h.sandbox.baselineErr = errors.New("sandbox down")

	if err := h.run(t); err == nil {
		t.Fatal("a failed baseline must be terminal")
	}
	// The baseline is the first Execute, so nothing was searched.
	if h.sandbox.execCalls != 1 {
		t.Fatalf("execCalls = %d, want only the failed baseline", h.sandbox.execCalls)
	}
	if _, ok := h.audits.find("sleepcycle_run_failed"); !ok {
		t.Fatalf("expected a run-failed audit: %+v", h.audits.records)
	}
}

func TestIntrospectionFailureIsTerminal(t *testing.T) {
	h := newHarness(t, testConfig())
	h.repo.findings = findingsFor("a", "b")
	h.sandbox.introspectErr = errors.New("sandbox down")

	if err := h.run(t); err == nil {
		t.Fatal("a failed introspection must be terminal")
	}
	if h.sandbox.execCalls != 0 {
		t.Fatalf("nothing should be measured after a failed introspection, got %d calls", h.sandbox.execCalls)
	}
}

// TestEligibleFindingsFailureIsTerminal: the search's only input.
func TestEligibleFindingsFailureIsTerminal(t *testing.T) {
	h := newHarness(t, testConfig())
	h.repo.findingsErr = errors.New("neo4j down")

	if err := h.run(t); err == nil {
		t.Fatal("a failed eligible-finding read must be terminal")
	}
}

// TestDocumentGoalIsRejected: a document goal has no objective to pin, so there
// is nothing to optimize — a clear terminal error, never a panic.
func TestDocumentGoalIsRejected(t *testing.T) {
	h := newHarness(t, testConfig())
	h.goals.goal = store.Goal{
		OptimizationFunctionID: "g1",
		TargetFields:           []domain.TargetField{{Name: "effective_date"}},
	}

	err := h.run(t)
	if !errors.Is(err, errDocumentGoal) {
		t.Fatalf("error = %v, want errDocumentGoal", err)
	}
	if h.sandbox.introspectCalls != 0 {
		t.Fatal("a document goal must be rejected before any sandbox work")
	}
}

// TestSweepAndResumeFailuresAreNonTerminal: neither is a prerequisite for
// producing valid macro-segments, and the resume pass may concern another goal
// entirely, so a fault in either must not sink the run.
func TestSweepAndResumeFailuresAreNonTerminal(t *testing.T) {
	h := newHarness(t, testConfig())
	h.repo.findings = findingsFor("a", "b")
	h.repo.staleErr = errors.New("neo4j hiccup")
	h.repo.pendingErr = errors.New("neo4j hiccup")
	h.sandbox.defaultValue = 1.0

	if err := h.run(t); err != nil {
		t.Fatalf("sweep and resume failures must be non-terminal, got %v", err)
	}
	if _, ok := h.audits.find("sleepcycle_staleness_sweep_failure"); !ok {
		t.Fatalf("expected a staleness-sweep failure audit: %+v", h.audits.records)
	}
	if _, ok := h.audits.find("sleepcycle_resume_failure"); !ok {
		t.Fatalf("expected a resume failure audit: %+v", h.audits.records)
	}
}
