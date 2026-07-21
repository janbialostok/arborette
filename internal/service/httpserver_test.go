package service_test

import (
	"net/http"
	"testing"
	"time"

	"github.com/arborette/arborette/internal/service"
)

// TestRunHTTPServerStartupError verifies a bind failure returns promptly instead
// of hanging forever waiting on a shutdown signal.
func TestRunHTTPServerStartupError(t *testing.T) {
	done := make(chan error, 1)
	go func() {
		// A malformed address makes ListenAndServe fail immediately.
		done <- service.RunHTTPServer("test", "missing-port", http.NewServeMux())
	}()

	select {
	case err := <-done:
		if err == nil {
			t.Fatal("expected a startup error, got nil")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("RunHTTPServer hung on a startup failure instead of returning")
	}
}
