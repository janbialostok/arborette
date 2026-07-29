package orchestrator

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/arborette/arborette/internal/domain"
	"github.com/arborette/arborette/internal/store"
)

// serve drives one request through the server's mux and returns the recorder.
func serve(srv *Server, method, target, contentType, body string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, target, strings.NewReader(body))
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	rec := httptest.NewRecorder()
	srv.Routes().ServeHTTP(rec, req)
	return rec
}

func TestHandleAudit(t *testing.T) {
	audits := &fakeAudits{}
	srv := newTestServer(&fakeGoals{}, audits, &fakeObjects{}, &fakeHeur{}, &fakeClaude{}, &fakeSandbox{})

	if rec := serve(srv, http.MethodPost, "/internal/audit", "application/json", "{nope"); rec.Code != http.StatusBadRequest {
		t.Fatalf("bad json status = %d, want 400", rec.Code)
	}
	if rec := serve(srv, http.MethodPost, "/internal/audit", "application/json", `{"detail":{}}`); rec.Code != http.StatusBadRequest {
		t.Fatalf("missing fields status = %d, want 400", rec.Code)
	}

	rec := serve(srv, http.MethodPost, "/internal/audit", "application/json",
		`{"action":"sleep_cycle_done","event_type":"job","detail":{"k":"v"}}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("valid audit status = %d, want 201", rec.Code)
	}
	if len(audits.records) != 1 {
		t.Fatalf("expected one appended record, got %d", len(audits.records))
	}
	got := audits.records[0]
	if got.Action != "sleep_cycle_done" || got.EventType != "job" || got.Actor != "analyst-test" {
		t.Fatalf("record not stamped/populated as expected: %+v", got)
	}
}

func TestSubmitGoalErrorPaths(t *testing.T) {
	t.Run("both sources", func(t *testing.T) {
		srv := newTestServer(&fakeGoals{}, &fakeAudits{}, &fakeObjects{}, &fakeHeur{}, &fakeClaude{}, &fakeSandbox{})
		body, ct := multipartBody(t, map[string]string{"goal": "g", "import_path": "x.csv"}, "file", "data.csv", "a\n")
		req := httptest.NewRequest(http.MethodPost, "/goals", body)
		req.Header.Set("Content-Type", ct)
		rec := httptest.NewRecorder()
		srv.Routes().ServeHTTP(rec, req)
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("both-sources status = %d, want 400", rec.Code)
		}
	})

	t.Run("matrix generation failure", func(t *testing.T) {
		claude := &fakeClaude{matrixErr: errors.New("upstream down")}
		srv := newTestServer(&fakeGoals{}, &fakeAudits{}, &fakeObjects{}, &fakeHeur{}, claude, &fakeSandbox{})
		body, ct := multipartBody(t, map[string]string{"goal": "g"}, "file", "data.csv", "a\n")
		req := httptest.NewRequest(http.MethodPost, "/goals", body)
		req.Header.Set("Content-Type", ct)
		rec := httptest.NewRecorder()
		srv.Routes().ServeHTTP(rec, req)
		if rec.Code != http.StatusBadGateway {
			t.Fatalf("matrix-failure status = %d, want 502", rec.Code)
		}
	})

	t.Run("persist failure", func(t *testing.T) {
		goals := &fakeGoals{insertErr: errors.New("db down")}
		srv := newTestServer(goals, &fakeAudits{}, &fakeObjects{}, &fakeHeur{},
			&fakeClaude{matrix: domain.EvaluationMatrix{Targets: []domain.Target{{Field: "revenue", Direction: domain.Maximize, Aggregation: "avg"}}}}, &fakeSandbox{})
		body, ct := multipartBody(t, map[string]string{"goal": "g"}, "file", "data.csv", "a\n")
		req := httptest.NewRequest(http.MethodPost, "/goals", body)
		req.Header.Set("Content-Type", ct)
		rec := httptest.NewRecorder()
		srv.Routes().ServeHTTP(rec, req)
		if rec.Code != http.StatusInternalServerError {
			t.Fatalf("persist-failure status = %d, want 500", rec.Code)
		}
	})

	t.Run("on-disk file not found maps to 404", func(t *testing.T) {
		dir := t.TempDir()
		srv := NewServer(nil, &fakeGoals{}, &fakeRuns{}, &fakeAudits{}, &fakeObjects{}, &fakeHeur{}, &fakeClaude{}, &fakeChat{}, &fakeSandbox{},
			NewHub(), StubLauncher{}, StubIdentity{ID: "analyst-test"}, dir, "job")
		if _, err := srv.ingestLocal(t.Context(), "missing.csv"); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("error = %v, want os.ErrNotExist", err)
		}
		// The handler maps that to 404.
		rec := httptest.NewRecorder()
		srv.writeIngestErr(rec, os.ErrNotExist)
		if rec.Code != http.StatusNotFound {
			t.Fatalf("writeIngestErr status = %d, want 404", rec.Code)
		}
	})
}

func TestTriggerSleepCycle(t *testing.T) {
	t.Run("success launches job and audits", func(t *testing.T) {
		goals := &fakeGoals{get: store.Goal{OptimizationFunctionID: "g1"}}
		audits := &fakeAudits{}
		launcher := &fakeLauncher{}
		srv := newTestServer(goals, audits, &fakeObjects{}, &fakeHeur{}, &fakeClaude{}, &fakeSandbox{})
		srv.jobs = launcher

		rec := serve(srv, http.MethodPost, "/goals/g1/sleep-cycle", "", "")
		if rec.Code != http.StatusAccepted {
			t.Fatalf("status = %d, want 202", rec.Code)
		}
		if launcher.jobName != "arborette-sleepcycle" || launcher.args["optimization_function_id"] != "g1" {
			t.Fatalf("job not launched with the expected name/args: %q %v", launcher.jobName, launcher.args)
		}
		if len(audits.records) != 1 || audits.records[0].Action != "sleep_cycle_trigger" {
			t.Fatalf("expected a sleep_cycle_trigger audit record: %+v", audits.records)
		}
	})

	t.Run("launch failure maps to 500", func(t *testing.T) {
		goals := &fakeGoals{get: store.Goal{OptimizationFunctionID: "g1"}}
		srv := newTestServer(goals, &fakeAudits{}, &fakeObjects{}, &fakeHeur{}, &fakeClaude{}, &fakeSandbox{})
		srv.jobs = &fakeLauncher{err: errors.New("batch unavailable")}

		rec := serve(srv, http.MethodPost, "/goals/g1/sleep-cycle", "", "")
		if rec.Code != http.StatusInternalServerError {
			t.Fatalf("status = %d, want 500", rec.Code)
		}
	})
}

func TestTriggerLoopCreatesRun(t *testing.T) {
	goalWithMatrix := store.Goal{OptimizationFunctionID: "g1",
		EvaluationMatrix: domain.EvaluationMatrix{Targets: []domain.Target{{Field: "revenue", Direction: domain.Maximize, Aggregation: "avg"}}}}

	t.Run("create failure returns 500 without recording a run", func(t *testing.T) {
		runs := &fakeRuns{createErr: errors.New("db down")}
		srv := newTestServer(&fakeGoals{get: goalWithMatrix}, &fakeAudits{}, &fakeObjects{}, &fakeHeur{}, &fakeClaude{}, &fakeSandbox{})
		srv.runs = runs
		rec := serve(srv, http.MethodPost, "/goals/g1/hypothesis-loop", "", "")
		if rec.Code != http.StatusInternalServerError {
			t.Fatalf("status = %d, want 500", rec.Code)
		}
		if len(runs.created) != 0 {
			t.Fatalf("a failed Create must not record a run: %v", runs.created)
		}
	})

	t.Run("success returns 202 and creates one running row", func(t *testing.T) {
		runs := &fakeRuns{}
		srv := newTestServer(&fakeGoals{get: goalWithMatrix}, &fakeAudits{}, &fakeObjects{}, &fakeHeur{}, &fakeClaude{}, &fakeSandbox{})
		srv.runs = runs
		rec := serve(srv, http.MethodPost, "/goals/g1/hypothesis-loop", "", "")
		if rec.Code != http.StatusAccepted {
			t.Fatalf("status = %d, want 202 (body %q)", rec.Code, rec.Body.String())
		}
		// Create is synchronous, before the goroutine launches, so this is race-free.
		if len(runs.created) != 1 {
			t.Fatalf("expected exactly one run created, got %v", runs.created)
		}
	})
}

func TestHeuristicSearchKClamping(t *testing.T) {
	cases := []struct {
		name  string
		query string
		wantK int
	}{
		{"default when absent", "q=x", defaultSearchK},
		{"default when non-numeric", "q=x&k=abc", defaultSearchK},
		{"default when non-positive", "q=x&k=0", defaultSearchK},
		{"honored when in range", "q=x&k=5", 5},
		{"clamped when too large", "q=x&k=100000", maxSearchK},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			heur := &fakeHeur{}
			srv := newTestServer(&fakeGoals{}, &fakeAudits{}, &fakeObjects{}, heur, &fakeClaude{}, &fakeSandbox{})
			rec := serve(srv, http.MethodGet, "/heuristics/search?"+c.query, "", "")
			if rec.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200", rec.Code)
			}
			if heur.gotK != c.wantK {
				t.Fatalf("k = %d, want %d", heur.gotK, c.wantK)
			}
		})
	}
}

func TestHeuristicHandlersErrorPaths(t *testing.T) {
	searchErr := &fakeHeur{queryErr: errors.New("vector store down")}
	srv := newTestServer(&fakeGoals{}, &fakeAudits{}, &fakeObjects{}, searchErr, &fakeClaude{}, &fakeSandbox{})
	if rec := serve(srv, http.MethodGet, "/heuristics/search?q=x", "", ""); rec.Code != http.StatusInternalServerError {
		t.Fatalf("search error status = %d, want 500", rec.Code)
	}

	traceErr := &fakeHeur{traceErr: errors.New("graph down")}
	srv = newTestServer(&fakeGoals{}, &fakeAudits{}, &fakeObjects{}, traceErr, &fakeClaude{}, &fakeSandbox{})
	if rec := serve(srv, http.MethodGet, "/heuristics/mh-1/trace", "", ""); rec.Code != http.StatusInternalServerError {
		t.Fatalf("trace error status = %d, want 500", rec.Code)
	}
}

func TestContentTypeForPath(t *testing.T) {
	if got := contentTypeForPath("dir/data.CSV"); got != "text/csv" {
		t.Fatalf("csv content-type = %q, want text/csv", got)
	}
	if got := contentTypeForPath("dir/data.parquet"); got != "application/octet-stream" {
		t.Fatalf("parquet content-type = %q, want application/octet-stream", got)
	}
}
