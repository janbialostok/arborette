package verifier

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/arborette/arborette/internal/domain"
	"github.com/arborette/arborette/internal/graph"
	"github.com/arborette/arborette/internal/store"
	"github.com/arborette/arborette/internal/verifier/groundtruth"
)

type fakeLocker struct {
	acquired bool
	err      error
	calls    int
	released int
}

func (f *fakeLocker) TryAcquireDiscoveryLock(context.Context, string, string) (func(), bool, error) {
	f.calls++
	if f.err != nil {
		return nil, false, f.err
	}
	if f.acquired {
		return func() { f.released++ }, true, nil
	}
	return nil, false, nil
}

type fakeGraph struct {
	found      bool
	foundAfter int // report a committed graph once getCalls reaches this (0 = never via this path)
	getErr     error
	getCalls   int
	columns    []domain.DataColumn
	edges      []domain.CausalEdge
	metaCalls  int
	deletes    int
}

func (f *fakeGraph) GetCausalGraph(context.Context, string, string) (domain.CausalGraph, bool, error) {
	f.getCalls++
	if f.getErr != nil {
		return domain.CausalGraph{}, false, f.getErr
	}
	if f.found || (f.foundAfter > 0 && f.getCalls >= f.foundAfter) {
		return domain.CausalGraph{Meta: domain.CausalGraphMeta{Version: 1}}, true, nil
	}
	return domain.CausalGraph{}, false, nil
}
func (f *fakeGraph) DeleteCausalGraphVersion(context.Context, string, string, int) error {
	f.deletes++
	return nil
}
func (f *fakeGraph) UpsertDataColumns(_ context.Context, cols []domain.DataColumn) error {
	f.columns = cols
	return nil
}
func (f *fakeGraph) UpsertCausalEdges(_ context.Context, edges []domain.CausalEdge) error {
	f.edges = edges
	return nil
}
func (f *fakeGraph) UpsertCausalGraphMeta(context.Context, domain.CausalGraphMeta) error {
	f.metaCalls++
	return nil
}
func (f *fakeGraph) ListEligibleFindings(context.Context, string) ([]graph.CausalTriplet, error) {
	return nil, nil
}

type fakeGoalReader struct{ goal store.Goal }

func (f fakeGoalReader) Get(context.Context, string) (store.Goal, error) { return f.goal, nil }

type fakeAudit struct {
	actions []string
	err     error
}

func (f *fakeAudit) Append(_ context.Context, action, _ string, _ map[string]any) error {
	f.actions = append(f.actions, action)
	return f.err
}

func newTestWorker(a analyzer, g graphWriter, lk locker) *Worker {
	w := NewWorker(a, g, nil, lk, fakeGoalReader{}, &fakeAudit{}, regressionConfig(), 0)
	// Squeeze the waiter timing so the tests do not sleep for seconds.
	w.pollMin = time.Millisecond
	w.pollMax = 2 * time.Millisecond
	w.waitBudget = 50 * time.Millisecond
	return w
}

// TestRunDiscoveryRechecksCommitted: acquiring the lock and finding a committed
// graph on the recheck short-circuits — no discovery runs, and the lock releases.
func TestRunDiscoveryRechecksCommitted(t *testing.T) {
	g := &fakeGraph{found: true}
	lk := &fakeLocker{acquired: true}
	w := newTestWorker(nil, g, lk)
	if err := w.RunDiscovery(context.Background(), "goal", "ref"); err != nil {
		t.Fatalf("RunDiscovery: %v", err)
	}
	if g.metaCalls != 0 {
		t.Fatalf("discovery ran despite a committed graph (metaCalls=%d)", g.metaCalls)
	}
	if lk.released != 1 {
		t.Fatalf("lock released %d times, want 1", lk.released)
	}
}

