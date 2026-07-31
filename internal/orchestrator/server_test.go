package orchestrator

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/arborette/arborette/internal/domain"
	"github.com/arborette/arborette/internal/graph"
	"github.com/arborette/arborette/internal/heuristics"
	"github.com/arborette/arborette/internal/llm"
	"github.com/arborette/arborette/internal/store"
)

type fakeGoals struct {
	inserted      *store.Goal
	registeredRef string
	registerErr   error
	get           store.Goal
	getErr        error
	insertErr     error
	list          []store.Goal
	listErr       error
}

func (f *fakeGoals) Insert(_ context.Context, g store.Goal) error {
	if f.insertErr != nil {
		return f.insertErr
	}
	f.inserted = &g
	return nil
}
func (f *fakeGoals) RegisterDataSourceRef(_ context.Context, ref string) error {
	if f.registerErr != nil {
		return f.registerErr
	}
	f.registeredRef = ref
	return nil
}
func (f *fakeGoals) Get(_ context.Context, _ string) (store.Goal, error) {
	return f.get, f.getErr
}
func (f *fakeGoals) List(_ context.Context) ([]store.Goal, error) {
	return f.list, f.listErr
}

// setStatusCall records one SetStatus invocation, including the caller's context
// error at call time so a test can assert the terminal write ran on a live
// (detached) context rather than the loop's cancelled one.
type setStatusCall struct {
	runID  string
	status store.RunStatus
	reason string
	ctxErr error
}

type fakeRuns struct {
	created     []string
	createErr   error
	statusCalls []setStatusCall
	latest      map[string]store.Run
	latestErr   error
}

func (f *fakeRuns) Create(_ context.Context, runID, _ string) error {
	if f.createErr != nil {
		return f.createErr
	}
	f.created = append(f.created, runID)
	return nil
}
func (f *fakeRuns) SetStatus(ctx context.Context, runID string, status store.RunStatus, reason string) error {
	f.statusCalls = append(f.statusCalls, setStatusCall{runID: runID, status: status, reason: reason, ctxErr: ctx.Err()})
	return nil
}
func (f *fakeRuns) LatestByGoal(_ context.Context, _ []string) (map[string]store.Run, error) {
	return f.latest, f.latestErr
}

// fakeQueue is an in-memory verification queue keyed by outcome, reproducing the
// store's guarded-claim semantics (a claim on an already-resolved entry loses)
// so handler tests exercise the real ownership and repair branches. Hooks let a
// test force a failure or observe compensation.
type fakeQueue struct {
	mu                sync.Mutex
	entries           map[string]store.VerificationEntry
	enqueued          []store.VerificationEntry
	unclaimed         []string
	enqueueErr        error
	getErr            error
	resolveErr        error
	getByOutcomeCalls int
}

func newFakeQueue(entries ...store.VerificationEntry) *fakeQueue {
	q := &fakeQueue{entries: map[string]store.VerificationEntry{}}
	for _, e := range entries {
		q.entries[e.OutcomeID] = e
	}
	return q
}

func (q *fakeQueue) Enqueue(_ context.Context, entry store.VerificationEntry) error {
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.enqueueErr != nil {
		return q.enqueueErr
	}
	entry.Status = store.QueuePending
	q.entries[entry.OutcomeID] = entry
	q.enqueued = append(q.enqueued, entry)
	return nil
}

func (q *fakeQueue) EnqueueResolved(_ context.Context, entry store.VerificationEntry, resolution store.QueueResolution, correctedValue string) error {
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.enqueueErr != nil {
		return q.enqueueErr
	}
	if _, exists := q.entries[entry.OutcomeID]; exists {
		return store.ErrAlreadyResolved
	}
	entry.Status = store.QueueResolved
	entry.Resolution = resolution
	entry.CorrectedValue = correctedValue
	q.entries[entry.OutcomeID] = entry
	q.enqueued = append(q.enqueued, entry)
	return nil
}

func (q *fakeQueue) Resolve(_ context.Context, outcomeID string, resolution store.QueueResolution, correctedValue string) error {
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.resolveErr != nil {
		return q.resolveErr
	}
	entry, ok := q.entries[outcomeID]
	if !ok || entry.Status != store.QueuePending {
		return store.ErrAlreadyResolved
	}
	entry.Status = store.QueueResolved
	entry.Resolution = resolution
	entry.CorrectedValue = correctedValue
	q.entries[outcomeID] = entry
	return nil
}

