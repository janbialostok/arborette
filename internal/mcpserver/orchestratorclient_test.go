package mcpserver

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestSubmitGoalPostsMultipart(t *testing.T) {
	var gotGoal, gotImportPath, gotContentType string
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotContentType = r.Header.Get("Content-Type")
		if err := r.ParseMultipartForm(1 << 20); err != nil {
			t.Errorf("parse multipart form: %v", err)
		}
		gotGoal = r.FormValue("goal")
		gotImportPath = r.FormValue("import_path")
		writeJSONResp(w, http.StatusCreated, `{"optimization_function_id":"opt-x"}`)
	}))
	defer ts.Close()

	c := NewOrchestratorClient(ts.URL, ts.Client())
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

func TestSubmitGoalSurfacesOrchestratorError(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeJSONResp(w, http.StatusBadRequest, `{"error":"import_path escapes the import directory"}`)
	}))
	defer ts.Close()

	c := NewOrchestratorClient(ts.URL, ts.Client())
	_, err := c.SubmitGoal(context.Background(), "grow revenue", "../secret.csv")
	if err == nil {
		t.Fatalf("expected an error for a rejected import_path")
	}
	if !strings.Contains(err.Error(), "import_path escapes the import directory") {
		t.Fatalf("error = %q, want the orchestrator's message surfaced", err)
	}
}

func TestSubmitGoalNonJSONErrorBodyIsMaskable(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
		_, _ = w.Write([]byte("<html>502 Bad Gateway</html>"))
	}))
	defer ts.Close()

	c := NewOrchestratorClient(ts.URL, ts.Client())
	_, err := c.SubmitGoal(context.Background(), "grow revenue", "data.csv")
	if err == nil {
		t.Fatalf("expected an error for a non-201 response")
	}
	// A response with no decodable {"error":...} body must NOT be tagged as an
	// analyst-safe OrchestratorError; it falls to the generic status error the
	// caller masks.
	var oerr *OrchestratorError
	if errors.As(err, &oerr) {
		t.Fatalf("non-JSON error body must not surface as OrchestratorError: %v", err)
	}
	if !strings.Contains(err.Error(), "status 502") {
		t.Fatalf("error = %q, want the status-code fallback", err)
	}
}

func TestSubmitGoalUndecodableSuccessBodyIsMaskable(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeJSONResp(w, http.StatusCreated, "not-json")
	}))
	defer ts.Close()

	c := NewOrchestratorClient(ts.URL, ts.Client())
	_, err := c.SubmitGoal(context.Background(), "grow revenue", "data.csv")
	if err == nil {
		t.Fatalf("expected a decode error for an undecodable 201 body")
	}
	// A malformed success body is a client-side failure, not an analyst-safe
	// OrchestratorError, so the caller masks it.
	var oerr *OrchestratorError
	if errors.As(err, &oerr) {
		t.Fatalf("a decode failure must not surface as OrchestratorError: %v", err)
	}
	if !strings.Contains(err.Error(), "decode orchestrator /goals response") {
		t.Fatalf("error = %q, want the decode-branch error", err)
	}
}

func writeJSONResp(w http.ResponseWriter, status int, body string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write([]byte(body))
}
