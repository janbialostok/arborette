package sleepcycle

import (
	"context"
	"encoding/json"
	"errors"
	"sort"
	"strings"
	"testing"

	"github.com/arborette/arborette/internal/domain"
	"github.com/arborette/arborette/internal/graph"
	"github.com/arborette/arborette/internal/llm"
	"github.com/arborette/arborette/internal/objective"
	"github.com/arborette/arborette/internal/sandboxclient"
	"github.com/arborette/arborette/internal/store"
)

// testConfig is the tuning the search tests run at unless a case overrides it:
// a floor of 0 and a lift of 0 keep support and lift out of the way so a test
// exercises exactly the axis it names. The publication cap is the production
// default rather than 0, because every worker test constructs through
// NewWorker's validation, which refuses a cap below 1.
func testConfig() Config {
	return Config{MaxMeasurements: 200, BeamWidth: 10, MaxOrder: 3, MinSupport: 0, MinLift: 0, MaxPublications: 20}
}

// segmentKey names a candidate by its filter columns, which is how the tests
// script per-candidate measurements. Every test atom is a distinct column, so
// the column set identifies the conjunction.
func segmentKey(filters []domain.Constraint) string {
	if len(filters) == 0 {
		return "baseline"
	}
	fields := make([]string, 0, len(filters))
	for _, f := range filters {
		fields = append(fields, strings.ToLower(f.Field))
	}
	sort.Strings(fields)
	return strings.Join(fields, "+")
}

// atomOn builds the single-predicate filter the tests use as an atom.
func atomOn(field string) domain.Constraint {
	return domain.Constraint{Field: field, Op: domain.GreaterThan, Value: 1}
}

// finding builds one eligible Phase-1 triplet introducing the given atoms. Its
// support of 100 clears every floor the tests configure, matching the fake
// sandbox's defaultSupport, so S* stays defined unless a case says otherwise.
//
// The effective filter set defaults to the new one, which is what a depth-1
// finding actually carries. Publication reads the effective set, so a fixture
// leaving it unset would decode to an empty set and be skipped as the baseline —
// use findingWithEffective for the depth-N shapes where the two differ.
func finding(id string, value float64, label string, fields ...string) graph.CausalTriplet {
	filters := make([]domain.Constraint, 0, len(fields))
	for _, f := range fields {
		filters = append(filters, atomOn(f))
	}
	return graph.CausalTriplet{
		State: domain.State{ID: "s-" + id},
		Intervention: domain.Intervention{ID: "i-" + id, Properties: map[string]any{
			"new_filters":       asProperty(filters),
			"effective_filters": asProperty(filters),
		}},
		Outcome: domain.Outcome{ID: "o-" + id, VerificationStatus: domain.VerificationVerified, Value: map[string]any{label: value}, Support: 100},
	}
}

// findingWithSupport overrides the default support to exercise the S* floor.
func findingWithSupport(id string, value float64, support int64, fields ...string) graph.CausalTriplet {
	f := finding(id, value, testObjectiveLabel, fields...)
	f.Outcome.Support = support
	return f
}

// findingWithEffective builds a deeper finding, whose effective segment is the
// cumulative branch its value was measured under rather than the single
// predicate it introduced.
func findingWithEffective(id string, value float64, support int64, newFields, effectiveFields []string) graph.CausalTriplet {
	f := findingWithSupport(id, value, support, newFields...)
	effective := make([]domain.Constraint, 0, len(effectiveFields))
	for _, field := range effectiveFields {
		effective = append(effective, atomOn(field))
	}
	f.Intervention.Properties["effective_filters"] = asProperty(effective)
	return f
}

// asProperty round-trips filters through the shape the graph layer actually
// returns them in -- []any of map[string]any, not a typed slice -- so the tests
// exercise the same decode path production does.
func asProperty(filters []domain.Constraint) any {
	b, err := json.Marshal(filters)
	if err != nil {
		panic(err)
	}
	var decoded any
	if err := json.Unmarshal(b, &decoded); err != nil {
		panic(err)
	}
	return decoded
}

type fakeRepo struct {
	findings          []graph.CausalTriplet
	findingsErr       error
	panicOnFindings   bool
	staleMarked       int
	staleErr          error
	metaHeuristics    []domain.MetaHeuristic
	metaHeuristicsErr error
	existing          map[string]domain.MetaHeuristic
	getErr            map[string]error
	createErrs        map[string]error
	states            []domain.State
	interventions     []domain.Intervention
	outcomes          []domain.Outcome
	preConditions     [][2]string
	produced          []domain.ProducedEdge
	heuristics        []domain.MetaHeuristic
	abstractedFrom    map[string][]string
	cleared           []string
}

