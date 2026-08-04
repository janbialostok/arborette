// Package orchestratorclient is the HTTP client every other service uses to
// reach the Orchestrator's REST surface: the Sleep-Cycle Worker's audit writes
// and the MCP Server's goal-submission proxy.
//
// The audit hop is mandatory, not stylistic: the audit boundary grants INSERT on
// audit_log to the orchestrator role alone, and every other service
// authenticates as the service role, which holds no privilege of any kind on
// that table (internal/store/migrations/0005_grants.up.sql). The Orchestrator is
// the sole audit-table writer, and this is how everyone else reaches it.
package orchestratorclient

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/url"
	"time"

	"github.com/arborette/arborette/internal/service"
)

// defaultTimeout bounds a single call so a stalled Orchestrator cannot block a
// long-running batch job. A caller with a slower call of its own injects its own
// client rather than raising this.
const defaultTimeout = 30 * time.Second

// The audit and verification-event paths are internal and guarded by the shared
// secret; /goals is the analyst-facing intake.
const (
	auditPath              = "/internal/audit"
	verificationEventsPath = "/internal/verification-events"
	goalsPath              = "/goals"
)

// OrchestratorError is a non-success response carrying the message the
// Orchestrator returned in its own {"error":...} body. That message is
// caller-safe to surface (the Orchestrator masks its own internals), unlike this
// client's transport/marshal/decode errors, which reference the internal service
// address and must be masked by the caller. A response with no decodable body
// falls to a plain status error for the same reason: nothing vouched for its
// contents.
type OrchestratorError struct {
	Status  int
	Message string
}

func (e *OrchestratorError) Error() string { return e.Message }

// Client calls the Orchestrator's REST service. It is built with primitive args
// (infra-constructor convention).
type Client struct {
	baseURL   string
	client    *http.Client
	authToken string
}

// NewClient points a client at the Orchestrator base URL. authToken is the shared
// secret the internal write surfaces require; empty presents no credential, which
// is what a caller reaching only the open routes passes. A nil httpClient gets one
// bounded by defaultTimeout.
func NewClient(baseURL, authToken string, httpClient *http.Client) *Client {
	if httpClient == nil {
		httpClient = &http.Client{Timeout: defaultTimeout}
	}
	return &Client{baseURL: baseURL, client: httpClient, authToken: authToken}
}

type auditRequest struct {
	Action    string         `json:"action"`
	EventType string         `json:"event_type"`
	Detail    map[string]any `json:"detail"`
}

// Append records one audit event. Following the orchestrator's own call sites,
// action is the long/specific name and eventType the short/categorical one. The
// endpoint answers 201 with no body on success.
func (c *Client) Append(ctx context.Context, action, eventType string, detail map[string]any) error {
	body, err := json.Marshal(auditRequest{Action: action, EventType: eventType, Detail: detail})
	if err != nil {
		return fmt.Errorf("marshal audit request: %w", err)
	}
	req, err := c.newRequest(ctx, auditPath, "application/json", bytes.NewReader(body))
	if err != nil {
		return err
	}
	return c.do(req, http.StatusCreated, nil)
}

type verificationEventRequest struct {
	GoalID string         `json:"goal_id"`
	Event  map[string]any `json:"event"`
}

// PublishVerification delivers one verification transition to the orchestrator, which
// fans it out on the goal's SSE channel and records it to the audit trail in one
// authenticated call — both sinks through a single hop. It is the verification sibling
// of Append; the endpoint answers 201 with no body on success.
func (c *Client) PublishVerification(ctx context.Context, goalID string, event map[string]any) error {
	body, err := json.Marshal(verificationEventRequest{GoalID: goalID, Event: event})
	if err != nil {
		return fmt.Errorf("marshal verification event: %w", err)
	}
	req, err := c.newRequest(ctx, verificationEventsPath, "application/json", bytes.NewReader(body))
	if err != nil {
		return err
	}
	return c.do(req, http.StatusCreated, nil)
}

// SubmitGoal registers an analyst goal against a data source on the
// Orchestrator's read-only import mount. It sends the import_path branch of the
// Orchestrator's multipart goal-intake form -- an MCP tool call carries
// structured JSON args, not a multipart file part, so the on-disk import_path is
// the natural data-source path.
func (c *Client) SubmitGoal(ctx context.Context, goal, importPath string) (string, error) {
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

	req, err := c.newRequest(ctx, goalsPath, mw.FormDataContentType(), &buf)
	if err != nil {
		return "", err
	}
	var out struct {
		OptimizationFunctionID string `json:"optimization_function_id"`
	}
	if err := c.do(req, http.StatusCreated, &out); err != nil {
		return "", err
	}
	return out.OptimizationFunctionID, nil
}

// VerifyFinding asks the Orchestrator to causally verify one observational finding.
// Dispatch is asynchronous -- the endpoint answers 202 and the result arrives on the
// goal's SSE channel and in the audit trail -- so this reports only that the request
// was accepted.
func (c *Client) VerifyFinding(ctx context.Context, goalID, findingID string) error {
	path := goalsPath + "/" + url.PathEscape(goalID) + "/findings/" + url.PathEscape(findingID) + "/verify"
	req, err := c.newRequest(ctx, path, "application/json", nil)
	if err != nil {
		return err
	}
	return c.do(req, http.StatusAccepted, nil)
}

// newRequest builds a POST carrying body under contentType, stamped with the
// shared secret when one is configured. An unconfigured secret sends no header
// rather than an empty one: the guard fails open on an empty configured token,
// but a malformed credential would be rejected outright.
func (c *Client) newRequest(ctx context.Context, path, contentType string, body io.Reader) (*http.Request, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+path, body)
	if err != nil {
		return nil, fmt.Errorf("build orchestrator %s request: %w", path, err)
	}
	req.Header.Set("Content-Type", contentType)
	if c.authToken != "" {
		req.Header.Set("Authorization", "Bearer "+c.authToken)
	}
	return req, nil
}

// do sends req and enforces wantStatus, decoding the response body into out when
// the caller asked for one. The path names the endpoint in every error and comes
// off the request rather than a second argument, so an error can only ever name
// the endpoint that produced it.
func (c *Client) do(req *http.Request, wantStatus int, out any) error {
	path := req.URL.Path
	resp, err := c.client.Do(req)
	if err != nil {
		return fmt.Errorf("call orchestrator %s: %w", path, err)
	}
	defer service.DrainAndClose(resp)
	if resp.StatusCode != wantStatus {
		var body struct {
			Error string `json:"error"`
		}
		if json.NewDecoder(resp.Body).Decode(&body) == nil && body.Error != "" {
			return &OrchestratorError{Status: resp.StatusCode, Message: body.Error}
		}
		return fmt.Errorf("orchestrator %s returned status %d", path, resp.StatusCode)
	}
	if out == nil {
		return nil
	}
	if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
		return fmt.Errorf("decode orchestrator %s response: %w", path, err)
	}
	return nil
}
