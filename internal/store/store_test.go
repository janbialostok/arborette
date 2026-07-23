package store_test

import (
	"context"
	"testing"
	"time"

	"github.com/arborette/arborette/internal/config"
	"github.com/arborette/arborette/internal/domain"
	"github.com/arborette/arborette/internal/store"
	"github.com/arborette/arborette/internal/testutil"
	"github.com/pgvector/pgvector-go"
)

func setup(t *testing.T, ctx context.Context) config.Config {
	t.Helper()
	cfg := testutil.RequireIntegration(t)
	testutil.SetupPostgres(t, ctx, cfg)
	return cfg
}

func pool(t *testing.T, ctx context.Context, dsn string) *store.Pool {
	t.Helper()
	p, err := store.NewPool(ctx, dsn)
	if err != nil {
		t.Fatalf("open pool: %v", err)
	}
	t.Cleanup(p.Close)
	return p
}

func vec768(seed float32) []float32 {
	v := make([]float32, 768)
	for i := range v {
		v[i] = seed + float32(i)*0.001
	}
	return v
}

func TestVectorRoundTrip(t *testing.T) {
	ctx := context.Background()
	cfg := setup(t, ctx)
	p := pool(t, ctx, cfg.Postgres.ServiceDSN())
	embeddings := store.NewEmbeddingStore(p)

	nodeID := testutil.NewID(t)
	want := vec768(0.5)
	if err := embeddings.Upsert(ctx, nodeID, want); err != nil {
		t.Fatalf("upsert embedding: %v", err)
	}

	var got pgvector.Vector
	if err := p.QueryRow(ctx,
		"SELECT embedding FROM meta_heuristic_embeddings WHERE node_id = $1", nodeID,
	).Scan(&got); err != nil {
		t.Fatalf("scan embedding: %v", err)
	}
	slice := got.Slice()
	if len(slice) != len(want) {
		t.Fatalf("dimension mismatch: got %d want %d", len(slice), len(want))
	}
	for i := range want {
		if slice[i] != want[i] {
			t.Fatalf("value mismatch at %d: got %f want %f", i, slice[i], want[i])
		}
	}
}

// oneHot builds a 768-d vector with the given nonzero index weights, keeping a
// nonzero norm so cosine distance is well-defined.
func oneHot(weights map[int]float32) []float32 {
	v := make([]float32, 768)
	for i, w := range weights {
		v[i] = w
	}
	return v
}

