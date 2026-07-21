package store_test

import (
	"context"
	"testing"

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
