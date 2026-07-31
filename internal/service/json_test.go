package service_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/arborette/arborette/internal/service"
)

func TestWriteJSONEncodesUnderStatus(t *testing.T) {
	rec := httptest.NewRecorder()

	service.WriteJSON(rec, http.StatusCreated, map[string]any{"optimization_function_id": "opt-1"})

	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201", rec.Code)
	}
	if got := rec.Header().Get("Content-Type"); got != "application/json" {
		t.Fatalf("content-type = %q, want application/json", got)
	}
	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	if body["optimization_function_id"] != "opt-1" {
		t.Fatalf("body = %v, want the encoded value", body)
	}
}

// TestWriteErrEnvelope pins the error shape every service's clients decode: the
// message is carried under "error" and nothing else. Both HTTP clients read that
// key to decide whether a failure is caller-safe to surface, so a rename here
// would silently downgrade every server-side reason to a bare status code.
func TestWriteErrEnvelope(t *testing.T) {
	rec := httptest.NewRecorder()

	service.WriteErr(rec, http.StatusBadRequest, "action and event_type are required")

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
	if got := rec.Header().Get("Content-Type"); got != "application/json" {
		t.Fatalf("content-type = %q, want application/json", got)
	}
	var body map[string]string
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	if len(body) != 1 || body["error"] != "action and event_type are required" {
		t.Fatalf("body = %v, want exactly {\"error\": msg}", body)
	}
}
