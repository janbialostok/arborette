package embedding

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"

	"github.com/arborette/arborette/internal/service"
)

// nomic-embed-text task-instruction prefixes. Ollama does not apply them
// automatically, so the provider prepends the method-appropriate one.
const (
	queryPrefix    = "search_query:"
	documentPrefix = "search_document:"
)

// OllamaProvider embeds text via an Ollama sidecar's HTTP API.
type OllamaProvider struct {
	url        string
	model      string
	dimensions int
	client     *http.Client
}

// NewOllamaProvider builds a provider pointed at the sidecar URL and model.
func NewOllamaProvider(url, model string, dimensions int) *OllamaProvider {
	return &OllamaProvider{
		url:        url,
		model:      model,
		dimensions: dimensions,
		client:     http.DefaultClient,
	}
}

// EmbedQuery embeds operational-state/search text with the search_query prefix.
func (p *OllamaProvider) EmbedQuery(ctx context.Context, text string) ([]float32, error) {
	return p.embed(ctx, queryPrefix+" "+text)
}

// EmbedDocument embeds stored text with the search_document prefix.
func (p *OllamaProvider) EmbedDocument(ctx context.Context, text string) ([]float32, error) {
	return p.embed(ctx, documentPrefix+" "+text)
}

// Dimensions reports the fixed vector width.
func (p *OllamaProvider) Dimensions() int {
	return p.dimensions
}

type ollamaRequest struct {
	Model  string `json:"model"`
	Prompt string `json:"prompt"`
}

type ollamaResponse struct {
	Embedding []float32 `json:"embedding"`
}

func (p *OllamaProvider) embed(ctx context.Context, prompt string) ([]float32, error) {
	body, err := json.Marshal(ollamaRequest{Model: p.model, Prompt: prompt})
	if err != nil {
		return nil, fmt.Errorf("marshal embedding request: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, p.url+"/api/embeddings", bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("build embedding request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := p.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("call ollama: %w", err)
	}
	defer service.DrainAndClose(resp)
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("ollama returned status %d", resp.StatusCode)
	}

	var parsed ollamaResponse
	if err := json.NewDecoder(resp.Body).Decode(&parsed); err != nil {
		return nil, fmt.Errorf("decode ollama response: %w", err)
	}
	if len(parsed.Embedding) != p.dimensions {
		return nil, fmt.Errorf("expected %d-d embedding, got %d", p.dimensions, len(parsed.Embedding))
	}
	return parsed.Embedding, nil
}
