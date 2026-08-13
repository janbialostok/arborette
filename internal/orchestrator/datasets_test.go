package orchestrator

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/arborette/arborette/internal/domain"
	"github.com/arborette/arborette/internal/store"
	"github.com/jackc/pgx/v5"
)

// datasetTestServer wires a server with fake collaborators pre-seeded for the
// datasets handlers: one dataset over "sources/nccd.csv" whose ref is registered
// (refExists), and a graph-embedding seam whose deletions are recorded.
func datasetTestServer(ds *fakeDatasets, repo graphRepo, goals goalStore, emb *fakeEmbeddings) *Server {
	if goals == nil {
		goals = &fakeGoals{refExists: true}
	}
	if emb == nil {
		emb = &fakeEmbeddings{}
	}
	if ds == nil {
		ds = &fakeDatasets{}
	}
	return testServer{datasets: ds, goals: goals, repo: repo, embeddings: emb, audits: &fakeAudits{}}.build()
}

func doReq(t *testing.T, srv *Server, method, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	var r *http.Request
	if body == "" {
		r = httptest.NewRequest(method, path, nil)
	} else {
		r = httptest.NewRequest(method, path, strings.NewReader(body))
	}
	rec := httptest.NewRecorder()
	srv.Routes().ServeHTTP(rec, r)
	return rec
}

func TestCreateDataset(t *testing.T) {
	// multipartBody builds the same form a real client sends (name field + file
	// part); the fake objects store absorbs the Put so no sandbox is involved.
	t.Run("uploads a source and echoes the row", func(t *testing.T) {
		body, ct := multipartBody(t, map[string]string{"name": "NCCD", "description": "kpi"},
			"file", "data.csv", "a\n")
		req := httptest.NewRequest(http.MethodPost, "/datasets", body)
		req.Header.Set("Content-Type", ct)
		rec := httptest.NewRecorder()
		goals := &fakeGoals{}
		srv := testServer{datasets: &fakeDatasets{}, goals: goals, objects: &fakeObjects{},
			audits: &fakeAudits{}}.build()
		srv.Routes().ServeHTTP(rec, req)

		if rec.Code != http.StatusCreated {
			t.Fatalf("status = %d, body %q", rec.Code, rec.Body.String())
		}
		var out datasetDTO
		if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
			t.Fatalf("decode: %v", err)
		}
		if out.ID == "" || out.Name != "NCCD" || out.Usage != "empty" {
			t.Fatalf("unexpected row: %+v", out)
		}
		if goals.registeredRef == "" || !strings.HasPrefix(goals.registeredRef, "datasources/") {
			t.Fatalf("ref not registered from ingest: %q", goals.registeredRef)
		}
	})

	t.Run("duplicate name is a 409", func(t *testing.T) {
		ds := &fakeDatasets{}
		ds.seed(store.Dataset{ID: "d1", Name: "NCCD", DataSourceRef: "datasources/x/data.csv"})
		body, ct := multipartBody(t, map[string]string{"name": "nccd"}, "file", "data.csv", "a\n")
		req := httptest.NewRequest(http.MethodPost, "/datasets", body)
		req.Header.Set("Content-Type", ct)
		rec := httptest.NewRecorder()
		srv := testServer{datasets: ds, goals: &fakeGoals{}, objects: &fakeObjects{},
			audits: &fakeAudits{}}.build()
		srv.Routes().ServeHTTP(rec, req)
		if rec.Code != http.StatusConflict {
			t.Fatalf("status = %d, body %q, want 409", rec.Code, rec.Body.String())
		}
	})

	t.Run("missing name is a 400", func(t *testing.T) {
		body, ct := multipartBody(t, map[string]string{}, "file", "data.csv", "a\n")
		req := httptest.NewRequest(http.MethodPost, "/datasets", body)
		req.Header.Set("Content-Type", ct)
		rec := httptest.NewRecorder()
		srv := testServer{datasets: &fakeDatasets{}, goals: &fakeGoals{}, objects: &fakeObjects{},
			audits: &fakeAudits{}}.build()
		srv.Routes().ServeHTTP(rec, req)
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("status = %d, want 400", rec.Code)
		}
	})

	t.Run("missing source maps through the ingest error", func(t *testing.T) {
		body, ct := multipartBody(t, map[string]string{"name": "N"}, "", "", "")
		req := httptest.NewRequest(http.MethodPost, "/datasets", body)
		req.Header.Set("Content-Type", ct)
		rec := httptest.NewRecorder()
		srv := testServer{datasets: &fakeDatasets{}, goals: &fakeGoals{}, objects: &fakeObjects{},
			audits: &fakeAudits{}}.build()
		srv.Routes().ServeHTTP(rec, req)
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("status = %d, want 400 (missing source)", rec.Code)
		}
	})

	t.Run("name conflict from the store is a 409", func(t *testing.T) {
		body, ct := multipartBody(t, map[string]string{"name": "N"}, "file", "data.csv", "a\n")
		req := httptest.NewRequest(http.MethodPost, "/datasets", body)
		req.Header.Set("Content-Type", ct)
		rec := httptest.NewRecorder()
		srv := testServer{datasets: &fakeDatasets{createErr: store.ErrNameConflict},
			goals: &fakeGoals{}, objects: &fakeObjects{}, audits: &fakeAudits{}}.build()
		srv.Routes().ServeHTTP(rec, req)
		if rec.Code != http.StatusConflict {
			t.Fatalf("status = %d, want 409", rec.Code)
		}
	})
}

