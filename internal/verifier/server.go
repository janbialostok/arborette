package verifier

import (
	"context"
	"encoding/json"
	"log"
	"net/http"
	"sync"

	"github.com/arborette/arborette/internal/service"
)

// runner runs one causal discovery for a (goal, data-source) pair. It is a narrow
// local interface so the serve surface tests with a fake; the wired binary passes a
// *Worker, whose RunDiscovery is reentrant across calls.
type runner interface {
	RunDiscovery(ctx context.Context, goalID, datasourceRef string) error
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

// verificationRequest is the dispatch body. Kind discriminates the verification kind
// so a later shell can add "verify" additively; this shell handles only "discovery".
type verificationRequest struct {
	GoalID        string `json:"goal_id"`
	DatasourceRef string `json:"datasource_ref"`
	Kind          string `json:"kind"`
}

// kindDiscovery is the only verification kind this shell handles. An unknown kind is
// rejected with 422 so a later shell's kind slots in without breaking the contract.
const kindDiscovery = "discovery"

// handleVerification accepts a discovery dispatch and runs it asynchronously,
// answering 202 before it completes. A duplicate dispatch for a (goal, data-source)
// already in flight on this instance returns 409 (retry later) — the HTTP layer never
// blocks; the cross-process wait is the advisory-lock loop's job. The run goroutine
// is fire-and-forget on a background context, so it outlives the request but SIGTERM
// kills a mid-run discovery exactly as it would kill a one-shot job.
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
	if req.Kind != kindDiscovery {
		service.WriteErr(w, http.StatusUnprocessableEntity, "unsupported verification kind")
		return
	}

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
