package store_test

import (
	"context"
	"testing"

	"github.com/arborette/arborette/internal/domain"
	"github.com/arborette/arborette/internal/store"
	"github.com/arborette/arborette/internal/testutil"
	"github.com/pgvector/pgvector-go"
)

// TestResetAllWipesEveryTable seeds at least one row in every table the reset
// covers -- datasets, goals, runs, both verification surfaces, the ref ledger,
// embeddings, audit, users, and sessions -- then runs ResetAll as owner and
// asserts the whole database is empty, and that a second run on the now-empty
// database succeeds (idempotence, spec FR-003). EmptyCounts is asserted to
// report zero only after the wipe, so it is verified against the emptied state
// rather than assuming the shared database starts clean (which would violate
// the per-row assertion discipline -- the counts it must trust here are the
// zeroes the wipe produced).
func TestResetAllWipesEveryTable(t *testing.T) {
	ctx := context.Background()
	cfg := setup(t, ctx)
	owner := pool(t, ctx, cfg.Postgres.OwnerDSN())
	reg := store.NewGoalRegistry(owner)

	ref := "datasources/" + testutil.NewID(t) + "/reset.csv"
	if err := reg.RegisterDataSourceRef(ctx, ref); err != nil {
		t.Fatalf("register ref: %v", err)
	}

	datasetID := seedDataset(t, ctx, owner)
	goalID := testutil.NewID(t)
	if err := reg.Insert(ctx, store.Goal{
		OptimizationFunctionID: goalID,
		GoalText:               "reset fixture objective",
		EvaluationMatrix: domain.EvaluationMatrix{
			Targets: []domain.Target{{Field: "revenue", Direction: domain.Maximize, Aggregation: "avg"}},
		},
		DataSourceRef: ref,
		DatasetID:     datasetID,
	}); err != nil {
		t.Fatalf("insert goal: %v", err)
	}

	runs := store.NewRuns(owner)
	runID := testutil.NewID(t)
	if err := runs.Create(ctx, runID, goalID); err != nil {
		t.Fatalf("create run: %v", err)
	}
	if err := runs.SetStatus(ctx, runID, store.RunCompleted, ""); err != nil {
		t.Fatalf("settle run: %v", err)
	}

	if err := store.NewVerificationQueue(owner).Enqueue(ctx, store.VerificationEntry{
		QueueID: testutil.NewID(t), OptimizationFunctionID: goalID, OutcomeID: testutil.NewID(t),
		Field: "amount", ExtractedValue: "1.0", Confidence: 0.9,
	}); err != nil {
		t.Fatalf("enqueue verification: %v", err)
	}

	if _, err := owner.Exec(ctx,
		"INSERT INTO causal_verifications (id, goal_id, intervention_id, graph_version, status) VALUES ($1,$2,$3,1,'pending')",
		testutil.NewID(t), goalID, testutil.NewID(t)); err != nil {
		t.Fatalf("insert causal verification: %v", err)
	}

	embNodeID := testutil.NewID(t)
	if _, err := owner.Exec(ctx,
		"INSERT INTO meta_heuristic_embeddings (node_id, embedding) VALUES ($1,$2)",
		embNodeID, pgvector.NewVector(make([]float32, 768))); err != nil {
		t.Fatalf("insert embedding: %v", err)
	}

	if _, err := owner.Exec(ctx,
		"INSERT INTO audit_log (actor, action, event_type, detail) VALUES ('reset-test','reset_fixture','goal', $1)",
		`{"optimization_function_id": "`+goalID+`"}`); err != nil {
		t.Fatalf("insert audit row: %v", err)
	}

	var userID string
	if err := owner.QueryRow(ctx,
		"INSERT INTO users (username, password_hash, role) VALUES ($1, 'x', 'member') RETURNING id",
		"reset-"+testutil.NewID(t)).Scan(&userID); err != nil {
		t.Fatalf("insert user: %v", err)
	}
	if _, err := owner.Exec(ctx,
		"INSERT INTO sessions (user_id, token_hash) VALUES ($1, $2)", userID, "h"+testutil.NewID(t)); err != nil {
		t.Fatalf("insert session: %v", err)
	}

	removed, err := store.ResetAll(ctx, owner)
	if err != nil {
		t.Fatalf("reset all: %v", err)
	}
	for _, table := range store.ResetTables {
		if removed[table] < 1 {
			t.Fatalf("reset removed %d rows from %s, want at least the seeded one", removed[table], table)
		}
	}

	counts, err := store.EmptyCounts(ctx, owner)
	if err != nil {
		t.Fatalf("counts after reset: %v", err)
	}
	for _, table := range store.ResetTables {
		if counts[table] != 0 {
			t.Fatalf("table %s has %d rows after reset, want 0", table, counts[table])
		}
	}

	// Idempotence: resetting an already-clean database succeeds and changes nothing.
	if _, err := store.ResetAll(ctx, owner); err != nil {
		t.Fatalf("second reset on clean database: %v", err)
	}
}