func TestListDatasets(t *testing.T) {
	ds := &fakeDatasets{}
	ds.seed(
		store.Dataset{ID: "d1", Name: "NCCD", DataSourceRef: "datasources/x/nccd.csv",
			Status: store.DatasetActive, ObjectiveCount: 3},
		store.Dataset{ID: "d2", Name: "ACS", DataSourceRef: "datasources/x/acs.csv",
			Status: store.DatasetArchived},
	)
	ds.objectivesByDataset = map[string][]store.Goal{"d1": {{OptimizationFunctionID: "g1"}}}
	srv := datasetTestServer(ds, nil, nil, nil)

	t.Run("all rows newest first with derived counts", func(t *testing.T) {
		rec := doReq(t, srv, http.MethodGet, "/datasets", "")
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, body %q", rec.Code, rec.Body.String())
		}
		var out []datasetDTO
		if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
			t.Fatalf("decode: %v", err)
		}
		if len(out) != 2 {
			t.Fatalf("got %d datasets, want 2", len(out))
		}
		if out[0].ObjectiveCount != 1 {
			t.Fatalf("d1 objective_count = %d, want 1 (derived)", out[0].ObjectiveCount)
		}
	})

	t.Run("q filters by name substring", func(t *testing.T) {
		rec := doReq(t, srv, http.MethodGet, "/datasets?q=acs", "")
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d", rec.Code)
		}
		var out []datasetDTO
		if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
			t.Fatalf("decode: %v", err)
		}
		if len(out) != 1 || out[0].ID != "d2" {
			t.Fatalf("filtered result = %+v, want only d2", out)
		}
	})
}

func TestGetDataset(t *testing.T) {
	ds := &fakeDatasets{}
	ds.seed(store.Dataset{ID: "d1", Name: "NCCD", Status: store.DatasetActive, DataSourceRef: "datasources/x/nccd.csv"})
	ds.objectivesByDataset = map[string][]store.Goal{"d1": {
		{OptimizationFunctionID: "g1", GoalText: "reduce latency"},
	}}
	srv := datasetTestServer(ds, nil, nil, nil)

	rec := doReq(t, srv, http.MethodGet, "/datasets/d1", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body %q", rec.Code, rec.Body.String())
	}
	var out datasetDetailDTO
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if out.ID != "d1" || out.Status != string(store.DatasetActive) || out.Usage != "in_use" {
		t.Fatalf("unexpected row: %+v", out)
	}
	if len(out.Objectives) != 1 || out.Objectives[0].OptimizationFunctionID != "g1" ||
		out.Objectives[0].DatasetID != "d1" {
		t.Fatalf("objectives = %+v, want the child g1 bound to d1", out.Objectives)
	}

	t.Run("unknown id is a 404", func(t *testing.T) {
		rec := doReq(t, srv, http.MethodGet, "/datasets/missing", "")
		if rec.Code != http.StatusNotFound {
			t.Fatalf("status = %d, want 404", rec.Code)
		}
	})
}

