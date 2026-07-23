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
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/arborette/arborette/internal/domain"
	"github.com/arborette/arborette/internal/graph"
	"github.com/arborette/arborette/internal/heuristics"
	"github.com/arborette/arborette/internal/llm"
	"github.com/arborette/arborette/internal/store"
)

type fakeGoals struct {
	inserted  *store.Goal
	get       store.Goal
	getErr    error
	insertErr error
	list      []store.Goal
	listErr   error
}

func (f *fakeGoals) Insert(_ context.Context, g store.Goal) error {
	if f.insertErr != nil {
		return f.insertErr
	}
	f.inserted = &g
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

type fakeAudits struct{ records []store.AuditRecord }

func (f *fakeAudits) Append(_ context.Context, r store.AuditRecord) error {
	f.records = append(f.records, r)
	return nil
}

type fakeObjects struct {
	puts   int
	putErr error
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
	matrix      domain.EvaluationMatrix
	matrixErr   error
	repair      domain.EvaluationMatrix
	repairErr   error
	repairCalls int
	proposal    llm.Proposal
	gotSchema   llm.SandboxSchema
	gotNodes    []llm.TreeContext
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
	return f.proposal, nil
}

// fakeRepo is a no-op graph.Repository that records the nodes writeTriplet
// persists, so a loop test can assert the objective-label keying end to end.
type fakeRepo struct {
	states   []domain.State
	outcomes []domain.Outcome
}

func (f *fakeRepo) CreateState(_ context.Context, s domain.State) error {
	f.states = append(f.states, s)
	return nil
}
func (f *fakeRepo) CreateIntervention(_ context.Context, _ domain.Intervention) error { return nil }
func (f *fakeRepo) CreateOutcome(_ context.Context, o domain.Outcome) error {
	f.outcomes = append(f.outcomes, o)
	return nil
}
func (f *fakeRepo) GetState(_ context.Context, _ string) (domain.State, error) {
	return domain.State{}, nil
}
func (f *fakeRepo) GetIntervention(_ context.Context, _ string) (domain.Intervention, error) {
	return domain.Intervention{}, nil
}
func (f *fakeRepo) GetOutcome(_ context.Context, _ string) (domain.Outcome, error) {
	return domain.Outcome{}, nil
}
func (f *fakeRepo) GetMetaHeuristic(_ context.Context, _ string) (domain.MetaHeuristic, error) {
	return domain.MetaHeuristic{}, nil
}
func (f *fakeRepo) CreatePreConditionFor(_ context.Context, _, _ string) error { return nil }
func (f *fakeRepo) CreateProduced(_ context.Context, _, _ string, _ domain.ProducedEdge) error {
	return nil
}
func (f *fakeRepo) CreateMetaHeuristic(_ context.Context, _ domain.MetaHeuristic, _ []string) error {
	return nil
}
func (f *fakeRepo) ClearEmbeddingPending(_ context.Context, _ string) error { return nil }
func (f *fakeRepo) ListEmbeddingPending(_ context.Context) ([]domain.MetaHeuristic, error) {
	return nil, nil
}
func (f *fakeRepo) UpdateOutcomeVerification(_ context.Context, _ string, _ domain.VerificationStatus, _ float64) error {
	return nil
}
func (f *fakeRepo) TraceCausalChain(_ context.Context, _ string) ([]graph.CausalTriplet, error) {
	return nil, nil
}

// fakeSandbox scripts per-call Execute results so the intake dry-run and the loop
// can be driven through their success and failure branches. Execute returns the
// response/error at the current call index, defaulting to an empty 200 once the
// script is exhausted.
type fakeSandbox struct {
	introspect    IntrospectResponse
	introspectErr error
	execResps     []ExecuteResponse
	execErrs      []error
	execCalls     int
}

func (f *fakeSandbox) Introspect(_ context.Context, _ IntrospectRequest) (IntrospectResponse, error) {
	return f.introspect, f.introspectErr
}
func (f *fakeSandbox) Execute(_ context.Context, _ ExecuteRequest) (ExecuteResponse, error) {
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

func newTestServer(goals goalStore, audits auditStore, objects objectStore, heur heuristicsService, claude claudeClient, sandbox sandboxExecutor) *Server {
	return newTestServerRepo(nil, goals, audits, objects, heur, claude, sandbox)
}

// newTestServerRepo is newTestServer with an explicit graph.Repository, for loop
// tests that assert the nodes writeTriplet persists.
func newTestServerRepo(repo graph.Repository, goals goalStore, audits auditStore, objects objectStore, heur heuristicsService, claude claudeClient, sandbox sandboxExecutor) *Server {
	return NewServer(repo, goals, &fakeRuns{}, audits, objects, heur, claude, sandbox,
		NewHub(), StubLauncher{}, StubIdentity{ID: "analyst-test"}, "", "arborette-sleepcycle")
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
	srv := NewServer(nil, &fakeGoals{}, &fakeRuns{}, &fakeAudits{}, objects, &fakeHeur{}, &fakeClaude{}, &fakeSandbox{},
		NewHub(), StubLauncher{}, StubIdentity{ID: "analyst-test"}, dir, "job")

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
	srv := NewServer(nil, &fakeGoals{}, &fakeRuns{}, &fakeAudits{}, objects, &fakeHeur{}, &fakeClaude{}, &fakeSandbox{},
		NewHub(), StubLauncher{}, StubIdentity{ID: "analyst-test"}, mount, "job")

	if _, err := srv.ingestLocal(context.Background(), "link.csv"); !errors.Is(err, errPathEscape) {
		t.Fatalf("error = %v, want errPathEscape", err)
	}
	if objects.puts != 0 {
		t.Fatalf("an escaping path must not be copied into the object store")
	}
}

func TestHandleStreamReplaysBufferedEvents(t *testing.T) {
	srv := newTestServer(&fakeGoals{}, &fakeAudits{}, &fakeObjects{}, &fakeHeur{}, &fakeClaude{}, &fakeSandbox{})
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
}
