package sleepcycle

import (
	"errors"
	"testing"
)

// TestFailedMeasurementPrunesItsSupersets is the prune-on-error rule: a failed
// measurement is treated as unknown — therefore insufficient — support, so the
// node is never expanded. Expanding it would mean conjoining supersets of a
// filter that already failed once.
func TestFailedMeasurementPrunesItsSupersets(t *testing.T) {
	h := newHarness(t, testConfig())
	h.repo.findings = findingsFor("a", "b")
	h.sandbox.baseline = 1.0
	h.sandbox.measurements = map[string]sandboxMeasurement{
		"a": {err: errors.New("duckdb blew up on a pathological filter")},
		"b": {value: 2.0, support: 100},
	}

	if err := h.run(t); err != nil {
		t.Fatalf("a failed measurement must never abort the run, got %v", err)
	}

	if h.sandbox.didMeasure("a+b") {
		t.Fatal("a+b must never be measured: its only generator {a} failed and was pruned")
	}
	if !h.sandbox.didMeasure("b") {
		t.Fatal("the run must continue past the failure and measure b")
	}
	if got := h.audits.count("sleepcycle_measurement_failure"); got != 1 {
		t.Fatalf("expected exactly one measurement-failure audit, got %d", got)
	}
}

// TestFailedMeasurementIsMemoized: a failed node is recorded and budget-consuming,
// so a policy that re-selected it would cost no second sandbox call.
func TestFailedMeasurementIsMemoized(t *testing.T) {
	cfg := testConfig()
	h := newHarness(t, cfg)
	h.repo.findings = findingsFor("a", "b")
	h.sandbox.baseline = 1.0
	h.sandbox.measurements = map[string]sandboxMeasurement{
		"a": {err: errors.New("boom")},
		"b": {value: 2.0, support: 100},
	}

	outcome := h.searchOnly(t)

	seen := map[string]int{}
	for _, key := range h.sandbox.segmentsMeasured() {
		seen[key]++
	}
	if seen["a"] != 1 {
		t.Fatalf("the failed node was measured %d times, want exactly once", seen["a"])
	}
	if len(outcome.measured) != 2 {
		t.Fatalf("measurements = %d, want 2 (the failure consumes budget like any other)", len(outcome.measured))
	}
	var failed int
	for _, m := range outcome.measured {
		if m.measurement.Failed {
			failed++
		}
	}
	if failed != 1 {
		t.Fatalf("the run-local table should carry exactly one failed entry, got %d", failed)
	}
}

// TestNonNumericValueIsAFailedMeasurement: a response the objective cannot be
// read out of is a miss, not a zero.
func TestNonNumericValueIsAFailedMeasurement(t *testing.T) {
	cfg := testConfig()
	h := newHarness(t, cfg)
	h.repo.findings = findingsFor("a", "b")
	h.sandbox.nonNumeric = map[string]bool{"a": true}
	h.sandbox.defaultValue = 1.0

	h.searchOnly(t)

	if h.audits.count("sleepcycle_measurement_failure") != 1 {
		t.Fatalf("a non-numeric value must be recorded as a failure, got %d audits",
			h.audits.count("sleepcycle_measurement_failure"))
	}
	if h.sandbox.didMeasure("a+b") {
		t.Fatal("a non-numeric measurement must prune its supersets like any other failure")
	}
}
