package sleepcycle

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/arborette/arborette/internal/domain"
	"github.com/arborette/arborette/internal/graph"
	"github.com/arborette/arborette/internal/store"
)

// TestConfigValidationRejectsBadTuning: the knobs come straight from env, so the
// guard is what stands between a typo and a worker that, say, runs with
// BeamWidth 0 and silently empties every frontier.
func TestConfigValidationRejectsBadTuning(t *testing.T) {
	cases := map[string]func(*Config){
		"max measurements below one": func(c *Config) { c.MaxMeasurements = 0 },
		"beam width below one":       func(c *Config) { c.BeamWidth = 0 },
		"max order below one":        func(c *Config) { c.MaxOrder = 0 },
		"negative min support":       func(c *Config) { c.MinSupport = -1 },
		"negative min lift":          func(c *Config) { c.MinLift = -0.1 },
		"max publications below one": func(c *Config) { c.MaxPublications = 0 },
	}
	for name, breakIt := range cases {
		t.Run(name, func(t *testing.T) {
			cfg := testConfig()
			breakIt(&cfg)
			if err := cfg.validate(); err == nil {
				t.Fatalf("expected %s to be rejected", name)
			}
			_, err := NewWorker(newFakeRepo(), newFakeSandbox(), &fakeClaude{}, &fakeProvider{},
				&fakeEmbeddings{}, &fakeGoals{}, &fakeAudits{}, cfg)
			if err == nil {
				t.Fatalf("NewWorker must refuse to start with %s", name)
			}
		})
	}
	if err := testConfig().validate(); err != nil {
		t.Fatalf("the default test tuning must validate, got %v", err)
	}
}

// TestRunTerminalInputFailures: each of these is a hard input with nothing
// meaningful to degrade to, so each must end the run rather than reporting a
// successful run with zero winners.
func TestRunTerminalInputFailures(t *testing.T) {
	t.Run("goal lookup", func(t *testing.T) {
		h := newHarness(t, testConfig())
		h.goals.err = errors.New("postgres down")
		if err := h.run(t); err == nil {
			t.Fatal("a failed goal lookup must be terminal")
		}
	})

	t.Run("objective cannot be pinned", func(t *testing.T) {
		h := newHarness(t, testConfig())
		// A legacy target with no aggregation: nothing measurable to pin.
		h.goals.goal = store.Goal{
			OptimizationFunctionID: "g1",
			DataSourceRef:          "ref.csv",
			EvaluationMatrix: domain.EvaluationMatrix{Targets: []domain.Target{
				{Field: "revenue", Direction: domain.Maximize},
			}},
		}
		if err := h.run(t); err == nil {
			t.Fatal("an unpinnable objective must be terminal")
		}
		if h.sandbox.execCalls != 0 {
			t.Fatalf("nothing should be measured without a pinned objective, got %d", h.sandbox.execCalls)
		}
	})

	t.Run("non-numeric baseline", func(t *testing.T) {
		h := newHarness(t, testConfig())
		h.repo.findings = findingsFor("a", "b")
		h.sandbox.nonNumeric = map[string]bool{"baseline": true}
		if err := h.run(t); err == nil {
			t.Fatal("a baseline that measures nothing usable must be terminal")
		}
	})
}

// TestAbstractionFailureBranchesAreIsolated walks the per-segment failure modes
// the abstraction stage promises to contain. Each runs two winning segments and
// asserts both were attempted and both audited, so a failure in one never
// short-circuits the other.
func TestAbstractionFailureBranchesAreIsolated(t *testing.T) {
	cfg := testConfig()
	cfg.MinLift = 0.05
	cfg.MinSupport = 30

	cases := map[string]func(*harness){
		"repair call fails": func(h *harness) {
			h.sandbox.schema.Columns = append(h.sandbox.schema.Columns, sandboxColumn("HomePlanet"))
			h.claude.alwaysLeak = "HomePlanet drives it"
			h.claude.repairErr = errors.New("claude down")
		},
		"meta-heuristic write fails": func(h *harness) {
			h.repo.createErrs["metaheuristic"] = errors.New("abstractedFrom rejected")
		},
		"embedding provider fails": func(h *harness) {
			h.provider.err = errors.New("ollama down")
		},
		"clearing the pending flag fails": func(h *harness) {
			h.repo.createErrs["clear_pending"] = errors.New("neo4j down")
		},
	}

	for name, breakIt := range cases {
		t.Run(name, func(t *testing.T) {
			h := winningHarness(t, cfg, map[string]float64{"a+b": 10.0, "a+c": 11.0}, "a", "b", "c")
			breakIt(h)

			if err := h.run(t); err != nil {
				t.Fatalf("a per-segment abstraction failure must never abort the run: %v", err)
			}
			if got := h.audits.count("sleepcycle_abstraction_failure"); got != 2 {
				t.Fatalf("expected one failure audit per segment, got %d", got)
			}
			if h.audits.count("sleepcycle_heuristic_injection") != 0 {
				t.Fatal("a segment that failed must not claim a successful injection")
			}
		})
	}
}

