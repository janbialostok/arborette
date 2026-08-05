package orchestratorclient

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// TestAppendPostsTheOrchestratorContract pins the wire shape against the
// Orchestrator's own handler: field names, the long-action/short-event-type
// ordering, and 201-as-success. Nothing else pins these two sides together, and
// because the worker logs and swallows every Append error, drift here would make
// every Sleep-Cycle audit record silently vanish while runs still report success.
func TestAppendPostsTheOrchestratorContract(t *testing.T) {
	var gotPath, gotContentType string
	var body map[string]any

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotContentType = r.Header.Get("Content-Type")
		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, &body)
		w.WriteHeader(http.StatusCreated)
	}))
	defer srv.Close()

	c := NewClient(srv.URL, "", srv.Client())
	err := c.Append(context.Background(), "sleepcycle_intervention", "intervention", map[string]any{
		"optimization_function_id": "g1",
		"intervention_id":          "i-1",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if gotPath != "/internal/audit" {
		t.Fatalf("path = %q, want /internal/audit", gotPath)
	}
	if gotContentType != "application/json" {
		t.Fatalf("content-type = %q, want application/json", gotContentType)
	}
	// The handler requires both of these non-empty and reads them under exactly
	// these keys; the long name is the action, the categorical one the event type.
	if body["action"] != "sleepcycle_intervention" {
		t.Fatalf("action = %v, want the long/specific name", body["action"])
	}
	if body["event_type"] != "intervention" {
		t.Fatalf("event_type = %v, want the short/categorical name", body["event_type"])
	}
	detail, ok := body["detail"].(map[string]any)
	if !ok || detail["optimization_function_id"] != "g1" {
		t.Fatalf("detail did not round-trip: %v", body["detail"])
	}
}

// TestAppendSurfacesTheOrchestratorMessage: a non-201 carrying the Orchestrator's
// own {"error": ...} body is returned as a typed error so a caller can log the
// real reason rather than a bare status.
func TestAppendSurfacesTheOrchestratorMessage(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error":"action and event_type are required"}`))
	}))
	defer srv.Close()

	err := NewClient(srv.URL, "", srv.Client()).Append(context.Background(), "", "", nil)

	var oerr *OrchestratorError
	if !errors.As(err, &oerr) {
		t.Fatalf("error = %v, want *OrchestratorError", err)
	}
	if oerr.Status != http.StatusBadRequest || oerr.Message != "action and event_type are required" {
		t.Fatalf("OrchestratorError = %+v, want {400, the decoded orchestrator reason}", oerr)
	}
}

// TestAppendFallsBackWhenBodyEmpty: a bodyless non-201 still names the status.
func TestAppendFallsBackWhenBodyEmpty(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	err := NewClient(srv.URL, "", srv.Client()).Append(context.Background(), "a", "b", nil)
	if err == nil {
		t.Fatal("expected an error for a 500")
	}
	// The endpoint is named as well as the status: one client speaks two of
	// them, and a swallowed audit failure reaches an operator as this string
	// alone, where "the audit hop is down" and "goal intake is down" must not
	// read the same.
	if !strings.Contains(err.Error(), "500") || !strings.Contains(err.Error(), "orchestrator "+auditPath) {
		t.Fatalf("error = %v, want a status fallback naming 500 and %s", err, auditPath)
	}
}

// TestNonSuccessJSONWithoutErrorKeyIsMaskable: a body that decodes but carries no
// "error" key vouches for nothing, so it must fall to the maskable status error.
// Tagging it caller-safe would hand the analyst an OrchestratorError whose message
// is the empty string -- a failure reported as blank text.
func TestNonSuccessJSONWithoutErrorKeyIsMaskable(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		respondJSON(w, http.StatusBadGateway, `{"detail":"upstream refused"}`)
	}))
	defer srv.Close()

	err := NewClient(srv.URL, "", srv.Client()).Append(context.Background(), "a", "b", nil)

	var oerr *OrchestratorError
	if errors.As(err, &oerr) {
		t.Fatalf("a body with no error key must not surface as OrchestratorError: %+v", oerr)
	}
	if !strings.Contains(err.Error(), "status 502") {
		t.Fatalf("error = %v, want the status-code fallback", err)
	}
}

