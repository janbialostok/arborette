package store_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/arborette/arborette/internal/domain"
	"github.com/arborette/arborette/internal/store"
	"github.com/arborette/arborette/internal/testutil"
	"github.com/jackc/pgx/v5"
)

// matrixFixture returns a tabular matrix for goal-registry fixtures.
func matrixFixture() domain.EvaluationMatrix {
	return domain.EvaluationMatrix{Targets: []domain.Target{{Field: "revenue", Direction: domain.Maximize, Aggregation: "avg"}}}
}

// containsDataset reports whether a list holds the dataset id, for per-row
// assertions that must tolerate other rows on the shared database.
func containsDataset(datasets []store.Dataset, id string) bool {
	for _, d := range datasets {
		if d.ID == id {
			return true
		}
	}
	return false
}

// seedDatasetRow inserts a dataset through the store and returns its id, for the
// integration tests that need one rather than a bare SQL seed. The name carries
// a fresh uuid, since lower(name) is globally unique on the shared database.
func seedDatasetRow(t *testing.T, ctx context.Context, p *store.Pool, ds *store.DatasetStore, name string) string {
	t.Helper()
	id, err := ds.Create(ctx, store.Dataset{
		Name:          "inv-" + name + "-" + testutil.NewID(t),
		Description:   "integration fixture",
		Status:        store.DatasetActive,
		DataSourceRef: "datasources/" + testutil.NewID(t) + "/" + name + ".csv",
	})
	if err != nil {
		t.Fatalf("create dataset %q: %v", name, err)
	}
	return id
}

// TestDatasetInventory exercises the inventory reads (US1): create, the ?q
// name-substring filter, the derived objective count riding the LEFT JOIN, and
// the detail Get. Per-row assertions only -- the shared database may hold other
// datasets.
func TestDatasetInventory(t *testing.T) {
	ctx := context.Background()
	cfg := setup(t, ctx)
	p := pool(t, ctx, cfg.Postgres.OrchestratorDSN())
	ds := store.NewDatasetStore(p)

	firstID := seedDatasetRow(t, ctx, p, ds, "acme")
	time.Sleep(2 * time.Millisecond) // keep created_at distinct so ordering is deterministic
	secondID := seedDatasetRow(t, ctx, p, ds, "beta")

	// A ?q filter narrows case-insensitively to the matching rows only. Per-row
	// assertions: the shared database accumulates prior runs' inv-acme-* fixtures,
	// so the filter's exact width is not ours to assert -- what is ours is that
	// the freshly created acme row is present and the beta row is absent.
	filtered, err := ds.List(ctx, "acme")
	if err != nil {
		t.Fatalf("list filtered: %v", err)
	}
	if !containsDataset(filtered, firstID) {
		t.Fatalf("q=acme omitted the acme dataset %q: %+v", firstID, filtered)
	}
	if containsDataset(filtered, secondID) {
		t.Fatalf("q=acme included the beta dataset %q: %+v", secondID, filtered)
	}

	// A goal bound to the second dataset makes its derived count 1, visible
	// through both List and Get (the LEFT JOIN the usage derives from).
	goalID := testutil.NewID(t)
	if err := store.NewGoalRegistry(p).Insert(ctx, store.Goal{
		OptimizationFunctionID: goalID,
		GoalText:               "grow beta revenue",
		EvaluationMatrix:       matrixFixture(),
		DataSourceRef:          "datasources/x/beta.csv",
		DatasetID:              secondID,
	}); err != nil {
		t.Fatalf("insert goal: %v", err)
	}

	all, err := ds.List(ctx, "")
	if err != nil {
		t.Fatalf("list all: %v", err)
	}
	for _, d := range all {
		if d.ID == secondID && d.ObjectiveCount != 1 {
			t.Fatalf("beta objective count = %d, want 1", d.ObjectiveCount)
		}
		if d.ID == firstID && d.ObjectiveCount != 0 {
			t.Fatalf("empty acme objective count = %d, want 0", d.ObjectiveCount)
		}
	}

	got, err := ds.Get(ctx, firstID)
	if err != nil {
		t.Fatalf("get dataset: %v", err)
	}
	if got.ObjectiveCount != 0 || got.Status != store.DatasetActive || got.Description == "" {
		t.Fatalf("detail mismatch: %+v", got)
	}
}

