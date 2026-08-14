// Package orchestrator is the REST/API service and the owner of the Active
// Hypothesis Loop. It is the only service the web UI talks to, the sole
// audit-table writer, and the owner of the goal registry. It registers analyst
// goals (translating them into an Evaluation Matrix via Claude), runs the
// interactive hypothesis-tree loop against the Sandbox Execution service on an
// explicit trigger, and streams live progress to the analyst's session.
package orchestrator

import (
	"context"
	"errors"
	"io"
	"log"
	"net/http"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/arborette/arborette/internal/domain"
	"github.com/arborette/arborette/internal/graph"
	"github.com/arborette/arborette/internal/heuristics"
	"github.com/arborette/arborette/internal/llm"
	"github.com/arborette/arborette/internal/service"
	"github.com/arborette/arborette/internal/store"
)

// Tree defaults are named constants so they are tunable against real sample data
// without touching call sites: breadth candidate interventions per node, depth
// refinement levels below the root. Worst case ≈ breadth + breadth² + breadth³ +
// breadth⁴ candidate evaluations per run at depth 4; improving-branch pruning bounds
// the fan-out well below that in practice, but loopTimeout/replayBufferSize are sized
// for the worst case.
const (
	defaultBreadth = 3
	defaultDepth   = 4
)

// The interfaces below are the narrow contracts the Server depends on, defined
// at the consumer so the handler layer is unit-testable with fakes and no infra.

// claudeClient is the structured-output Claude calls the loop needs, named for
// the collaborator (the `claude` field) rather than any single method. The two
// document methods sit here beside the tabular ones: the document intake path
// derives extractable fields, and the extraction loop measures each field.
type claudeClient interface {
	GenerateEvaluationMatrix(ctx context.Context, goalText string, schema llm.SandboxSchema, windowed bool) (domain.EvaluationMatrix, error)
	RepairEvaluationMatrix(ctx context.Context, goalText string, schema llm.SandboxSchema, prior domain.EvaluationMatrix, validationErr string, windowed bool) (domain.EvaluationMatrix, error)
	ProposeInterventionTree(ctx context.Context, goalText string, matrix domain.EvaluationMatrix, schema llm.SandboxSchema, node llm.TreeContext) (llm.Proposal, error)
	RepairInterventionTree(ctx context.Context, goalText string, matrix domain.EvaluationMatrix, schema llm.SandboxSchema, node llm.TreeContext, prior llm.Proposal, validationErr string) (llm.Proposal, error)
	IntrospectDocumentFields(ctx context.Context, goalText, sample string) ([]domain.TargetField, error)
	Extract(ctx context.Context, pdf []byte, field domain.TargetField, method string) (string, float64, error)
	ClassifyGoalIntent(ctx context.Context, goal llm.GoalIntentInput) (llm.GoalIntentResult, error)
}

// chatStreamer is the streaming Claude the agent-preview endpoint drives. It is
// a separate collaborator from claudeClient because the preview runs on a
// different API surface -- streamed, and connected to the MCP server -- and
// because a handler streaming to the browser needs no structured-output calls.
type chatStreamer interface {
	Chat(ctx context.Context, system string, msgs []llm.ChatMessage, emit func(llm.ChatEvent) error) error
}

// graphRepo is the graph surface the handlers and the loop need: the triplet
// writes the loop persists, the extraction reads and resolution writes the
// review surface drives, and the causal reads and corrections the router routes on --
// the finding lookup behind an explicit verify, the eligible-findings pool
// auto-promotion ranks, and the analyst's edge correction.
type graphRepo interface {
	CreateState(ctx context.Context, s domain.State) error
	CreateIntervention(ctx context.Context, i domain.Intervention) error
	CreateOutcome(ctx context.Context, o domain.Outcome) error
	CreatePreConditionFor(ctx context.Context, stateID, interventionID string) error
	CreateProduced(ctx context.Context, interventionID, outcomeID string, edge domain.ProducedEdge) error
	ListExtractionOutcomes(ctx context.Context, goalID string) ([]graph.ExtractionOutcome, error)
	GetExtractionOutcome(ctx context.Context, outcomeID string) (graph.ExtractionOutcome, error)
	UpdateOutcomeVerification(ctx context.Context, outcomeID string, status domain.VerificationStatus, confidence float64) error
	CorrectOutcome(ctx context.Context, outcomeID string, value map[string]any, provenance *domain.ProvenanceLocator, status domain.VerificationStatus, confidence float64) error
	GetCausalGraph(ctx context.Context, goalID, datasourceRef string) (domain.CausalGraph, bool, error)
	GetIntervention(ctx context.Context, id string) (domain.Intervention, error)
	GetMetaHeuristic(ctx context.Context, id string) (domain.MetaHeuristic, error)
	ListEligibleFindings(ctx context.Context, goalID string) ([]graph.CausalTriplet, error)
	CorrectCausalEdge(ctx context.Context, goalID, datasourceRef string, correction domain.EdgeCorrection) (int, []string, error)
	DeleteMetaHeuristic(ctx context.Context, id string) error
	DeleteGoalGraph(ctx context.Context, goalID string) error
}