// TestFailedEmbedLeavesTheNodePending is the crash-window invariant: the pending
// flag is cleared only after pgvector accepts the vector, so a failed upsert
// leaves the node for the next run's resume pass rather than marking it done.
func TestFailedEmbedLeavesTheNodePending(t *testing.T) {
	cfg := testConfig()
	cfg.MinLift = 0.05
	cfg.MinSupport = 30
	h := winningHarness(t, cfg, map[string]float64{"a+b": 10.0}, "a", "b")
	h.embeddings.err = errors.New("pgvector down")

	if err := h.run(t); err != nil {
		t.Fatalf("run: %v", err)
	}
	if len(h.repo.heuristics) != 1 {
		t.Fatalf("the node should still be written, got %d", len(h.repo.heuristics))
	}
	if len(h.repo.cleared) != 0 {
		t.Fatal("the pending flag must not be cleared when the vector write failed")
	}
}

// TestEmptySegmentIsNotAuditedAsASandboxFault: a conjunction matching zero rows
// is a routine search result, not a fault. It still cannot rank or win — there is
// no objective to measure — but mislabeling it would bury real sandbox faults.
func TestEmptySegmentIsNotAuditedAsASandboxFault(t *testing.T) {
	cfg := testConfig()
	h := newHarness(t, cfg)
	h.repo.findings = findingsFor("a", "b")
	h.sandbox.baseline = 1.0
	h.sandbox.defaultValue = 2.0
	// An empty-filtered aggregate: the objective scans as NULL, support is 0.
	h.sandbox.nonNumeric = map[string]bool{"a+b": true}
	h.sandbox.measurements = map[string]sandboxMeasurement{
		"a":   {value: 2.0, support: 100},
		"b":   {value: 2.0, support: 100},
		"a+b": {value: 0, support: 0},
	}

	outcome := h.searchOnly(t)

	if h.audits.count("sleepcycle_measurement_failure") != 0 {
		t.Fatalf("an empty segment must not be audited as a sandbox fault: %+v", h.audits.records)
	}
	for _, m := range outcome.measured {
		if m.node.Order() == 2 && !m.measurement.Failed {
			t.Fatal("an empty segment has no objective, so it must stay out of the frontier and the winner set")
		}
	}
}

// TestEmptyCountSegmentCannotWin is the shape the NULL-aggregate case above does
// NOT cover. avg/sum/min/max scan an empty segment as SQL NULL, but count returns
// a real 0 — a perfectly readable number backed by no rows. Under Minimize at a
// support floor of 0 that zero is the best value on the board, so without the
// support check it is written back as the run's winning macro-segment and
// abstracted into a published heuristic describing an empty population.
func TestEmptyCountSegmentCannotWin(t *testing.T) {
	cfg := testConfig()
	cfg.MinSupport = 0
	cfg.MinLift = 0
	h := newHarness(t, cfg)
	h.goals.goal = minimizeGoal()
	h.repo.findings = []graph.CausalTriplet{
		finding("1", 2.0, testObjectiveLabel, "a"),
		finding("2", 3.0, testObjectiveLabel, "b"),
	}
	h.sandbox.baseline = 10.0
	h.sandbox.measurements = map[string]sandboxMeasurement{
		"a": {value: 8.0, support: 100},
		"b": {value: 9.0, support: 100},
		// Readable, numeric, and the lowest value measured — backed by zero rows.
		"a+b": {value: 0.0, support: 0},
	}

	if err := h.run(t); err != nil {
		t.Fatalf("run: %v", err)
	}
	if len(h.repo.interventions) != 0 {
		t.Fatalf("a zero-row segment must never win: %+v", h.repo.interventions)
	}
	// Both findings improve on the baseline and are published, so this segment must
	// be named directly.
	if h.didAbstract(t, "a", "b") {
		t.Fatal("a zero-row segment must not reach abstraction")
	}
}

// TestMissingRowCountIsAFault is the other half: a response without the count the
// request asked for means the sandbox did not answer the question, and silently
// reading it as support 0 would put every atom below the default floor and end
// every run empty with no indication anything broke.
func TestMissingRowCountIsAFault(t *testing.T) {
	cfg := testConfig()
	h := newHarness(t, cfg)
	h.repo.findings = findingsFor("a", "b")
	h.sandbox.omitRowCount = true
	h.sandbox.defaultValue = 2.0

	h.searchOnly(t)

	if h.audits.count("sleepcycle_measurement_failure") == 0 {
		t.Fatalf("a missing row count must be recorded as a failure: %+v", h.audits.records)
	}
}

