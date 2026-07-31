package service_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/arborette/arborette/internal/service"
)

// okHandler records whether the guarded handler ran, so a test can tell a 401
// from a handler that ran and happened to write 200.
func okHandler(reached *bool) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		*reached = true
		w.WriteHeader(http.StatusOK)
	})
}

func TestBearerAuth(t *testing.T) {
	cases := []struct {
		name       string
		header     string
		wantStatus int
		wantReach  bool
	}{
		{"missing header", "", http.StatusUnauthorized, false},
		{"wrong scheme", "Basic s3cret", http.StatusUnauthorized, false},
		{"bare token without scheme", "s3cret", http.StatusUnauthorized, false},
		{"wrong token", "Bearer nope", http.StatusUnauthorized, false},
		{"wrong token of a different length", "Bearer n", http.StatusUnauthorized, false},
		{"correct token", "Bearer s3cret", http.StatusOK, true},
		{"lowercase scheme", "bearer s3cret", http.StatusOK, true},
		{"uppercase scheme", "BEARER s3cret", http.StatusOK, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var reached bool
			h := service.BearerAuth("s3cret", okHandler(&reached))

			req := httptest.NewRequest(http.MethodPost, "/", nil)
			if tc.header != "" {
				req.Header.Set("Authorization", tc.header)
			}
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)

			if rec.Code != tc.wantStatus {
				t.Fatalf("status = %d, want %d", rec.Code, tc.wantStatus)
			}
			if reached != tc.wantReach {
				t.Fatalf("next handler reached = %v, want %v", reached, tc.wantReach)
			}
			if tc.wantStatus != http.StatusUnauthorized {
				return
			}
			// Read off Result(), not the recorder's live header map: the map keeps
			// accepting writes after the response is committed, so a challenge set
			// too late still shows up there while never reaching the client.
			resp := rec.Result()
			if challenge := resp.Header.Get("WWW-Authenticate"); challenge != "Bearer" {
				t.Fatalf("WWW-Authenticate = %q, want %q", challenge, "Bearer")
			}
			if ct := resp.Header.Get("Content-Type"); ct != "application/json" {
				t.Fatalf("Content-Type = %q, want application/json", ct)
			}
			// The rejection answers in the shared error envelope (see
			// TestWriteErrEnvelope), not a bare status line.
			var body map[string]string
			if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
				t.Fatalf("decode 401 body: %v", err)
			}
			if len(body) != 1 || body["error"] != "unauthorized" {
				t.Fatalf(`401 body = %v, want {"error": "unauthorized"}`, body)
			}
		})
	}
}

// TestBearerAuthEmptyTokenPasses pins the documented fail-open: an unset token
// serves the handler unwrapped so the local stack runs without minting one.
func TestBearerAuthEmptyTokenPasses(t *testing.T) {
	var reached bool
	h := service.BearerAuth("", okHandler(&reached))

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/", nil))

	if rec.Code != http.StatusOK || !reached {
		t.Fatalf("empty token must serve unwrapped (status=%d reached=%v)", rec.Code, reached)
	}
}