// causalVerificationStore is the causal-verification surface the router reads and
// invalidates: the analyst-facing listing, and the staleness marking a graph
// correction triggers. The records themselves are written by the Verifier.
type causalVerificationStore interface {
	ListForGoal(ctx context.Context, goalID string) ([]store.CausalVerification, error)
	MarkStale(ctx context.Context, goalID string, columns []string) ([]string, error)
}

// graphLocker is the cross-process single-flight over a (goal, data-source) key --
// the same Postgres advisory lock discovery runs behind. A correction is a
// read-modify-write of the whole edge set, so it has to hold that lock too: two
// interleaved corrections would each copy forward the version they read and one
// analyst's edit would silently vanish.
type graphLocker interface {
	TryAcquireDiscoveryLock(ctx context.Context, goalID, datasourceRef string) (release func(), acquired bool, err error)
}

type sandboxExecutor interface {
	Introspect(ctx context.Context, req IntrospectRequest) (IntrospectResponse, error)
	Execute(ctx context.Context, req ExecuteRequest) (ExecuteResponse, error)
	DocumentText(ctx context.Context, req DocumentTextRequest) (DocumentTextResponse, error)
}

type goalStore interface {
	Insert(ctx context.Context, goal store.Goal) error
	RegisterDataSourceRef(ctx context.Context, ref string) error
	DataSourceRefExists(ctx context.Context, ref string) (bool, error)
	Get(ctx context.Context, optimizationFunctionID string) (store.Goal, error)
	List(ctx context.Context) ([]store.Goal, error)
	ListByDataset(ctx context.Context, datasetID string) ([]store.Goal, error)
	SetClaimError(ctx context.Context, optimizationFunctionID, reason string) error
	SetDatasetID(ctx context.Context, optimizationFunctionID, datasetID string) error
	Delete(ctx context.Context, optimizationFunctionID string) error
	// The access-scoped reads are the ownership gate: the goal inherits its
	// dataset's access set, so every goal-keyed surface answers 404 uniformly
	// for "missing" and "exists but not yours".
	GetAccessible(ctx context.Context, optimizationFunctionID, userID string) (store.Goal, error)
	ListAccessible(ctx context.Context, userID string) ([]store.Goal, error)
	ListByDatasetAccessible(ctx context.Context, datasetID, userID string) ([]store.Goal, error)
}

// runStore is the run-lifecycle surface the loop and the objectives list need.
// FailOrphaned is deliberately absent: boot-time reconciliation calls it on the
// concrete store in cmd/orchestrator, not through the Server.
type runStore interface {
	Create(ctx context.Context, runID, optimizationFunctionID string) error
	SetStatus(ctx context.Context, runID string, status store.RunStatus, failureReason string) error
	LatestByGoal(ctx context.Context, goalIDs []string) (map[string]store.Run, error)
}

// verificationQueue is the human-review queue surface the loop (routing) and the
// verification handlers (listing, claiming, repairing) share. Enqueue and
// EnqueueResolved are separate rather than one upsert because they answer
// different questions: the loop queues a below-threshold outcome for later
// review, while a resolution of a never-queued outcome records review that has
// already happened.
type verificationQueue interface {
	Enqueue(ctx context.Context, entry store.VerificationEntry) error
	EnqueueResolved(ctx context.Context, entry store.VerificationEntry, resolution store.QueueResolution, correctedValue string) error
	Resolve(ctx context.Context, outcomeID string, resolution store.QueueResolution, correctedValue string) error
	Unclaim(ctx context.Context, outcomeID string, resolution store.QueueResolution) error
	GetByOutcome(ctx context.Context, outcomeID string) (store.VerificationEntry, error)
	ListForGoal(ctx context.Context, goalID string, status store.QueueStatus) ([]store.VerificationEntry, error)
}

type auditStore interface {
	Append(ctx context.Context, record store.AuditRecord) error
}