func newFakeRepo() *fakeRepo {
	return &fakeRepo{
		existing:       map[string]domain.MetaHeuristic{},
		getErr:         map[string]error{},
		createErrs:     map[string]error{},
		abstractedFrom: map[string][]string{},
	}
}

func (f *fakeRepo) ListEligibleFindings(context.Context, string) ([]graph.CausalTriplet, error) {
	if f.panicOnFindings {
		panic("collaborator exploded")
	}
	return f.findings, f.findingsErr
}
func (f *fakeRepo) MarkStaleMetaHeuristics(context.Context, string) (int, error) {
	return f.staleMarked, f.staleErr
}
func (f *fakeRepo) CreateState(_ context.Context, s domain.State) error {
	if err := f.createErrs["state"]; err != nil {
		return err
	}
	f.states = append(f.states, s)
	return nil
}
func (f *fakeRepo) CreateIntervention(_ context.Context, i domain.Intervention) error {
	if err := f.createErrs["intervention:"+stringProp(i.Properties["canonical_filter"])]; err != nil {
		return err
	}
	if err := f.createErrs["intervention"]; err != nil {
		return err
	}
	f.interventions = append(f.interventions, i)
	return nil
}
func (f *fakeRepo) CreateOutcome(_ context.Context, o domain.Outcome) error {
	if err := f.createErrs["outcome"]; err != nil {
		return err
	}
	f.outcomes = append(f.outcomes, o)
	return nil
}
func (f *fakeRepo) CreatePreConditionFor(_ context.Context, stateID, interventionID string) error {
	if err := f.createErrs["pre_condition"]; err != nil {
		return err
	}
	f.preConditions = append(f.preConditions, [2]string{stateID, interventionID})
	return nil
}
func (f *fakeRepo) CreateProduced(_ context.Context, _, _ string, edge domain.ProducedEdge) error {
	if err := f.createErrs["produced"]; err != nil {
		return err
	}
	f.produced = append(f.produced, edge)
	return nil
}
func (f *fakeRepo) GetMetaHeuristic(_ context.Context, id string) (domain.MetaHeuristic, error) {
	if err, ok := f.getErr[id]; ok {
		return domain.MetaHeuristic{}, err
	}
	if mh, ok := f.existing[id]; ok {
		return mh, nil
	}
	return domain.MetaHeuristic{}, graph.ErrNotFound
}
func (f *fakeRepo) CreateMetaHeuristic(_ context.Context, mh domain.MetaHeuristic, abstractedFrom []string) error {
	if err := f.createErrs["metaheuristic"]; err != nil {
		return err
	}
	f.heuristics = append(f.heuristics, mh)
	f.abstractedFrom[mh.ID] = abstractedFrom
	return nil
}
func (f *fakeRepo) ClearEmbeddingPending(_ context.Context, id string) error {
	if err := f.createErrs["clear_pending"]; err != nil {
		return err
	}
	f.cleared = append(f.cleared, id)
	return nil
}
func (f *fakeRepo) ListMetaHeuristics(context.Context) ([]domain.MetaHeuristic, error) {
	return f.metaHeuristics, f.metaHeuristicsErr
}

func stringProp(v any) string {
	s, _ := v.(string)
	return s
}

// fakeSandbox answers each measurement from a per-segment script keyed by the
// candidate's filter columns, so a test states what each conjunction measures
// rather than counting call indexes. It records every request, letting a test
// assert exactly which candidates reached the sandbox.
type fakeSandbox struct {
	schema         sandboxclient.Schema
	introspectErr  error
	baseline       float64
	baselineErr    error
	measurements   map[string]sandboxMeasurement
	defaultValue   float64
	defaultSupport int64
	failAll        bool
	// nonNumeric marks segments whose response carries a null objective, so the
	// value cannot be read out — the shape an empty-filtered aggregate returns.
	nonNumeric map[string]bool
	// omitRowCount drops the reserved count key even though it was requested,
	// simulating a sandbox that did not answer the question asked.
	omitRowCount bool

	introspectCalls int
	execCalls       int
	measured        []string
}

type sandboxMeasurement struct {
	value   float64
	support int64
	err     error
}

func newFakeSandbox() *fakeSandbox {
	return &fakeSandbox{
		schema:         sandboxclient.Schema{Columns: []sandboxclient.Column{{Name: "revenue", Type: "DOUBLE"}}},
		measurements:   map[string]sandboxMeasurement{},
		defaultSupport: 100,
	}
}