// TestSimilaritySearchRankingAndLimit seeds three embeddings at known cosine
// distances from the query and asserts the nearest come back first and that k
// caps the result count — behavior a single-row test cannot exercise.
func TestSimilaritySearchRankingAndLimit(t *testing.T) {
	ctx := context.Background()
	cfg := setup(t, ctx)
	testutil.TruncateEmbeddings(t, ctx, cfg)
	p := pool(t, ctx, cfg.Postgres.ServiceDSN())
	embeddings := store.NewEmbeddingStore(p)

	query := oneHot(map[int]float32{0: 1})
	nearID := testutil.NewID(t) // identical direction -> distance 0
	midID := testutil.NewID(t)  // 45 degrees
	farID := testutil.NewID(t)  // orthogonal -> distance 1
	if err := embeddings.Upsert(ctx, nearID, oneHot(map[int]float32{0: 1})); err != nil {
		t.Fatalf("upsert near: %v", err)
	}
	if err := embeddings.Upsert(ctx, midID, oneHot(map[int]float32{0: 1, 1: 1})); err != nil {
		t.Fatalf("upsert mid: %v", err)
	}
	if err := embeddings.Upsert(ctx, farID, oneHot(map[int]float32{1: 1})); err != nil {
		t.Fatalf("upsert far: %v", err)
	}

	got, err := embeddings.SimilaritySearch(ctx, query, 2)
	if err != nil {
		t.Fatalf("similarity search: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("expected k=2 to cap results, got %d", len(got))
	}
	if got[0] != nearID || got[1] != midID {
		t.Fatalf("expected [near, mid] by cosine distance, got %v", got)
	}
}

// TestValidateEmbeddingDimension covers both branches of the startup guard: the
// configured dimension matching the migrated vector(768) column, and a mismatch
// producing the descriptive error the three embedding-consuming services fail on.
func TestValidateEmbeddingDimension(t *testing.T) {
	ctx := context.Background()
	cfg := setup(t, ctx)
	p := pool(t, ctx, cfg.Postgres.ServiceDSN())

	if err := store.ValidateEmbeddingDimension(ctx, p, 768); err != nil {
		t.Fatalf("expected 768 to match the column, got error: %v", err)
	}
	if err := store.ValidateEmbeddingDimension(ctx, p, 999); err == nil {
		t.Fatal("expected a mismatch error for dimension 999")
	}
}

func TestWrongDimensionRejected(t *testing.T) {
	ctx := context.Background()
	cfg := setup(t, ctx)
	p := pool(t, ctx, cfg.Postgres.ServiceDSN())

	nodeID := testutil.NewID(t)
	_, err := p.Exec(ctx,
		"INSERT INTO meta_heuristic_embeddings (node_id, embedding) VALUES ($1, $2)",
		nodeID, pgvector.NewVector([]float32{0.1, 0.2, 0.3}),
	)
	if err == nil {
		t.Fatal("expected wrong-dimension insert to be rejected")
	}
}

func TestAuditBoundaryOrchestrator(t *testing.T) {
	ctx := context.Background()
	cfg := setup(t, ctx)
	p := pool(t, ctx, cfg.Postgres.OrchestratorDSN())

	audit := store.NewAuditLog(p)
	if err := audit.Append(ctx, store.AuditRecord{
		Actor: "analyst", Action: "register_goal", EventType: "goal", Detail: map[string]any{"k": "v"},
	}); err != nil {
		t.Fatalf("orchestrator append audit: %v", err)
	}

	if _, err := p.Exec(ctx, "UPDATE audit_log SET actor = 'x'"); err == nil {
		t.Fatal("expected orchestrator UPDATE on audit_log to be denied")
	}
	if _, err := p.Exec(ctx, "DELETE FROM audit_log"); err == nil {
		t.Fatal("expected orchestrator DELETE on audit_log to be denied")
	}
}

func TestAuditBoundaryServiceDenied(t *testing.T) {
	ctx := context.Background()
	cfg := setup(t, ctx)
	p := pool(t, ctx, cfg.Postgres.ServiceDSN())

	if _, err := p.Exec(ctx,
		"INSERT INTO audit_log (actor, action, event_type) VALUES ('x','y','z')",
	); err == nil {
		t.Fatal("expected service INSERT on audit_log to be denied")
	}
	// QueryRow+Scan reads the row so the permission error (which pgx defers past
	// Query) surfaces and the connection is released.
	var count int
	if err := p.QueryRow(ctx, "SELECT count(*) FROM audit_log").Scan(&count); err == nil {
		t.Fatal("expected service SELECT on audit_log to be denied")
	}
}

func TestGoalRegistryGrants(t *testing.T) {
	ctx := context.Background()
	cfg := setup(t, ctx)

	orchestrator := pool(t, ctx, cfg.Postgres.OrchestratorDSN())
	registry := store.NewGoalRegistry(orchestrator)

	goalID := testutil.NewID(t)
	goal := store.Goal{
		OptimizationFunctionID: goalID,
		GoalText:               "minimize false positives",
		EvaluationMatrix: domain.EvaluationMatrix{
			Targets:     []domain.Target{{Field: "false_positive_rate", Direction: domain.Minimize}},
			Constraints: []domain.Constraint{{Field: "latency_ms", Op: domain.LessThan, Value: 200}},
		},
		DataSourceRef: "s3://arborette/data.csv",
	}
	if err := registry.Insert(ctx, goal); err != nil {
		t.Fatalf("orchestrator insert goal: %v", err)
	}
	got, err := registry.Get(ctx, goalID)
	if err != nil {
		t.Fatalf("orchestrator get goal: %v", err)
	}
	if got.GoalText != goal.GoalText || len(got.EvaluationMatrix.Targets) != 1 {
		t.Fatalf("goal round-trip mismatch: %+v", got)
	}

	// service role: SELECT on goal_registry is granted; write is not.
	service := pool(t, ctx, cfg.Postgres.ServiceDSN())
	var count int
	if err := service.QueryRow(ctx, "SELECT count(*) FROM goal_registry").Scan(&count); err != nil {
		t.Fatalf("service select goal_registry: %v", err)
	}
	if _, err := service.Exec(ctx,
		"INSERT INTO goal_registry (optimization_function_id, goal_text, evaluation_matrix, datasource_ref) VALUES ($1,'x','{}','y')",
		testutil.NewID(t),
	); err == nil {
		t.Fatal("expected service INSERT on goal_registry to be denied")
	}
}

// seedGoal inserts a minimal registered goal a run row can reference, returning
// its id.
func seedGoal(t *testing.T, ctx context.Context, p *store.Pool) string {
	t.Helper()
	goalID := testutil.NewID(t)
	if err := store.NewGoalRegistry(p).Insert(ctx, store.Goal{
		OptimizationFunctionID: goalID,
		GoalText:               "grow revenue",
		EvaluationMatrix:       domain.EvaluationMatrix{Targets: []domain.Target{{Field: "revenue", Direction: domain.Maximize, Aggregation: "avg"}}},
		DataSourceRef:          "s3://arborette/data.csv",
	}); err != nil {
		t.Fatalf("seed goal: %v", err)
	}
	return goalID
}

func TestRunsLifecycle(t *testing.T) {
	ctx := context.Background()
	cfg := setup(t, ctx)
	p := pool(t, ctx, cfg.Postgres.OrchestratorDSN())
	runs := store.NewRuns(p)
	goalID := seedGoal(t, ctx, p)

	runID := testutil.NewID(t)
	if err := runs.Create(ctx, runID, goalID); err != nil {
		t.Fatalf("create run: %v", err)
	}

	latest, err := runs.LatestByGoal(ctx, []string{goalID})
	if err != nil {
		t.Fatalf("latest by goal: %v", err)
	}
	if run, ok := latest[goalID]; !ok || run.Status != store.RunRunning || run.EndedAt != nil || run.FailureReason != nil {
		t.Fatalf("expected a running run with no end/reason: %+v (ok=%v)", run, ok)
	}

	if err := runs.SetStatus(ctx, runID, store.RunCompleted, ""); err != nil {
		t.Fatalf("set status: %v", err)
	}
	latest, err = runs.LatestByGoal(ctx, []string{goalID})
	if err != nil {
		t.Fatalf("latest by goal: %v", err)
	}
	if run := latest[goalID]; run.Status != store.RunCompleted || run.EndedAt == nil || run.FailureReason != nil {
		t.Fatalf("completed run should have ended_at set and NULL reason: %+v", run)
	}

	// A newer run for the same goal wins the latest-by-goal read. The sleep keeps
	// the two started_at defaults distinct so the DESC ordering is deterministic.
	time.Sleep(2 * time.Millisecond)
	newerID := testutil.NewID(t)
	if err := runs.Create(ctx, newerID, goalID); err != nil {
		t.Fatalf("create newer run: %v", err)
	}
	latest, err = runs.LatestByGoal(ctx, []string{goalID})
	if err != nil {
		t.Fatalf("latest by goal: %v", err)
	}
	if latest[goalID].RunID != newerID {
		t.Fatalf("latest run = %q, want the newer %q", latest[goalID].RunID, newerID)
	}

	// A goal with no runs is absent from the map; an empty id set returns empty.
	if _, ok := latest[testutil.NewID(t)]; ok {
		t.Fatal("a goal with no runs must be absent from the map")
	}
	empty, err := runs.LatestByGoal(ctx, nil)
	if err != nil || len(empty) != 0 {
		t.Fatalf("empty goalIDs = (%v, %v), want (empty map, nil)", empty, err)
	}
}

func TestGoalRegistryList(t *testing.T) {
	ctx := context.Background()
	cfg := setup(t, ctx)
	p := pool(t, ctx, cfg.Postgres.OrchestratorDSN())
	registry := store.NewGoalRegistry(p)

	olderID := testutil.NewID(t)
	if err := registry.Insert(ctx, store.Goal{OptimizationFunctionID: olderID, GoalText: "older",
		EvaluationMatrix: domain.EvaluationMatrix{Targets: []domain.Target{{Field: "revenue", Direction: domain.Maximize, Aggregation: "avg"}}},
		DataSourceRef:    "ref"}); err != nil {
		t.Fatalf("insert older: %v", err)
	}
	time.Sleep(2 * time.Millisecond) // keep created_at distinct so DESC ordering is deterministic
	newerID := testutil.NewID(t)
	if err := registry.Insert(ctx, store.Goal{OptimizationFunctionID: newerID, GoalText: "newer",
		EvaluationMatrix: domain.EvaluationMatrix{Targets: []domain.Target{{Field: "cost", Direction: domain.Minimize, Aggregation: "sum"}}},
		DataSourceRef:    "ref"}); err != nil {
		t.Fatalf("insert newer: %v", err)
	}

	goals, err := registry.List(ctx)
	if err != nil {
		t.Fatalf("list goals: %v", err)
	}
	// The shared database may hold other goals, so assert relative order and the
	// per-row matrix round-trip for the two this test inserted.
	pos := make(map[string]int, len(goals))
	byID := make(map[string]store.Goal, len(goals))
	for i, g := range goals {
		pos[g.OptimizationFunctionID] = i
		byID[g.OptimizationFunctionID] = g
	}
	oi, ok1 := pos[olderID]
	ni, ok2 := pos[newerID]
	if !ok1 || !ok2 {
		t.Fatalf("both inserted goals must appear in the list")
	}
	if ni >= oi {
		t.Fatalf("newest-first: newer (%d) must precede older (%d)", ni, oi)
	}
	if m := byID[newerID].EvaluationMatrix; len(m.Targets) != 1 || m.Targets[0].Aggregation != "sum" {
		t.Fatalf("newer goal matrix not round-tripped: %+v", m)
	}
}

func TestRunsFailOrphaned(t *testing.T) {
	ctx := context.Background()
	cfg := setup(t, ctx)
	p := pool(t, ctx, cfg.Postgres.OrchestratorDSN())
	runs := store.NewRuns(p)
	goalID := seedGoal(t, ctx, p)

	runningID, doneID := testutil.NewID(t), testutil.NewID(t)
	if err := runs.Create(ctx, runningID, goalID); err != nil {
		t.Fatalf("create running run: %v", err)
	}
	if err := runs.Create(ctx, doneID, goalID); err != nil {
		t.Fatalf("create done run: %v", err)
	}
	if err := runs.SetStatus(ctx, doneID, store.RunCompleted, ""); err != nil {
		t.Fatalf("complete run: %v", err)
	}

	// FailOrphaned is a global sweep; the shared test database may hold running
	// rows from other tests, so assert it reconciled at least our one and verify
	// the observable effect per row rather than an exact table-wide count.
	n, err := runs.FailOrphaned(ctx, "orchestrator restarted")
	if err != nil {
		t.Fatalf("fail orphaned: %v", err)
	}
	if n < 1 {
		t.Fatalf("reconciled %d runs, want at least the running one", n)
	}

	var status, reason string
	if err := p.QueryRow(ctx, "SELECT status, failure_reason FROM runs WHERE run_id = $1", runningID).Scan(&status, &reason); err != nil {
		t.Fatalf("read reconciled run: %v", err)
	}
	if status != "failed" || reason != "orchestrator restarted" {
		t.Fatalf("reconciled run = (%q, %q), want (failed, orchestrator restarted)", status, reason)
	}
	var doneStatus string
	if err := p.QueryRow(ctx, "SELECT status FROM runs WHERE run_id = $1", doneID).Scan(&doneStatus); err != nil {
		t.Fatalf("read completed run: %v", err)
	}
	if doneStatus != "completed" {
		t.Fatalf("a settled run must be untouched, got %q", doneStatus)
	}
}

func TestRunsSetStatusFailedAndMultiGoal(t *testing.T) {
	ctx := context.Background()
	cfg := setup(t, ctx)
	p := pool(t, ctx, cfg.Postgres.OrchestratorDSN())
	runs := store.NewRuns(p)
	goalA := seedGoal(t, ctx, p)
	goalB := seedGoal(t, ctx, p)

	runA, runB := testutil.NewID(t), testutil.NewID(t)
	if err := runs.Create(ctx, runA, goalA); err != nil {
		t.Fatalf("create run A: %v", err)
	}
	if err := runs.Create(ctx, runB, goalB); err != nil {
		t.Fatalf("create run B: %v", err)
	}
	if err := runs.SetStatus(ctx, runA, store.RunFailed, "field not present in schema: X"); err != nil {
		t.Fatalf("set failed: %v", err)
	}

	latest, err := runs.LatestByGoal(ctx, []string{goalA, goalB})
	if err != nil {
		t.Fatalf("latest by goal: %v", err)
	}
	// Each goal keys to its own latest run: A failed with the persisted reason,
	// B still running with none.
	if a := latest[goalA]; a.RunID != runA || a.Status != store.RunFailed || a.FailureReason == nil || *a.FailureReason != "field not present in schema: X" {
		t.Fatalf("goal A latest should be its failed run with the reason: %+v", a)
	}
	if b := latest[goalB]; b.RunID != runB || b.Status != store.RunRunning || b.FailureReason != nil {
		t.Fatalf("goal B latest should be its running run with no reason: %+v", b)
	}
}

func TestRunsGrants(t *testing.T) {
	ctx := context.Background()
	cfg := setup(t, ctx)

	orchestrator := pool(t, ctx, cfg.Postgres.OrchestratorDSN())
	runs := store.NewRuns(orchestrator)
	goalID := seedGoal(t, ctx, orchestrator)

	// orchestrator: INSERT + UPDATE + SELECT are granted.
	runID := testutil.NewID(t)
	if err := runs.Create(ctx, runID, goalID); err != nil {
		t.Fatalf("orchestrator create run: %v", err)
	}
	if err := runs.SetStatus(ctx, runID, store.RunCompleted, ""); err != nil {
		t.Fatalf("orchestrator set status: %v", err)
	}

	// service: SELECT on runs is granted; write is not.
	service := pool(t, ctx, cfg.Postgres.ServiceDSN())
	var count int
	if err := service.QueryRow(ctx, "SELECT count(*) FROM runs").Scan(&count); err != nil {
		t.Fatalf("service select runs: %v", err)
	}
	if _, err := service.Exec(ctx,
		"INSERT INTO runs (run_id, optimization_function_id, status) VALUES ($1,$2,'running')",
		testutil.NewID(t), goalID,
	); err == nil {
		t.Fatal("expected service INSERT on runs to be denied")
	}
}

func TestEmbeddingGrants(t *testing.T) {
	ctx := context.Background()
	cfg := setup(t, ctx)

	// orchestrator: SELECT only on embeddings.
	orchestrator := pool(t, ctx, cfg.Postgres.OrchestratorDSN())
	var count int
	if err := orchestrator.QueryRow(ctx, "SELECT count(*) FROM meta_heuristic_embeddings").Scan(&count); err != nil {
		t.Fatalf("orchestrator select embeddings: %v", err)
	}
	if _, err := orchestrator.Exec(ctx,
		"INSERT INTO meta_heuristic_embeddings (node_id, embedding) VALUES ($1,$2)",
		testutil.NewID(t), pgvector.NewVector(vec768(0.1)),
	); err == nil {
		t.Fatal("expected orchestrator INSERT on embeddings to be denied")
	}

	// service: SELECT/INSERT/UPDATE on embeddings.
	service := pool(t, ctx, cfg.Postgres.ServiceDSN())
	embeddings := store.NewEmbeddingStore(service)
	nodeID := testutil.NewID(t)
	if err := embeddings.Upsert(ctx, nodeID, vec768(0.2)); err != nil {
		t.Fatalf("service upsert embedding: %v", err)
	}
	if err := embeddings.Upsert(ctx, nodeID, vec768(0.3)); err != nil {
		t.Fatalf("service update embedding: %v", err)
	}
}