func (q *fakeQueue) Unclaim(ctx context.Context, outcomeID string, resolution store.QueueResolution) error {
	q.mu.Lock()
	defer q.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return err
	}
	entry, ok := q.entries[outcomeID]
	if !ok || entry.Status != store.QueueResolved || entry.Resolution != resolution {
		return nil
	}
	entry.Status = store.QueuePending
	entry.Resolution = ""
	entry.CorrectedValue = ""
	q.entries[outcomeID] = entry
	q.unclaimed = append(q.unclaimed, outcomeID)
	return nil
}

func (q *fakeQueue) GetByOutcome(_ context.Context, outcomeID string) (store.VerificationEntry, error) {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.getByOutcomeCalls++
	if q.getErr != nil {
		return store.VerificationEntry{}, q.getErr
	}
	entry, ok := q.entries[outcomeID]
	if !ok {
		return store.VerificationEntry{}, pgx.ErrNoRows
	}
	return entry, nil
}

// entry and pendingOutcomes read queue state under the lock, so a test can
// assert on (or act on) it while the loop polls from its own goroutine.
func (q *fakeQueue) entry(outcomeID string) store.VerificationEntry {
	q.mu.Lock()
	defer q.mu.Unlock()
	return q.entries[outcomeID]
}

func (q *fakeQueue) pendingOutcomes() []string {
	q.mu.Lock()
	defer q.mu.Unlock()
	var ids []string
	for id, entry := range q.entries {
		if entry.Status == store.QueuePending {
			ids = append(ids, id)
		}
	}
	return ids
}

func (q *fakeQueue) ListForGoal(_ context.Context, goalID string, status store.QueueStatus) ([]store.VerificationEntry, error) {
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.getErr != nil {
		return nil, q.getErr
	}
	var out []store.VerificationEntry
	for _, e := range q.entries {
		if e.OptimizationFunctionID != goalID {
			continue
		}
		if status != "" && e.Status != status {
			continue
		}
		out = append(out, e)
	}
	return out, nil
}

type fakeAudits struct{ records []store.AuditRecord }

func (f *fakeAudits) Append(ctx context.Context, r store.AuditRecord) error {
	// A real pool fails a write on a cancelled context, so this does too --
	// otherwise a handler that passes the wrong context looks correct here.
	if err := ctx.Err(); err != nil {
		return err
	}
	f.records = append(f.records, r)
	return nil
}

type fakeObjects struct {
	puts    int
	putErr  error
	getData []byte
	getErr  error
}

func (fakeObjects) NewKey(parts ...string) string { return strings.Join(parts, "/") }
func (f *fakeObjects) Put(_ context.Context, _ string, r io.Reader, _ string) error {
	io.Copy(io.Discard, r)
	if f.putErr != nil {
		return f.putErr
	}
	f.puts++
	return nil
}
func (f *fakeObjects) Get(_ context.Context, _ string) (io.ReadCloser, error) {
	if f.getErr != nil {
		return nil, f.getErr
	}
	return io.NopCloser(bytes.NewReader(f.getData)), nil
}

type fakeHeur struct {
	matches  []heuristics.Match
	triplets []graph.CausalTriplet
	queryErr error
	traceErr error
	gotK     int
}

func (f *fakeHeur) Query(_ context.Context, _ string, k int) ([]heuristics.Match, error) {
	f.gotK = k
	return f.matches, f.queryErr
}
func (f *fakeHeur) Trace(_ context.Context, _ string) ([]graph.CausalTriplet, error) {
	return f.triplets, f.traceErr
}

type fakeClaude struct {
	matrix            domain.EvaluationMatrix
	matrixErr         error
	repair            domain.EvaluationMatrix
	repairErr         error
	repairCalls       int
	proposal          llm.Proposal
	proposalErr       error
	childProposal     *llm.Proposal
	gotSchema         llm.SandboxSchema
	gotNodes          []llm.TreeContext
	treeRepair        llm.Proposal
	treeRepairErr     error
	treeRepairCalls   int
	treeRepairPrior   []llm.Proposal
	treeRepairErrMsg  []string
	fields            []domain.TargetField
	fieldsErr         error
	gotSample         string
	extractValue      string
	extractConfidence float64
	extractErr        error
	extractCalls      int
	gotMethods        []string
}

