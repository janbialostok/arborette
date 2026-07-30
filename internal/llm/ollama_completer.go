package llm

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
)

// ollamaCompleter implements completer via Ollama's API.
type ollamaCompleter struct {
	endpoint string
	model    string
	client   *http.Client
}

func newOllamaCompleter(endpoint, model string) (*ollamaCompleter, error) {
	return &ollamaCompleter{
		endpoint: endpoint,
		model:    model,
		client:   http.DefaultClient,
	}, nil
}

// ollamaRequest is the Ollama chat request body.
type ollamaRequest struct {
	Model    string            `json:"model"`
	Messages []ollamaMessage   `json:"messages"`
	Stream   bool              `json:"stream"`
	Format   string            `json:"format,omitempty"`
	Options  map[string]any    `json:"options,omitempty"`
}

type ollamaMessage struct {
	Role    string              `json:"role"`
	Content string              `json:"content"`
	Images  []string            `json:"images,omitempty"`
}

// ollamaResponse is the non-streaming Ollama chat response.
type ollamaResponse struct {
	Message struct {
		Content string `json:"content"`
	} `json:"message"`
	Done bool `json:"done"`
}

func (o *ollamaCompleter) complete(ctx context.Context, system, user string, schema map[string]any) (string, error) {
	return o.call(ctx, system, user, nil, schema)
}

func (o *ollamaCompleter) completeWithPDF(ctx context.Context, system string, pdf []byte, instruction string, schema map[string]any) (string, error) {
	b64 := base64.StdEncoding.EncodeToString(pdf)
	return o.call(ctx, system, instruction, []string{b64}, schema)
}

func (o *ollamaCompleter) call(ctx context.Context, system, user string, images []string, schema map[string]any) (string, error) {
	if schema != nil {
		hint := schemaHint(schema)
		system = "You MUST respond with ONLY a valid JSON object. No preamble, no explanation, no markdown formatting.\n\n" + system
		if hint != "" {
			system = system + "\n\nExpected JSON structure: " + hint
		}
	}
	req := ollamaRequest{
		Model:    o.model,
		Stream:   false,
		Messages: []ollamaMessage{},
	}
	if system != "" {
		req.Messages = append(req.Messages, ollamaMessage{Role: "system", Content: system})
	}
	userMsg := ollamaMessage{Role: "user", Content: user}
	if len(images) > 0 {
		userMsg.Images = images
	}
	req.Messages = append(req.Messages, userMsg)

	if schema != nil {
		req.Format = "json"
		req.Options = map[string]any{
			"temperature": 0.0,
		}
	}

	body, err := json.Marshal(req)
	if err != nil {
		return "", fmt.Errorf("ollama marshal request: %w", err)
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, o.endpoint+"/api/chat", bytes.NewReader(body))
	if err != nil {
		return "", fmt.Errorf("ollama create request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")

	resp, err := o.client.Do(httpReq)
	if err != nil {
		return "", fmt.Errorf("ollama request: %w", err)
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", fmt.Errorf("ollama read response: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("ollama API error (status %d): %s", resp.StatusCode, string(respBody))
	}

	var chatResp ollamaResponse
	if err := json.Unmarshal(respBody, &chatResp); err != nil {
		return "", fmt.Errorf("ollama parse response: %w", err)
	}
	return chatResp.Message.Content, nil
}