func TestUpdateDataset(t *testing.T) {
	ds := &fakeDatasets{}
	ds.seed(store.Dataset{ID: "d1", Name: "NCCD", Description: "kpi", DataSourceRef: "datasources/x/nccd.csv"})
	srv := datasetTestServer(ds, nil, nil, nil)

	t.Run("edits metadata only", func(t *testing.T) {
		rec := doReq(t, srv, http.MethodPatch, "/datasets/d1", `{"description":"updated","status":"archived"}`)
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, body %q", rec.Code, rec.Body.String())
		}
		var out datasetDTO
		if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
			t.Fatalf("decode: %v", err)
		}
		if out.Description != "updated" || out.Status != "archived" {
			t.Fatalf("unexpected row: %+v", out)
		}
	})

	t.Run("rename onto existing name is a 409", func(t *testing.T) {
		ds2 := &fakeDatasets{}
		ds2.seed(store.Dataset{ID: "d1", Name: "NCCD", DataSourceRef: "r1"},
			store.Dataset{ID: "d2", Name: "ACS", DataSourceRef: "r2"})
		srv := datasetTestServer(ds2, nil, nil, nil)
		rec := doReq(t, srv, http.MethodPatch, "/datasets/d1", `{"name":"acs"}`)
		if rec.Code != http.StatusConflict {
			t.Fatalf("status = %d, want 409", rec.Code)
		}
	})

	t.Run("datasource_ref is rejected", func(t *testing.T) {
		rec := doReq(t, srv, http.MethodPatch, "/datasets/d1", `{"datasource_ref":"datasources/x/sneak.csv"}`)
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("status = %d, want 400", rec.Code)
		}
		if ds.datasets[0].DataSourceRef != "datasources/x/nccd.csv" {
			t.Fatalf("the immutable ref must not be rewritten: %+v", ds.datasets[0])
		}
	})

	t.Run("invalid status is a 400", func(t *testing.T) {
		rec := doReq(t, srv, http.MethodPatch, "/datasets/d1", `{"status":"deleted"}`)
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("status = %d, want 400", rec.Code)
		}
	})

	t.Run("unknown id is a 404", func(t *testing.T) {
		rec := doReq(t, srv, http.MethodPatch, "/datasets/nope", `{"name":"x"}`)
		if rec.Code != http.StatusNotFound {
			t.Fatalf("status = %d, want 404", rec.Code)
		}
	})
}