// TestAppendReportsTransportFailure: an unreachable Orchestrator is an error, not
// a silent success — the worker's swallow is a deliberate choice at the call
// site, so the client must actually report the failure.
func TestAppendReportsTransportFailure(t *testing.T) {
	// Port 9 (discard) refuses connections.
	err := NewClient("http://127.0.0.1:9", "", nil).Append(context.Background(), "a", "b", nil)
	if err == nil {
		t.Fatal("expected a transport error when the orchestrator is unreachable")
	}
	// Asserted on the prefix this client writes, not just the path: the wrapped
	// *url.Error already embeds the request URL, so a bare Contains(auditPath)
	// passes even when the client labels every error with the wrong endpoint.
	if !strings.Contains(err.Error(), "call orchestrator "+auditPath) {
		t.Fatalf("error = %v, want the failing endpoint named by the client", err)
	}
}

// TestAppendReportsUnmarshalableDetail: a detail value json.Marshal refuses stops
// the call before it is made, and must still be reported. The worker builds detail
// straight from sandbox measurements, and an aggregate that overflowed DOUBLE
// space arrives as +Inf — which Marshal rejects — so a swallowed marshal error
// would erase the only record explaining a run that produced nothing.
func TestAppendReportsUnmarshalableDetail(t *testing.T) {
	err := NewClient("http://127.0.0.1:9", "", nil).
		Append(context.Background(), "a", "b", map[string]any{"best_single": math.Inf(1)})
	if err == nil {
		t.Fatal("expected an error for a detail value json.Marshal cannot encode")
	}
	if !strings.Contains(err.Error(), "marshal audit request") {
		t.Fatalf("error = %v, want the marshal-branch error", err)
	}
}

// TestNilHTTPClientTakesDefaultTimeout pins the bound a caller gets by passing
// nil. The MCP server overrides it precisely because this default is too short
// for goal submission, so a caller that stopped overriding would silently cut a
// two-minute call to this — visible only as timeouts under load.
func TestNilHTTPClientTakesDefaultTimeout(t *testing.T) {
	if got := NewClient("http://orchestrator:8080", "", nil).client.Timeout; got != defaultTimeout {
		t.Fatalf("nil http.Client timeout = %v, want %v", got, defaultTimeout)
	}
	injected := &http.Client{Timeout: time.Hour}
	if got := NewClient("http://orchestrator:8080", "", injected).client; got != injected {
		t.Fatal("an injected http.Client must be used as-is")
	}
}

// TestSubmitGoalPostsMultipart pins the intake form's wire shape: the Orchestrator
// reads these two fields off a multipart body, and an MCP tool call carries neither
// a file part nor a form, so this client is the only thing constructing one.
func TestSubmitGoalPostsMultipart(t *testing.T) {
	var gotGoal, gotImportPath, gotContentType string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotContentType = r.Header.Get("Content-Type")
		if err := r.ParseMultipartForm(1 << 20); err != nil {
			t.Errorf("parse multipart form: %v", err)
		}
		gotGoal = r.FormValue("goal")
		gotImportPath = r.FormValue("import_path")
		respondJSON(w, http.StatusCreated, `{"optimization_function_id":"opt-x"}`)
	}))
	defer srv.Close()

	c := NewClient(srv.URL, "", srv.Client())
	optID, err := c.SubmitGoal(context.Background(), "grow revenue", "data.csv")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if optID != "opt-x" {
		t.Fatalf("optID = %q, want opt-x", optID)
	}
	if !strings.HasPrefix(gotContentType, "multipart/form-data") {
		t.Fatalf("content-type = %q, want multipart/form-data", gotContentType)
	}
	if gotGoal != "grow revenue" || gotImportPath != "data.csv" {
		t.Fatalf("form fields = goal:%q import_path:%q, want grow revenue / data.csv", gotGoal, gotImportPath)
	}
}

