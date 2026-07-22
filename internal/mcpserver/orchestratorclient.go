// Package mcpserver is the read-side interface for downstream AI agents: it
// exposes the accumulated Meta-Heuristic knowledge (get_optimized_heuristics,
// trace_causal_chain) and goal registration (submit_analyst_goal) as MCP tools
// over Streamable HTTP. The read tools sit on the shared heuristics query seam;
// the server never mutates state directly, proxying goal submission to the
// Orchestrator's REST surface.
package mcpserver

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"mime/multipart"
	"net/http"
	"time"
)

// defaultOrchestratorTimeout bounds a single goal-submission proxy call so an MCP
// tool invocation cannot block forever on a stalled Orchestrator.
const defaultOrchestratorTimeout = 2 * time.Minute

// OrchestratorError carries a message the Orchestrator returned in its response
// body. That message is analyst-safe to surface (the Orchestrator masks its own
// internals), unlike the client's transport/marshal/decode errors, which
// reference the internal service address and must be masked by the caller.
type OrchestratorError struct{ Message string }

func (e *OrchestratorError) Error() string { return e.Message }

// OrchestratorClient calls the Orchestrator's REST service to proxy writes the
// MCP server never performs directly. It is built with primitive args
// (infra-constructor convention).
type OrchestratorClient struct {
	baseURL string
	client  *http.Client
}

// NewOrchestratorClient points a client at the Orchestrator base URL. A nil
// httpClient gets one bounded by defaultOrchestratorTimeout.
func NewOrchestratorClient(baseURL string, httpClient *http.Client) *OrchestratorClient {
	if httpClient == nil {
		httpClient = &http.Client{Timeout: defaultOrchestratorTimeout}
	}
	return &OrchestratorClient{baseURL: baseURL, client: httpClient}
}

// SubmitGoal registers an analyst goal against a data source on the
// Orchestrator's read-only import mount. It sends the import_path branch of the
// Orchestrator's multipart goal-intake form -- an MCP tool call carries
// structured JSON args, not a multipart file part, so the on-disk import_path is
// the natural data-source path. On a non-201 response carrying the
// Orchestrator's own {"error":...} body, it returns that message as an
// OrchestratorError (analyst-safe, so the caller sees the real validation
// reason); every other failure is a plain error the caller masks.
func (c *OrchestratorClient) SubmitGoal(ctx context.Context, goal, importPath string) (string, error) {
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	if err := mw.WriteField("goal", goal); err != nil {
		return "", fmt.Errorf("write goal field: %w", err)
	}
	if err := mw.WriteField("import_path", importPath); err != nil {
		return "", fmt.Errorf("write import_path field: %w", err)
	}
	if err := mw.Close(); err != nil {
		return "", fmt.Errorf("close multipart: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/goals", &buf)
	if err != nil {
		return "", fmt.Errorf("build goal request: %w", err)
	}
	req.Header.Set("Content-Type", mw.FormDataContentType())

	resp, err := c.client.Do(req)
	if err != nil {
		return "", fmt.Errorf("call orchestrator /goals: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusCreated {
		var body struct {
			Error string `json:"error"`
		}
		if err := json.NewDecoder(resp.Body).Decode(&body); err == nil && body.Error != "" {
			return "", &OrchestratorError{Message: body.Error}
		}
		return "", fmt.Errorf("orchestrator /goals returned status %d", resp.StatusCode)
	}

	var out struct {
		OptimizationFunctionID string `json:"optimization_function_id"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return "", fmt.Errorf("decode orchestrator /goals response: %w", err)
	}
	return out.OptimizationFunctionID, nil
}
