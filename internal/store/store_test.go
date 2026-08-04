package store_test

import (
	"context"
	"encoding/json"
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
	embeddings := store.NewEmbeddingStore(p, 2)

	nodeID := testutil.NewID(t)
	want := vec768(0.5)
	if err := embeddings.Upsert(ctx, nodeID, "", want); err != nil {
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
	// A generous floor keeps this test about ranking and the k cap, not the floor.
	embeddings := store.NewEmbeddingStore(p, 2)

	query := oneHot(map[int]float32{0: 1})
	nearID := testutil.NewID(t) // identical direction -> distance 0
	midID := testutil.NewID(t)  // 45 degrees
	farID := testutil.NewID(t)  // orthogonal -> distance 1
	if err := embeddings.Upsert(ctx, nearID, "", oneHot(map[int]float32{0: 1})); err != nil {
		t.Fatalf("upsert near: %v", err)
	}
	if err := embeddings.Upsert(ctx, midID, "", oneHot(map[int]float32{0: 1, 1: 1})); err != nil {
		t.Fatalf("upsert mid: %v", err)
	}
	if err := embeddings.Upsert(ctx, farID, "", oneHot(map[int]float32{1: 1})); err != nil {
		t.Fatalf("upsert far: %v", err)
	}

	got, err := embeddings.SimilaritySearch(ctx, query, 2, store.SearchScope{CrossGoal: true})
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

// TestSearchScopeFiltersByGoal: a goal-scoped search returns only that goal's
// rows, while a cross-goal search returns every row including the NULL-goal
// legacy corpus that binds no goal parameter at all.
func TestSearchScopeFiltersByGoal(t *testing.T) {
	ctx := context.Background()
	cfg := setup(t, ctx)
	testutil.TruncateEmbeddings(t, ctx, cfg)
	p := pool(t, ctx, cfg.Postgres.ServiceDSN())
	embeddings := store.NewEmbeddingStore(p, 2)

	goalA, goalB := testutil.NewID(t), testutil.NewID(t)
	aID, bID, legacyID := testutil.NewID(t), testutil.NewID(t), testutil.NewID(t)
	v := oneHot(map[int]float32{0: 1})
	for id, goal := range map[string]string{aID: goalA, bID: goalB, legacyID: ""} {
		if err := embeddings.Upsert(ctx, id, goal, v); err != nil {
			t.Fatalf("upsert %q: %v", id, err)
		}
	}

	scoped, err := embeddings.SimilaritySearch(ctx, v, 10, store.SearchScope{GoalID: goalA})
	if err != nil {
		t.Fatalf("goal-scoped search: %v", err)
	}
	if len(scoped) != 1 || scoped[0] != aID {
		t.Fatalf("goal-scoped search must return only goalA's row, got %v", scoped)
	}

	cross, err := embeddings.SimilaritySearch(ctx, v, 10, store.SearchScope{CrossGoal: true})
	if err != nil {
		t.Fatalf("cross-goal search: %v", err)
	}
	if len(cross) != 3 {
		t.Fatalf("cross-goal search must return every row including the NULL-goal legacy one, got %v", cross)
	}
}

// TestDistanceFloorExcludesDistantRows: a row beyond the floor is dropped, and a
// query distant from everything returns empty rather than the corpus ranked by
// how distant it is.
func TestDistanceFloorExcludesDistantRows(t *testing.T) {
	ctx := context.Background()
	cfg := setup(t, ctx)
	testutil.TruncateEmbeddings(t, ctx, cfg)
	p := pool(t, ctx, cfg.Postgres.ServiceDSN())
	embeddings := store.NewEmbeddingStore(p, 0.5)

	nearID, farID := testutil.NewID(t), testutil.NewID(t)
	if err := embeddings.Upsert(ctx, nearID, "", oneHot(map[int]float32{0: 1})); err != nil {
		t.Fatalf("upsert near: %v", err)
	}
	if err := embeddings.Upsert(ctx, farID, "", oneHot(map[int]float32{1: 1})); err != nil {
		t.Fatalf("upsert far: %v", err)
	}

	// Query aligned with near (distance 0); far is orthogonal (distance 1 > 0.5).
	got, err := embeddings.SimilaritySearch(ctx, oneHot(map[int]float32{0: 1}), 10, store.SearchScope{CrossGoal: true})
	if err != nil {
		t.Fatalf("similarity search: %v", err)
	}
	if len(got) != 1 || got[0] != nearID {
		t.Fatalf("the floor must drop the orthogonal row, got %v", got)
	}

	// A query orthogonal to every stored row is beyond the floor from all of them.
	empty, err := embeddings.SimilaritySearch(ctx, oneHot(map[int]float32{500: 1}), 10, store.SearchScope{CrossGoal: true})
	if err != nil {
		t.Fatalf("distant search: %v", err)
	}
	if len(empty) != 0 {
		t.Fatalf("a query beyond the floor from everything must return empty, got %v", empty)
	}
}

// TestUpsertNullGoalThenRepaired: an empty goalID writes SQL NULL, and a later
// re-embed under a real goal repairs the row without a goal-less caller ever
// re-blanking it.
func TestUpsertNullGoalThenRepaired(t *testing.T) {
	ctx := context.Background()
	cfg := setup(t, ctx)
	testutil.TruncateEmbeddings(t, ctx, cfg)
	p := pool(t, ctx, cfg.Postgres.ServiceDSN())
	embeddings := store.NewEmbeddingStore(p, 2)

	nodeID := testutil.NewID(t)
	if err := embeddings.Upsert(ctx, nodeID, "", vec768(0.2)); err != nil {
		t.Fatalf("upsert null-goal: %v", err)
	}
	if goal := goalOf(t, ctx, embeddings, nodeID); goal != "" {
		t.Fatalf("empty goalID must write NULL, got %q", goal)
	}

	realGoal := testutil.NewID(t)
	if err := embeddings.Upsert(ctx, nodeID, realGoal, vec768(0.3)); err != nil {
		t.Fatalf("re-upsert with a real goal: %v", err)
	}
	if goal := goalOf(t, ctx, embeddings, nodeID); goal != realGoal {
		t.Fatalf("a re-embed under a real goal must repair the NULL row, got %q want %q", goal, realGoal)
	}

	// A goal-less re-embed must never re-blank the now-populated goal.
	if err := embeddings.Upsert(ctx, nodeID, "", vec768(0.4)); err != nil {
		t.Fatalf("goal-less re-upsert: %v", err)
	}
	if goal := goalOf(t, ctx, embeddings, nodeID); goal != realGoal {
		t.Fatalf("a goal-less re-embed must not re-blank a populated goal, got %q", goal)
	}
}

// TestSetGoalIDRepairsOnlyNullRows pins the goal-repair write's guard directly at
// the store: SetGoalID populates a NULL-goal row but never overwrites a row that
// already carries a goal (WHERE goal_id IS NULL), and is a harmless no-op on an
// absent node. This is the discriminating test for the reconcile goal-repair arm
// that the fake cannot model.
func TestSetGoalIDRepairsOnlyNullRows(t *testing.T) {
	ctx := context.Background()
	cfg := setup(t, ctx)
	testutil.TruncateEmbeddings(t, ctx, cfg)
	p := pool(t, ctx, cfg.Postgres.ServiceDSN())
	embeddings := store.NewEmbeddingStore(p, 2)

	nodeID := testutil.NewID(t)
	if err := embeddings.Upsert(ctx, nodeID, "", vec768(0.2)); err != nil {
		t.Fatalf("seed null-goal row: %v", err)
	}

	firstGoal := testutil.NewID(t)
	if err := embeddings.SetGoalID(ctx, nodeID, firstGoal); err != nil {
		t.Fatalf("set goal on a NULL row: %v", err)
	}
	if goal := goalOf(t, ctx, embeddings, nodeID); goal != firstGoal {
		t.Fatalf("SetGoalID must populate a NULL row, got %q want %q", goal, firstGoal)
	}

	// A second SetGoalID must not overwrite the populated goal.
	secondGoal := testutil.NewID(t)
	if err := embeddings.SetGoalID(ctx, nodeID, secondGoal); err != nil {
		t.Fatalf("second set goal: %v", err)
	}
	if goal := goalOf(t, ctx, embeddings, nodeID); goal != firstGoal {
		t.Fatalf("SetGoalID must never overwrite a populated goal, got %q want %q", goal, firstGoal)
	}

	// An absent node is a no-op, not an error.
	if err := embeddings.SetGoalID(ctx, testutil.NewID(t), firstGoal); err != nil {
		t.Fatalf("SetGoalID on an absent node must be a no-op, got %v", err)
	}
}

// goalOf reads the persisted goal_id of one node via ListNodeRefs (empty string
// for a NULL row, per the store's COALESCE).
func goalOf(t *testing.T, ctx context.Context, e *store.EmbeddingStore, nodeID string) string {
	t.Helper()
	refs, err := e.ListNodeRefs(ctx)
	if err != nil {
		t.Fatalf("list node refs: %v", err)
	}
	for _, ref := range refs {
		if ref.NodeID == nodeID {
			return ref.GoalID
		}
	}
	t.Fatalf("node %q has no embedding row", nodeID)
	return ""
}

// TestGoalScopedRecallUnderFiltering: a goal-scoped search returns all of a
// minority goal's in-floor rows up to k even when the corpus is dominated by
// another goal. enable_seqscan is disabled on a single-connection pool so the
// query takes the hnsw path -- exercising the iterative_scan wrapping rather than
// an exact seq-scan filter. At fixture scale hnsw already covers the whole table,
// so this pins the goal-filter recall; a true under-return needs a large corpus.
func TestGoalScopedRecallUnderFiltering(t *testing.T) {
	ctx := context.Background()
	cfg := setup(t, ctx)
	testutil.TruncateEmbeddings(t, ctx, cfg)

	// One connection so a session-level SET persists into the store's own search
	// transaction, which the store manages and does not expose.
	p := pool(t, ctx, cfg.Postgres.ServiceDSN()+" pool_max_conns=1")
	if _, err := p.Exec(ctx, "SET enable_seqscan = off"); err != nil {
		t.Fatalf("disable seqscan: %v", err)
	}
	embeddings := store.NewEmbeddingStore(p, 2)

	minorityGoal, majorityGoal := testutil.NewID(t), testutil.NewID(t)
	query := oneHot(map[int]float32{0: 1})
	minority := map[string]bool{}
	for i := 0; i < 3; i++ {
		id := testutil.NewID(t)
		minority[id] = true
		if err := embeddings.Upsert(ctx, id, minorityGoal, query); err != nil {
			t.Fatalf("upsert minority: %v", err)
		}
	}
	for i := 0; i < 50; i++ {
		if err := embeddings.Upsert(ctx, testutil.NewID(t), majorityGoal, query); err != nil {
			t.Fatalf("upsert majority: %v", err)
		}
	}

	got, err := embeddings.SimilaritySearch(ctx, query, 3, store.SearchScope{GoalID: minorityGoal})
	if err != nil {
		t.Fatalf("goal-scoped search: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("goal-scoped search must recall all %d minority rows, got %d: %v", len(minority), len(got), got)
	}
	for _, id := range got {
		if !minority[id] {
			t.Fatalf("goal-scoped search returned a non-minority row %q", id)
		}
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

func TestValidateVectorExtensionVersion(t *testing.T) {
	ctx := context.Background()
	cfg := setup(t, ctx)
	p := pool(t, ctx, cfg.Postgres.ServiceDSN())

	// The test container is pinned to a >= 0.8.0 pgvector, so the query-and-parse
	// wrapper must read a real version and accept it.
	if err := store.ValidateVectorExtensionVersion(ctx, p); err != nil {
		t.Fatalf("the pinned pgvector (>= 0.8.0) must pass the version check: %v", err)
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

// TestDataSourceRegistry round-trips the ref registry the sandbox scopes requests
// against and pins its grant boundary: the orchestrator writes refs (idempotently),
// the service role (the sandbox's runtime role) may only read them to validate, and
// an unknown ref reads false rather than erroring.
func TestDataSourceRegistry(t *testing.T) {
	ctx := context.Background()
	cfg := setup(t, ctx)

	orchestrator := pool(t, ctx, cfg.Postgres.OrchestratorDSN())
	orchReg := store.NewGoalRegistry(orchestrator)

	ref := "datasources/" + testutil.NewID(t) + "/orders.csv"

	if exists, err := orchReg.DataSourceRefExists(ctx, ref); err != nil || exists {
		t.Fatalf("unregistered ref: exists=%v err=%v, want false/nil", exists, err)
	}
	if err := orchReg.RegisterDataSourceRef(ctx, ref); err != nil {
		t.Fatalf("orchestrator register ref: %v", err)
	}
	// Re-registering the same ref is a no-op (ON CONFLICT DO NOTHING), which every
	// run after the first goal registration relies on.
	if err := orchReg.RegisterDataSourceRef(ctx, ref); err != nil {
		t.Fatalf("re-register ref must be idempotent: %v", err)
	}
	if exists, err := orchReg.DataSourceRefExists(ctx, ref); err != nil || !exists {
		t.Fatalf("registered ref: exists=%v err=%v, want true/nil", exists, err)
	}

	// The service role reads the registry to validate, exactly as the wired sandbox
	// does; it holds SELECT but not INSERT.
	service := pool(t, ctx, cfg.Postgres.ServiceDSN())
	svcReg := store.NewGoalRegistry(service)
	if exists, err := svcReg.DataSourceRefExists(ctx, ref); err != nil || !exists {
		t.Fatalf("service validate registered ref: exists=%v err=%v, want true/nil", exists, err)
	}
	if err := svcReg.RegisterDataSourceRef(ctx, "datasources/"+testutil.NewID(t)+"/x.csv"); err == nil {
		t.Fatal("expected service INSERT on data_source_registry to be denied")
	}
}

func TestGoalRegistryDocumentGoal(t *testing.T) {
	ctx := context.Background()
	cfg := setup(t, ctx)

	p := pool(t, ctx, cfg.Postgres.OrchestratorDSN())
	registry := store.NewGoalRegistry(p)

	// A document goal writes a NULL evaluation_matrix + populated target_fields.
	docID := testutil.NewID(t)
	docGoal := store.Goal{
		OptimizationFunctionID: docID,
		GoalText:               "extract the key contract dates",
		TargetFields: []domain.TargetField{
			{Name: "effective_date", Description: "the effective date"},
			{Name: "termination_date", Description: "the termination date"},
		},
		DataSourceRef: "s3://arborette/contract.pdf",
	}
	if err := registry.Insert(ctx, docGoal); err != nil {
		t.Fatalf("insert document goal: %v", err)
	}
	got, err := registry.Get(ctx, docID)
	if err != nil {
		t.Fatalf("get document goal: %v", err)
	}
	if !got.IsDocument() || len(got.TargetFields) != 2 || got.TargetFields[0].Name != "effective_date" {
		t.Fatalf("document goal round-trip mismatch: %+v", got)
	}
	if len(got.EvaluationMatrix.Targets) != 0 {
		t.Fatalf("document goal should decode a zero-value matrix, got %+v", got.EvaluationMatrix)
	}

	// A tabular goal alongside it: List must decode a mix of NULL-matrix (document)
	// and NULL-target_fields (tabular) rows without a decode error -- the null-guard
	// that otherwise breaks handleListGoals once one document goal exists.
	tabID := testutil.NewID(t)
	if err := registry.Insert(ctx, store.Goal{
		OptimizationFunctionID: tabID,
		GoalText:               "grow revenue",
		EvaluationMatrix:       domain.EvaluationMatrix{Targets: []domain.Target{{Field: "revenue", Direction: domain.Maximize, Aggregation: "avg"}}},
		DataSourceRef:          "s3://arborette/data.csv",
	}); err != nil {
		t.Fatalf("insert tabular goal: %v", err)
	}

	goals, err := registry.List(ctx)
	if err != nil {
		t.Fatalf("list mixed goals: %v", err)
	}
	byID := map[string]store.Goal{}
	for _, g := range goals {
		byID[g.OptimizationFunctionID] = g
	}
	if g := byID[docID]; !g.IsDocument() || len(g.TargetFields) != 2 {
		t.Fatalf("document goal not listed with its target fields: %+v", g)
	}
	if g := byID[tabID]; g.IsDocument() || len(g.EvaluationMatrix.Targets) != 1 {
		t.Fatalf("tabular goal not listed with its matrix: %+v", g)
	}
}

// TestGoalRegistryWindowBindings round-trips the entity-key/time-column window
// bindings: a goal that binds them reads them back verbatim, and a goal that binds
// neither reads them back as empty strings (the COALESCE of their NULL columns).
func TestGoalRegistryWindowBindings(t *testing.T) {
	ctx := context.Background()
	cfg := setup(t, ctx)
	p := pool(t, ctx, cfg.Postgres.OrchestratorDSN())
	registry := store.NewGoalRegistry(p)

	boundID := testutil.NewID(t)
	if err := registry.Insert(ctx, store.Goal{
		OptimizationFunctionID: boundID,
		GoalText:               "flag anomalous velocity",
		EvaluationMatrix:       domain.EvaluationMatrix{Targets: []domain.Target{{Field: "amount", Direction: domain.Maximize, Aggregation: "avg"}}},
		DataSourceRef:          "s3://arborette/txns.csv",
		EntityKeyColumn:        "account_id",
		TimeColumn:             "ts",
	}); err != nil {
		t.Fatalf("insert windowed goal: %v", err)
	}
	got, err := registry.Get(ctx, boundID)
	if err != nil {
		t.Fatalf("get windowed goal: %v", err)
	}
	if got.EntityKeyColumn != "account_id" || got.TimeColumn != "ts" {
		t.Fatalf("window bindings round-trip mismatch: entity=%q time=%q", got.EntityKeyColumn, got.TimeColumn)
	}

	// A goal with no bindings reads both back as empty (NULL columns COALESCEd).
	unboundID := testutil.NewID(t)
	if err := registry.Insert(ctx, store.Goal{
		OptimizationFunctionID: unboundID,
		GoalText:               "grow revenue",
		EvaluationMatrix:       domain.EvaluationMatrix{Targets: []domain.Target{{Field: "revenue", Direction: domain.Maximize, Aggregation: "avg"}}},
		DataSourceRef:          "s3://arborette/data.csv",
	}); err != nil {
		t.Fatalf("insert unbound goal: %v", err)
	}
	got, err = registry.Get(ctx, unboundID)
	if err != nil {
		t.Fatalf("get unbound goal: %v", err)
	}
	if got.EntityKeyColumn != "" || got.TimeColumn != "" {
		t.Fatalf("unbound goal must read empty bindings, got entity=%q time=%q", got.EntityKeyColumn, got.TimeColumn)
	}
}

// TestGoalRegistryTrackAndClaim round-trips the routing track and the extracted
// claim: a verify-track goal reads back the claim document byte-identically (the
// Verifier decodes exactly what intake validated), a goal registered with no track
// falls to explore rather than an empty string, and SetClaimError records the
// cannot-construct reason the Verifier reports at dispatch time.
func TestGoalRegistryTrackAndClaim(t *testing.T) {
	ctx := context.Background()
	cfg := setup(t, ctx)
	p := pool(t, ctx, cfg.Postgres.OrchestratorDSN())
	registry := store.NewGoalRegistry(p)

	claim, err := json.Marshal(domain.ClaimSpec{
		Filters:   []domain.Constraint{{Field: "tier", Op: domain.Equal, Operand: &domain.LiteralValue{String: strPtr("gold")}}},
		Direction: domain.Maximize,
	})
	if err != nil {
		t.Fatalf("marshal claim: %v", err)
	}
	verifyID := testutil.NewID(t)
	if err := registry.Insert(ctx, store.Goal{
		OptimizationFunctionID: verifyID,
		GoalText:               "do gold-tier accounts spend more?",
		EvaluationMatrix:       domain.EvaluationMatrix{Targets: []domain.Target{{Field: "revenue", Direction: domain.Maximize, Aggregation: "avg"}}},
		DataSourceRef:          "s3://arborette/data.csv",
		Track:                  store.TrackVerify,
		Claim:                  claim,
	}); err != nil {
		t.Fatalf("insert verify goal: %v", err)
	}
	got, err := registry.Get(ctx, verifyID)
	if err != nil {
		t.Fatalf("get verify goal: %v", err)
	}
	if got.Track != store.TrackVerify {
		t.Fatalf("track = %q, want verify", got.Track)
	}
	var decoded domain.ClaimSpec
	if err := json.Unmarshal(got.Claim, &decoded); err != nil {
		t.Fatalf("stored claim does not decode: %v (%s)", err, got.Claim)
	}
	if len(decoded.Filters) != 1 || decoded.Filters[0].Field != "tier" || decoded.Direction != domain.Maximize {
		t.Fatalf("claim round-trip mismatch: %+v", decoded)
	}
	if got.ClaimError != "" {
		t.Fatalf("a constructed claim carries no error, got %q", got.ClaimError)
	}

	// A goal inserted with no track defaults to explore: the column list is explicit,
	// so the SQL DEFAULT never fires and Insert has to supply it.
	exploreID := seedGoal(t, ctx, p)
	got, err = registry.Get(ctx, exploreID)
	if err != nil {
		t.Fatalf("get default-track goal: %v", err)
	}
	if got.Track != store.TrackExplore {
		t.Fatalf("track = %q, want the explore default", got.Track)
	}
	if got.Claim != nil {
		t.Fatalf("an explore goal carries no claim, got %s", got.Claim)
	}

	const reason = "the claim names filter columns that are not in the data source: region"
	if err := registry.SetClaimError(ctx, exploreID, reason); err != nil {
		t.Fatalf("set claim error: %v", err)
	}
	got, err = registry.Get(ctx, exploreID)
	if err != nil {
		t.Fatalf("get after set claim error: %v", err)
	}
	if got.ClaimError != reason {
		t.Fatalf("claim error = %q, want the recorded reason", got.ClaimError)
	}

	goals, err := registry.List(ctx)
	if err != nil {
		t.Fatalf("list goals: %v", err)
	}
	for _, g := range goals {
		if g.Track == "" {
			t.Fatalf("goal %q listed with an empty track", g.OptimizationFunctionID)
		}
	}
}

func strPtr(s string) *string { return &s }

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

	// orchestrator: SELECT + DELETE on embeddings, no INSERT/UPDATE. It retires
	// rows the graph no longer backs; it never writes one.
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

	// service: SELECT/INSERT/UPDATE/DELETE on embeddings.
	service := pool(t, ctx, cfg.Postgres.ServiceDSN())
	embeddings := store.NewEmbeddingStore(service, 2)
	nodeID := testutil.NewID(t)
	if err := embeddings.Upsert(ctx, nodeID, "", vec768(0.2)); err != nil {
		t.Fatalf("service upsert embedding: %v", err)
	}
	if err := embeddings.Upsert(ctx, nodeID, "", vec768(0.3)); err != nil {
		t.Fatalf("service update embedding: %v", err)
	}
	if err := embeddings.Delete(ctx, nodeID); err != nil {
		t.Fatalf("service delete embedding: %v", err)
	}
	// The two seams can race on one orphan, so the loser must not see an error.
	if err := embeddings.Delete(ctx, nodeID); err != nil {
		t.Fatalf("service re-delete embedding: %v", err)
	}

	// Both seams retire on read as different roles, so DELETE has to reach each
	// one or the repair only logs.
	orphanID := testutil.NewID(t)
	if err := embeddings.Upsert(ctx, orphanID, "", vec768(0.4)); err != nil {
		t.Fatalf("seed orphan embedding: %v", err)
	}
	if err := store.NewEmbeddingStore(orchestrator, 2).Delete(ctx, orphanID); err != nil {
		t.Fatalf("orchestrator delete embedding: %v", err)
	}
	if err := orchestrator.QueryRow(ctx,
		"SELECT count(*) FROM meta_heuristic_embeddings WHERE node_id = $1", orphanID,
	).Scan(&count); err != nil {
		t.Fatalf("orchestrator count after delete: %v", err)
	}
	if count != 0 {
		t.Fatalf("expected the orphan row gone, found %d", count)
	}

	// 0008 widened the orchestrator to DELETE only. UPDATE is the neighbouring
	// privilege a `GRANT DELETE, UPDATE` slip would have added silently, and the
	// INSERT denial above would not catch it.
	if _, err := orchestrator.Exec(ctx,
		"UPDATE meta_heuristic_embeddings SET embedding = $1 WHERE node_id = $2",
		pgvector.NewVector(vec768(0.5)), nodeID,
	); err == nil {
		t.Fatal("expected orchestrator UPDATE on embeddings to be denied")
	}
}
