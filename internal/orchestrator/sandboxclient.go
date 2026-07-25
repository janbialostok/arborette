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

// IntrospectResponse returns the schema and per-target column bindings. For a
// document source Schema.Kind is "document", TargetBindings is empty, and Sample
// carries a first-page text excerpt grounding the field-introspection call.
type IntrospectResponse struct {
	Schema         schemaDTO       `json:"schema"`
	TargetBindings []TargetBinding `json:"target_bindings"`
	Sample         string          `json:"sample,omitempty"`
}

// DocumentTextRequest asks the sandbox for a document's ordered per-page text.
type DocumentTextRequest struct {
	DataSourceRef string `json:"data_source_ref"`
}

// DocumentTextResponse returns a document's per-page plain text (page index to
// text), the run-invariant substrate the loop caches for provenance search.
type DocumentTextResponse struct {
	Pages []string `json:"pages"`
}

// ExecuteRequest measures one aggregate over a data source under hard-constraint
// filters. ValueExpression, when set, is the objective value expression measured
// in place of the bare Target; ObjectiveLabel is the key the measured value is
// returned under. Both are omitempty so the legacy field-keyed request is
// unchanged.
type ExecuteRequest struct {
	DataSourceRef   string                  `json:"data_source_ref"`
	Type            domain.InterventionType `json:"type"`
	Aggregation     string                  `json:"aggregation"`
	Target          domain.Target           `json:"target"`
	ValueExpression *domain.Expression      `json:"value_expression,omitempty"`
	ObjectiveLabel  string                  `json:"objective_label,omitempty"`
	Filters         []domain.Constraint     `json:"filters"`
}

// ExecuteResponse carries the single measured aggregate keyed by the request's
// objective label when set, falling back to the target field for the legacy path.
type ExecuteResponse struct {
	Value map[string]any `json:"value"`
}

// SandboxError is a non-200 response from the sandbox, carrying the HTTP status
// and the decoded {error} body (or a status fallback when the body is empty).
// Callers classify by Status: a 400 is an analyst-fixable compile/validation
// failure whose Message is the authoritative reason; a 5xx is a sandbox fault.
type SandboxError struct {
	Status  int
	Message string
}

func (e *SandboxError) Error() string { return e.Message }

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

// DocumentText POSTs a per-page text request to /document/text. The whole read is
// deterministic PDF parsing, so it stays inside the 2-minute sandbox bound; only
// the LLM extraction runs orchestrator-side to escape that timeout.
func (c *SandboxClient) DocumentText(ctx context.Context, req DocumentTextRequest) (DocumentTextResponse, error) {
	var resp DocumentTextResponse
	if err := c.post(ctx, "/document/text", req, &resp); err != nil {
		return DocumentTextResponse{}, err
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
		message := fmt.Sprintf("sandbox %s returned status %d", path, resp.StatusCode)
		var body struct {
			Error string `json:"error"`
		}
		if json.NewDecoder(resp.Body).Decode(&body) == nil && body.Error != "" {
			message = body.Error
		}
		return &SandboxError{Status: resp.StatusCode, Message: message}
	}
	if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
		return fmt.Errorf("decode sandbox %s response: %w", path, err)
	}
	return nil
}
