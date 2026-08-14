package orchestrator

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/arborette/arborette/internal/store"
)

// seedBoundDataset seeds one active dataset the binding tests submit against.
func seedBoundDataset() *fakeDatasets {
	ds := &fakeDatasets{}
	ds.seed(store.Dataset{ID: "d1", Name: "NCCD", Status: store.DatasetActive, DataSourceRef: "datasources/x/nccd.csv", OwnerID: testAnalystID})
	return ds
}

// postBoundGoal submits a goal carrying only dataset_id -- no file/import_path.
func postBoundGoal(t *testing.T, srv *Server, datasetID string) *httptest.ResponseRecorder {
	t.Helper()
	body, ct := multipartBody(t, map[string]string{"goal": "grow revenue", "dataset_id": datasetID}, "", "", "")
	req := httptest.NewRequest(http.MethodPost, "/goals", body)
	req.Header.Set("Content-Type", ct)
	rec := httptest.NewRecorder()
	srv.Routes().ServeHTTP(rec, req)
	return rec
}

// TestSubmitGoalToDataset: a goal submitted with dataset_id binds to that
// dataset's already-registered ref -- no upload, no implicit mint -- and the
// 201 confirms the parent along with the goal id.
func TestSubmitGoalToDataset(t *testing.T) {
	ds := seedBoundDataset()
	srv := testServer{
		goals:    &fakeGoals{},
		datasets: ds,
		claude:   &fakeClaude{matrix: fittedMatrix()},
		sandbox:  &fakeSandbox{introspect: revenueSchema(), execResps: []ExecuteResponse{{Value: map[string]any{"avg(revenue)": 10.0}}}},
	}.build()

	rec := postBoundGoal(t, srv, "d1")
	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201 (body %q)", rec.Code, rec.Body.String())
	}
	if srv.goals.(*fakeGoals).registeredRef != "datasources/x/nccd.csv" {
		t.Fatalf("registered ref = %q, want the dataset's ref (no minted ingest)", srv.goals.(*fakeGoals).registeredRef)
	}
	inserted := srv.goals.(*fakeGoals).inserted
	if inserted == nil || inserted.DatasetID != "d1" || inserted.DataSourceRef != "datasources/x/nccd.csv" {
		t.Fatalf("bound goal not persisted with the dataset: %+v", inserted)
	}
	var resp struct {
		OptimizationFunctionID string `json:"optimization_function_id"`
		DatasetID              string `json:"dataset_id"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if resp.DatasetID != "d1" {
		t.Fatalf("dataset_id in response = %q, want d1", resp.DatasetID)
	}
}

// TestSubmitGoalImplicitDataset: the legacy path (no dataset_id) mints an
// implicit dataset around the freshly ingested ref, so the goal still lands
// under a parent (FR-007).
func TestSubmitGoalImplicitDataset(t *testing.T) {
	ds := &fakeDatasets{}
	srv := testServer{
		goals:    &fakeGoals{},
		datasets: ds,
		claude:   &fakeClaude{matrix: fittedMatrix()},
		sandbox:  &fakeSandbox{introspect: revenueSchema(), execResps: []ExecuteResponse{{Value: map[string]any{"avg(revenue)": 10.0}}}},
	}.build()

	rec := postGoal(t, srv)
	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201 (body %q)", rec.Code, rec.Body.String())
	}
	inserted := srv.goals.(*fakeGoals).inserted
	if inserted == nil || inserted.DatasetID == "" {
		t.Fatalf("legacy goal has no dataset parent: %+v", inserted)
	}
	ref := srv.goals.(*fakeGoals).registeredRef
	ds.mu.Lock()
	var created *store.Dataset
	for i := range ds.datasets {
		if ds.datasets[i].DataSourceRef == ref {
			created = &ds.datasets[i]
		}
	}
	ds.mu.Unlock()
	if created == nil {
		t.Fatal("implicit dataset was not created for the legacy ref")
	}
	if created.Name != implicitDatasetName(ref) {
		t.Fatalf("implicit name = %q, want %q", created.Name, implicitDatasetName(ref))
	}
	if inserted.DatasetID != created.ID {
		t.Fatalf("goal bound to %q, want the implicit %q", inserted.DatasetID, created.ID)
	}
}

// TestSubmitGoalRefusesBadDataset: an unknown dataset and an archived dataset
// are both refused before any goal row is written. The unknown/inaccessible
// case is the uniform 404 (don't leak existence); the archived-but-accessible
// case keeps its 409.
func TestSubmitGoalRefusesBadDataset(t *testing.T) {
	t.Run("unknown dataset", func(t *testing.T) {
		srv := testServer{
			goals:    &fakeGoals{},
			datasets: &fakeDatasets{},
			claude:   &fakeClaude{matrix: fittedMatrix()},
			sandbox:  &fakeSandbox{introspect: revenueSchema()},
		}.build()
		rec := postBoundGoal(t, srv, "ghost")
		if rec.Code != http.StatusNotFound {
			t.Fatalf("status = %d, want 404 (body %q)", rec.Code, rec.Body.String())
		}
		if !strings.Contains(rec.Body.String(), "dataset not found") {
			t.Fatalf("404 body must name the missing dataset: %q", rec.Body.String())
		}
		if srv.goals.(*fakeGoals).inserted != nil {
			t.Fatal("a refused goal must not be persisted")
		}
	})

	t.Run("archived dataset", func(t *testing.T) {
		ds := &fakeDatasets{}
		ds.seed(store.Dataset{ID: "d1", Name: "Old", Status: store.DatasetArchived, DataSourceRef: "datasources/x/old.csv", OwnerID: testAnalystID})
		srv := testServer{
			goals:    &fakeGoals{},
			datasets: ds,
			claude:   &fakeClaude{matrix: fittedMatrix()},
			sandbox:  &fakeSandbox{introspect: revenueSchema()},
		}.build()
		rec := postBoundGoal(t, srv, "d1")
		if rec.Code != http.StatusConflict {
			t.Fatalf("status = %d, want 409 (body %q)", rec.Code, rec.Body.String())
		}
		if !strings.Contains(rec.Body.String(), "archived") {
			t.Fatalf("409 body must explain the archive gate: %q", rec.Body.String())
		}
		if srv.goals.(*fakeGoals).inserted != nil {
			t.Fatal("a refused goal must not be persisted")
		}
	})
}

// TestListGoalsByDataset: ?dataset_id narrows the objectives list to one
// dataset's children; without the param the full list stands.
func TestListGoalsByDataset(t *testing.T) {
	goals := &fakeGoals{list: []store.Goal{
		{OptimizationFunctionID: "g1", DatasetID: "d1"},
		{OptimizationFunctionID: "g2", DatasetID: "d2"},
	}}
	srv := testServer{goals: goals, datasets: &fakeDatasets{}, objects: &fakeObjects{}, heur: &fakeHeur{}, claude: &fakeClaude{}, sandbox: &fakeSandbox{}}.build()

	req := httptest.NewRequest(http.MethodGet, "/goals?dataset_id=d1", nil)
	rec := httptest.NewRecorder()
	srv.Routes().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d (body %q)", rec.Code, rec.Body.String())
	}
	var rows []goalListItemDTO
	if err := json.Unmarshal(rec.Body.Bytes(), &rows); err != nil {
		t.Fatalf("decode list: %v", err)
	}
	if len(rows) != 1 || rows[0].OptimizationFunctionID != "g1" {
		t.Fatalf("dataset-scoped list = %+v, want only g1", rows)
	}

	req = httptest.NewRequest(http.MethodGet, "/goals", nil)
	rec = httptest.NewRecorder()
	srv.Routes().ServeHTTP(rec, req)
	if err := json.Unmarshal(rec.Body.Bytes(), &rows); err != nil {
		t.Fatalf("decode full list: %v", err)
	}
	if len(rows) != 2 {
		t.Fatalf("unscoped list = %d rows, want 2", len(rows))
	}
}

// TestObjectivesCarryRunStatus: the dataset detail's objectives carry each
// goal's latest run status (completed here) beside the parent binding, and a
// goal with no run reads the synthetic "no run".
func TestObjectivesCarryRunStatus(t *testing.T) {
	t.Run("completed", func(t *testing.T) {
		ds := &fakeDatasets{}
		ds.seed(store.Dataset{ID: "d1", Name: "NCCD", Status: store.DatasetActive, DataSourceRef: "datasources/x/nccd.csv", OwnerID: testAnalystID})
		ds.objectivesByDataset = map[string][]store.Goal{"d1": {{OptimizationFunctionID: "g1", GoalText: "grow", DatasetID: "d1"}}}
		srv := testServer{datasets: ds, goals: &fakeGoals{}, objects: &fakeObjects{}, heur: &fakeHeur{}, claude: &fakeClaude{}, sandbox: &fakeSandbox{}}.build()
		srv.runs = &fakeRuns{latest: map[string]store.Run{"g1": {Status: store.RunCompleted}}}

		req := httptest.NewRequest(http.MethodGet, "/datasets/d1", nil)
		rec := httptest.NewRecorder()
		srv.Routes().ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d (body %q)", rec.Code, rec.Body.String())
		}
		var dto datasetDetailDTO
		if err := json.Unmarshal(rec.Body.Bytes(), &dto); err != nil {
			t.Fatalf("decode detail: %v", err)
		}
		if len(dto.Objectives) != 1 || dto.Objectives[0].Status != "completed" {
			t.Fatalf("objectives = %+v, want status completed", dto.Objectives)
		}
	})

	t.Run("no run is synthetic", func(t *testing.T) {
		ds := &fakeDatasets{}
		ds.seed(store.Dataset{ID: "d1", Name: "NCCD", Status: store.DatasetActive, DataSourceRef: "datasources/x/nccd.csv", OwnerID: testAnalystID})
		ds.objectivesByDataset = map[string][]store.Goal{"d1": {{OptimizationFunctionID: "g1", GoalText: "grow", DatasetID: "d1"}}}
		srv := testServer{datasets: ds, goals: &fakeGoals{}, objects: &fakeObjects{}, heur: &fakeHeur{}, claude: &fakeClaude{}, sandbox: &fakeSandbox{}}.build()

		req := httptest.NewRequest(http.MethodGet, "/datasets/d1", nil)
		rec := httptest.NewRecorder()
		srv.Routes().ServeHTTP(rec, req)
		var dto datasetDetailDTO
		if err := json.Unmarshal(rec.Body.Bytes(), &dto); err != nil {
			t.Fatalf("decode detail: %v", err)
		}
		if len(dto.Objectives) != 1 || dto.Objectives[0].Status != "no run" {
			t.Fatalf("objectives = %+v, want status \"no run\"", dto.Objectives)
		}
	})
}

// TestReconcileDatasetsBindsOrphans: the boot reconciler binds a goal without a
// dataset parent to the dataset for its ref and audits the reassignment, leaving
// already-parented goals untouched.
func TestReconcileDatasetsBindsOrphans(t *testing.T) {
	goals := &fakeGoals{list: []store.Goal{
		{OptimizationFunctionID: "g1", DataSourceRef: "datasources/x/orphan.csv"},
		{OptimizationFunctionID: "g2", DatasetID: "d1"},
	}}
	ds := &fakeDatasets{}
	audits := &fakeAudits{}
	ReconcileDatasets(context.Background(), goals, ds, nil, audits, StubIdentity{ID: "analyst-test"})

	created := ds.currentDatasets()
	if len(created) != 1 || created[0].DataSourceRef != "datasources/x/orphan.csv" {
		t.Fatalf("reconciler created %+v, want one dataset for the orphaned ref", created)
	}
	records := audits.records()
	if len(records) != 1 {
		t.Fatalf("dataset_reconcile audited %d record(s), want 1", len(records))
	}
	detail := records[0].Detail
	if detail["optimization_function_id"] != "g1" || detail["data_source_ref"] != "datasources/x/orphan.csv" {
		t.Fatalf("reconcile audit detail = %+v, want the vocabulary keys", detail)
	}
}
