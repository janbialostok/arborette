package sleepcycle

import (
	"errors"
	"testing"

	"github.com/arborette/arborette/internal/domain"
	"github.com/arborette/arborette/internal/graph"
)

// winningHarness scripts a run where the named conjunctions clear the gate.
func winningHarness(t *testing.T, cfg Config, wins map[string]float64, atoms ...string) *harness {
	t.Helper()
	h := newHarness(t, cfg)
	h.repo.findings = []graph.CausalTriplet{finding("1", 1.0, testObjectiveLabel, atoms...)}
	h.sandbox.baseline = 1.0
	h.sandbox.defaultValue = 1.0
	h.sandbox.measurements = map[string]sandboxMeasurement{}
	for key, value := range wins {
		h.sandbox.measurements[key] = sandboxMeasurement{value: value, support: 100}
	}
	return h
}

func TestWriteBackPersistsOneCompleteTriplet(t *testing.T) {
	cfg := testConfig()
	cfg.MinLift = 0.05
	cfg.MinSupport = 30
	h := winningHarness(t, cfg, map[string]float64{"a+b": 10.0}, "a", "b")

	if err := h.run(t); err != nil {
		t.Fatalf("run: %v", err)
	}

	if len(h.repo.states) != 1 || len(h.repo.interventions) != 1 || len(h.repo.outcomes) != 1 {
		t.Fatalf("expected exactly one of each node, got states=%d interventions=%d outcomes=%d",
			len(h.repo.states), len(h.repo.interventions), len(h.repo.outcomes))
	}
	if len(h.repo.preConditions) != 1 || len(h.repo.produced) != 1 {
		t.Fatalf("expected one of each edge, got pre_condition=%d produced=%d",
			len(h.repo.preConditions), len(h.repo.produced))
	}

	edge := h.repo.produced[0]
	if edge.EpistemicSource != domain.EpistemicObservational {
		t.Fatalf("epistemic source = %q, want observational", edge.EpistemicSource)
	}
	// Global-baseline-relative, the documented split from Phase 1's parent-relative
	// marginal effect.
	if edge.EffectSize != 10.0-1.0 {
		t.Fatalf("effect size = %v, want value - global baseline (9)", edge.EffectSize)
	}
	if edge.Confidence != 1.0 {
		t.Fatalf("confidence = %v, want 1.0 for a measured query outcome", edge.Confidence)
	}

	iv := h.repo.interventions[0]
	if !iv.SleepDerived {
		t.Fatal("the derived intervention must be flagged sleep-derived")
	}
	if iv.GoalID != "g1" {
		t.Fatalf("goal id = %q, want g1", iv.GoalID)
	}
	if iv.Properties["support"] != int64(100) {
		t.Fatalf("support = %v, want the measured row count 100", iv.Properties["support"])
	}
	if h.repo.outcomes[0].Support != 100 {
		t.Fatalf("outcome support = %d, want the measured row count 100", h.repo.outcomes[0].Support)
	}
	if h.repo.outcomes[0].VerificationStatus != domain.VerificationVerified {
		t.Fatalf("a measured query outcome must be verified, got %q", h.repo.outcomes[0].VerificationStatus)
	}
	// The State + PRE_CONDITION_FOR edge is what keeps the derived finding visible
	// to trace_causal_chain, which matches only complete paths.
	if h.repo.preConditions[0][0] != h.repo.states[0].ID {
		t.Fatal("the pre-condition edge must originate at the baseline state")
	}
}

func TestTwoWinnersShareOneBaselineState(t *testing.T) {
	cfg := testConfig()
	cfg.MinLift = 0.05
	cfg.MinSupport = 30
	h := winningHarness(t, cfg, map[string]float64{"a+b": 10.0, "a+c": 11.0}, "a", "b", "c")

	if err := h.run(t); err != nil {
		t.Fatalf("run: %v", err)
	}
	if len(h.repo.interventions) != 2 {
		t.Fatalf("expected two macro-segments, got %d", len(h.repo.interventions))
	}
	// Written per segment (so it falls under per-segment failure isolation) but on
	// a deterministic id, so it MERGEs to exactly one shared node.
	if len(h.repo.states) != 2 {
		t.Fatalf("each segment must issue its own baseline-state write, got %d", len(h.repo.states))
	}
	if h.repo.states[0].ID != h.repo.states[1].ID {
		t.Fatalf("both writes must target one shared baseline state, got %q and %q",
			h.repo.states[0].ID, h.repo.states[1].ID)
	}
	if h.repo.interventions[0].ID == h.repo.interventions[1].ID {
		t.Fatal("two distinct macro-segments must not share an intervention id")
	}
}

