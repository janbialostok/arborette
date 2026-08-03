package verifier

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// fakeRunner records dispatches and can block one run in-flight so the duplicate
// (409) path is deterministic (claim is synchronous, so the slot is held before the
// second request arrives).
type fakeRunner struct {
	block       chan struct{}
	calls       chan struct{}
	verifyCalls chan struct{}
}

func (f *fakeRunner) RunDiscovery(context.Context, string, string) error {
	if f.calls != nil {
		f.calls <- struct{}{}
	}
	if f.block != nil {
		<-f.block
	}
	return nil
}

func (f *fakeRunner) VerifyOne(context.Context, string, string, string, bool) error {
	if f.verifyCalls != nil {
		f.verifyCalls <- struct{}{}
	}
	return nil
}

func postVerification(srv *Server, body string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost, "/verifications", strings.NewReader(body))
	rec := httptest.NewRecorder()
	srv.Routes().ServeHTTP(rec, req)
	return rec
}

func TestVerifierServeValidation(t *testing.T) {
	srv := NewServer(&fakeRunner{}, 1<<20)
	cases := []struct {
		name string
		body string
		want int
	}{
		{"invalid body", `{`, http.StatusBadRequest},
		{"missing goal", `{"datasource_ref":"r","kind":"discovery"}`, http.StatusBadRequest},
		{"missing ref", `{"goal_id":"g","kind":"discovery"}`, http.StatusBadRequest},
		{"unknown kind", `{"goal_id":"g","datasource_ref":"r","kind":"bogus"}`, http.StatusUnprocessableEntity},
		{"verify without intervention", `{"goal_id":"g","datasource_ref":"r","kind":"verify"}`, http.StatusBadRequest},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if code := postVerification(srv, c.body).Code; code != c.want {
				t.Fatalf("status = %d, want %d", code, c.want)
			}
		})
	}
}

func TestVerifierServeDispatchAndDuplicate(t *testing.T) {
	runner := &fakeRunner{block: make(chan struct{}), calls: make(chan struct{}, 1)}
	srv := NewServer(runner, 1<<20)
	body := `{"goal_id":"g","datasource_ref":"r","kind":"discovery"}`

	// First dispatch is accepted (202) and the run goroutine starts and blocks.
	if code := postVerification(srv, body).Code; code != http.StatusAccepted {
		t.Fatalf("first dispatch status = %d, want 202", code)
	}
	<-runner.calls // ensure the run is in flight, holding the in-flight slot

	// A duplicate for the same (goal, data source) is rejected while in flight.
	if code := postVerification(srv, body).Code; code != http.StatusConflict {
		t.Fatalf("duplicate dispatch status = %d, want 409", code)
	}

	close(runner.block) // release the run so its goroutine clears the slot
}

// TestVerifierServeVerifyDispatch: a verify dispatch with an intervention id is
// accepted (202) and runs VerifyOne, and — unlike discovery — is not blocked by the
// (goal, data-source) in-flight claim, so a second verify for the same pair also runs.
func TestVerifierServeVerifyDispatch(t *testing.T) {
	runner := &fakeRunner{verifyCalls: make(chan struct{}, 2)}
	srv := NewServer(runner, 1<<20)
	body := `{"goal_id":"g","datasource_ref":"r","kind":"verify","intervention_id":"i1"}`

	for i := 0; i < 2; i++ {
		if code := postVerification(srv, body).Code; code != http.StatusAccepted {
			t.Fatalf("verify dispatch %d status = %d, want 202", i, code)
		}
		<-runner.verifyCalls
	}
}