func TestDeleteDataset(t *testing.T) {
	ref := "datasources/x/nccd.csv"

	t.Run("non-empty dataset is 409 with the offending objectives", func(t *testing.T) {
		ds := &fakeDatasets{}
		ds.seed(store.Dataset{ID: "d1", Name: "NCCD", DataSourceRef: ref})
		ds.objectivesByDataset = map[string][]store.Goal{"d1": {
			{OptimizationFunctionID: "g1", GoalText: "reduce latency"},
		}}
		audits := &fakeAudits{}
		srv := testServer{datasets: ds, goals: &fakeGoals{}, repo: nil, embeddings: &fakeEmbeddings{}, audits: audits}.build()
		rec := doReq(t, srv, http.MethodDelete, "/datasets/d1", "")
		if rec.Code != http.StatusConflict {
			t.Fatalf("status = %d, body %q, want 409", rec.Code, rec.Body.String())
		}
		var out map[string]any
		if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
			t.Fatalf("decode: %v", err)
		}
		objs := out["objectives"].([]any)
		if len(objs) != 1 {
			t.Fatalf("objectives = %+v, want the one blocking goal", objs)
		}
		first := objs[0].(map[string]any)
		if first["optimization_function_id"] != "g1" || first["dataset_id"] != "d1" {
			t.Fatalf("blocking objective = %v, want g1 bound to d1", first)
		}
		if audits.records() != nil {
			t.Fatalf("refused delete must not audit")
		}
	})

	t.Run("empty dataset is 204 and retires an unreferenced ref", func(t *testing.T) {
		ds := &fakeDatasets{}
		ds.seed(store.Dataset{ID: "d1", Name: "NCCD", DataSourceRef: ref})
		ds.refUsage = map[string][2]int{ref: {0, 0}}
		objects := &fakeObjects{}
		audits := &fakeAudits{}
		srv := testServer{datasets: ds, goals: &fakeGoals{}, objects: objects, repo: nil, embeddings: &fakeEmbeddings{}, audits: audits}.build()
		rec := doReq(t, srv, http.MethodDelete, "/datasets/d1", "")
		if rec.Code != http.StatusNoContent {
			t.Fatalf("status = %d, body %q, want 204", rec.Code, rec.Body.String())
		}
		if got := objects.deletedKeys(); len(got) != 1 || got[0] != ref {
			t.Fatalf("retired objects = %+v, want [%q]", got, ref)
		}
		if got := ds.deletedRefs; len(got) != 1 || got[0] != ref {
			t.Fatalf("deleted ref rows = %+v, want [%q]", got, ref)
		}
		recs := audits.records()
		if len(recs) != 1 || recs[0].Action != "dataset_delete" {
			t.Fatalf("audit = %+v, want one dataset_delete", recs)
		}
		if detail := recs[0].Detail; detail["dataset_id"] != "d1" || detail["data_source_ref"] != ref {
			t.Fatalf("audit detail = %v, want dataset_id + data_source_ref keys", detail)
		}
	})

	t.Run("a still-referenced ref is kept", func(t *testing.T) {
		ds := &fakeDatasets{}
		ds.seed(store.Dataset{ID: "d1", Name: "NCCD", DataSourceRef: ref})
		ds.refUsage = map[string][2]int{ref: {1, 0}}
		objects := &fakeObjects{}
		srv := testServer{datasets: ds, goals: &fakeGoals{}, objects: objects, repo: nil, embeddings: &fakeEmbeddings{}, audits: &fakeAudits{}}.build()
		rec := doReq(t, srv, http.MethodDelete, "/datasets/d1", "")
		if rec.Code != http.StatusNoContent {
			t.Fatalf("status = %d, want 204", rec.Code)
		}
		if got := objects.deletedKeys(); len(got) != 0 {
			t.Fatalf("object %q retired while another dataset still references the ref", got)
		}
		if len(ds.deletedRefs) != 0 {
			t.Fatalf("ref row deleted while still referenced: %+v", ds.deletedRefs)
		}
	})

	t.Run("unknown id is a 404", func(t *testing.T) {
		srv := datasetTestServer(nil, nil, nil, nil)
		rec := doReq(t, srv, http.MethodDelete, "/datasets/missing", "")
		if rec.Code != http.StatusNotFound {
			t.Fatalf("status = %d, want 404", rec.Code)
		}
	})
}