// TestAuditFailuresNeverBreakTheWork: an audit hiccup must not take down the
// work it describes.
func TestAuditFailuresNeverBreakTheWork(t *testing.T) {
	cfg := testConfig()
	cfg.MinLift = 0.05
	cfg.MinSupport = 30
	h := winningHarness(t, cfg, map[string]float64{"a+b": 10.0}, "a", "b")
	h.audits.err = errors.New("orchestrator down")

	if err := h.run(t); err != nil {
		t.Fatalf("an audit failure must be logged and swallowed, got %v", err)
	}
	if len(h.repo.heuristics) != 1 {
		t.Fatalf("the macro-segment must still be abstracted, got %d heuristics", len(h.repo.heuristics))
	}
}

// TestWrongDirectionSegmentNeverWins pins the winner gate's direction check. Lift
// is a pure magnitude, so without the improvement test a segment that moves the
// objective the WRONG way by more than MinLift would clear the bar and be written
// back as a verified finding contradicted by its own measurement.
//
// The finding sits on the wrong side of the baseline so it is not itself
// publishable. That is load-bearing rather than incidental: a depth-1 finding
// introducing {a,b} carries that pair as its own effective segment, so a
// publishable one would be abstracted under the very canonical this test asserts
// against — naming the segment cannot separate them, and the Claude-call count is
// the only assertion that can.
func TestWrongDirectionSegmentNeverWins(t *testing.T) {
	cfg := testConfig()
	cfg.MinLift = 0.05
	cfg.MinSupport = 30
	h := newHarness(t, cfg)
	h.goals.goal = minimizeGoal()
	h.repo.findings = []graph.CausalTriplet{finding("1", 15.0, testObjectiveLabel, "a", "b")}
	h.sandbox.baseline = 10.0
	h.sandbox.measurements = map[string]sandboxMeasurement{
		"a": {value: 8.0, support: 100},
		"b": {value: 9.0, support: 100},
		// Ten times S* in the wrong direction for a Minimize goal. Its magnitude
		// clears MinLift nine times over, so only the direction check excludes it.
		"a+b": {value: 150.0, support: 100},
	}

	if err := h.run(t); err != nil {
		t.Fatalf("run: %v", err)
	}
	if len(h.repo.interventions) != 0 {
		t.Fatalf("a segment moving the objective the wrong way must never win: %+v", h.repo.interventions)
	}
	if h.claude.abstractCalls != 0 {
		t.Fatal("a wrong-direction segment must not reach abstraction")
	}
}

// TestBestSingleIsDirectionalUnderMinimize pins S* itself, not the gate that
// consumes it. Picking the largest finding value regardless of direction is
// invisible under Maximize -- the two coincide -- but under Minimize it hands the
// gate the WORST single segment as the bar to beat, so a macro-segment that is
// plainly worse than a finding already on record clears it and is written back.
func TestBestSingleIsDirectionalUnderMinimize(t *testing.T) {
	cfg := testConfig()
	cfg.MinLift = 0.05
	cfg.MinSupport = 30
	h := newHarness(t, cfg)
	h.goals.goal = minimizeGoal()
	// S* is 2.0, the lowest value on record. A direction-blind maximum picks 10.0.
	h.repo.findings = []graph.CausalTriplet{
		finding("1", 2.0, testObjectiveLabel, "a"),
		finding("2", 10.0, testObjectiveLabel, "b"),
	}
	h.sandbox.baseline = 12.0
	h.sandbox.measurements = map[string]sandboxMeasurement{
		"a": {value: 8.0, support: 100},
		"b": {value: 9.0, support: 100},
		// Between the two findings: worse than the real S* of 2.0, but comfortably
		// past MinLift against a bar mistakenly set to 10.0.
		"a+b": {value: 5.0, support: 100},
	}

	if err := h.run(t); err != nil {
		t.Fatalf("run: %v", err)
	}
	if len(h.repo.interventions) != 0 {
		t.Fatalf("a segment worse than the best single finding must not win: %+v", h.repo.interventions)
	}
}

