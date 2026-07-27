package sleepcycle

import (
	"testing"

	"github.com/arborette/arborette/internal/graph"
)

// TestDegenerateRuns covers the three ways a run legitimately produces no
// Meta-Heuristic. All three are successes, not failures: each records why and
// returns nil.
func TestDegenerateRuns(t *testing.T) {
	cases := []struct {
		name     string
		cfg      func(Config) Config
		findings []graph.CausalTriplet
		script   map[string]sandboxMeasurement
		atoms    int
		hasBest  bool
	}{
		{
			name:     "zero eligible findings",
			cfg:      func(c Config) Config { return c },
			findings: nil,
			atoms:    0,
			hasBest:  false,
		},
		{
			name:     "a single atom cannot be conjoined",
			cfg:      func(c Config) Config { return c },
			findings: findingsFor("a"),
			atoms:    1,
			hasBest:  true,
		},
		{
			name: "nothing clears the lift gate",
			cfg: func(c Config) Config {
				c.MinLift = 0.5
				return c
			},
			findings: findingsFor("a", "b"),
			script: map[string]sandboxMeasurement{
				"a":   {value: 1.0, support: 100},
				"b":   {value: 1.0, support: 100},
				"a+b": {value: 1.01, support: 100}, // real but immaterial
			},
			atoms:   2,
			hasBest: true,
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			h := newHarness(t, c.cfg(testConfig()))
			h.repo.findings = c.findings
			h.sandbox.baseline = 1.0
			if c.script != nil {
				h.sandbox.measurements = c.script
			}

			if err := h.run(t); err != nil {
				t.Fatalf("a degenerate run is a successful run, got %v", err)
			}
			if len(h.repo.heuristics) != 0 {
				t.Fatalf("expected zero CreateMetaHeuristic calls, got %d", len(h.repo.heuristics))
			}
			if h.claude.abstractCalls != 0 {
				t.Fatalf("expected no Claude calls, got %d", h.claude.abstractCalls)
			}

			rec, ok := h.audits.find("sleepcycle_no_abstraction")
			if !ok {
				t.Fatalf("expected a no-abstraction audit: %+v", h.audits.records)
			}
			if rec.detail["atoms"] != c.atoms {
				t.Fatalf("audit atoms = %v, want %d", rec.detail["atoms"], c.atoms)
			}
			if reason, _ := rec.detail["reason"].(string); reason == "" {
				t.Fatal("the audit must carry a distinguishing reason")
			}
			// S* is undefined over an empty finding set, and a fabricated 0 would be
			// indistinguishable from a legitimately-measured 0.
			best := rec.detail["best_single"]
			if c.hasBest && best == nil {
				t.Fatal("best_single must be recorded when eligible findings exist")
			}
			if !c.hasBest && best != nil {
				t.Fatalf("best_single must be nil with no eligible findings, got %v", best)
			}
		})
	}
}

// TestRealImprovementOverAZeroBaselineWins covers the degenerate-denominator
// case end to end: with S* at exactly 0 the relative-lift divisor is floored, and
// a genuine improvement must still clear the gate rather than trip on the
// arithmetic. (An equal-and-zero value cannot reach the divisor at all — the
// improvement check rejects it one line earlier.)
func TestRealImprovementOverAZeroBaselineWins(t *testing.T) {
	cfg := testConfig()
	cfg.MinLift = 0.05
	h := newHarness(t, cfg)
	// Every eligible finding measured 0, so S* is 0 — the denominator's edge case.
	h.repo.findings = []graph.CausalTriplet{
		finding("1", 0, testObjectiveLabel, "a"),
		finding("2", 0, testObjectiveLabel, "b"),
	}
	h.sandbox.baseline = 0
	h.sandbox.measurements = map[string]sandboxMeasurement{
		"a":   {value: 0, support: 100},
		"b":   {value: 0, support: 100},
		"a+b": {value: 5.0, support: 100},
	}

	if err := h.run(t); err != nil {
		t.Fatalf("run: %v", err)
	}
	if len(h.repo.interventions) != 1 {
		t.Fatalf("a real improvement over a zero baseline must still win, got %d", len(h.repo.interventions))
	}
}