// TestDatasetLifecycle covers create-conflict, metadata update, and the empty
// delete with refcount-aware retirement (US2/US6 store side).
func TestDatasetLifecycle(t *testing.T) {
	ctx := context.Background()
	cfg := setup(t, ctx)
	p := pool(t, ctx, cfg.Postgres.OrchestratorDSN())
	ds := store.NewDatasetStore(p)

	id := seedDatasetRow(t, ctx, p, ds, "sales")
	created, err := ds.Get(ctx, id)
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	// Duplicate names conflict case-insensitively (the lower(name) index).
	if _, err := ds.Create(ctx, store.Dataset{
		Name:          strings.ToUpper(created.Name),
		Description:   "",
		DataSourceRef: "datasources/" + testutil.NewID(t) + "/x.csv",
	}); !errors.Is(err, store.ErrNameConflict) {
		t.Fatalf("duplicate (case-insensitive) name: err=%v, want ErrNameConflict", err)
	}

	// Metadata-only update: name, description, status; the ref is untouched.
	if err := ds.Update(ctx, id, created.Name, "renamed for 2024", store.DatasetArchived); err != nil {
		t.Fatalf("update: %v", err)
	}
	updated, err := ds.Get(ctx, id)
	if err != nil {
		t.Fatalf("read updated: %v", err)
	}
	if updated.Description != "renamed for 2024" || updated.Status != store.DatasetArchived || updated.DataSourceRef != created.DataSourceRef {
		t.Fatalf("update did not persist: %+v", updated)
	}

	// A dataset's ref is not unique: a second dataset shares it, so retirement
	// must not drop the ref while more than one entity refers to it. The ref
	// registry row scope lives in the handler (DataSourceRefUsage decides);
	// the store's own DeleteDataSourceRef is unconditional.
	ref := "datasources/" + testutil.NewID(t) + "/shared.csv"
	if err := store.NewGoalRegistry(p).RegisterDataSourceRef(ctx, ref); err != nil {
		t.Fatalf("register ref: %v", err)
	}
	sharedID, err := ds.Create(ctx, store.Dataset{
		Name:          "shared-first-" + testutil.NewID(t),
		Description:   "",
		Status:        store.DatasetActive,
		DataSourceRef: ref,
	})
	if err != nil {
		t.Fatalf("create first ref-sharing dataset: %v", err)
	}
	if _, err := ds.Create(ctx, store.Dataset{
		Name:          "shared-second-" + testutil.NewID(t),
		Description:   "",
		Status:        store.DatasetActive,
		DataSourceRef: ref,
	}); err != nil {
		t.Fatalf("create second ref-sharing dataset: %v", err)
	}
	if d, g, err := ds.DataSourceRefUsage(ctx, ref); err != nil || d != 2 || g != 0 {
		t.Fatalf("ref usage after two datasets = (%d,%d) err=%v, want (2,0)", d, g, err)
	}
	if err := ds.DeleteDataSourceRef(ctx, ref); err != nil {
		t.Fatalf("delete ref: %v", err)
	}
	if exists, err := store.NewGoalRegistry(p).DataSourceRefExists(ctx, ref); err != nil || exists {
		t.Fatalf("deleted ref still exists: exists=%v err=%v (the store deletes unconditionally; the handler owns the count)", exists, err)
	}

	// A dataset that still holds a goal cannot be deleted outright -- the goal's
	// NO ACTION FK stands in the way until the goal is removed.
	boundID := testutil.NewID(t)
	if err := store.NewGoalRegistry(p).Insert(ctx, store.Goal{
		OptimizationFunctionID: boundID,
		GoalText:               "blocks its dataset delete",
		EvaluationMatrix:       matrixFixture(),
		DataSourceRef:          "datasources/" + testutil.NewID(t) + "/data.csv",
		DatasetID:              id,
	}); err != nil {
		t.Fatalf("insert bound goal: %v", err)
	}
	if err := ds.Delete(ctx, id); err == nil {
		t.Fatal("expected deleting a dataset that still holds a goal to fail on the FK")
	}
	// Removing the goal frees the dataset; the delete then succeeds.
	if err := store.NewGoalRegistry(p).Delete(ctx, boundID); err != nil {
		t.Fatalf("delete bound goal: %v", err)
	}
	if err := ds.Delete(ctx, id); err != nil {
		t.Fatalf("delete dataset after its goal left: %v", err)
	}

	// Empty dataset delete succeeds.
	if err := ds.Delete(ctx, sharedID); err != nil {
		t.Fatalf("delete empty dataset: %v", err)
	}
	if _, err := ds.Get(ctx, sharedID); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("get deleted shared dataset: err=%v, want pgx.ErrNoRows", err)
	}
}

