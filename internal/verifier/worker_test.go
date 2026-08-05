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

	graphValue         domain.CausalGraph // returned by GetCausalGraph when found (Meta.Version defaults to 1)
	intervention       domain.Intervention
	getInterventionErr error
	causalEvidence     graph.CausalEvidence
	hasCausalEvidence  bool
	causalWrites       int
	writtenVersion     int
	writtenOutcome     domain.Outcome
	writtenEdge        domain.ProducedEdge
	supersedes         int
	supersededVersion  int

	// The triplet writes a reified claim persists, recorded so a test can assert the
	// nodes and the deterministic ids it minted.
	states        []domain.State
	interventions []domain.Intervention
	outcomes      []domain.Outcome
	preConditions [][2]string
	// produced records the edge's endpoints alongside its properties: the endpoints are
	// what make a reified triplet coherent, so a test asserting deterministic ids must
	// be able to see which pair the edge actually linked.
	produced []producedEdge
}

func (f *fakeGraph) GetCausalGraph(context.Context, string, string) (domain.CausalGraph, bool, error) {
	f.getCalls++
	if f.getErr != nil {
		return domain.CausalGraph{}, false, f.getErr
	}
	if f.found || (f.foundAfter > 0 && f.getCalls >= f.foundAfter) {
		g := f.graphValue
		if g.Meta.Version == 0 {
			g.Meta.Version = 1
		}
		return g, true, nil
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
func (f *fakeGraph) GetIntervention(context.Context, string) (domain.Intervention, error) {
	return f.intervention, f.getInterventionErr
}
func (f *fakeGraph) GetCausalEvidence(context.Context, string) (graph.CausalEvidence, bool, error) {
	return f.causalEvidence, f.hasCausalEvidence, nil
}
func (f *fakeGraph) WriteCausalVerification(_ context.Context, _ string, version int, outcome domain.Outcome, edge domain.ProducedEdge) error {
	f.causalWrites++
	f.writtenVersion = version
	f.writtenOutcome = outcome
	f.writtenEdge = edge
	return nil
}
func (f *fakeGraph) SupersedePriorCausalOutcomes(_ context.Context, _ string, version int) error {
	f.supersedes++
	f.supersededVersion = version
	return nil
}
func (f *fakeGraph) CreateState(_ context.Context, s domain.State) error {
	f.states = append(f.states, s)
	return nil
}
func (f *fakeGraph) CreateIntervention(_ context.Context, i domain.Intervention) error {
	f.interventions = append(f.interventions, i)
	return nil
}
func (f *fakeGraph) CreateOutcome(_ context.Context, o domain.Outcome) error {
	f.outcomes = append(f.outcomes, o)
	return nil
}
func (f *fakeGraph) CreatePreConditionFor(_ context.Context, stateID, interventionID string) error {
	f.preConditions = append(f.preConditions, [2]string{stateID, interventionID})
	return nil
}
func (f *fakeGraph) CreateProduced(_ context.Context, interventionID, outcomeID string, edge domain.ProducedEdge) error {
	f.produced = append(f.produced, producedEdge{interventionID: interventionID, outcomeID: outcomeID, edge: edge})
	return nil
}

// producedEdge is one recorded PRODUCED write: the pair it linked and its properties.
type producedEdge struct {
	interventionID string
	outcomeID      string
	edge           domain.ProducedEdge
}

type fakeGoalReader struct{ goal store.Goal }

func (f fakeGoalReader) Get(context.Context, string) (store.Goal, error) { return f.goal, nil }

type fakeAudit struct {
	actions []string
	events  []map[string]any
	err     error
}

func (f *fakeAudit) Append(_ context.Context, action, _ string, _ map[string]any) error {
	f.actions = append(f.actions, action)
	return f.err
}

func (f *fakeAudit) PublishVerification(_ context.Context, _ string, event map[string]any) error {
	f.events = append(f.events, event)
	return f.err
}

func newTestWorker(a analyzer, g graphWriter, lk locker) *Worker {
	w := NewWorker(a, g, nil, lk, fakeGoalReader{}, &fakeAudit{}, nil, regressionConfig(), 0, time.Second)
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
	w := NewWorker(newFakeAnalyzer(ds), g, nil, lk, fakeGoalReader{goal: store.Goal{GoalText: "maximize Y"}}, audit, nil, regressionConfig(), 0, time.Second)

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
		fakeGoalReader{goal: store.Goal{GoalText: "maximize Y"}}, audit, nil, regressionConfig(), 0, time.Second)

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