func (f *fakeClaude) GenerateEvaluationMatrix(_ context.Context, _ string, schema llm.SandboxSchema) (domain.EvaluationMatrix, error) {
	f.gotSchema = schema
	return f.matrix, f.matrixErr
}
func (f *fakeClaude) RepairEvaluationMatrix(_ context.Context, _ string, _ llm.SandboxSchema, _ domain.EvaluationMatrix, _ string) (domain.EvaluationMatrix, error) {
	f.repairCalls++
	return f.repair, f.repairErr
}
func (f *fakeClaude) ProposeInterventionTree(_ context.Context, _ string, _ domain.EvaluationMatrix, _ llm.SandboxSchema, node llm.TreeContext) (llm.Proposal, error) {
	f.gotNodes = append(f.gotNodes, node)
	if f.proposalErr != nil {
		return llm.Proposal{}, f.proposalErr
	}
	if !node.IsRoot && f.childProposal != nil {
		return *f.childProposal, nil
	}
	return f.proposal, nil
}
func (f *fakeClaude) RepairInterventionTree(_ context.Context, _ string, _ domain.EvaluationMatrix, _ llm.SandboxSchema, _ llm.TreeContext, prior llm.Proposal, validationErr string) (llm.Proposal, error) {
	f.treeRepairCalls++
	f.treeRepairPrior = append(f.treeRepairPrior, prior)
	f.treeRepairErrMsg = append(f.treeRepairErrMsg, validationErr)
	return f.treeRepair, f.treeRepairErr
}
func (f *fakeClaude) IntrospectDocumentFields(_ context.Context, _, sample string) ([]domain.TargetField, error) {
	f.gotSample = sample
	return f.fields, f.fieldsErr
}
func (f *fakeClaude) Extract(_ context.Context, _ []byte, _ domain.TargetField, method string) (string, float64, error) {
	f.extractCalls++
	f.gotMethods = append(f.gotMethods, method)
	if f.extractErr != nil {
		return "", 0, f.extractErr
	}
	return f.extractValue, f.extractConfidence, nil
}

// verificationCall records one applied resolution so a handler test can assert
// what reached the graph.
type verificationCall struct {
	outcomeID  string
	status     domain.VerificationStatus
	confidence float64
}

// valueCall records the value half of one correction written back to an outcome.
type valueCall struct {
	outcomeID  string
	value      map[string]any
	provenance *domain.ProvenanceLocator
}

// fakeRepo is a no-op graphRepo that records the nodes writeTriplet
// persists, so a loop test can assert the objective-label keying end to end.
// The extraction-outcome fields additionally back the verification handlers:
// extraction is returned by both outcome reads, and the update hooks record what
// a resolution wrote or fail it on demand.
type fakeRepo struct {
	states        []domain.State
	interventions []domain.Intervention
	outcomes      []domain.Outcome
	produced      []domain.ProducedEdge

	mu             sync.Mutex
	statuses       map[string]domain.VerificationStatus
	extractionGets int
	// extractionReadErrs fails that many reads before succeeding, standing in for
	// a transient graph fault during a long blocking wait.
	extractionReadErrs int
	// onVerify fires after a successful verification write, so a test can make
	// the client disconnect at exactly that moment.
	onVerify       func()
	extraction     graph.ExtractionOutcome
	extractionErr  error
	extractionList []graph.ExtractionOutcome
	verifications  []verificationCall
	verifyErr      error
	values         []valueCall
	valueErr       error
}

func (f *fakeRepo) CreateState(_ context.Context, s domain.State) error {
	f.states = append(f.states, s)
	return nil
}
func (f *fakeRepo) CreateIntervention(_ context.Context, i domain.Intervention) error {
	f.interventions = append(f.interventions, i)
	return nil
}
func (f *fakeRepo) CreateOutcome(_ context.Context, o domain.Outcome) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.outcomes = append(f.outcomes, o)
	if f.statuses == nil {
		f.statuses = map[string]domain.VerificationStatus{}
	}
	f.statuses[o.ID] = o.VerificationStatus
	return nil
}
func (f *fakeRepo) CreatePreConditionFor(_ context.Context, _, _ string) error { return nil }
func (f *fakeRepo) CreateProduced(_ context.Context, _, _ string, edge domain.ProducedEdge) error {
	f.produced = append(f.produced, edge)
	return nil
}
func (f *fakeRepo) UpdateOutcomeVerification(_ context.Context, outcomeID string, status domain.VerificationStatus, confidence float64) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.onVerify != nil {
		f.onVerify()
	}
	if f.verifyErr != nil {
		return f.verifyErr
	}
	f.verifications = append(f.verifications, verificationCall{outcomeID: outcomeID, status: status, confidence: confidence})
	// The recorded write is what a later read must observe, so the repair path,
	// the compensation check, and the loop's blocking gate all see the same state
	// a real graph would return.
	f.extraction.VerificationStatus = status
	f.setStatusLocked(outcomeID, status)
	return nil
}

