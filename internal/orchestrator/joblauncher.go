package orchestrator

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"time"

	"github.com/arborette/arborette/internal/service"
)

// JobLauncher invokes a named ephemeral job (the Sleep-Cycle Worker) as an
// infra-level invocation, not a direct code call into the worker. The contract
// is the job/container name; the launcher owns how it is dispatched.
//
// Three implementers sit behind this one interface: StubLauncher (nothing
// configured -- logs and succeeds), HTTPLauncher (a worker running in serve mode
// locally), and the future AWS Batch SubmitJob (the production path). Swapping
// among them is a wiring choice in cmd/orchestrator, never a change to callers.
type JobLauncher interface {
	Launch(ctx context.Context, jobName string, args map[string]string) error
}

// StubLauncher records the named-job invocation and returns success. It does not
// shell out to docker: the compose orchestrator block mounts no Docker socket,
// so a real docker invocation would fail at runtime. The production seam (AWS
// Batch SubmitJob) replaces this behind the interface.
//
// Argument contract the production launcher must honor: the worker binary takes
// the goal as `-goal <optimization_function_id>` from the args map key of that
// name, or SLEEPCYCLE_GOAL_ID as an environment fallback.
type StubLauncher struct{}

// Launch logs the invocation the production launcher would dispatch.
func (StubLauncher) Launch(_ context.Context, jobName string, args map[string]string) error {
	log.Printf("orchestrator: sleep-cycle job %q invoked with args %v", jobName, args)
	return nil
}

// launchTimeout bounds one dispatch. Launch is only an acknowledgment -- the
// worker answers 202 before the cycle runs -- so a short timeout is right; a
// stalled dispatch must not hold the analyst's trigger request open.
const launchTimeout = 10 * time.Second

// LaunchError is a non-202 response from a launch endpoint, carrying the status
// and the message the worker returned in its {"error":...} body. That message is
// caller-safe to surface; a response with no decodable body falls to a plain
// status error, matching the orchestrator's other internal HTTP clients.
type LaunchError struct {
	Status  int
	Message string
}

func (e *LaunchError) Error() string { return e.Message }

// HTTPLauncher dispatches a job by POSTing the args map to a configured launch
// endpoint -- a worker running in serve mode. endpointURL is the full endpoint
// (e.g. http://sleepcycle-serve:8084/runs): the launcher POSTs to it verbatim and
// appends no path, so the same type points at any serve-mode worker's endpoint
// (the Verifier's differently-pathed endpoint reuses it unchanged). authToken is
// the shared secret the worker verifies; empty presents no credential.
type HTTPLauncher struct {
	endpointURL string
	client      *http.Client
	authToken   string
}

// NewHTTPLauncher points a launcher at a worker's launch endpoint. A nil
// httpClient gets one bounded by launchTimeout (infra-constructor convention).
func NewHTTPLauncher(endpointURL, authToken string, httpClient *http.Client) *HTTPLauncher {
	if httpClient == nil {
		httpClient = &http.Client{Timeout: launchTimeout}
	}
	return &HTTPLauncher{endpointURL: endpointURL, client: httpClient, authToken: authToken}
}

// Launch POSTs args as the JSON body to the configured endpoint, stamped with the
// shared secret when one is configured. jobName is log-only: the endpoint URL, not
// the name, selects the worker. Anything but 202 is an error carrying the status
// and the worker's decoded message.
func (l *HTTPLauncher) Launch(ctx context.Context, jobName string, args map[string]string) error {
	body, err := json.Marshal(args)
	if err != nil {
		return fmt.Errorf("marshal launch args: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, l.endpointURL, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("build launch request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	// An unconfigured secret sends no header rather than an empty one: the worker
	// fails open on an empty configured token, but a malformed credential is rejected.
	if l.authToken != "" {
		req.Header.Set("Authorization", "Bearer "+l.authToken)
	}

	log.Printf("orchestrator: launching job %q via %s", jobName, l.endpointURL)
	resp, err := l.client.Do(req)
	if err != nil {
		return fmt.Errorf("dispatch job %q: %w", jobName, err)
	}
	defer service.DrainAndClose(resp)
	if resp.StatusCode != http.StatusAccepted {
		var b struct {
			Error string `json:"error"`
		}
		if json.NewDecoder(resp.Body).Decode(&b) == nil && b.Error != "" {
			return &LaunchError{Status: resp.StatusCode, Message: b.Error}
		}
		return fmt.Errorf("launch endpoint returned status %d", resp.StatusCode)
	}
	return nil
}