// userStore is the accounts surface the handlers and the session guard need. It
// is a narrow slice of store.UserSessionStore: registration, sign-in lookup,
// the /me and admin reads, the seed/last-admin oracle, and the field-mutation
// write. A fake can stand in for any of these without touching Postgres.
type userStore interface {
	Create(ctx context.Context, username, passwordHash string, seedAdmin bool) (store.User, error)
	GetByUsername(ctx context.Context, username string) (store.User, error)
	GetByID(ctx context.Context, id string) (store.User, error)
	List(ctx context.Context) ([]store.User, error)
	CountActiveAdmins(ctx context.Context) (int, error)
	Update(ctx context.Context, id string, updates store.UserUpdate) (store.User, error)
	// EarliestActiveAdmin resolves the caretaker owner that pre-ownership and
	// system-created datasets are attributed to, and the only account allowed
	// to read the legacy NULL-goal heuristic corpus.
	EarliestActiveAdmin(ctx context.Context) (store.User, error)
}

// sessionStore is the signed-in-cookie ledger surface: create on sign-in, look
// up on every guarded request, delete on sign-out.
type sessionStore interface {
	CreateSession(ctx context.Context, tokenHash, userID string) (store.Session, error)
	GetSessionByTokenHash(ctx context.Context, tokenHash string) (store.Session, error)
	DeleteSessionByTokenHash(ctx context.Context, tokenHash string) error
}

type objectStore interface {
	NewKey(parts ...string) string
	Put(ctx context.Context, key string, r io.Reader, contentType string) error
	Get(ctx context.Context, key string) (io.ReadCloser, error)
	Delete(ctx context.Context, key string) error
}

// datasetStore is the datasets-table seam the inventory, editor, and delete
// handlers share. It is a narrow slice of store.DatasetStore: exactly the
// operations the HTTP surface needs, so a fake can stand in without pulling in
// the whole persistence layer.
type datasetStore interface {
	Create(ctx context.Context, dataset store.Dataset) (string, error)
	Get(ctx context.Context, id string) (store.Dataset, error)
	GetByRef(ctx context.Context, ref string) (store.Dataset, error)
	List(ctx context.Context, query string) ([]store.Dataset, error)
	Update(ctx context.Context, id, name, description string, status store.DatasetStatus) error
	CountObjectives(ctx context.Context, id string) (int, error)
	ListObjectives(ctx context.Context, datasetID string) ([]store.Goal, error)
	Delete(ctx context.Context, id string) error
	Touch(ctx context.Context, id string) error
	DataSourceRefUsage(ctx context.Context, ref string) (datasets, goals int, err error)
	DeleteDataSourceRef(ctx context.Context, ref string) error
	// The access-scoped surface is the ownership gate every dataset route goes
	// through: lists are filtered by the acting user's access, single-object
	// reads and existence checks answer 404/uniformly, and the share CRUD is
	// the working-access grant an owner manages.
	GetAccessible(ctx context.Context, id, userID string) (store.Dataset, error)
	ListAccessible(ctx context.Context, query, userID string) ([]store.Dataset, error)
	CanAccess(ctx context.Context, id, userID string) (bool, error)
	Share(ctx context.Context, datasetID, userID, sharedBy string) error
	RevokeShare(ctx context.Context, datasetID, userID string) error
	ListShares(ctx context.Context, datasetID string) ([]store.ShareGrant, error)
}

// embeddingsStore is the pgvector seam the delete paths use to retire a
// Meta-Heuristic or a goal's whole embedding corpus. Both deletes are idempotent:
// removing an absent row is not an error.
type embeddingsStore interface {
	Delete(ctx context.Context, nodeID string) error
	DeleteByGoal(ctx context.Context, goalID string) error
}

type heuristicsService interface {
	Query(ctx context.Context, stateString string, k int, scope store.SearchScope) ([]heuristics.Match, error)
	Trace(ctx context.Context, metaHeuristicID string) ([]graph.CausalTriplet, error)
}

// RouterConfig bundles the knobs that govern when verification runs: whether
// autonomous auto-promotion is on at all (the operator kill switch), how many
// findings it promotes and at what shrinkage level it ranks them, and how many stale
// records one graph correction may re-dispatch. They travel as one value because
// they are one policy, and because four bare ints and bools in a positional
// constructor is a slot-swap waiting to happen.
type RouterConfig struct {
	AutoPromoteEnabled    bool
	AutoPromoteTopN       int
	AutoPromoteShrinkageK int
	StaleReverifyCap      int
}

