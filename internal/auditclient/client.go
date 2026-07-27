// Package auditclient appends audit records over the Orchestrator's internal
// audit-write API.
//
// The HTTP hop is mandatory, not stylistic: the audit boundary grants INSERT on
// audit_log to the orchestrator role alone, and the Sleep-Cycle Worker
// authenticates as the service role, which holds no privilege of any kind on
// that table (internal/store/migrations/0005_grants.up.sql). The Orchestrator is
// the sole audit-table writer, and this is how everyone else reaches it.
package auditclient

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"time"
)

// defaultTimeout bounds a single audit append so a stalled Orchestrator cannot
// block a long-running batch job.
const defaultTimeout = 30 * time.Second

// AuditError carries a message the Orchestrator returned in its response body.
type AuditError struct{ Message string }

func (e *AuditError) Error() string { return e.Message }

// Client posts audit records to the Orchestrator. It is built with primitive
// args (infra-constructor convention).
type Client struct {
	baseURL string
	client  *http.Client
}

// NewClient points a client at the Orchestrator base URL. A nil httpClient gets
// one bounded by defaultTimeout.
func NewClient(baseURL string, httpClient *http.Client) *Client {
	if httpClient == nil {
		httpClient = &http.Client{Timeout: defaultTimeout}
	}
	return &Client{baseURL: baseURL, client: httpClient}
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
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/internal/audit", bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("build audit request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.client.Do(req)
	if err != nil {
		return fmt.Errorf("call orchestrator /internal/audit: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		var decoded struct {
			Error string `json:"error"`
		}
		if json.NewDecoder(resp.Body).Decode(&decoded) == nil && decoded.Error != "" {
			return &AuditError{Message: decoded.Error}
		}
		return fmt.Errorf("orchestrator /internal/audit returned status %d", resp.StatusCode)
	}
	return nil
}
