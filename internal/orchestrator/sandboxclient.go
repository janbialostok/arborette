package orchestrator

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"github.com/arborette/arborette/internal/domain"
)

// defaultSandboxTimeout bounds a single sandbox call so the background
// hypothesis loop (which runs on a non-cancellable context) cannot block forever
// on a stalled sandbox.
const defaultSandboxTimeout = 2 * time.Minute

// The request/response structs below are orchestrator-local, CGO-free copies of
// the Sandbox Execution service's wire contract (internal/sandbox/server.go).
// The orchestrator must NOT import internal/sandbox: that package blank-imports
// the CGO DuckDB driver, which would break the orchestrator's CGO_ENABLED=0
// distroless-static build. Their fields are typed from the dependency-free
// internal/domain package. Keep them byte-for-byte JSON-compatible with the
// sandbox structs — a documented coupling.

type columnDTO struct {
	Name string `json:"name"`
	Type string `json:"type"`
}

type schemaDTO struct {
	Kind    string      `json:"kind"`
	Columns []columnDTO `json:"columns"`
}

// TargetBinding reports which introspected column backs an optimization target;
// Matched is false with an empty Column when no column matched.
type TargetBinding struct {
	Target  string `json:"target"`
	Column  string `json:"column"`
	Matched bool   `json:"matched"`
}

// IntrospectRequest asks the sandbox for a data source's schema plus a binding
// of each optimization target to a column.
type IntrospectRequest struct {
	DataSourceRef string          `json:"data_source_ref"`
	Targets       []domain.Target `json:"targets"`
}

// IntrospectResponse returns the schema and per-target column bindings.
type IntrospectResponse struct {
	Schema         schemaDTO       `json:"schema"`
	TargetBindings []TargetBinding `json:"target_bindings"`
}

// ExecuteRequest measures one aggregate over a data source under hard-constraint
// filters.
type ExecuteRequest struct {
	DataSourceRef string                  `json:"data_source_ref"`
	Type          domain.InterventionType `json:"type"`
	Aggregation   string                  `json:"aggregation"`
	Target        domain.Target           `json:"target"`
	Filters       []domain.Constraint     `json:"filters"`
}

// ExecuteResponse carries the single measured aggregate keyed by target field.
type ExecuteResponse struct {
	Value map[string]any `json:"value"`
}

// SandboxClient calls the Sandbox Execution HTTP service. It is built with
// primitive args (infra-constructor convention).
type SandboxClient struct {
	baseURL string
	client  *http.Client
}

// NewSandboxClient points a client at the sandbox base URL. A nil httpClient
// gets one bounded by defaultSandboxTimeout.
func NewSandboxClient(baseURL string, httpClient *http.Client) *SandboxClient {
	if httpClient == nil {
		httpClient = &http.Client{Timeout: defaultSandboxTimeout}
	}
	return &SandboxClient{baseURL: baseURL, client: httpClient}
}

// Introspect POSTs an introspection request to /introspect.
func (c *SandboxClient) Introspect(ctx context.Context, req IntrospectRequest) (IntrospectResponse, error) {
	var resp IntrospectResponse
	if err := c.post(ctx, "/introspect", req, &resp); err != nil {
		return IntrospectResponse{}, err
	}
	return resp, nil
}

// Execute POSTs an execute request to /execute.
func (c *SandboxClient) Execute(ctx context.Context, req ExecuteRequest) (ExecuteResponse, error) {
	var resp ExecuteResponse
	if err := c.post(ctx, "/execute", req, &resp); err != nil {
		return ExecuteResponse{}, err
	}
	return resp, nil
}

func (c *SandboxClient) post(ctx context.Context, path string, in, out any) error {
	body, err := json.Marshal(in)
	if err != nil {
		return fmt.Errorf("marshal sandbox request: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+path, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("build sandbox request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.client.Do(req)
	if err != nil {
		return fmt.Errorf("call sandbox %s: %w", path, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("sandbox %s returned status %d", path, resp.StatusCode)
	}
	if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
		return fmt.Errorf("decode sandbox %s response: %w", path, err)
	}
	return nil
}