// SessionConfig bundles the accounts/sessions collaborators and the cookie
// knobs in one value, mirroring RouterConfig's bundling of a related policy:
// three positional args where two could easily be swapped is a slot-swap
// waiting to happen. A nil Users/Sessions fails the session guard open (unit
// tests that do not exercise accounts leave them unset); production always
// wires store.NewUserSessionStore. CookieName defaults to "arborette_session"
// when empty; CookieSecure marks the cookie Secure-only (set via
// ARBORETTE_SESSION_SECURE, off by default for local plain-HTTP compose).
type SessionConfig struct {
	Users        userStore
	Sessions     sessionStore
	CookieName   string
	CookieSecure bool
}

// Server is the HTTP surface of the orchestrator, holding its collaborators.
// histograms holds the confidence distribution of each in-flight run, keyed by
// goal like the hub; see registerHistogram for why that state is in-memory and
// how its lifetime is serialized against the hub's.
type Server struct {
	repo                graphRepo
	goals               goalStore
	runs                runStore
	queue               verificationQueue
	causalVerifications causalVerificationStore
	graphLock           graphLocker
	audits              auditStore
	objects             objectStore
	datasets            datasetStore
	embeddings          embeddingsStore
	heur                heuristicsService
	users               userStore
	sessions            sessionStore
	claude              claudeClient
	chat                chatStreamer
	sandbox             sandboxExecutor
	hub                 *Hub
	jobs                JobLauncher
	verifierJobs        JobLauncher
	identity            Identity
	router              RouterConfig
	cookieName          string
	cookieSecure        bool
	localImportDir      string
	sleepCycleJobName   string
	internalAuthToken   string
	hitlThreshold       float64
	blockingLoopTimeout time.Duration
	verificationPoll    time.Duration
	keepaliveInterval   time.Duration
	correctionLockWait  time.Duration
	correctionLockPoll  time.Duration

	histMu     sync.Mutex
	histograms map[string]*confidenceHistogram
}

// NewServer wires the server from its collaborators (infra-constructor
// convention).
func NewServer(
	repo graphRepo,
	goals goalStore,
	runs runStore,
	queue verificationQueue,
	causalVerifications causalVerificationStore,
	graphLock graphLocker,
	audits auditStore,
	objects objectStore,
	datasets datasetStore,
	embeddings embeddingsStore,
	heur heuristicsService,
	claude claudeClient,
	chat chatStreamer,
	sandbox sandboxExecutor,
	hub *Hub,
	jobs JobLauncher,
	verifierJobs JobLauncher,
	identity Identity,
	router RouterConfig,
	sessionCfg SessionConfig,
	localImportDir, sleepCycleJobName, internalAuthToken string,
	hitlThreshold float64,
	blockingLoopTimeout time.Duration,
) *Server {
	cookieName := sessionCfg.CookieName
	if cookieName == "" {
		cookieName = sessionCookieDefaultName
	}
	return &Server{
		repo:                repo,
		goals:               goals,
		runs:                runs,
		queue:               queue,
		causalVerifications: causalVerifications,
		graphLock:           graphLock,
		router:              router,
		audits:              audits,
		objects:             objects,
		datasets:            datasets,
		embeddings:          embeddings,
		heur:                heur,
		users:               sessionCfg.Users,
		sessions:            sessionCfg.Sessions,
		claude:              claude,
		chat:                chat,
		sandbox:             sandbox,
		hub:                 hub,
		jobs:                jobs,
		verifierJobs:        verifierJobs,
		identity:            identity,
		cookieName:          cookieName,
		cookieSecure:        sessionCfg.CookieSecure,
		localImportDir:      localImportDir,
		sleepCycleJobName:   sleepCycleJobName,
		internalAuthToken:   internalAuthToken,
		hitlThreshold:       hitlThreshold,
		blockingLoopTimeout: blockingLoopTimeout,
		verificationPoll:    defaultVerificationPoll,
		keepaliveInterval:   defaultKeepaliveInterval,
		correctionLockWait:  defaultCorrectionLockWait,
		correctionLockPoll:  defaultCorrectionLockPoll,
		histograms:          map[string]*confidenceHistogram{},
	}
}

