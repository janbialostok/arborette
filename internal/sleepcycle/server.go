package sleepcycle

import (
	"context"
	"encoding/json"
	"log"
	"net/http"
	"sync"

	"github.com/arborette/arborette/internal/service"
)

// runner runs one Sleep Cycle for a goal. It is a narrow local interface so the
// serve surface can be tested with a fake; the wired binary passes a *Worker,
// whose Run already validates its config once and is reentrant across calls.
type runner interface {
	Run(ctx context.Context, goalID string) error
}

// Server is the serve-mode HTTP surface of the Sleep-Cycle Worker. It is the
// local/long-running alternative to the one-shot Batch job: the worker is wired
// once and this surface runs one cycle per POST /runs, matching AWS Batch
// SubmitJob semantics (accept, dispatch, answer before the work completes). The
// in-flight set collapses a goal's concurrent triggers to one live run.
type Server struct {
	runner       runner
	maxBodyBytes int64

	mu       sync.Mutex
	inflight map[string]struct{}
}

// NewServer wires the serve surface to a runner and the request body cap. It
// takes primitives and a narrow interface only (infra-constructor convention).
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
	mux.HandleFunc("POST /runs", s.handleRun)
	return mux
}

// runRequest is the launch body: one goal to run a cycle for. It mirrors the
// args map the Orchestrator's launcher POSTs (key optimization_function_id).
type runRequest struct {
	OptimizationFunctionID string `json:"optimization_function_id"`
}

// handleRun accepts a launch and dispatches the cycle asynchronously: a cycle
// takes minutes, so the handler returns 202 before it completes rather than
// holding the trigger request open, matching AWS Batch SubmitJob. Two deliberate
// local-mode divergences from Batch: a duplicate trigger for a goal whose run is
// still in flight returns 409 rather than queuing a second run; and the run
// goroutine is fire-and-forget on a background context, so it outlives the HTTP
// request but SIGTERM kills a mid-run cycle exactly as it would kill the one-shot
// Batch job -- graceful shutdown drains in-flight HTTP requests, not runs.
func (s *Server) handleRun(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, s.maxBodyBytes)

	var req runRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		service.WriteErr(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if req.OptimizationFunctionID == "" {
		service.WriteErr(w, http.StatusBadRequest, "optimization_function_id is required")
		return
	}

	id := req.OptimizationFunctionID
	if !s.claim(id) {
		service.WriteErr(w, http.StatusConflict, "a run for this goal is already in flight")
		return
	}

	go func() {
		defer s.release(id)
		log.Printf("sleepcycle: run started for goal %q", id)
		if err := s.runner.Run(context.Background(), id); err != nil {
			log.Printf("sleepcycle: run for goal %q failed: %v", id, err)
			return
		}
		log.Printf("sleepcycle: run for goal %q complete", id)
	}()

	service.WriteJSON(w, http.StatusAccepted, map[string]any{"optimization_function_id": id})
}

// claim marks a goal in flight, reporting false when a run is already live for it
// so the caller can reject the duplicate. release clears the entry once the run
// goroutine returns, so a later trigger for the same goal is accepted.
func (s *Server) claim(id string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, live := s.inflight[id]; live {
		return false
	}
	s.inflight[id] = struct{}{}
	return true
}

func (s *Server) release(id string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.inflight, id)
}
