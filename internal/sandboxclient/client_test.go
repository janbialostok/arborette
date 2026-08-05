package sandboxclient

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

	c := NewClient(srv.URL, "", srv.Client())
	_, err := c.Execute(context.Background(), ExecuteRequest{})

	var se *SandboxError
	if !errors.As(err, &se) {
		t.Fatalf("error = %v, want *SandboxError", err)
	}
	if se.Status != http.StatusBadRequest || se.Message != "field not present in schema: X" {
		t.Fatalf("SandboxError = %+v, want {400, decoded reason}", se)
	}
}

// TestPostStampsBearerToken asserts the wire: a configured token arrives as a
// bearer header on every request, and an empty token sends no header at all.
func TestPostStampsBearerToken(t *testing.T) {
	cases := []struct {
		name       string
		token      string
		wantHeader string
	}{
		{"configured token is stamped", "s3cret", "Bearer s3cret"},
		{"empty token sends none", "", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var got string
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				got = r.Header.Get("Authorization")
				w.WriteHeader(http.StatusOK)
				_, _ = w.Write([]byte(`{}`))
			}))
			defer srv.Close()

			c := NewClient(srv.URL, tc.token, srv.Client())
			if _, err := c.Execute(context.Background(), ExecuteRequest{}); err != nil {
				t.Fatalf("execute: %v", err)
			}
			if got != tc.wantHeader {
				t.Fatalf("Authorization = %q, want %q", got, tc.wantHeader)
			}
		})
	}
}

func TestPostFallsBackWhenBodyEmpty(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	c := NewClient(srv.URL, "", srv.Client())
	_, err := c.Introspect(context.Background(), IntrospectRequest{})

	var se *SandboxError
	if !errors.As(err, &se) {
		t.Fatalf("error = %v, want *SandboxError", err)
	}
	if se.Status != http.StatusInternalServerError || !strings.Contains(se.Message, "returned status 500") {
		t.Fatalf("SandboxError = %+v, want a status-fallback message", se)
	}
}