// Routes returns the mux. Analyst-facing routes run behind the session guard
// (the only exempt paths are the sign-in pair /register and /login); the
// internal audit write and verification-event hook stay service-to-service:
// they carry a shared secret no analyst has to hold and never pass through the
// guard. The outer catch-all alternates fine with the method-prefixed internal
// patterns -- ServeMux picks the most specific match, so /internal/* reaches
// BearerAuth and analyst routes reach the guard.
func (s *Server) Routes() http.Handler {
	analyst := http.NewServeMux()
	analyst.HandleFunc("POST /goals", s.handleSubmitGoal)
	analyst.HandleFunc("GET /goals", s.handleListGoals)
	analyst.HandleFunc("POST /goals/{id}/hypothesis-loop", s.handleTriggerLoop)
	analyst.HandleFunc("GET /goals/{id}/stream", s.handleStream)
	analyst.HandleFunc("POST /goals/{id}/chat", s.handleChat)
	analyst.HandleFunc("POST /goals/{id}/sleep-cycle", s.handleTriggerSleepCycle)
	analyst.HandleFunc("GET /goals/{id}/verifications", s.handleListVerifications)
	analyst.HandleFunc("POST /goals/{id}/verifications/{outcomeID}", s.handleResolveVerification)
	analyst.HandleFunc("GET /goals/{id}/outcomes", s.handleListOutcomes)
	analyst.HandleFunc("GET /goals/{id}/outcomes/{outcomeID}/excerpt", s.handleOutcomeExcerpt)
	analyst.HandleFunc("GET /goals/{id}/causal-graph", s.handleCausalGraph)
	analyst.HandleFunc("POST /goals/{id}/causal-graph/corrections", s.handleCausalCorrection)
	analyst.HandleFunc("POST /goals/{id}/findings/{interventionID}/verify", s.handleVerifyFinding)
	analyst.HandleFunc("GET /goals/{id}/causal-verifications", s.handleListCausalVerifications)
	analyst.HandleFunc("GET /heuristics/search", s.handleHeuristicSearch)
	analyst.HandleFunc("GET /heuristics/{id}/trace", s.handleHeuristicTrace)
	analyst.HandleFunc("DELETE /heuristics/{id}", s.handleDeleteHeuristic)
	analyst.HandleFunc("DELETE /goals/{id}", s.handleDeleteGoal)
	analyst.HandleFunc("GET /datasets", s.handleListDatasets)
	analyst.HandleFunc("POST /datasets", s.handleCreateDataset)
	analyst.HandleFunc("GET /datasets/{id}", s.handleGetDataset)
	analyst.HandleFunc("PATCH /datasets/{id}", s.handleUpdateDataset)
	analyst.HandleFunc("DELETE /datasets/{id}", s.handleDeleteDataset)
	analyst.HandleFunc("GET /datasets/{id}/shares", s.handleListShares)
	analyst.HandleFunc("PUT /datasets/{id}/shares/{username}", s.handleShareDataset)
	analyst.HandleFunc("DELETE /datasets/{id}/shares/{username}", s.handleRevokeShare)
	analyst.HandleFunc("POST /register", s.handleRegister)
	analyst.HandleFunc("POST /login", s.handleLogin)
	analyst.HandleFunc("POST /logout", s.handleLogout)
	analyst.HandleFunc("GET /me", s.handleMe)
	analyst.HandleFunc("PATCH /me", s.handlePatchMe)
	analyst.HandleFunc("GET /users", s.handleListUsers)
	analyst.HandleFunc("POST /users", s.handleCreateUser)
	analyst.HandleFunc("PATCH /users/{id}", s.handleUpdateUser)

	mux := http.NewServeMux()
	mux.Handle("/", s.sessionMiddleware().wrap(sessionExemptPaths, analyst))
	mux.Handle("POST /internal/audit", service.BearerAuth(s.internalAuthToken, http.HandlerFunc(s.handleAudit)))
	mux.Handle("POST /internal/verification-events", service.BearerAuth(s.internalAuthToken, http.HandlerFunc(s.handleVerificationEvent)))
	return mux
}

// sessionMiddleware wires the guard from the server's collaborators. It is the
// seam the account test-server overrides to exercise real session behavior.
func (s *Server) sessionMiddleware() *sessionGuard {
	return &sessionGuard{
		sessions: s.sessions,
		users:    s.users,
		cookie:   s.cookieName,
	}
}

// lookupGoal fetches a goal, writing a 404 for a missing id and a masked 500
// otherwise. The bool is false when a response has already been written.
func (s *Server) lookupGoal(ctx context.Context, w http.ResponseWriter, id, userID string) (store.Goal, bool) {
	// The store read runs on a detached context: a cancelled request (a client
	// hangup) must still resolve the goal so the handler's own cancellation logic
	// decides the quiet abort, exactly as the streaming replay and chat-disconnect
	// paths depend on. The real pool fails a call on a cancelled context, so
	// threading r.Context() through blind would turn a walk-away into a server
	// error.
	goal, err := s.goals.GetAccessible(context.WithoutCancel(ctx), id, userID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			service.WriteErr(w, http.StatusNotFound, "goal not found")
			return store.Goal{}, false
		}
		log.Printf("orchestrator: get goal %q: %v", id, err)
		service.WriteErr(w, http.StatusInternalServerError, "internal error")
		return store.Goal{}, false
	}
	return goal, true
}
