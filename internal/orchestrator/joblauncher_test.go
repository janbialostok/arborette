package orchestrator

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/arborette/arborette/internal/service"
)

// TestHTTPLauncherLaunch pins the dispatch contract: the launcher POSTs the args
// map verbatim to the configured endpoint, appends nothing to its path, stamps the
// bearer secret only when configured, and maps the response status to nil-or-error.
func TestHTTPLauncherLaunch(t *testing.T) {
	args := map[string]string{"optimization_function_id": "goal-1"}

	t.Run("posts args to the configured path with auth", func(t *testing.T) {
		var (
			gotMethod string
			gotPath   string
			gotAuth   string
			gotBody   map[string]string
		)
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			gotMethod = r.Method
			gotPath = r.URL.Path
			gotAuth = r.Header.Get("Authorization")
			raw, _ := io.ReadAll(r.Body)
			json.Unmarshal(raw, &gotBody)
			service.WriteJSON(w, http.StatusAccepted, map[string]any{"optimization_function_id": "goal-1"})
		}))
		defer srv.Close()

		l := NewHTTPLauncher(srv.URL+"/runs", "secret", srv.Client())
		if err := l.Launch(context.Background(), "arborette-sleepcycle", args); err != nil {
			t.Fatalf("Launch: %v", err)
		}
		if gotMethod != http.MethodPost {
			t.Fatalf("method = %q, want POST", gotMethod)
		}
		if gotPath != "/runs" {
			t.Fatalf("path = %q, want /runs (the type must append nothing)", gotPath)
		}
		if gotAuth != "Bearer secret" {
			t.Fatalf("authorization = %q, want %q", gotAuth, "Bearer secret")
		}
		if gotBody["optimization_function_id"] != "goal-1" {
			t.Fatalf("body = %v, want the args map", gotBody)
		}
	})

	t.Run("presents no credential when the token is empty", func(t *testing.T) {
		var gotAuth string
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			gotAuth = r.Header.Get("Authorization")
			w.WriteHeader(http.StatusAccepted)
		}))
		defer srv.Close()

		l := NewHTTPLauncher(srv.URL+"/runs", "", srv.Client())
		if err := l.Launch(context.Background(), "job", args); err != nil {
			t.Fatalf("Launch: %v", err)
		}
		if gotAuth != "" {
			t.Fatalf("authorization = %q, want no header", gotAuth)
		}
	})

	t.Run("surfaces the worker error message on a non-202", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			service.WriteErr(w, http.StatusConflict, "a run for this goal is already in flight")
		}))
		defer srv.Close()

		l := NewHTTPLauncher(srv.URL+"/runs", "", srv.Client())
		err := l.Launch(context.Background(), "job", args)
		var le *LaunchError
		if !errors.As(err, &le) {
			t.Fatalf("error = %v, want *LaunchError", err)
		}
		if le.Status != http.StatusConflict {
			t.Fatalf("status = %d, want %d", le.Status, http.StatusConflict)
		}
		if le.Message != "a run for this goal is already in flight" {
			t.Fatalf("message = %q, want the worker's message", le.Message)
		}
	})

	t.Run("errors on a bodyless non-202", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusInternalServerError)
		}))
		defer srv.Close()

		l := NewHTTPLauncher(srv.URL+"/runs", "", srv.Client())
		if err := l.Launch(context.Background(), "job", args); err == nil {
			t.Fatal("Launch returned nil, want an error on 500")
		}
	})

	t.Run("errors when the endpoint is unreachable", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
		url := srv.URL
		srv.Close() // nothing listens now, so the dispatch is refused

		l := NewHTTPLauncher(url+"/runs", "", srv.Client())
		if err := l.Launch(context.Background(), "job", args); err == nil {
			t.Fatal("Launch returned nil, want a connection error")
		}
	})
}
