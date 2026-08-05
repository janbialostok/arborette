package sleepcycle

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeRunner stands in for *Worker. It records the goals it ran, announces each
// start on a buffered channel so a test can observe the async dispatch, and
// blocks on a gate until the test releases it -- which is how a run is held "in
// flight" to exercise the duplicate-trigger path. Closing the gate lets every
// run (present and future) return immediately.
type fakeRunner struct {
	started chan string
	gate    chan struct{}

	mu    sync.Mutex
	goals []string
}

func newFakeRunner() *fakeRunner {
	return &fakeRunner{started: make(chan string, 8), gate: make(chan struct{})}
}

func (f *fakeRunner) Run(_ context.Context, goalID string) error {
	f.mu.Lock()
	f.goals = append(f.goals, goalID)
	f.mu.Unlock()
	f.started <- goalID
	<-f.gate
	return nil
}

// serve drives one request against the serve mux and returns the recorder.
func serve(srv *Server, body string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost, "/runs", strings.NewReader(body))
	rec := httptest.NewRecorder()
	srv.Routes().ServeHTTP(rec, req)
	return rec
}

func TestServerHandleRun(t *testing.T) {
	cases := []struct {
		name       string
		body       string
		wantStatus int
	}{
		{"accepts a valid launch", `{"optimization_function_id":"goal-1"}`, http.StatusAccepted},
		{"rejects a missing id", `{"optimization_function_id":""}`, http.StatusBadRequest},
		{"rejects an absent id field", `{}`, http.StatusBadRequest},
		{"rejects malformed JSON", `{not json`, http.StatusBadRequest},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fr := newFakeRunner()
			close(fr.gate) // runs return immediately; this test does not hold one in flight
			srv := NewServer(fr, 1<<20)

			rec := serve(srv, tc.body)
			if rec.Code != tc.wantStatus {
				t.Fatalf("status = %d, want %d", rec.Code, tc.wantStatus)
			}
			if tc.wantStatus != http.StatusAccepted {
				return
			}

			var resp struct {
				OptimizationFunctionID string `json:"optimization_function_id"`
			}
			if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
				t.Fatalf("decode 202 body: %v", err)
			}
			if resp.OptimizationFunctionID != "goal-1" {
				t.Fatalf("response id = %q, want %q", resp.OptimizationFunctionID, "goal-1")
			}

			// The run is dispatched asynchronously, so confirm it reached the runner
			// with the decoded id rather than trusting the 202 alone.
			select {
			case got := <-fr.started:
				if got != "goal-1" {
					t.Fatalf("runner ran goal %q, want %q", got, "goal-1")
				}
			case <-time.After(time.Second):
				t.Fatal("runner was never invoked")
			}
		})
	}
}

// TestServerRunInFlight pins the duplicate-trigger contract: while a goal's run is
// live a second trigger for it is a 409, and once that run finishes a fresh
// trigger is accepted again.
func TestServerRunInFlight(t *testing.T) {
	fr := newFakeRunner() // gate open: the first run blocks, staying in flight
	srv := NewServer(fr, 1<<20)

	body := `{"optimization_function_id":"goal-1"}`
	if rec := serve(srv, body); rec.Code != http.StatusAccepted {
		t.Fatalf("first trigger status = %d, want %d", rec.Code, http.StatusAccepted)
	}
	select {
	case <-fr.started:
	case <-time.After(time.Second):
		t.Fatal("first run never started")
	}

	if rec := serve(srv, body); rec.Code != http.StatusConflict {
		t.Fatalf("duplicate trigger status = %d, want %d", rec.Code, http.StatusConflict)
	}

	close(fr.gate) // let the first run return so its in-flight entry clears

	// The entry clears when the run goroutine returns, which races the release;
	// poll until a fresh trigger for the same goal is accepted again.
	var last int
	for range 200 {
		last = serve(srv, body).Code
		if last == http.StatusAccepted {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("trigger after completion status = %d, want %d", last, http.StatusAccepted)
}

// TestServerBodyCap confirms the request body cap rejects an oversized body
// rather than reading it unbounded.
func TestServerBodyCap(t *testing.T) {
	fr := newFakeRunner()
	close(fr.gate)
	srv := NewServer(fr, 8) // smaller than any real launch body

	big := `{"optimization_function_id":"` + strings.Repeat("x", 64) + `"}`
	if rec := serve(srv, big); rec.Code != http.StatusBadRequest {
		t.Fatalf("oversized body status = %d, want %d", rec.Code, http.StatusBadRequest)
	}
}