// TestRunDiscoveryWaitsForCommitted: losing the lock and then observing a committed
// graph returns without discovering (the waiter returns the peer's result).
func TestRunDiscoveryWaitsForCommitted(t *testing.T) {
	g := &fakeGraph{found: true}
	lk := &fakeLocker{acquired: false}
	w := newTestWorker(nil, g, lk)
	if err := w.RunDiscovery(context.Background(), "goal", "ref"); err != nil {
		t.Fatalf("RunDiscovery: %v", err)
	}
	if g.metaCalls != 0 {
		t.Fatalf("discovery ran while a peer had committed (metaCalls=%d)", g.metaCalls)
	}
}

// TestRunDiscoveryRunsAndPersists: acquiring the lock with no committed graph runs
// the full pipeline and persists columns → edges → meta with bracketing audit events.
func TestRunDiscoveryRunsAndPersists(t *testing.T) {
	ds := groundtruth.Generate(groundtruth.DefaultConfig())
	g := &fakeGraph{found: false}
	lk := &fakeLocker{acquired: true}
	audit := &fakeAudit{}
	w := NewWorker(newFakeAnalyzer(ds), g, nil, lk, fakeGoalReader{goal: store.Goal{GoalText: "maximize Y"}}, audit, regressionConfig(), 0)

	if err := w.RunDiscovery(context.Background(), "goal", "ref"); err != nil {
		t.Fatalf("RunDiscovery: %v", err)
	}
	if g.deletes != 1 || len(g.columns) != 7 || len(g.edges) != 5 || g.metaCalls != 1 {
		t.Fatalf("persist wrong: deletes=%d cols=%d edges=%d meta=%d", g.deletes, len(g.columns), len(g.edges), g.metaCalls)
	}
	if len(audit.actions) != 2 || audit.actions[0] != "causal_discovery_started" || audit.actions[1] != "causal_discovery_complete" {
		t.Fatalf("audit actions = %v, want started+complete", audit.actions)
	}
	if lk.released != 1 {
		t.Fatalf("lock released %d times, want 1", lk.released)
	}
}

// TestRunDiscoverySurvivesAuditFailure pins the non-fatal audit invariant: an
// audit-append failure is logged and swallowed, so the graph is still persisted and
// discovery returns nil rather than sinking a completed run.
func TestRunDiscoverySurvivesAuditFailure(t *testing.T) {
	ds := groundtruth.Generate(groundtruth.DefaultConfig())
	g := &fakeGraph{found: false}
	audit := &fakeAudit{err: errors.New("audit 401")}
	w := NewWorker(newFakeAnalyzer(ds), g, nil, &fakeLocker{acquired: true},
		fakeGoalReader{goal: store.Goal{GoalText: "maximize Y"}}, audit, regressionConfig(), 0)

	if err := w.RunDiscovery(context.Background(), "goal", "ref"); err != nil {
		t.Fatalf("RunDiscovery must not fail on an audit error: %v", err)
	}
	if g.metaCalls != 1 {
		t.Fatalf("graph not persisted despite audit failure (metaCalls=%d)", g.metaCalls)
	}
	if len(audit.actions) != 2 {
		t.Fatalf("audit attempted %d times, want 2 (both still attempted)", len(audit.actions))
	}
}

func TestRunDiscoveryAcquireError(t *testing.T) {
	lk := &fakeLocker{err: errors.New("db down")}
	w := newTestWorker(nil, &fakeGraph{}, lk)
	if err := w.RunDiscovery(context.Background(), "goal", "ref"); err == nil {
		t.Fatalf("expected an error when the lock acquire fails")
	}
}

// TestRunDiscoveryTimesOut: perpetually losing the lock while no committed graph
// appears exits the waiter loop with a timeout rather than spinning forever.
func TestRunDiscoveryTimesOut(t *testing.T) {
	g := &fakeGraph{found: false}
	lk := &fakeLocker{acquired: false}
	w := newTestWorker(nil, g, lk)
	if err := w.RunDiscovery(context.Background(), "goal", "ref"); err == nil {
		t.Fatalf("expected a timeout error when discovery never commits")
	}
}