// setStatusLocked records an outcome's status for reads by id. Callers hold f.mu.
func (f *fakeRepo) setStatusLocked(outcomeID string, status domain.VerificationStatus) {
	if f.statuses == nil {
		f.statuses = map[string]domain.VerificationStatus{}
	}
	f.statuses[outcomeID] = status
}
func (f *fakeRepo) CorrectOutcome(_ context.Context, outcomeID string, value map[string]any, provenance *domain.ProvenanceLocator, status domain.VerificationStatus, confidence float64) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.valueErr != nil {
		return f.valueErr
	}
	f.setStatusLocked(outcomeID, status)
	f.values = append(f.values, valueCall{outcomeID: outcomeID, value: value, provenance: provenance})
	f.verifications = append(f.verifications, verificationCall{outcomeID: outcomeID, status: status, confidence: confidence})
	f.extraction.VerificationStatus = status
	f.extraction.Value = value
	f.extraction.Provenance = provenance
	return nil
}

// polls reports how many times the blocking gate read an outcome's status.
func (f *fakeRepo) polls() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.extractionGets
}

func (f *fakeRepo) ListExtractionOutcomes(_ context.Context, _ string) ([]graph.ExtractionOutcome, error) {
	return f.extractionList, f.extractionErr
}
func (f *fakeRepo) GetExtractionOutcome(ctx context.Context, outcomeID string) (graph.ExtractionOutcome, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.extractionGets++
	if err := ctx.Err(); err != nil {
		return graph.ExtractionOutcome{}, err
	}
	if f.extractionReadErrs > 0 {
		f.extractionReadErrs--
		return graph.ExtractionOutcome{}, errors.New("neo4j unreachable")
	}
	if f.extractionErr != nil {
		return graph.ExtractionOutcome{}, f.extractionErr
	}
	// An outcome this fake actually persisted reads back with its own status, so
	// the loop's blocking gate observes verdicts the way it would against a real
	// graph; handler tests that seed only `extraction` keep using it.
	if status, ok := f.statuses[outcomeID]; ok {
		out := f.extraction
		out.OutcomeID = outcomeID
		out.VerificationStatus = status
		return out, nil
	}
	return f.extraction, nil
}

// fakeSandbox scripts per-call Execute results so the intake dry-run and the loop
// can be driven through their success and failure branches. Execute returns the
// response/error at the current call index, defaulting to an empty 200 once the
// script is exhausted.
type fakeSandbox struct {
	introspect      IntrospectResponse
	introspectErr   error
	introspectCalls int
	execResps       []ExecuteResponse
	execErrs        []error
	execCalls       int
	execReqs        []ExecuteRequest
	docPages        []string
	docTextErr      error
}

func (f *fakeSandbox) Introspect(_ context.Context, _ IntrospectRequest) (IntrospectResponse, error) {
	f.introspectCalls++
	return f.introspect, f.introspectErr
}
func (f *fakeSandbox) DocumentText(_ context.Context, _ DocumentTextRequest) (DocumentTextResponse, error) {
	return DocumentTextResponse{Pages: f.docPages}, f.docTextErr
}
func (f *fakeSandbox) Execute(_ context.Context, req ExecuteRequest) (ExecuteResponse, error) {
	f.execReqs = append(f.execReqs, req)
	i := f.execCalls
	f.execCalls++
	var resp ExecuteResponse
	if i < len(f.execResps) {
		resp = f.execResps[i]
	}
	var err error
	if i < len(f.execErrs) {
		err = f.execErrs[i]
	}
	return resp, err
}

type fakeLauncher struct {
	jobName string
	args    map[string]string
	err     error
}

func (f *fakeLauncher) Launch(_ context.Context, jobName string, args map[string]string) error {
	f.jobName = jobName
	f.args = args
	return f.err
}

