package verifier

import (
	"context"
	"encoding/json"
	"log"
	"net/http"
	"sync"

	"github.com/arborette/arborette/internal/service"
)

// runner runs the Verifier's two dispatch kinds for a (goal, data-source) pair. It is
// a narrow local interface so the serve surface tests with a fake; the wired binary
// passes a *Worker, whose RunDiscovery and VerifyOne are both reentrant across calls.
type runner interface {
	RunDiscovery(ctx context.Context, goalID, datasourceRef string) error
	VerifyOne(ctx context.Context, goalID, interventionID, datasourceRef string, budgeted bool) error
}

// Server is the serve-mode HTTP surface of the Verifier. It mirrors the Sleep-Cycle
// worker's serve surface: the worker is wired once and this surface dispatches one
// discovery per POST /verifications, answering 202 before it completes (a discovery
// takes minutes) or 409 when a run for the same (goal, data-source) is already in
// flight on this instance. R4's "concurrent requesters wait on the one result" is
// delivered cross-process by the advisory-lock recheck loop inside RunDiscovery, not
// by this HTTP layer, which never blocks waiting for a result.
type Server struct {
	runner       runner
	maxBodyBytes int64

	mu       sync.Mutex
	inflight map[string]struct{}
}

// NewServer wires the serve surface to a runner and the request body cap (infra-
// constructor convention).
func NewServer(runner runner, maxBodyBytes int64) *Server {
	return &Server{
		runner:       runner,
		maxBodyBytes: maxBodyBytes,
		inflight:     make(map[string]struct{}),
	}
}

// Routes returns the mux for the serve-mode endpoint.
func (s *Server) Routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /verifications", s.handleVerification)
	return mux
}

// verificationRequest is the dispatch body. Kind discriminates the dispatch kind:
// "discovery" ensures the causal graph; "verify" runs one finding's atomic
// verification (InterventionID names the finding; Budgeted flags a budget-charged
// dispatch).
type verificationRequest struct {
	GoalID         string `json:"goal_id"`
	DatasourceRef  string `json:"datasource_ref"`
	Kind           string `json:"kind"`
	InterventionID string `json:"intervention_id,omitempty"`
	Budgeted       bool   `json:"budgeted,omitempty"`
}

// Dispatch kinds. An unknown kind is rejected with 422 so a later kind slots in
// without breaking the contract.
const (
	kindDiscovery = "discovery"
	kindVerify    = "verify"
)

// handleVerification accepts a discovery or verify dispatch and runs it
// asynchronously, answering 202 before it completes. The run goroutine is
// fire-and-forget on a background context, so it outlives the request but SIGTERM
// kills a mid-run job exactly as it would a one-shot.
//
// Only the discovery kind uses the in-instance (goal, data-source) claim guard (one
// graph per pair; a duplicate returns 409). The verify kind is deliberately not
// guarded here: the per-goal in-flight cap allows N>1 concurrent verifications for one
// goal (distinct interventions share the pair key), and DispatchAccept's coalesce on
// (goal, intervention, graph-version) is the sole concurrency/idempotency authority.
func (s *Server) handleVerification(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, s.maxBodyBytes)

	var req verificationRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		service.WriteErr(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if req.GoalID == "" || req.DatasourceRef == "" {
		service.WriteErr(w, http.StatusBadRequest, "goal_id and datasource_ref are required")
		return
	}

	switch req.Kind {
	case kindDiscovery:
		s.dispatchDiscovery(w, req)
	case kindVerify:
		s.dispatchVerify(w, req)
	default:
		service.WriteErr(w, http.StatusUnprocessableEntity, "unsupported verification kind")
	}
}

// dispatchDiscovery claims the (goal, data-source) key, answering 409 on a duplicate,
// and runs discovery asynchronously.
func (s *Server) dispatchDiscovery(w http.ResponseWriter, req verificationRequest) {
	key := req.GoalID + "\x00" + req.DatasourceRef
	if !s.claim(key) {
		service.WriteErr(w, http.StatusConflict, "a discovery for this goal and data source is already in flight")
		return
	}

	go func() {
		defer s.release(key)
		log.Printf("verifier: discovery run started for goal %q", req.GoalID)
		if err := s.runner.RunDiscovery(context.Background(), req.GoalID, req.DatasourceRef); err != nil {
			log.Printf("verifier: discovery for goal %q failed: %v", req.GoalID, err)
			return
		}
		log.Printf("verifier: discovery for goal %q complete", req.GoalID)
	}()

	service.WriteJSON(w, http.StatusAccepted, map[string]any{"goal_id": req.GoalID, "datasource_ref": req.DatasourceRef})
}

// dispatchVerify runs one finding's verification asynchronously. Concurrency and
// idempotency are DispatchAccept's job, not the HTTP layer's, so there is no in-flight
// claim here.
func (s *Server) dispatchVerify(w http.ResponseWriter, req verificationRequest) {
	if req.InterventionID == "" {
		service.WriteErr(w, http.StatusBadRequest, "intervention_id is required for a verify dispatch")
		return
	}

	go func() {
		log.Printf("verifier: verification started for goal %q intervention %q", req.GoalID, req.InterventionID)
		if err := s.runner.VerifyOne(context.Background(), req.GoalID, req.InterventionID, req.DatasourceRef, req.Budgeted); err != nil {
			log.Printf("verifier: verification for goal %q intervention %q failed: %v", req.GoalID, req.InterventionID, err)
			return
		}
		log.Printf("verifier: verification for goal %q intervention %q complete", req.GoalID, req.InterventionID)
	}()

	service.WriteJSON(w, http.StatusAccepted, map[string]any{
		"goal_id": req.GoalID, "intervention_id": req.InterventionID, "datasource_ref": req.DatasourceRef,
	})
}

// claim marks a (goal, data-source) key in flight, reporting false when a run is
// already live for it. release clears the entry once the run goroutine returns.
func (s *Server) claim(key string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, live := s.inflight[key]; live {
		return false
	}
	s.inflight[key] = struct{}{}
	return true
}

func (s *Server) release(key string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.inflight, key)
}
