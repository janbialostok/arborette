package llm

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sort"
)

// deepinfraCompleter implements completer via DeepInfra's OpenAI-compatible API.
type deepinfraCompleter struct {
	apiKey  string
	model   string
	baseURL string
	client  *http.Client
}

func newDeepInfraCompleter(apiKey, model, baseURL string) (*deepinfraCompleter, error) {
	return &deepinfraCompleter{
		apiKey:  apiKey,
		model:   model,
		baseURL: baseURL,
		client:  http.DefaultClient,
	}, nil
}

// deepinfraRequest is a subset of the OpenAI chat completions request body.
type deepinfraRequest struct {
	Model     string             `json:"model"`
	MaxTokens int                `json:"max_tokens"`
	Messages  []deepinfraMessage `json:"messages"`
}

type deepinfraMessage struct {
	Role    string               `json:"role"`
	Content []deepinfraContentPart `json:"content"`
}

// deepinfraImageURL holds the URL for image/file content parts.
type deepinfraImageURL struct {
	URL string `json:"url"`
}

type deepinfraContentPart struct {
	Type     string             `json:"type"`
	Text     string             `json:"text,omitempty"`
	ImageURL *deepinfraImageURL `json:"image_url,omitempty"`
}

// deepinfraResponse is the relevant subset of the OpenAI chat completions body.
type deepinfraResponse struct {
	Choices []struct {
		Message struct {
			Content string `json:"content"`
		} `json:"message"`
		FinishReason string `json:"finish_reason"`
	} `json:"choices"`
}

func (d *deepinfraCompleter) complete(ctx context.Context, system, user string, schema map[string]any) (string, error) {
	return d.call(ctx, system, []deepinfraContentPart{{Type: "text", Text: user}}, schema)
}

func (d *deepinfraCompleter) completeWithPDF(ctx context.Context, system string, pdf []byte, instruction string, schema map[string]any) (string, error) {
	b64 := base64.StdEncoding.EncodeToString(pdf)
	parts := []deepinfraContentPart{
		{Type: "image_url", ImageURL: &deepinfraImageURL{URL: "data:application/pdf;base64," + b64}},
		{Type: "text", Text: instruction},
	}
	return d.call(ctx, system, parts, schema)
}

// sortedKeys returns the map's string keys in lexical order for deterministic output.
func sortedKeys(m map[string]any) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// schemaHint extracts the expected property names and types from a JSON schema
// map and returns a compact structural example to inject into the system prompt.
// It recursively reveals nested object and array-item schemas so the model
// understands what fields belong inside sub-objects and array elements.
func schemaHint(schema map[string]any) string {
	t, _ := schema["type"].(string)
	switch t {
	case "object":
		props, _ := schema["properties"].(map[string]any)
		if props == nil {
			return "{...}"
		}
		required, _ := schema["required"].([]any)
		requiredSet := make(map[string]bool, len(required))
		for _, r := range required {
			if s, ok := r.(string); ok {
				requiredSet[s] = true
			}
		}
		hint := "{"
		first := true
		keys := sortedKeys(props)
		for _, k := range keys {
			if !requiredSet[k] {
				continue
			}
			ps, _ := props[k].(map[string]any)
			if !first {
				hint += ", "
			}
			hint += `"` + k + `":` + schemaHint(ps)
			first = false
		}
		hint += "}"
		return hint
	case "array":
		items, _ := schema["items"].(map[string]any)
		if items == nil {
			return "[]"
		}
		return "[" + schemaHint(items) + "]"
	case "string":
		return `"..."`
	case "number":
		return "0"
	case "boolean":
		return "true"
	default:
		// Handle anyOf unions — show the first variant
		anyOf, _ := schema["anyOf"].([]any)
		if len(anyOf) > 0 {
			if firstSchema, ok := anyOf[0].(map[string]any); ok {
				return schemaHint(firstSchema)
			}
		}
		return "..."
	}
}

func (d *deepinfraCompleter) call(ctx context.Context, system string, content []deepinfraContentPart, schema map[string]any) (string, error) {
	if schema != nil {
		hint := schemaHint(schema)
		system = "You MUST respond with ONLY a valid JSON object. No preamble, no explanation, no markdown formatting.\n\n" + system
		if hint != "" {
			system = system + "\n\nExpected JSON structure: " + hint
		}
	}
	req := deepinfraRequest{
		Model:     d.model,
		MaxTokens: maxTokens,
		Messages: []deepinfraMessage{
			{Role: "system", Content: []deepinfraContentPart{{Type: "text", Text: system}}},
			{Role: "user", Content: content},
		},
	}

	body, err := json.Marshal(req)
	if err != nil {
		return "", fmt.Errorf("deepinfra marshal request: %w", err)
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, d.baseURL+"/chat/completions", bytes.NewReader(body))
	if err != nil {
		return "", fmt.Errorf("deepinfra create request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Authorization", "Bearer "+d.apiKey)

	resp, err := d.client.Do(httpReq)
	if err != nil {
		return "", fmt.Errorf("deepinfra request: %w", err)
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", fmt.Errorf("deepinfra read response: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("deepinfra API error (status %d): %s", resp.StatusCode, string(respBody))
	}

	var chatResp deepinfraResponse
	if err := json.Unmarshal(respBody, &chatResp); err != nil {
		return "", fmt.Errorf("deepinfra parse response: %w (body: %.200s)", err, string(respBody))
	}
	if len(chatResp.Choices) == 0 {
		return "", fmt.Errorf("deepinfra: empty choices")
	}
	contentStr := chatResp.Choices[0].Message.Content
	if chatResp.Choices[0].FinishReason == "length" {
		return "", errMaxTokens
	}
	if contentStr == "" {
		return "", fmt.Errorf("deepinfra: empty content (finish_reason: %s)", chatResp.Choices[0].FinishReason)
	}
	return contentStr, nil
}