// fakeChat stands in for the streaming Claude, recording what the handler passed.
type fakeChat struct {
	events []llm.ChatEvent
	err    error
	ctx    context.Context
	system string
	msgs   []llm.ChatMessage
}

func (f *fakeChat) Chat(ctx context.Context, system string, msgs []llm.ChatMessage, emit func(llm.ChatEvent) error) error {
	f.ctx, f.system, f.msgs = ctx, system, msgs
	for _, ev := range f.events {
		if err := emit(ev); err != nil {
			return err
		}
	}
	return f.err
}

// testHITLThreshold is the review threshold every test server runs with, fixed
// here so a test states confidences relative to a known bar.
const testHITLThreshold = 0.8

// testServer is the one place in the package that spells NewServer's argument
// list. Every helper below fills in the fields it varies and leaves the rest at
// their zero value, so a new constructor parameter is a single edit here rather
// than one per construction site -- and no test can silently pass a positional
// argument into the wrong slot of a long positional call.
type testServer struct {
	repo              graphRepo
	queue             verificationQueue
	goals             goalStore
	audits            auditStore
	objects           objectStore
	heur              heuristicsService
	claude            claudeClient
	sandbox           sandboxExecutor
	localImportDir    string
	internalAuthToken string
}

// build wires the server, defaulting every collaborator the caller left unset and
// pacing the human-scale timers down. Callers that want the production pacing
// override the two fields afterward.
func (ts testServer) build() *Server {
	orElse := func(set, fallback any) any {
		if set == nil {
			return fallback
		}
		return set
	}
	srv := NewServer(
		ts.repo,
		orElse(ts.goals, &fakeGoals{}).(goalStore),
		&fakeRuns{},
		orElse(ts.queue, newFakeQueue()).(verificationQueue),
		orElse(ts.audits, &fakeAudits{}).(auditStore),
		orElse(ts.objects, &fakeObjects{}).(objectStore),
		orElse(ts.heur, &fakeHeur{}).(heuristicsService),
		orElse(ts.claude, &fakeClaude{}).(claudeClient),
		&fakeChat{},
		orElse(ts.sandbox, &fakeSandbox{}).(sandboxExecutor),
		NewHub(), StubLauncher{}, StubIdentity{ID: "analyst-test"},
		ts.localImportDir, "arborette-sleepcycle", ts.internalAuthToken,
		testHITLThreshold, time.Minute,
	)
	// The blocking gate and the stream keepalive are both paced for humans; a
	// test asserting that they fire at all should not pay for that.
	srv.verificationPoll = time.Millisecond
	srv.keepaliveInterval = time.Millisecond
	return srv
}

func newTestServer(goals goalStore, audits auditStore, objects objectStore, heur heuristicsService, claude claudeClient, sandbox sandboxExecutor) *Server {
	return newTestServerRepo(nil, goals, audits, objects, heur, claude, sandbox)
}

// newTestServerRepo is newTestServer with an explicit graphRepo, for loop
// tests that assert the nodes writeTriplet persists.
func newTestServerRepo(repo graphRepo, goals goalStore, audits auditStore, objects objectStore, heur heuristicsService, claude claudeClient, sandbox sandboxExecutor) *Server {
	return newTestServerQueue(repo, newFakeQueue(), goals, audits, objects, heur, claude, sandbox)
}

// newTestServerQueue is newTestServerRepo with an explicit verification queue,
// for the review handlers and the loop's routing and blocking-gate paths.
func newTestServerQueue(repo graphRepo, queue verificationQueue, goals goalStore, audits auditStore, objects objectStore, heur heuristicsService, claude claudeClient, sandbox sandboxExecutor) *Server {
	return testServer{
		repo: repo, queue: queue, goals: goals, audits: audits,
		objects: objects, heur: heur, claude: claude, sandbox: sandbox,
	}.build()
}

// newTestServerAuth carries the token through NewServer rather than assigning it
// onto the struct: the constructor is what arms the guard, and a token dropped
// there leaves Routes wiring BearerAuth(""), which serves the handler unwrapped.
func newTestServerAuth(internalAuthToken string, audits auditStore) *Server {
	return testServer{audits: audits, internalAuthToken: internalAuthToken}.build()
}

// newTestServerImport varies the read-only import mount, for the ingestion paths.
func newTestServerImport(localImportDir string, objects objectStore) *Server {
	return testServer{localImportDir: localImportDir, objects: objects}.build()
}

