// Package orchestrator is the REST/API service and the owner of the Active
// Hypothesis Loop. It is the only service the web UI talks to, the sole
// audit-table writer, and the owner of the goal registry. It registers analyst
// goals (translating them into an Evaluation Matrix via Claude), runs the
// interactive hypothesis-tree loop against the Sandbox Execution service on an
// explicit trigger, and streams live progress to the analyst's session.
package orchestrator

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net/http"

	"github.com/jackc/pgx/v5"

	"github.com/arborette/arborette/internal/domain"
	"github.com/arborette/arborette/internal/graph"
	"github.com/arborette/arborette/internal/heuristics"
	"github.com/arborette/arborette/internal/llm"
	"github.com/arborette/arborette/internal/store"
)

// Tree defaults are named constants so they are tunable against real sample data
// without touching call sites: breadth candidate interventions per node, depth
// refinement levels below the root. Worst case ≈ breadth + breadth² candidate
// evaluations per run.
const (
	defaultBreadth = 3
	defaultDepth   = 2
)

// The interfaces below are the narrow contracts the Server depends on, defined
// at the consumer so the handler layer is unit-testable with fakes and no infra.
// cmd/orchestrator passes the concrete *llm.Client, *SandboxClient,
// *store.GoalRegistry, *store.AuditLog, *objectstore.Client, and
// *heuristics.Service, which satisfy them.

// claudeClient is the two structured-output Claude calls the loop needs, named
// for the collaborator (the `claude` field) rather than either single method.
type claudeClient interface {
	GenerateEvaluationMatrix(ctx context.Context, goalText string) (domain.EvaluationMatrix, error)
	ProposeInterventionTree(ctx context.Context, goalText string, matrix domain.EvaluationMatrix, schema llm.SandboxSchema, node llm.TreeContext) (llm.Proposal, error)
}

type sandboxExecutor interface {
	Introspect(ctx context.Context, req IntrospectRequest) (IntrospectResponse, error)
	Execute(ctx context.Context, req ExecuteRequest) (ExecuteResponse, error)
}

type goalStore interface {
	Insert(ctx context.Context, goal store.Goal) error
	Get(ctx context.Context, optimizationFunctionID string) (store.Goal, error)
}

type auditStore interface {
	Append(ctx context.Context, record store.AuditRecord) error
}

type objectStore interface {
	NewKey(parts ...string) string
	Put(ctx context.Context, key string, r io.Reader, contentType string) error
}

type heuristicsService interface {
	Query(ctx context.Context, stateString string, k int) ([]heuristics.Match, error)
	Trace(ctx context.Context, metaHeuristicID string) ([]graph.CausalTriplet, error)
}

// Server is the HTTP surface of the orchestrator, holding its collaborators.
type Server struct {
	repo              graph.Repository
	goals             goalStore
	audits            auditStore
	objects           objectStore
	heur              heuristicsService
	claude            claudeClient
	sandbox           sandboxExecutor
	hub               *Hub
	jobs              JobLauncher
	identity          Identity
	localImportDir    string
	sleepCycleJobName string
}

// NewServer wires the server from its collaborators (infra-constructor
// convention).
func NewServer(
	repo graph.Repository,
	goals goalStore,
	audits auditStore,
	objects objectStore,
	heur heuristicsService,
	claude claudeClient,
	sandbox sandboxExecutor,
	hub *Hub,
	jobs JobLauncher,
	identity Identity,
	localImportDir, sleepCycleJobName string,
) *Server {
	return &Server{
		repo:              repo,
		goals:             goals,
		audits:            audits,
		objects:           objects,
		heur:              heur,
		claude:            claude,
		sandbox:           sandbox,
		hub:               hub,
		jobs:              jobs,
		identity:          identity,
		localImportDir:    localImportDir,
		sleepCycleJobName: sleepCycleJobName,
	}
}

// Routes returns the mux with method-prefixed patterns.
func (s *Server) Routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /goals", s.handleSubmitGoal)
	mux.HandleFunc("POST /goals/{id}/hypothesis-loop", s.handleTriggerLoop)
	mux.HandleFunc("GET /goals/{id}/stream", s.handleStream)
	mux.HandleFunc("POST /goals/{id}/sleep-cycle", s.handleTriggerSleepCycle)
	mux.HandleFunc("GET /heuristics/search", s.handleHeuristicSearch)
	mux.HandleFunc("GET /heuristics/{id}/trace", s.handleHeuristicTrace)
	mux.HandleFunc("POST /internal/audit", s.handleAudit)
	return mux
}

// lookupGoal fetches a goal, writing a 404 for a missing id and a masked 500
// otherwise. The bool is false when a response has already been written.
func (s *Server) lookupGoal(ctx context.Context, w http.ResponseWriter, id string) (store.Goal, bool) {
	goal, err := s.goals.Get(ctx, id)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			writeErr(w, http.StatusNotFound, "goal not found")
			return store.Goal{}, false
		}
		log.Printf("orchestrator: get goal %q: %v", id, err)
		writeErr(w, http.StatusInternalServerError, "internal error")
		return store.Goal{}, false
	}
	return goal, true
}

type errorResponse struct {
	Error string `json:"error"`
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(v); err != nil {
		log.Printf("orchestrator: encode response: %v", err)
	}
}

func writeErr(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, errorResponse{Error: msg})
}
