package auditclient

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
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

	c := NewClient(srv.URL, srv.Client())
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

	err := NewClient(srv.URL, srv.Client()).Append(context.Background(), "", "", nil)

	var ae *AuditError
	if !errors.As(err, &ae) {
		t.Fatalf("error = %v, want *AuditError", err)
	}
	if ae.Message != "action and event_type are required" {
		t.Fatalf("message = %q, want the decoded orchestrator reason", ae.Message)
	}
}

// TestAppendFallsBackWhenBodyEmpty: a bodyless non-201 still names the status.
func TestAppendFallsBackWhenBodyEmpty(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	err := NewClient(srv.URL, srv.Client()).Append(context.Background(), "a", "b", nil)
	if err == nil {
		t.Fatal("expected an error for a 500")
	}
	if !strings.Contains(err.Error(), "500") {
		t.Fatalf("error = %v, want a status fallback naming 500", err)
	}
}

// TestAppendReportsTransportFailure: an unreachable Orchestrator is an error, not
// a silent success — the worker's swallow is a deliberate choice at the call
// site, so the client must actually report the failure.
func TestAppendReportsTransportFailure(t *testing.T) {
	// Port 9 (discard) refuses connections.
	err := NewClient("http://127.0.0.1:9", nil).Append(context.Background(), "a", "b", nil)
	if err == nil {
		t.Fatal("expected a transport error when the orchestrator is unreachable")
	}
}