// multipartBody builds a multipart/form-data body with the given fields and an
// optional file part.
func multipartBody(t *testing.T, fields map[string]string, fileField, fileName, content string) (*bytes.Buffer, string) {
	t.Helper()
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	for k, v := range fields {
		if err := mw.WriteField(k, v); err != nil {
			t.Fatalf("write field: %v", err)
		}
	}
	if fileField != "" {
		fw, err := mw.CreateFormFile(fileField, fileName)
		if err != nil {
			t.Fatalf("create form file: %v", err)
		}
		fw.Write([]byte(content))
	}
	if err := mw.Close(); err != nil {
		t.Fatalf("close multipart: %v", err)
	}
	return &buf, mw.FormDataContentType()
}

func TestSubmitGoalValidation(t *testing.T) {
	srv := newTestServer(&fakeGoals{}, &fakeAudits{}, &fakeObjects{}, &fakeHeur{}, &fakeClaude{}, &fakeSandbox{})

	cases := []struct {
		name       string
		fields     map[string]string
		file       bool
		wantStatus int
	}{
		{"missing goal", map[string]string{}, true, http.StatusBadRequest},
		{"missing data source", map[string]string{"goal": "grow revenue"}, false, http.StatusBadRequest},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			fileField := ""
			if c.file {
				fileField = "file"
			}
			body, contentType := multipartBody(t, c.fields, fileField, "data.csv", "a,b\n1,2\n")
			req := httptest.NewRequest(http.MethodPost, "/goals", body)
			req.Header.Set("Content-Type", contentType)
			rec := httptest.NewRecorder()
			srv.Routes().ServeHTTP(rec, req)
			if rec.Code != c.wantStatus {
				t.Fatalf("status = %d, want %d (body %q)", rec.Code, c.wantStatus, rec.Body.String())
			}
		})
	}
}