// TestSubmitGoalSurfacesOrchestratorError: a rejection the Orchestrator explained
// reaches the analyst verbatim. Masking it would answer a fixable mistake with
// "internal error", leaving them nothing to act on.
func TestSubmitGoalSurfacesOrchestratorError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		respondJSON(w, http.StatusBadRequest, `{"error":"import_path escapes the import directory"}`)
	}))
	defer srv.Close()

	c := NewClient(srv.URL, "", srv.Client())
	_, err := c.SubmitGoal(context.Background(), "grow revenue", "../secret.csv")
	if err == nil {
		t.Fatalf("expected an error for a rejected import_path")
	}
	if !strings.Contains(err.Error(), "import_path escapes the import directory") {
		t.Fatalf("error = %q, want the orchestrator's message surfaced", err)
	}
}

// TestSubmitGoalNonJSONErrorBodyIsMaskable is the other half of that split: a
// gateway's HTML page is not the Orchestrator speaking, so nothing vouches for it
// and it must stay maskable rather than reach the analyst as an explanation.
func TestSubmitGoalNonJSONErrorBodyIsMaskable(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
		_, _ = w.Write([]byte("<html>502 Bad Gateway</html>"))
	}))
	defer srv.Close()

	c := NewClient(srv.URL, "", srv.Client())
	_, err := c.SubmitGoal(context.Background(), "grow revenue", "data.csv")
	if err == nil {
		t.Fatalf("expected an error for a non-201 response")
	}
	var oerr *OrchestratorError
	if errors.As(err, &oerr) {
		t.Fatalf("non-JSON error body must not surface as OrchestratorError: %v", err)
	}
	if !strings.Contains(err.Error(), "status 502") {
		t.Fatalf("error = %q, want the status-code fallback", err)
	}
}

// TestSubmitGoalUndecodableSuccessBodyIsMaskable: a malformed 201 is this client's
// own failure, not a message from the Orchestrator, so it is masked too — and the
// error names the endpoint whose response could not be read.
func TestSubmitGoalUndecodableSuccessBodyIsMaskable(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		respondJSON(w, http.StatusCreated, "not-json")
	}))
	defer srv.Close()

	c := NewClient(srv.URL, "", srv.Client())
	_, err := c.SubmitGoal(context.Background(), "grow revenue", "data.csv")
	if err == nil {
		t.Fatalf("expected a decode error for an undecodable 201 body")
	}
	var oerr *OrchestratorError
	if errors.As(err, &oerr) {
		t.Fatalf("a decode failure must not surface as OrchestratorError: %v", err)
	}
	if !strings.Contains(err.Error(), "decode orchestrator /goals response") {
		t.Fatalf("error = %q, want the decode-branch error", err)
	}
}

// TestAuthTokenIsPresentedOnlyWhenConfigured pins the sender half of the internal
// shared secret. A worker whose token is missing or wrong gets a 401 the run's
// audit path logs and swallows, so a header that silently stops being sent would
// erase the audit trail while every run still reports success.
func TestAuthTokenIsPresentedOnlyWhenConfigured(t *testing.T) {
	var gotAuth string
	var hadAuth bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		_, hadAuth = r.Header["Authorization"]
		w.WriteHeader(http.StatusCreated)
	}))
	defer srv.Close()

	if err := NewClient(srv.URL, "s3cret", srv.Client()).Append(context.Background(), "a", "b", nil); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if gotAuth != "Bearer s3cret" {
		t.Fatalf("Authorization = %q, want the configured bearer token", gotAuth)
	}

	if err := NewClient(srv.URL, "", srv.Client()).Append(context.Background(), "a", "b", nil); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if hadAuth {
		t.Fatalf("Authorization = %q, want no header at all without a token", gotAuth)
	}
}

func respondJSON(w http.ResponseWriter, status int, body string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write([]byte(body))
}
