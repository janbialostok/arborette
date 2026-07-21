package embedding_test

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/arborette/arborette/internal/embedding"
	"github.com/arborette/arborette/internal/testutil"
)

// TestPrefixes verifies the method-appropriate prefix and dimension validation
// against a fake sidecar, so it runs without infrastructure.
func TestPrefixes(t *testing.T) {
	const dim = 4
	var gotPrompt string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var req struct {
			Prompt string `json:"prompt"`
		}
		_ = json.Unmarshal(body, &req)
		gotPrompt = req.Prompt
		json.NewEncoder(w).Encode(map[string]any{"embedding": make([]float32, dim)})
	}))
	defer srv.Close()

	provider := embedding.NewOllamaProvider(srv.URL, "nomic-embed-text", dim)
	if provider.Dimensions() != dim {
		t.Fatalf("Dimensions() = %d, want %d", provider.Dimensions(), dim)
	}

	if _, err := provider.EmbedQuery(context.Background(), "hello"); err != nil {
		t.Fatalf("EmbedQuery: %v", err)
	}
	if !strings.HasPrefix(gotPrompt, "search_query: ") {
		t.Fatalf("query prompt missing search_query prefix: %q", gotPrompt)
	}

	if _, err := provider.EmbedDocument(context.Background(), "hello"); err != nil {
		t.Fatalf("EmbedDocument: %v", err)
	}
	if !strings.HasPrefix(gotPrompt, "search_document: ") {
		t.Fatalf("document prompt missing search_document prefix: %q", gotPrompt)
	}
}

func TestWrongDimensionErrors(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{"embedding": make([]float32, 3)})
	}))
	defer srv.Close()

	provider := embedding.NewOllamaProvider(srv.URL, "nomic-embed-text", 768)
	if _, err := provider.EmbedQuery(context.Background(), "hello"); err == nil {
		t.Fatal("expected error when sidecar returns wrong dimension")
	}
}

// TestOllamaLive checks the real sidecar returns 768-d vectors for both paths.
func TestOllamaLive(t *testing.T) {
	cfg := testutil.RequireIntegration(t)
	provider := embedding.NewOllamaProvider(cfg.Ollama.URL, cfg.Ollama.Model, cfg.Embedding.Dimension)

	q, err := provider.EmbedQuery(context.Background(), "hello")
	if err != nil {
		t.Fatalf("EmbedQuery: %v", err)
	}
	if len(q) != 768 {
		t.Fatalf("EmbedQuery length = %d, want 768", len(q))
	}
	d, err := provider.EmbedDocument(context.Background(), "hello")
	if err != nil {
		t.Fatalf("EmbedDocument: %v", err)
	}
	if len(d) != 768 {
		t.Fatalf("EmbedDocument length = %d, want 768", len(d))
	}
}