func TestSubmitGoalSuccess(t *testing.T) {
	goals := &fakeGoals{}
	audits := &fakeAudits{}
	// A fitted matrix carries an aggregation; the objective degenerates to a bare
	// ColumnRef over the field, so the dry-run pins and measures avg(revenue).
	claude := &fakeClaude{matrix: fittedMatrix()}
	sandbox := &fakeSandbox{
		introspect: revenueSchema(),
		execResps:  []ExecuteResponse{{Value: map[string]any{"avg(revenue)": 10.0}}},
	}
	srv := newTestServer(goals, audits, &fakeObjects{}, &fakeHeur{}, claude, sandbox)

	body, contentType := multipartBody(t, map[string]string{"goal": "grow revenue"}, "file", "data.csv", "revenue\n10\n")
	req := httptest.NewRequest(http.MethodPost, "/goals", body)
	req.Header.Set("Content-Type", contentType)
	rec := httptest.NewRecorder()
	srv.Routes().ServeHTTP(rec, req)

	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201 (body %q)", rec.Code, rec.Body.String())
	}
	var resp struct {
		OptimizationFunctionID string `json:"optimization_function_id"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if resp.OptimizationFunctionID == "" {
		t.Fatalf("expected an optimization_function_id in the response")
	}
	if goals.inserted == nil || goals.inserted.OptimizationFunctionID != resp.OptimizationFunctionID {
		t.Fatalf("goal not persisted with the returned id: %+v", goals.inserted)
	}
	if len(audits.records) != 1 || audits.records[0].Actor != "analyst-test" {
		t.Fatalf("expected one audit record stamped with the stub identity: %+v", audits.records)
	}
	// The introspected schema is fitted to the objective, not the goal text alone.
	if len(claude.gotSchema.Columns) != 1 || claude.gotSchema.Columns[0].Name != "revenue" {
		t.Fatalf("matrix generation was not given the introspected schema: %+v", claude.gotSchema)
	}
	if claude.repairCalls != 0 {
		t.Fatalf("a valid objective must not trigger repair, got %d calls", claude.repairCalls)
	}
}

func TestTriggerEndpointsUnknownGoal(t *testing.T) {
	goals := &fakeGoals{getErr: pgx.ErrNoRows}
	srv := newTestServer(goals, &fakeAudits{}, &fakeObjects{}, &fakeHeur{}, &fakeClaude{}, &fakeSandbox{})

	for _, path := range []string{"/goals/unknown/hypothesis-loop", "/goals/unknown/sleep-cycle"} {
		req := httptest.NewRequest(http.MethodPost, path, nil)
		rec := httptest.NewRecorder()
		srv.Routes().ServeHTTP(rec, req)
		if rec.Code != http.StatusNotFound {
			t.Fatalf("%s status = %d, want 404", path, rec.Code)
		}
	}
}

func TestListGoals(t *testing.T) {
	t.Run("joins goals to their latest run status, synthesizing no run", func(t *testing.T) {
		failReason := "field not present in schema: X"
		goals := &fakeGoals{list: []store.Goal{
			{OptimizationFunctionID: "g1", GoalText: "grow revenue"},
			{OptimizationFunctionID: "g2", GoalText: "cut cost"},
		}}
		srv := newTestServer(goals, &fakeAudits{}, &fakeObjects{}, &fakeHeur{}, &fakeClaude{}, &fakeSandbox{})
		srv.runs = &fakeRuns{latest: map[string]store.Run{
			"g1": {Status: store.RunFailed, FailureReason: &failReason},
		}}

		req := httptest.NewRequest(http.MethodGet, "/goals", nil)
		rec := httptest.NewRecorder()
		srv.Routes().ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200 (body %q)", rec.Code, rec.Body.String())
		}
		var out []goalListItemDTO
		if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
			t.Fatalf("decode: %v", err)
		}
		if len(out) != 2 {
			t.Fatalf("expected 2 items, got %d", len(out))
		}
		if out[0].OptimizationFunctionID != "g1" || out[0].Status != "failed" || out[0].FailureReason != failReason {
			t.Fatalf("g1 should carry its failed status + reason: %+v", out[0])
		}
		if out[1].Status != "no run" || out[1].FailureReason != "" {
			t.Fatalf("g2 with no run should be synthesized: %+v", out[1])
		}
	})

	t.Run("empty registry is a non-nil array", func(t *testing.T) {
		srv := newTestServer(&fakeGoals{}, &fakeAudits{}, &fakeObjects{}, &fakeHeur{}, &fakeClaude{}, &fakeSandbox{})
		req := httptest.NewRequest(http.MethodGet, "/goals", nil)
		rec := httptest.NewRecorder()
		srv.Routes().ServeHTTP(rec, req)
		if body := strings.TrimSpace(rec.Body.String()); body != "[]" {
			t.Fatalf("empty list body = %q, want []", body)
		}
	})

	t.Run("a goal-list error is a 500", func(t *testing.T) {
		srv := newTestServer(&fakeGoals{listErr: errors.New("db down")}, &fakeAudits{}, &fakeObjects{}, &fakeHeur{}, &fakeClaude{}, &fakeSandbox{})
		req := httptest.NewRequest(http.MethodGet, "/goals", nil)
		rec := httptest.NewRecorder()
		srv.Routes().ServeHTTP(rec, req)
		if rec.Code != http.StatusInternalServerError {
			t.Fatalf("status = %d, want 500", rec.Code)
		}
	})

	t.Run("a latest-runs error is a 500", func(t *testing.T) {
		srv := newTestServer(&fakeGoals{list: []store.Goal{{OptimizationFunctionID: "g1"}}}, &fakeAudits{}, &fakeObjects{}, &fakeHeur{}, &fakeClaude{}, &fakeSandbox{})
		srv.runs = &fakeRuns{latestErr: errors.New("db down")}
		req := httptest.NewRequest(http.MethodGet, "/goals", nil)
		rec := httptest.NewRecorder()
		srv.Routes().ServeHTTP(rec, req)
		if rec.Code != http.StatusInternalServerError {
			t.Fatalf("status = %d, want 500", rec.Code)
		}
	})
}

func TestHeuristicSearch(t *testing.T) {
	heur := &fakeHeur{matches: []heuristics.Match{
		{MetaHeuristic: domain.MetaHeuristic{ID: "mh-1", Definition: "scale reads"}},
	}}
	srv := newTestServer(&fakeGoals{}, &fakeAudits{}, &fakeObjects{}, heur, &fakeClaude{}, &fakeSandbox{})

	req := httptest.NewRequest(http.MethodGet, "/heuristics/search?q=hot+shard&k=3", nil)
	rec := httptest.NewRecorder()
	srv.Routes().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %q)", rec.Code, rec.Body.String())
	}
	var out []heuristicMatchDTO
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(out) != 1 || out[0].ID != "mh-1" || out[0].Definition != "scale reads" {
		t.Fatalf("unexpected search result: %+v", out)
	}
}

func TestHeuristicSearchMissingQuery(t *testing.T) {
	srv := newTestServer(&fakeGoals{}, &fakeAudits{}, &fakeObjects{}, &fakeHeur{}, &fakeClaude{}, &fakeSandbox{})
	req := httptest.NewRequest(http.MethodGet, "/heuristics/search", nil)
	rec := httptest.NewRecorder()
	srv.Routes().ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
}

func TestHeuristicTrace(t *testing.T) {
	heur := &fakeHeur{triplets: []graph.CausalTriplet{{
		State:        domain.State{ID: "s-1", Properties: map[string]any{"value": 10.0}},
		Intervention: domain.Intervention{ID: "i-1", Type: domain.InterventionQuery, Properties: map[string]any{}},
		Outcome:      domain.Outcome{ID: "o-1", VerificationStatus: domain.VerificationVerified, Value: map[string]any{"revenue": 12.0}},
	}}}
	srv := newTestServer(&fakeGoals{}, &fakeAudits{}, &fakeObjects{}, heur, &fakeClaude{}, &fakeSandbox{})

	req := httptest.NewRequest(http.MethodGet, "/heuristics/mh-1/trace", nil)
	rec := httptest.NewRecorder()
	srv.Routes().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %q)", rec.Code, rec.Body.String())
	}
	var out []tripletDTO
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(out) != 1 || out[0].Intervention.Type != "query" || out[0].Outcome.VerificationStatus != "verified" {
		t.Fatalf("unexpected trace result: %+v", out)
	}
}

func TestIngestLocalReadsWithinMount(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "data.csv"), []byte("revenue\n10\n"), 0o600); err != nil {
		t.Fatalf("write file: %v", err)
	}
	objects := &fakeObjects{}
	srv := newTestServerImport(dir, objects)

	ref, err := srv.ingestLocal(context.Background(), "data.csv")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if ref == "" || objects.puts != 1 {
		t.Fatalf("expected the in-mount file to be copied into the object store (ref=%q puts=%d)", ref, objects.puts)
	}
}

func TestIngestLocalRejectsSymlinkEscape(t *testing.T) {
	mount := t.TempDir()
	outside := t.TempDir()
	if err := os.WriteFile(filepath.Join(outside, "secret.csv"), []byte("x\n"), 0o600); err != nil {
		t.Fatalf("write outside file: %v", err)
	}
	// A symlink inside the mount pointing outside it must not be readable.
	if err := os.Symlink(filepath.Join(outside, "secret.csv"), filepath.Join(mount, "link.csv")); err != nil {
		t.Fatalf("symlink: %v", err)
	}
	objects := &fakeObjects{}
	srv := newTestServerImport(mount, objects)

	if _, err := srv.ingestLocal(context.Background(), "link.csv"); !errors.Is(err, errPathEscape) {
		t.Fatalf("error = %v, want errPathEscape", err)
	}
	if objects.puts != 0 {
		t.Fatalf("an escaping path must not be copied into the object store")
	}
}

func TestHandleStreamReplaysBufferedEvents(t *testing.T) {
	goals := &fakeGoals{get: store.Goal{OptimizationFunctionID: "run-1"}}
	srv := newTestServer(goals, &fakeAudits{}, &fakeObjects{}, &fakeHeur{}, &fakeClaude{}, &fakeSandbox{})
	srv.hub.Publish("run-1", Event{Type: "triplet", Payload: map[string]any{"value": 5.0}})

	// An already-cancelled context: the replay is written before the loop checks
	// the context, so the buffered frame still lands, then the handler returns.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	req := httptest.NewRequestWithContext(ctx, http.MethodGet, "/goals/run-1/stream", nil)
	req.SetPathValue("id", "run-1")
	rec := httptest.NewRecorder()
	srv.handleStream(rec, req)

	body := rec.Body.String()
	if !strings.Contains(body, "data: ") || !strings.Contains(body, `"triplet"`) {
		t.Fatalf("expected a replayed SSE frame, got %q", body)
	}
	if ct := rec.Header().Get("Content-Type"); ct != "text/event-stream" {
		t.Fatalf("content-type = %q, want text/event-stream", ct)
	}
	if !rec.Flushed {
		t.Fatalf("events were not flushed as they were written")
	}
}