// TestGoalDatasetBinding (US3): the goal registry round-trips its dataset parent
// and re-parenting write, and the delete guards stand on a live store -- a
// running run and a pending causal verification each refuse deletion.
func TestGoalDatasetBinding(t *testing.T) {
	ctx := context.Background()
	cfg := setup(t, ctx)
	orch := pool(t, ctx, cfg.Postgres.OrchestratorDSN())
	svc := pool(t, ctx, cfg.Postgres.ServiceDSN())
	reg := store.NewGoalRegistry(orch)
	ds := store.NewDatasetStore(orch)

	firstID := seedDatasetRow(t, ctx, orch, ds, "first")
	secondID := seedDatasetRow(t, ctx, orch, ds, "second")

	goalID := testutil.NewID(t)
	if err := reg.Insert(ctx, store.Goal{
		OptimizationFunctionID: goalID,
		GoalText:               "bound objective",
		EvaluationMatrix:       matrixFixture(),
		DataSourceRef:          "datasources/" + testutil.NewID(t) + "/data.csv",
		DatasetID:              firstID,
	}); err != nil {
		t.Fatalf("insert bound goal: %v", err)
	}

	// ListByDataset scopes to the parent; the row carries the parent id back.
	byDataset, err := reg.ListByDataset(ctx, firstID)
	if err != nil || len(byDataset) != 1 || byDataset[0].OptimizationFunctionID != goalID {
		t.Fatalf("ListByDataset = (%d rows, %v), want the bound goal", len(byDataset), err)
	}
	if byDataset[0].DatasetID != firstID {
		t.Fatalf("ListByDataset did not round-trip DatasetID: %+v", byDataset[0])
	}

	// Re-parenting (the boot reconciliation write) moves the goal and only it.
	if err := reg.SetDatasetID(ctx, goalID, secondID); err != nil {
		t.Fatalf("re-parent goal: %v", err)
	}
	got, err := reg.Get(ctx, goalID)
	if err != nil || got.DatasetID != secondID {
		t.Fatalf("re-parented goal = %+v err=%v, want dataset %q", got, err, secondID)
	}
	if orphaned, err := reg.ListByDataset(ctx, firstID); err != nil || len(orphaned) != 0 {
		t.Fatalf("first dataset still lists %d goal(s) after re-parent", len(orphaned))
	}
	if err := reg.SetDatasetID(ctx, testutil.NewID(t), secondID); err == nil {
		t.Fatal("re-parenting an unknown goal must error")
	}

	// Running-run guard.
	runs := store.NewRuns(orch)
	runID := testutil.NewID(t)
	if err := runs.Create(ctx, runID, goalID); err != nil {
		t.Fatalf("create running run: %v", err)
	}
	if err := reg.Delete(ctx, goalID); !errors.Is(err, store.ErrGoalRunning) {
		t.Fatalf("delete with running run: err=%v, want ErrGoalRunning", err)
	}
	if err := runs.SetStatus(ctx, runID, store.RunCompleted, ""); err != nil {
		t.Fatalf("settle run: %v", err)
	}

	// Pending-verification guard, cleared by a terminal Complete.
	causal := store.NewCausalVerifications(svc, time.Minute, 10, 100)
	rec, accepted, err := causal.DispatchAccept(ctx, dispatchRec(t, goalID, testutil.NewID(t), 1, false))
	if err != nil || !accepted {
		t.Fatalf("dispatch causal verification: accepted=%v err=%v", accepted, err)
	}
	if err := reg.Delete(ctx, goalID); !errors.Is(err, store.ErrVerificationPending) {
		t.Fatalf("delete with pending verification: err=%v, want ErrVerificationPending", err)
	}
	completed, err := causal.Complete(ctx, rec.ID, "causally_verified", nil, nil, nil, nil, nil)
	if err != nil || !completed {
		t.Fatalf("complete verification: completed=%v err=%v", completed, err)
	}

	// With nothing outstanding the delete succeeds and clears the goal's runs.
	if err := reg.Delete(ctx, goalID); err != nil {
		t.Fatalf("delete goal: %v", err)
	}
	if still, err := reg.ListByDataset(ctx, secondID); err != nil || len(still) != 0 {
		t.Fatalf("goal still listed after delete: %d rows err=%v", len(still), err)
	}
}

