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
	matrix    domain.EvaluationMatrix
	matrixErr error
}

func (f *fakeClaude) GenerateEvaluationMatrix(_ context.Context, _ string) (domain.EvaluationMatrix, error) {
	return f.matrix, f.matrixErr
}
func (f *fakeClaude) ProposeInterventionTree(_ context.Context, _ string, _ domain.EvaluationMatrix, _ llm.SandboxSchema, _ llm.TreeContext) (llm.Proposal, error) {
	return llm.Proposal{}, nil
}

type fakeSandbox struct{ introspect IntrospectResponse }

func (f *fakeSandbox) Introspect(_ context.Context, _ IntrospectRequest) (IntrospectResponse, error) {
	return f.introspect, nil
}
func (f *fakeSandbox) Execute(_ context.Context, _ ExecuteRequest) (ExecuteResponse, error) {
	return ExecuteResponse{}, nil
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
	return NewServer(nil, goals, audits, objects, heur, claude, sandbox,
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
	claude := &fakeClaude{matrix: domain.EvaluationMatrix{Targets: []domain.Target{{Field: "revenue", Direction: domain.Maximize}}}}
	sandbox := &fakeSandbox{introspect: IntrospectResponse{TargetBindings: []TargetBinding{{Target: "revenue", Column: "revenue", Matched: true}}}}
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
	srv := NewServer(nil, &fakeGoals{}, &fakeAudits{}, objects, &fakeHeur{}, &fakeClaude{}, &fakeSandbox{},
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
	srv := NewServer(nil, &fakeGoals{}, &fakeAudits{}, objects, &fakeHeur{}, &fakeClaude{}, &fakeSandbox{},
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