// TestWriteBackFailureIsolatesTheSegment is the load-bearing case: a half-written
// segment must be omitted from the returned slice, not merely logged. Leaving it
// in would send abstraction down its not-found happy path, spend a full Claude
// call on it, and then hand CreateMetaHeuristic an id that was never written.
func TestWriteBackFailureIsolatesTheSegment(t *testing.T) {
	cfg := testConfig()
	cfg.MinLift = 0.05
	cfg.MinSupport = 30
	h := winningHarness(t, cfg, map[string]float64{"a+b": 10.0, "a+c": 11.0}, "a", "b", "c")

	// Fail the first winner's intervention write; the canonical filter keys the
	// script so the choice does not depend on iteration order.
	failing := canonicalFor(t, "a", "b")
	h.repo.createErrs["intervention:"+failing] = errors.New("neo4j write failed")

	if err := h.run(t); err != nil {
		t.Fatalf("a write-back failure must be isolated, not terminal: %v", err)
	}

	if got := h.audits.count("sleepcycle_writeback_failure"); got != 1 {
		t.Fatalf("expected exactly one write-back failure audit, got %d", got)
	}
	if len(h.repo.interventions) != 1 {
		t.Fatalf("the surviving winner must still be written, got %d interventions", len(h.repo.interventions))
	}
	if h.repo.interventions[0].Properties["canonical_filter"] == failing {
		t.Fatal("the failed segment must not appear among the written interventions")
	}
	// Only the surviving winner reaches abstraction — the returned slice, not the
	// search's original winner list, is what the abstraction stage iterates.
	if h.claude.abstractCalls != 1 {
		t.Fatalf("expected one abstraction (for the surviving winner), got %d", h.claude.abstractCalls)
	}
	if len(h.repo.heuristics) != 1 {
		t.Fatalf("expected one meta-heuristic, got %d", len(h.repo.heuristics))
	}
}

// TestEveryWriteFailureIsolatesTheSegment covers all five graph calls, not just
// the intervention. Each is error-checked before the next specifically so a
// failed node write never reaches the edge write that would silently no-op on it
// — a zero-row MATCH is not an error in Cypher, so an edge write against a
// missing node succeeds while linking nothing, leaving exactly the orphaned
// fragment trace_causal_chain drops.
func TestEveryWriteFailureIsolatesTheSegment(t *testing.T) {
	cfg := testConfig()
	cfg.MinLift = 0.05
	cfg.MinSupport = 30

	for _, failing := range []string{"state", "intervention", "outcome", "pre_condition", "produced"} {
		t.Run(failing, func(t *testing.T) {
			h := winningHarness(t, cfg, map[string]float64{"a+b": 10.0}, "a", "b")
			h.repo.createErrs[failing] = errors.New(failing + " write failed")

			if err := h.run(t); err != nil {
				t.Fatalf("a write-back failure must be isolated, not terminal: %v", err)
			}
			if got := h.audits.count("sleepcycle_writeback_failure"); got != 1 {
				t.Fatalf("expected one write-back failure audit, got %d", got)
			}
			// The load-bearing consequence: the half-written segment is excluded
			// from the returned slice, so abstraction never pays a Claude call for
			// it and never hands CreateMetaHeuristic an id that was never written.
			if h.claude.abstractCalls != 0 {
				t.Fatalf("a half-written segment must not be abstracted, got %d Claude calls", h.claude.abstractCalls)
			}
			if len(h.repo.heuristics) != 0 {
				t.Fatalf("a half-written segment must produce no meta-heuristic, got %d", len(h.repo.heuristics))
			}
		})
	}
}

// canonicalFor builds the canonical key for a conjunction of test atoms, so a
// test can address a specific segment without depending on map iteration order.
func canonicalFor(t *testing.T, fields ...string) string {
	t.Helper()
	filters := make([]domain.Constraint, 0, len(fields))
	for _, f := range fields {
		filters = append(filters, atomOn(f))
	}
	return mustCanonical(t, filters)
}