// TestDeleteGoalRetiresEmbeddings (US6 store side): a deleted goal's embedding
// corpus and run rows go with it, freeing its dataset for deletion.
func TestDeleteGoalRetiresEmbeddings(t *testing.T) {
	ctx := context.Background()
	cfg := setup(t, ctx)
	orch := pool(t, ctx, cfg.Postgres.OrchestratorDSN())
	svc := pool(t, ctx, cfg.Postgres.ServiceDSN())
	reg := store.NewGoalRegistry(orch)
	ds := store.NewDatasetStore(orch)
	embeddings := store.NewEmbeddingStore(svc, 2)

	datasetID := seedDatasetRow(t, ctx, orch, ds, "emb")
	goalID := testutil.NewID(t)
	if err := reg.Insert(ctx, store.Goal{
		OptimizationFunctionID: goalID,
		GoalText:               "embeddings retire with this",
		EvaluationMatrix:       matrixFixture(),
		DataSourceRef:          "datasources/" + testutil.NewID(t) + "/data.csv",
		DatasetID:              datasetID,
	}); err != nil {
		t.Fatalf("insert goal: %v", err)
	}

	nodeID := testutil.NewID(t)
	if err := embeddings.Upsert(ctx, nodeID, goalID, vec768(0.25)); err != nil {
		t.Fatalf("upsert embedding: %v", err)
	}

	if err := reg.Delete(ctx, goalID); err != nil {
		t.Fatalf("delete goal: %v", err)
	}
	// The goal delete clears runs/verifications; the orchestrator then retires
	// embeddings. Both writes together leave the node unreachable by goal.
	if err := embeddings.DeleteByGoal(ctx, goalID); err != nil {
		t.Fatalf("delete embeddings by goal: %v", err)
	}
	refs, err := embeddings.ListNodeRefs(ctx)
	if err != nil {
		t.Fatalf("list node refs: %v", err)
	}
	for _, ref := range refs {
		if ref.NodeID == nodeID {
			t.Fatalf("node %q still maps to goal %q after delete", nodeID, ref.GoalID)
		}
	}

	// With the goal gone the dataset is empty and deletable.
	if err := ds.Delete(ctx, datasetID); err != nil {
		t.Fatalf("delete emptied dataset: %v", err)
	}
	if n, err := ds.CountObjectives(ctx, datasetID); err != nil || n != 0 {
		t.Fatalf("count objectives after delete = %d err=%v, want 0", n, err)
	}
}