func TestDeleteGoal(t *testing.T) {
	t.Run("success retires graph and embeddings and audits with the vocabulary", func(t *testing.T) {
		repo := &fakeRepo{}
		ds := &fakeDatasets{}
		ds.seed(store.Dataset{ID: "dset-1", Name: "NCCD", DataSourceRef: "sources/x.csv"})
		emb := &fakeEmbeddings{}
		audits := &fakeAudits{}
		goals := &fakeGoals{get: store.Goal{OptimizationFunctionID: "g1", DataSourceRef: "sources/x.csv"}}
		srv := testServer{repo: repo, goals: goals, datasets: ds, embeddings: emb, audits: audits}.build()

		rec := doReq(t, srv, http.MethodDelete, "/goals/g1", "")
		if rec.Code != http.StatusNoContent {
			t.Fatalf("status = %d, body %q, want 204", rec.Code, rec.Body.String())
		}
		if got := repo.deletedGoalGraphs; len(got) != 1 || got[0] != "g1" {
			t.Fatalf("graph deletes = %+v, want [g1]", got)
		}
		if got := emb.deletedByGoal; len(got) != 1 || got[0] != "g1" {
			t.Fatalf("embedding deletes = %+v, want [g1]", got)
		}
		recs := audits.records()
		if len(recs) != 1 || recs[0].Action != "objective_delete" {
			t.Fatalf("audit = %+v, want one objective_delete", recs)
		}
		detail := recs[0].Detail
		if detail["optimization_function_id"] != "g1" ||
			detail["data_source_ref"] != "sources/x.csv" ||
			detail["dataset_id"] != "dset-1" {
			t.Fatalf("audit detail = %v, want the full vocabulary keys", detail)
		}
	})

	t.Run("a running goal is a 409 and retires nothing", func(t *testing.T) {
		repo := &fakeRepo{}
		emb := &fakeEmbeddings{}
		audits := &fakeAudits{}
		goals := &fakeGoals{
			get:       store.Goal{OptimizationFunctionID: "g1", DataSourceRef: "r1"},
			deleteErr: store.ErrGoalRunning,
		}
		srv := testServer{repo: repo, goals: goals, embeddings: emb, audits: audits, datasets: &fakeDatasets{}}.build()
		rec := doReq(t, srv, http.MethodDelete, "/goals/g1", "")
		if rec.Code != http.StatusConflict {
			t.Fatalf("status = %d, want 409", rec.Code)
		}
		if len(repo.deletedGoalGraphs) != 0 || len(emb.deletedByGoal) != 0 || len(audits.records()) != 0 {
			t.Fatalf("guarded delete must not touch graph, embeddings, or audit")
		}
	})

	t.Run("a pending verification is a 409", func(t *testing.T) {
		goals := &fakeGoals{get: store.Goal{OptimizationFunctionID: "g1"}, deleteErr: store.ErrVerificationPending}
		srv := testServer{repo: &fakeRepo{}, goals: goals, datasets: &fakeDatasets{}, embeddings: &fakeEmbeddings{}, audits: &fakeAudits{}}.build()
		rec := doReq(t, srv, http.MethodDelete, "/goals/g1", "")
		if rec.Code != http.StatusConflict {
			t.Fatalf("status = %d, want 409", rec.Code)
		}
	})

	t.Run("unknown goal is a 404", func(t *testing.T) {
		goals := &fakeGoals{get: store.Goal{}, getErr: pgx.ErrNoRows}
		srv := testServer{repo: &fakeRepo{}, goals: goals, datasets: &fakeDatasets{}, embeddings: &fakeEmbeddings{}, audits: &fakeAudits{}}.build()
		rec := doReq(t, srv, http.MethodDelete, "/goals/g1", "")
		if rec.Code != http.StatusNotFound {
			t.Fatalf("status = %d, want 404", rec.Code)
		}
	})
}

func TestDeleteHeuristic(t *testing.T) {
	t.Run("success removes node and embedding and audits", func(t *testing.T) {
		repo := &fakeRepo{
			metaHeuristicsByID: map[string]domain.MetaHeuristic{
				"mh-1": {ID: "mh-1", GoalID: "g1"},
			},
		}
		emb := &fakeEmbeddings{}
		audits := &fakeAudits{}
		srv := testServer{repo: repo, embeddings: emb, audits: audits, goals: &fakeGoals{}, datasets: &fakeDatasets{}}.build()
		rec := doReq(t, srv, http.MethodDelete, "/heuristics/mh-1", "")
		if rec.Code != http.StatusNoContent {
			t.Fatalf("status = %d, body %q, want 204", rec.Code, rec.Body.String())
		}
		if got := repo.deletedHeuristics; len(got) != 1 || got[0] != "mh-1" {
			t.Fatalf("graph deletes = %+v, want [mh-1]", got)
		}
		if got := emb.deleted; len(got) != 1 || got[0] != "mh-1" {
			t.Fatalf("embedding deletes = %+v, want [mh-1]", got)
		}
		recs := audits.records()
		if len(recs) != 1 || recs[0].Action != "heuristic_remove" {
			t.Fatalf("audit = %+v, want one heuristic_remove", recs)
		}
		detail := recs[0].Detail
		if detail["heuristic_id"] != "mh-1" || detail["optimization_function_id"] != "g1" {
			t.Fatalf("audit detail = %v, want heuristic_id + optimization_function_id", detail)
		}
	})

	t.Run("unknown heuristic is a 404", func(t *testing.T) {
		srv := testServer{repo: &fakeRepo{}, goals: &fakeGoals{}, datasets: &fakeDatasets{}, embeddings: &fakeEmbeddings{}, audits: &fakeAudits{}}.build()
		rec := doReq(t, srv, http.MethodDelete, "/heuristics/mh-nope", "")
		if rec.Code != http.StatusNotFound {
			t.Fatalf("status = %d, want 404", rec.Code)
		}
	})
}
