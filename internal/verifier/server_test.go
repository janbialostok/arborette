package verifier

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/arborette/arborette/internal/orchestrator"
)

// fakeRunner records dispatches and can block one run in-flight so the duplicate
// (409) path is deterministic (claim is synchronous, so the slot is held before the
// second request arrives).
type fakeRunner struct {
	block       chan struct{}
	calls       chan struct{}
	verifyCalls chan struct{}
	claimCalls  chan struct{}
	budgeted    chan bool
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

func (f *fakeRunner) VerifyOne(_ context.Context, _, _, _ string, budgeted bool) error {
	if f.budgeted != nil {
		f.budgeted <- budgeted
	}
	if f.verifyCalls != nil {
		f.verifyCalls <- struct{}{}
	}
	return nil
}

func (f *fakeRunner) VerifyClaim(context.Context, string, string) error {
	if f.claimCalls != nil {
		f.claimCalls <- struct{}{}
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

// TestVerifierServeClaimDispatch: a claim dispatch carries no intervention id (the
// Verifier reifies one from the stored claim) and is accepted asynchronously.
func TestVerifierServeClaimDispatch(t *testing.T) {
	runner := &fakeRunner{claimCalls: make(chan struct{}, 1)}
	srv := NewServer(runner, 1<<20)

	if code := postVerification(srv, `{"goal_id":"g","datasource_ref":"r","kind":"claim"}`).Code; code != http.StatusAccepted {
		t.Fatalf("claim dispatch status = %d, want 202", code)
	}
	<-runner.claimCalls
}

// TestVerifierServeBudgetedFlag: the budget flag arrives as the JSON string the
// launcher's string-pair contract produces, and an unparseable one is a 400 rather
// than a silent fall to unbudgeted -- misreading it would charge or spare the wrong
// dispatch.
func TestVerifierServeBudgetedFlag(t *testing.T) {
	cases := []struct {
		name string
		body string
		want bool
	}{
		{"budgeted", `{"goal_id":"g","datasource_ref":"r","kind":"verify","intervention_id":"i1","budgeted":"true"}`, true},
		{"exempt", `{"goal_id":"g","datasource_ref":"r","kind":"verify","intervention_id":"i1","budgeted":"false"}`, false},
		{"absent", `{"goal_id":"g","datasource_ref":"r","kind":"verify","intervention_id":"i1"}`, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			runner := &fakeRunner{budgeted: make(chan bool, 1)}
			srv := NewServer(runner, 1<<20)
			if code := postVerification(srv, c.body).Code; code != http.StatusAccepted {
				t.Fatalf("status = %d, want 202", code)
			}
			if got := <-runner.budgeted; got != c.want {
				t.Fatalf("budgeted = %v, want %v", got, c.want)
			}
		})
	}

	srv := NewServer(&fakeRunner{}, 1<<20)
	malformed := `{"goal_id":"g","datasource_ref":"r","kind":"verify","intervention_id":"i1","budgeted":"yes please"}`
	if code := postVerification(srv, malformed).Code; code != http.StatusBadRequest {
		t.Fatalf("malformed budgeted status = %d, want 400", code)
	}
}

// TestVerifierServeAcceptsLauncherBody drives the serve surface with a body the
// orchestrator's own launcher produced, rather than one this test hand-wrote. The
// launcher marshals a map of string pairs verbatim, so every field crosses as a JSON
// string -- a mismatch there is invisible to a fake-launcher unit test on either side
// and would 400 every real dispatch.
func TestVerifierServeAcceptsLauncherBody(t *testing.T) {
	runner := &fakeRunner{verifyCalls: make(chan struct{}, 1), budgeted: make(chan bool, 1)}
	backend := httptest.NewServer(NewServer(runner, 1<<20).Routes())
	defer backend.Close()

	launcher := orchestrator.NewHTTPLauncher(backend.URL+"/verifications", "", nil)
	err := launcher.Launch(context.Background(), "arborette-verifier", map[string]string{
		"kind":            "verify",
		"goal_id":         "g",
		"intervention_id": "i1",
		"datasource_ref":  "r",
		"budgeted":        "true",
	})
	if err != nil {
		t.Fatalf("launcher-produced dispatch was rejected: %v", err)
	}
	if got := <-runner.budgeted; !got {
		t.Fatalf("budgeted = false, want true from the launcher body")
	}
	<-runner.verifyCalls
}
