package orchestrator

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestPostDecodesSandboxErrorBody(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error":"field not present in schema: X"}`))
	}))
	defer srv.Close()

	c := NewSandboxClient(srv.URL, srv.Client())
	_, err := c.Execute(context.Background(), ExecuteRequest{})

	var se *SandboxError
	if !errors.As(err, &se) {
		t.Fatalf("error = %v, want *SandboxError", err)
	}
	if se.Status != http.StatusBadRequest || se.Message != "field not present in schema: X" {
		t.Fatalf("SandboxError = %+v, want {400, decoded reason}", se)
	}
}

func TestPostFallsBackWhenBodyEmpty(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	c := NewSandboxClient(srv.URL, srv.Client())
	_, err := c.Introspect(context.Background(), IntrospectRequest{})

	var se *SandboxError
	if !errors.As(err, &se) {
		t.Fatalf("error = %v, want *SandboxError", err)
	}
	if se.Status != http.StatusInternalServerError || !strings.Contains(se.Message, "returned status 500") {
		t.Fatalf("SandboxError = %+v, want a status-fallback message", se)
	}
}