// TestUndefinedBestSingleBlocksAllWinners: findings can contribute atoms while
// carrying no value under the pinned objective label, so S* is undefined even
// though the search runs. Without the guard, S* would default to a fabricated 0
// and the epsilon-floored divisor would let effectively every conjunction win
// against a comparison point that was never measured.
func TestUndefinedBestSingleBlocksAllWinners(t *testing.T) {
	cfg := testConfig()
	cfg.MinLift = 0.05
	h := newHarness(t, cfg)
	// Two atoms (so the search is not degenerate) but the outcomes are keyed by a
	// different label, so no eligible finding yields a value for this objective.
	h.repo.findings = []graph.CausalTriplet{
		finding("1", 5.0, "avg(something_else)", "a"),
		finding("2", 6.0, "avg(something_else)", "b"),
	}
	h.sandbox.baseline = 1.0
	h.sandbox.measurements = map[string]sandboxMeasurement{
		"a":   {value: 2.0, support: 100},
		"b":   {value: 3.0, support: 100},
		"a+b": {value: 99.0, support: 100},
	}

	if err := h.run(t); err != nil {
		t.Fatalf("run: %v", err)
	}
	if len(h.repo.interventions) != 0 {
		t.Fatalf("nothing may win when S* is undefined: %+v", h.repo.interventions)
	}
	rec, ok := h.audits.find("sleepcycle_no_abstraction")
	if !ok {
		t.Fatalf("expected a no-abstraction audit: %+v", h.audits.records)
	}
	if rec.detail["best_single"] != nil {
		t.Fatalf("best_single must stay nil when undefined, got %v", rec.detail["best_single"])
	}
}

// TestRunRecoversFromAPanic: one collaborator panicking must be contained and
// reported as a failed run, not escape and take down the job process.
func TestRunRecoversFromAPanic(t *testing.T) {
	h := newHarness(t, testConfig())
	h.repo.panicOnFindings = true

	err := h.run(t)
	if err == nil {
		t.Fatal("a panic must surface as a terminal error, not a successful run")
	}
	if !strings.Contains(err.Error(), "panicked") {
		t.Fatalf("error = %v, want it to name the panic", err)
	}
	if _, ok := h.audits.find("sleepcycle_run_failed"); !ok {
		t.Fatalf("expected a run-failed audit: %+v", h.audits.records)
	}
}

// TestCancelledRunIsNotReportedComplete: per-candidate and per-segment failures
// are deliberately isolated, so a cancelled run would otherwise walk to the end
// with no error and be audited as a clean completion — making a truncated run
// indistinguishable from a genuinely degenerate one.
//
// It also pins the terminal audit's detached context: the audit fake honors the
// context it is handed, so writing this record on the run's own (already
// cancelled) context would drop it and fail the final assertion.
func TestCancelledRunIsNotReportedComplete(t *testing.T) {
	h := newHarness(t, testConfig())
	h.repo.findings = findingsFor("a", "b")
	h.sandbox.defaultValue = 1.0

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	if err := h.worker.Run(ctx, "g1"); err == nil {
		t.Fatal("a cancelled run must not report success")
	}
	if _, ok := h.audits.find("sleepcycle_run_complete"); ok {
		t.Fatalf("a cancelled run must not be audited as complete: %+v", h.audits.records)
	}
	if _, ok := h.audits.find("sleepcycle_run_failed"); !ok {
		t.Fatalf("expected a run-failed audit: %+v", h.audits.records)
	}
}

// TestFailedMeasurementIsExcludedFromTheFrontier: at a support floor of zero the
// support test alone no longer excludes a failed node, so the explicit exclusion
// is what stops a fabricated zero-delta node from occupying a beam slot and
// displacing a real candidate.
//
// The fixture is built so the exclusion is the ONLY thing that changes the
// outcome. A failed measurement carries Value 0, so against a negative baseline
// its fabricated delta is the BEST on the level — without the exclusion it wins
// the single beam slot outright. It is also the highest-ranked atom, which can
// prefix nothing, so the level it seeds is empty and the search stops one level
// early. With the exclusion a real survivor takes the slot and expands normally.
func TestFailedMeasurementIsExcludedFromTheFrontier(t *testing.T) {
	cfg := testConfig()
	cfg.BeamWidth = 1
	cfg.MinSupport = 0
	atoms, err := buildAtoms(findingsFor("a", "b", "c"))
	if err != nil {
		t.Fatalf("build atoms: %v", err)
	}
	p := newBeamPolicy(atoms, cfg, -10, domain.Maximize)

	for {
		node, ok := p.Select()
		if !ok {
			break
		}
		if node.filters[0].Field == "c" {
			p.Update(node, Measurement{Failed: true})
		} else {
			// Worse than the baseline, so its real delta is -10 against the failed
			// node's fabricated +10.
			p.Update(node, Measurement{Value: -20, Support: 100})
		}
		if p.level > 1 {
			break
		}
	}

	if p.Done() {
		t.Fatal("a failed node took the beam slot and ended the search a level early")
	}
	if len(p.pending) == 0 {
		t.Fatal("the frontier admitted only the failed node, so no candidate was generated")
	}
	for _, n := range p.pending {
		if slices.Contains(n.keys, atoms[2].key) && n.Order() == 1 {
			t.Fatalf("the failed node itself entered the next level: %s", n.canonical)
		}
	}
}
