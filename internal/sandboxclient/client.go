// Package sandboxclient is the HTTP client every non-CGO service uses to reach
// the Sandbox Execution service. It is shared by the Orchestrator's hypothesis
// loop and the Sleep-Cycle Worker's lattice search.
//
// The request/response structs below are CGO-free copies of the Sandbox
// Execution service's wire contract (internal/sandbox/server.go). Callers must
// NOT import internal/sandbox: that package blank-imports the CGO DuckDB
// driver, which would break their CGO_ENABLED=0 distroless-static builds. Their
// fields are typed from the dependency-free internal/domain package. Keep them
// byte-for-byte JSON-compatible with the sandbox structs — a documented coupling.
package sandboxclient

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"github.com/arborette/arborette/internal/domain"
	"github.com/arborette/arborette/internal/service"
)

// defaultSandboxTimeout bounds a single sandbox call so a background caller
// (the hypothesis loop, the Sleep-Cycle search) cannot block forever on a
// stalled sandbox.
const defaultSandboxTimeout = 2 * time.Minute

// RowCountKey is the reserved Value key carrying a measurement's matched row
// count when IncludeRowCount is set. RenderObjectiveLabel output is always an
// agg(expr) shape, so it can never collide with this key.
const RowCountKey = "row_count"

// Column is one introspected column name and its type. DistinctValues carries the
// column's distinct value set when it is a low-cardinality categorical column, and
// is nil otherwise (high-cardinality, continuous, or not probed); omitempty so the
// legacy value-less schema is unchanged on the wire.
type Column struct {
	Name           string   `json:"name"`
	Type           string   `json:"type"`
	DistinctValues []string `json:"distinct_values,omitempty"`
}

// Schema is a data source's introspected shape.
type Schema struct {
	Kind    string   `json:"kind"`
	Columns []Column `json:"columns"`
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
	Schema         Schema          `json:"schema"`
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
// returned under; IncludeRowCount additionally returns the matched row count
// under RowCountKey, measured in the same scan. All three are omitempty so the
// legacy field-keyed request is unchanged.
type ExecuteRequest struct {
	DataSourceRef   string                  `json:"data_source_ref"`
	Type            domain.InterventionType `json:"type"`
	Aggregation     string                  `json:"aggregation"`
	Target          domain.Target           `json:"target"`
	ValueExpression *domain.Expression      `json:"value_expression,omitempty"`
	ObjectiveLabel  string                  `json:"objective_label,omitempty"`
	EntityKeyColumn string                  `json:"entity_key_column,omitempty"`
	TimeColumn      string                  `json:"time_column,omitempty"`
	IncludeRowCount bool                    `json:"include_row_count,omitempty"`
	Filters         []domain.Constraint     `json:"filters"`
}

// ExecuteResponse carries the single measured aggregate keyed by the request's
// objective label when set, falling back to the target field for the legacy
// path, plus the reserved RowCountKey entry when IncludeRowCount was set.
type ExecuteResponse struct {
	Value map[string]any `json:"value"`
}

// RowCount reads the reserved row-count key from a response, tolerating the
// float64 and json.Number shapes JSON decoding produces. The typed accessor
// lives beside the DTO so no caller re-derives the number handling.
func RowCount(resp ExecuteResponse) (int64, bool) {
	raw, ok := resp.Value[RowCountKey]
	if !ok || raw == nil {
		return 0, false
	}
	switch n := raw.(type) {
	case float64:
		return int64(n), true
	case json.Number:
		i, err := n.Int64()
		return i, err == nil
	default:
		return 0, false
	}
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

// Client calls the Sandbox Execution HTTP service. It is built with primitive
// args (infra-constructor convention).
type Client struct {
	baseURL   string
	client    *http.Client
	authToken string
}

// NewClient points a client at the sandbox base URL. authToken is the shared secret
// the sandbox verifies on every route; empty presents no credential (which the
// sandbox's own empty-token fail-open accepts). A nil httpClient gets one bounded by
// defaultSandboxTimeout.
func NewClient(baseURL, authToken string, httpClient *http.Client) *Client {
	if httpClient == nil {
		httpClient = &http.Client{Timeout: defaultSandboxTimeout}
	}
	return &Client{baseURL: baseURL, client: httpClient, authToken: authToken}
}

// Introspect POSTs an introspection request to /introspect.
func (c *Client) Introspect(ctx context.Context, req IntrospectRequest) (IntrospectResponse, error) {
	var resp IntrospectResponse
	if err := c.post(ctx, "/introspect", req, &resp); err != nil {
		return IntrospectResponse{}, err
	}
	return resp, nil
}

// Execute POSTs an execute request to /execute.
func (c *Client) Execute(ctx context.Context, req ExecuteRequest) (ExecuteResponse, error) {
	var resp ExecuteResponse
	if err := c.post(ctx, "/execute", req, &resp); err != nil {
		return ExecuteResponse{}, err
	}
	return resp, nil
}

// DocumentText POSTs a per-page text request to /document/text. The whole read is
// deterministic PDF parsing, so it stays inside the 2-minute sandbox bound; only
// the LLM extraction runs orchestrator-side to escape that timeout.
func (c *Client) DocumentText(ctx context.Context, req DocumentTextRequest) (DocumentTextResponse, error) {
	var resp DocumentTextResponse
	if err := c.post(ctx, "/document/text", req, &resp); err != nil {
		return DocumentTextResponse{}, err
	}
	return resp, nil
}

func (c *Client) post(ctx context.Context, path string, in, out any) error {
	body, err := json.Marshal(in)
	if err != nil {
		return fmt.Errorf("marshal sandbox request: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+path, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("build sandbox request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	// An unconfigured secret sends no header rather than an empty one: the sandbox
	// fails open on an empty configured token, but a malformed credential would be
	// rejected outright.
	if c.authToken != "" {
		req.Header.Set("Authorization", "Bearer "+c.authToken)
	}

	resp, err := c.client.Do(req)
	if err != nil {
		return fmt.Errorf("call sandbox %s: %w", path, err)
	}
	defer service.DrainAndClose(resp)
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