func (f *fakeSandbox) Introspect(context.Context, sandboxclient.IntrospectRequest) (sandboxclient.IntrospectResponse, error) {
	f.introspectCalls++
	return sandboxclient.IntrospectResponse{Schema: f.schema}, f.introspectErr
}

func (f *fakeSandbox) Execute(_ context.Context, req sandboxclient.ExecuteRequest) (sandboxclient.ExecuteResponse, error) {
	key := segmentKey(req.Filters)
	f.execCalls++
	f.measured = append(f.measured, key)
	if key == "baseline" {
		if f.baselineErr != nil {
			return sandboxclient.ExecuteResponse{}, f.baselineErr
		}
		return f.response(req, f.baseline, f.defaultSupport), nil
	}
	if f.failAll {
		return sandboxclient.ExecuteResponse{}, errors.New("sandbox down")
	}
	m, ok := f.measurements[key]
	if !ok {
		return f.response(req, f.defaultValue, f.defaultSupport), nil
	}
	if m.err != nil {
		return sandboxclient.ExecuteResponse{}, m.err
	}
	return f.response(req, m.value, m.support), nil
}

func (f *fakeSandbox) response(req sandboxclient.ExecuteRequest, value float64, support int64) sandboxclient.ExecuteResponse {
	out := map[string]any{req.ObjectiveLabel: value}
	if f.nonNumeric[segmentKey(req.Filters)] {
		out[req.ObjectiveLabel] = nil
	}
	if req.IncludeRowCount && !f.omitRowCount {
		out[sandboxclient.RowCountKey] = float64(support)
	}
	return sandboxclient.ExecuteResponse{Value: out}
}

// segmentsMeasured returns the non-baseline candidates that reached the sandbox.
func (f *fakeSandbox) segmentsMeasured() []string {
	var out []string
	for _, k := range f.measured {
		if k != "baseline" {
			out = append(out, k)
		}
	}
	return out
}

func (f *fakeSandbox) didMeasure(key string) bool {
	for _, k := range f.measured {
		if k == key {
			return true
		}
	}
	return false
}

// fakeClaude scripts abstraction responses per call. Per the repo's caveat about
// scripted doubles, it returns a defaulted clean definition once the script is
// exhausted -- so a bounded-repair test must script one response per attempt
// plus the initial call.
type fakeClaude struct {
	definitions []string
	// alwaysLeak, when set, is returned by every call regardless of depth, so a
	// per-segment test does not have to reason about how far a shared script has
	// advanced across segments.
	alwaysLeak    string
	abstractErr   error
	repairErr     error
	abstractCalls int
	repairCalls   int
}

func (f *fakeClaude) next() string {
	if f.alwaysLeak != "" {
		return f.alwaysLeak
	}
	i := f.abstractCalls + f.repairCalls - 1
	if i < len(f.definitions) {
		return f.definitions[i]
	}
	return "[Primary Population Center] raises [System Output]"
}

func (f *fakeClaude) AbstractMetaHeuristic(context.Context, string, llm.MacroSegment) (llm.Abstraction, error) {
	f.abstractCalls++
	if f.abstractErr != nil {
		return llm.Abstraction{}, f.abstractErr
	}
	return llm.Abstraction{Definition: f.next()}, nil
}

func (f *fakeClaude) RepairMetaHeuristic(context.Context, string, llm.MacroSegment, llm.Abstraction, string) (llm.Abstraction, error) {
	f.repairCalls++
	if f.repairErr != nil {
		return llm.Abstraction{}, f.repairErr
	}
	return llm.Abstraction{Definition: f.next()}, nil
}

type fakeProvider struct {
	err   error
	calls int
}

func (f *fakeProvider) EmbedQuery(context.Context, string) ([]float32, error) {
	return nil, errors.New("the write path must use EmbedDocument")
}
func (f *fakeProvider) EmbedDocument(context.Context, string) ([]float32, error) {
	f.calls++
	return []float32{0.1, 0.2}, f.err
}
func (f *fakeProvider) Dimensions() int { return 2 }

type fakeEmbeddings struct {
	err          error
	upserted     []string
	upsertGoal   map[string]string
	refs         []store.NodeRef
	refsErr      error
	setGoalErr   error
	goalRepaired map[string]string
}

func (f *fakeEmbeddings) Upsert(_ context.Context, nodeID, goalID string, _ []float32) error {
	if f.err != nil {
		return f.err
	}
	if f.upsertGoal == nil {
		f.upsertGoal = map[string]string{}
	}
	f.upserted = append(f.upserted, nodeID)
	f.upsertGoal[nodeID] = goalID
	return nil
}

func (f *fakeEmbeddings) ListNodeRefs(context.Context) ([]store.NodeRef, error) {
	return f.refs, f.refsErr
}

func (f *fakeEmbeddings) SetGoalID(_ context.Context, nodeID, goalID string) error {
	if f.setGoalErr != nil {
		return f.setGoalErr
	}
	if f.goalRepaired == nil {
		f.goalRepaired = map[string]string{}
	}
	f.goalRepaired[nodeID] = goalID
	return nil
}

type fakeGoals struct {
	goal store.Goal
	err  error
}

func (f *fakeGoals) Get(context.Context, string) (store.Goal, error) { return f.goal, f.err }

type auditRecord struct {
	action    string
	eventType string
	detail    map[string]any
}

type fakeAudits struct {
	records []auditRecord
	err     error
}

// Append honors its context so a test can tell a detached audit write from one
// made on the run's own context. A real sink would fail the same way, and the
// terminal audit exists precisely to survive a run whose context is already done.
func (f *fakeAudits) Append(ctx context.Context, action, eventType string, detail map[string]any) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	f.records = append(f.records, auditRecord{action: action, eventType: eventType, detail: detail})
	return f.err
}

func (f *fakeAudits) count(action string) int {
	n := 0
	for _, r := range f.records {
		if r.action == action {
			n++
		}
	}
	return n
}

func (f *fakeAudits) find(action string) (auditRecord, bool) {
	for _, r := range f.records {
		if r.action == action {
			return r, true
		}
	}
	return auditRecord{}, false
}

// tabularGoal is the standard maximize-avg(revenue) goal the worker tests run.
func tabularGoal() store.Goal {
	return store.Goal{
		OptimizationFunctionID: "g1",
		GoalText:               "grow revenue",
		DataSourceRef:          "ref.csv",
		EvaluationMatrix: domain.EvaluationMatrix{Targets: []domain.Target{
			{Field: "revenue", Direction: domain.Maximize, Aggregation: "avg"},
		}},
	}
}

// minimizeGoal is tabularGoal with the direction flipped, so a test can exercise
// the Minimize ranking and gating paths.
func minimizeGoal() store.Goal {
	g := tabularGoal()
	g.EvaluationMatrix.Targets[0].Direction = domain.Minimize
	return g
}

const testObjectiveLabel = "avg(revenue)"

// objectiveFor is the pinned objective the constructor-level tests measure
// against. It is derived from the goal fixture rather than hand-built so the
// aggregation and value expression are the real ones — a test that drives
// write-back directly persists those onto its nodes, and a hand-built objective
// would write a shape no production run produces.
func objectiveFor(t *testing.T, direction domain.TargetDirection) objective.Objective {
	t.Helper()
	goal := tabularGoal()
	if direction == domain.Minimize {
		goal = minimizeGoal()
	}
	obj, err := objective.Pin(goal.EvaluationMatrix)
	if err != nil {
		t.Fatalf("pin objective: %v", err)
	}
	return obj
}

// harness wires a Worker over fakes, returning both so a test can script inputs
// and assert on what each collaborator saw.
type harness struct {
	worker     *Worker
	repo       *fakeRepo
	sandbox    *fakeSandbox
	claude     *fakeClaude
	provider   *fakeProvider
	embeddings *fakeEmbeddings
	goals      *fakeGoals
	audits     *fakeAudits
}

func newHarness(t *testing.T, cfg Config) *harness {
	t.Helper()
	h := &harness{
		repo:       newFakeRepo(),
		sandbox:    newFakeSandbox(),
		claude:     &fakeClaude{},
		provider:   &fakeProvider{},
		embeddings: &fakeEmbeddings{},
		goals:      &fakeGoals{goal: tabularGoal()},
		audits:     &fakeAudits{},
	}
	worker, err := NewWorker(h.repo, h.sandbox, h.claude, h.provider, h.embeddings, h.goals, h.audits, cfg)
	if err != nil {
		t.Fatalf("new worker: %v", err)
	}
	h.worker = worker
	return h
}

func (h *harness) run(t *testing.T) error {
	t.Helper()
	return h.worker.Run(context.Background(), "g1")
}

// didAbstract reports whether the run published the segment named by these test
// atoms, addressing it by the same deterministic id the abstraction stage mints.
// A test about one segment needs this rather than a Claude-call count: publication
// draws on Phase-1 findings too, so other segments legitimately reach abstraction
// in the same run.
func (h *harness) didAbstract(t *testing.T, fields ...string) bool {
	t.Helper()
	id := derivedID(goalNamespace("g1"), roleMetaHeuristic, canonicalFor(t, fields...))
	for _, mh := range h.repo.heuristics {
		if mh.ID == id {
			return true
		}
	}
	return false
}
