package orchestrator

// Integration tests for the dataset ownership & sharing feature. Gated on
// ARBORETTE_INTEGRATION like the rest of the suite, run against the live
// compose stack (`make up` then `make test`). These tests share one Postgres
// database with every other package, so they assert per-row effects only and
// register every fixture for per-row teardown through testutil immediately
// after creating it.

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/arborette/arborette/internal/config"
	"github.com/arborette/arborette/internal/domain"
	"github.com/arborette/arborette/internal/store"
	"github.com/arborette/arborette/internal/testutil"
	"github.com/jackc/pgx/v5"
)

// errorsIs is the errors.Is spelling the caretaker-resolution uses against the
// pgx no-rows sentinel, whose error identity is a wrapped sentinel.
func errorsIs(err, target error) bool {
	return errors.Is(err, target)
}

// accessServer wires a server whose DB-backed collaborators hit the live stack
// and whose infra seams (object store, graph, embeddings, LLM, sandbox) are
// fakes. It is the fixture the ownership tests drive `/datasets`, `/goals`, and
// the goal-keyed routes through, so the access predicate is exercised over the
// real SQL while nothing external is touched.
func accessServer(t *testing.T, ctx context.Context, cfg config.Config) (*Server, *store.Pool) {
	t.Helper()
	p, err := store.NewPool(ctx, cfg.Postgres.OrchestratorDSN())
	if err != nil {
		t.Fatalf("open orchestrator pool: %v", err)
	}
	t.Cleanup(p.Close)
	users := store.NewUserSessionStore(p)
	srv := testServer{
		repo:     &fakeRepo{},
		datasets: store.NewDatasetStore(p),
		goals:    store.NewGoalRegistry(p),
		users:    users,
		sessions: users,
		audits:   &fakeAudits{},
	}.build()
	return srv, p
}

// registerUser creates a real account through the user store and registers its
// teardown. The returned id is the fixture's identity for session minting.
func registerUser(t *testing.T, ctx context.Context, cfg config.Config, us *store.UserSessionStore, username string) store.User {
	t.Helper()
	u, err := us.Create(ctx, username, "hash-"+username, false)
	if err != nil {
		t.Fatalf("create user %q: %v", username, err)
	}
	testutil.RegisterUserCleanup(t, ctx, cfg, u.ID)
	return u
}

// signIn creates a session for the user and returns an authenticated request
// for the given method/path, the way the web client's cookie would hit it.
func signIn(t *testing.T, ctx context.Context, us *store.UserSessionStore, u store.User, method, path, body string) *http.Request {
	t.Helper()
	token, err := newMintToken()
	if err != nil {
		t.Fatalf("mint session token: %v", err)
	}
	if _, err := us.CreateSession(ctx, tokenHash(token), u.ID); err != nil {
		t.Fatalf("create session for %q: %v", u.Username, err)
	}
	var req *http.Request
	if body == "" {
		req = httptest.NewRequest(method, path, nil)
	} else {
		req = httptest.NewRequest(method, path, strings.NewReader(body))
	}
	req.AddCookie(&http.Cookie{Name: "arborette_session", Value: token})
	return req
}

// listDatasetIDs decodes a /datasets response into the id set it returned.
func listDatasetIDs(t *testing.T, rec *httptest.ResponseRecorder) []string {
	t.Helper()
	var out []datasetDTO
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode /datasets body %q: %v", rec.Body.String(), err)
	}
	ids := make([]string, 0, len(out))
	for _, d := range out {
		ids = append(ids, d.ID)
	}
	return ids
}

// TestOwnershipInventoryBoundary (US1): two users on the shared database; one
// creates a dataset, the other's inventory shows none of it in any form --
// neither the list nor a direct detail link -- while the creator sees it.
func TestOwnershipInventoryBoundary(t *testing.T) {
	ctx := context.Background()
	cfg := testutil.RequireIntegration(t)
	testutil.SetupPostgres(t, ctx, cfg)
	srv, p := accessServer(t, ctx, cfg)
	us := store.NewUserSessionStore(p)

	alice := registerUser(t, ctx, cfg, us, "own-alice-"+testutil.NewID(t))
	bob := registerUser(t, ctx, cfg, us, "own-bob-"+testutil.NewID(t))

	ds := store.NewDatasetStore(p)
	datasetID, err := ds.Create(ctx, store.Dataset{
		Name:          "own-boundary-" + testutil.NewID(t),
		Description:   "fixture",
		Status:        store.DatasetActive,
		DataSourceRef: "datasources/" + testutil.NewID(t) + "/boundary.csv",
		OwnerID:       bob.ID,
	})
	if err != nil {
		t.Fatalf("create dataset: %v", err)
	}
	testutil.RegisterDatasetCleanup(t, ctx, cfg, datasetID)

	// Alice's inventory: nothing -- no placeholders, no count, no error.
	aliceList := httptest.NewRecorder()
	srv.Routes().ServeHTTP(aliceList, signIn(t, ctx, us, alice, http.MethodGet, "/datasets", ""))
	if aliceList.Code != http.StatusOK {
		t.Fatalf("alice list status = %d, body %q", aliceList.Code, aliceList.Body.String())
	}
	for _, id := range listDatasetIDs(t, aliceList) {
		if id == datasetID {
			t.Fatalf("alice's inventory leaked bob's dataset %q", datasetID)
		}
	}

	// Bob's inventory: the dataset is his, classified owner, and listed.
	bobList := httptest.NewRecorder()
	srv.Routes().ServeHTTP(bobList, signIn(t, ctx, us, bob, http.MethodGet, "/datasets", ""))
	if bobList.Code != http.StatusOK {
		t.Fatalf("bob list status = %d, body %q", bobList.Code, bobList.Body.String())
	}
	found := false
	var out []datasetDTO
	if err := json.Unmarshal(bobList.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode bob list: %v", err)
	}
	for _, d := range out {
		if d.ID != datasetID {
			continue
		}
		found = true
		if d.Access != "owner" {
			t.Fatalf("bob's own dataset classified access=%q, want owner", d.Access)
		}
	}
	if !found {
		t.Fatalf("bob's inventory omitted his own dataset %q: %+v", datasetID, out)
	}

	// Alice's direct detail link: uniform 404, same as an unknown id.
	aliceDetail := httptest.NewRecorder()
	srv.Routes().ServeHTTP(aliceDetail, signIn(t, ctx, us, alice, http.MethodGet, "/datasets/"+datasetID, ""))
	if aliceDetail.Code != http.StatusNotFound {
		t.Fatalf("alice detail status = %d, body %q, want 404", aliceDetail.Code, aliceDetail.Body.String())
	}

	// Bob's detail link serves and touches (the access event the inventory ranks by).
	bobDetail := httptest.NewRecorder()
	srv.Routes().ServeHTTP(bobDetail, signIn(t, ctx, us, bob, http.MethodGet, "/datasets/"+datasetID, ""))
	if bobDetail.Code != http.StatusOK {
		t.Fatalf("bob detail status = %d, body %q", bobDetail.Code, bobDetail.Body.String())
	}
	time.Sleep(2 * time.Millisecond) // deterministic ordering not asserted here, just present

	// Sharing flips alice's view: after a grant she lists it and it is "shared".
	if err := ds.Share(ctx, datasetID, alice.ID, bob.ID); err != nil {
		t.Fatalf("share dataset with alice: %v", err)
	}
	aliceShared := httptest.NewRecorder()
	srv.Routes().ServeHTTP(aliceShared, signIn(t, ctx, us, alice, http.MethodGet, "/datasets", ""))
	if aliceShared.Code != http.StatusOK {
		t.Fatalf("alice shared list status = %d", aliceShared.Code)
	}
	shared := false
	if err := json.Unmarshal(aliceShared.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode alice shared list: %v", err)
	}
	for _, d := range out {
		if d.ID != datasetID {
			continue
		}
		shared = true
		if d.Access != "shared" {
			t.Fatalf("shared dataset classified access=%q, want shared", d.Access)
		}
	}
	if !shared {
		t.Fatalf("alice's inventory omitted the shared dataset %q", datasetID)
	}

	// And after revocation her view goes dark again.
	if err := ds.RevokeShare(ctx, datasetID, alice.ID); err != nil {
		t.Fatalf("revoke share: %v", err)
	}
	aliceRevoked := httptest.NewRecorder()
	srv.Routes().ServeHTTP(aliceRevoked, signIn(t, ctx, us, alice, http.MethodGet, "/datasets", ""))
	for _, id := range listDatasetIDs(t, aliceRevoked) {
		if id == datasetID {
			t.Fatalf("alice still sees the revoked dataset %q", datasetID)
		}
	}
}

// TestOwnerlessDatasetInvisible (US7 store side): a dataset with a NULL owner
// (pre-ownership residue on the shared stack) is visible to nobody until
// attributed, not even to an admin who is not the caretaker.
func TestOwnerlessDatasetInvisible(t *testing.T) {
	ctx := context.Background()
	cfg := testutil.RequireIntegration(t)
	testutil.SetupPostgres(t, ctx, cfg)
	srv, p := accessServer(t, ctx, cfg)
	us := store.NewUserSessionStore(p)

	ds := store.NewDatasetStore(p)
	datasetID, err := ds.Create(ctx, store.Dataset{
		Name:          "own-ownerless-" + testutil.NewID(t),
		Description:   "fixture",
		Status:        store.DatasetActive,
		DataSourceRef: "datasources/" + testutil.NewID(t) + "/ownerless.csv",
	})
	if err != nil {
		t.Fatalf("create ownerless dataset: %v", err)
	}
	testutil.RegisterDatasetCleanup(t, ctx, cfg, datasetID)

	member := registerUser(t, ctx, cfg, us, "own-member-"+testutil.NewID(t))
	rec := httptest.NewRecorder()
	srv.Routes().ServeHTTP(rec, signIn(t, ctx, us, member, http.MethodGet, "/datasets/"+datasetID, ""))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("ownerless dataset detail status = %d, want 404 (invisible to a non-caretaker)", rec.Code)
	}

	// Attribution to the caretaker flips it visible to the caretaker only.
	// Guarantee an active admin exists (a fresh stack has none yet; the shared
	// suite's other packages seed their own), then resolve the caretaker as the
	// runtime rule does: earliest active admin by created_at, id.
	caretaker, err := us.EarliestActiveAdmin(ctx)
	if errorsIs(err, pgx.ErrNoRows) {
		caretaker, err = us.Create(ctx, "own-caretaker-"+testutil.NewID(t), "hash-caretaker", true)
		testutil.RegisterUserCleanup(t, ctx, cfg, caretaker.ID)
		if err != nil {
			t.Fatalf("seed caretaker admin: %v", err)
		}
	} else if err != nil {
		t.Fatalf("earliest active admin: %v", err)
	}
	if _, err := p.Exec(ctx, "UPDATE datasets SET owner_id = $1 WHERE id = $2", caretaker.ID, datasetID); err != nil {
		t.Fatalf("attribute ownerless dataset: %v", err)
	}
	// LIFO teardown would delete an attributed caretaker before the dataset that
	// references it, tripping datasets.owner_id's FK. Re-register the dataset
	// cleanup (idempotent) so it runs first and the caretaker can go after.
	testutil.RegisterDatasetCleanup(t, ctx, cfg, datasetID)
	caretakerRec := httptest.NewRecorder()
	srv.Routes().ServeHTTP(caretakerRec, signIn(t, ctx, us, caretaker, http.MethodGet, "/datasets/"+datasetID, ""))
	if caretakerRec.Code != http.StatusOK {
		t.Fatalf("caretaker detail status = %d, want 200 after attribution", caretakerRec.Code)
	}

	// The member still cannot see it after attribution.
	memberAfter := httptest.NewRecorder()
	srv.Routes().ServeHTTP(memberAfter, signIn(t, ctx, us, member, http.MethodGet, "/datasets/"+datasetID, ""))
	if memberAfter.Code != http.StatusNotFound {
		t.Fatalf("member detail status = %d, want 404 after attribution", memberAfter.Code)
	}
}

// TestChildrenInheritDatasetVisibility (US2): every child object under B's
// dataset -- goals, runs, verifications -- is reachable exactly when the acting
// user can access the dataset. A's deep links and list reads all answer the
// uniform 404 / empty inventory while B (the owner) sees everything, and B
// sharing the dataset with A flips A's view for the whole subtree at once.
func TestChildrenInheritDatasetVisibility(t *testing.T) {
	ctx := context.Background()
	cfg := testutil.RequireIntegration(t)
	testutil.SetupPostgres(t, ctx, cfg)
	srv, p := accessServer(t, ctx, cfg)
	us := store.NewUserSessionStore(p)

	alice := registerUser(t, ctx, cfg, us, "own-child-alice-"+testutil.NewID(t))
	bob := registerUser(t, ctx, cfg, us, "own-child-bob-"+testutil.NewID(t))

	ds := store.NewDatasetStore(p)
	datasetID, err := ds.Create(ctx, store.Dataset{
		Name:          "own-children-" + testutil.NewID(t),
		Description:   "fixture",
		Status:        store.DatasetActive,
		DataSourceRef: "datasources/" + testutil.NewID(t) + "/children.csv",
		OwnerID:       bob.ID,
	})
	if err != nil {
		t.Fatalf("create dataset: %v", err)
	}
	testutil.RegisterDatasetCleanup(t, ctx, cfg, datasetID)

	goals := store.NewGoalRegistry(p)
	threshold := 0.5
	goalID := testutil.NewID(t)
	if err := goals.Insert(ctx, store.Goal{
		OptimizationFunctionID: goalID,
		GoalText:               "minimize latency under load",
		EvaluationMatrix:       domain.EvaluationMatrix{},
		DataSourceRef:          "datasources/" + datasetID + "/children.csv",
		DatasetID:              datasetID,
		ConfidenceThreshold:    &threshold,
		EpochMode:              store.EpochSpeculative,
		Track:                  store.TrackExplore,
		CreatedBy:              bob.ID,
	}); err != nil {
		t.Fatalf("insert goal: %v", err)
	}
	testutil.RegisterGoalCleanup(t, ctx, cfg, goalID)

	// Alice's list reads the empty inventory -- no goal at all.
	aliceGoals := httptest.NewRecorder()
	srv.Routes().ServeHTTP(aliceGoals, signIn(t, ctx, us, alice, http.MethodGet, "/goals", ""))
	if aliceGoals.Code != http.StatusOK {
		t.Fatalf("alice goals list status = %d", aliceGoals.Code)
	}
	if !strings.Contains(aliceGoals.Body.String(), "[]") {
		t.Fatalf("alice's goal list is not empty: %s", aliceGoals.Body.String())
	}

	// Alice's dataset-tagged list and her direct deep links: uniform 404/empty.
	aliceScoped := httptest.NewRecorder()
	srv.Routes().ServeHTTP(aliceScoped, signIn(t, ctx, us, alice,
		http.MethodGet, "/goals?dataset_id="+datasetID, ""))
	if aliceScoped.Code != http.StatusOK || !strings.Contains(aliceScoped.Body.String(), "[]") {
		t.Fatalf("alice scoped goal list = %d %s, want empty", aliceScoped.Code, aliceScoped.Body.String())
	}
	aliceDetail := httptest.NewRecorder()
	srv.Routes().ServeHTTP(aliceDetail, signIn(t, ctx, us, alice, http.MethodGet, "/goals/"+goalID+"/outcomes", ""))
	if aliceDetail.Code != http.StatusNotFound {
		t.Fatalf("alice goal outcomes status = %d, want 404", aliceDetail.Code)
	}

	// Bob sees his goal through every surface.
	bobGoals := httptest.NewRecorder()
	srv.Routes().ServeHTTP(bobGoals, signIn(t, ctx, us, bob, http.MethodGet, "/goals", ""))
	if bobGoals.Code != http.StatusOK || !strings.Contains(bobGoals.Body.String(), goalID) {
		t.Fatalf("bob goals list = %d %s, want his goal", bobGoals.Code, bobGoals.Body.String())
	}
	bobDetail := httptest.NewRecorder()
	srv.Routes().ServeHTTP(bobDetail, signIn(t, ctx, us, bob, http.MethodGet, "/goals/"+goalID+"/outcomes", ""))
	if bobDetail.Code != http.StatusOK {
		t.Fatalf("bob goal outcomes status = %d", bobDetail.Code)
	}

	// The dataset detail endpoints the inventory and objective views share: A
	// cannot enumerate B's goals off the dataset id either.
	aliceDataset := httptest.NewRecorder()
	srv.Routes().ServeHTTP(aliceDataset, signIn(t, ctx, us, alice, http.MethodGet, "/datasets/"+datasetID, ""))
	if aliceDataset.Code != http.StatusNotFound {
		t.Fatalf("alice dataset detail status = %d, want 404", aliceDataset.Code)
	}

	// Sharing the dataset flips A's whole subtree at once.
	if err := ds.Share(ctx, datasetID, alice.ID, bob.ID); err != nil {
		t.Fatalf("share dataset: %v", err)
	}
	aliceAfter := httptest.NewRecorder()
	srv.Routes().ServeHTTP(aliceAfter, signIn(t, ctx, us, alice, http.MethodGet, "/goals", ""))
	if aliceAfter.Code != http.StatusOK || !strings.Contains(aliceAfter.Body.String(), goalID) {
		t.Fatalf("alice goals after share = %d %s, want the shared goal", aliceAfter.Code, aliceAfter.Body.String())
	}
	aliceDetailAfter := httptest.NewRecorder()
	srv.Routes().ServeHTTP(aliceDetailAfter, signIn(t, ctx, us, alice, http.MethodGet, "/goals/"+goalID+"/outcomes", ""))
	if aliceDetailAfter.Code != http.StatusOK {
		t.Fatalf("alice goal outcomes after share = %d, want 200", aliceDetailAfter.Code)
	}

	// And revocation hides the subtree from her again.
	if err := ds.RevokeShare(ctx, datasetID, alice.ID); err != nil {
		t.Fatalf("revoke share: %v", err)
	}
	aliceRevoked := httptest.NewRecorder()
	srv.Routes().ServeHTTP(aliceRevoked, signIn(t, ctx, us, alice, http.MethodGet, "/goals", ""))
	if aliceRevoked.Code != http.StatusOK || strings.Contains(aliceRevoked.Body.String(), goalID) {
		t.Fatalf("alice goals after revoke = %d %s, want empty", aliceRevoked.Code, aliceRevoked.Body.String())
	}
}

// TestOwnershipAtCreation (US3): creating something makes the creator its owner
// immediately. A dataset minted through POST /datasets is owned by the acting
// user and invisible to everyone else; a goal registered through POST /goals
// into a shared dataset records the submitter as created_by and inherits the
// dataset's access set -- visible to whoever can reach the dataset, no more.
func TestOwnershipAtCreation(t *testing.T) {
	ctx := context.Background()
	cfg := testutil.RequireIntegration(t)
	testutil.SetupPostgres(t, ctx, cfg)
	srv, p := accessServer(t, ctx, cfg)
	us := store.NewUserSessionStore(p)

	alice := registerUser(t, ctx, cfg, us, "own-create-alice-"+testutil.NewID(t))
	bob := registerUser(t, ctx, cfg, us, "own-create-bob-"+testutil.NewID(t))

	// Alice creates a dataset through the real handler: a multipart upload whose
	// object-store seam is the fake, everything below it the live DB. signIn()
	// rebuilds the request from a raw body, which would drop the multipart
	// framing, so mint the session cookie here and attach it to the real request.
	token, err := newMintToken()
	if err != nil {
		t.Fatalf("mint session token: %v", err)
	}
	if _, err := us.CreateSession(ctx, tokenHash(token), alice.ID); err != nil {
		t.Fatalf("create session for %q: %v", alice.Username, err)
	}
	body, contentType := multipartBody(t, map[string]string{
		"name":        "own-created-" + testutil.NewID(t),
		"description": "fixture",
	}, "file", "created.csv", "a,b\n1,2\n")
	createReq := httptest.NewRequest(http.MethodPost, "/datasets", body)
	createReq.Header.Set("Content-Type", contentType)
	createReq.AddCookie(&http.Cookie{Name: "arborette_session", Value: token})
	createRec := httptest.NewRecorder()
	srv.Routes().ServeHTTP(createRec, createReq)
	if createRec.Code != http.StatusCreated {
		t.Fatalf("create dataset status = %d, body %q", createRec.Code, createRec.Body.String())
	}

	// Resolve the minted dataset id and confirm ownership: the creator owns it.
	datasetID := ""
	{
		var list []datasetDTO
		listRec := httptest.NewRecorder()
		srv.Routes().ServeHTTP(listRec, signIn(t, ctx, us, alice, http.MethodGet, "/datasets", ""))
		if err := json.Unmarshal(listRec.Body.Bytes(), &list); err != nil {
			t.Fatalf("decode alice inventory: %v", err)
		}
		for _, d := range list {
			if d.OwnerID == alice.ID && d.Name != "" && strings.HasPrefix(d.Name, "own-created-") {
				datasetID = d.ID
				if d.OwnerID != alice.ID {
					t.Fatalf("created dataset owner_id = %q, want the creator %q", d.OwnerID, alice.ID)
				}
			}
		}
		if datasetID == "" {
			t.Fatalf("alice's inventory omitted her freshly created dataset: %+v", list)
		}
	}
	testutil.RegisterDatasetCleanup(t, ctx, cfg, datasetID)

	// Bob's inventory: the brand-new dataset is not his, so it must not appear.
	bobList := httptest.NewRecorder()
	srv.Routes().ServeHTTP(bobList, signIn(t, ctx, us, bob, http.MethodGet, "/datasets", ""))
	for _, id := range listDatasetIDs(t, bobList) {
		if id == datasetID {
			t.Fatalf("bob's inventory leaked alice's new dataset %q", datasetID)
		}
	}

	// Share the dataset with bob, then bob registers a goal into it. The bound
	// path exercises the real handler: dataset_id binds to the shared dataset's
	// ref, and the submission records bob as the goal's creator.
	ds := store.NewDatasetStore(p)
	if err := ds.Share(ctx, datasetID, bob.ID, alice.ID); err != nil {
		t.Fatalf("share dataset with bob: %v", err)
	}
	submitServer, sp := accessSubmitServer(t, ctx, cfg)
	defer sp.Close()
	goalBody, goalCT := multipartBody(t, map[string]string{
		"goal":       "minimize latency",
		"dataset_id": datasetID,
	}, "", "", "")
	bobToken, err := newMintToken()
	if err != nil {
		t.Fatalf("mint session token: %v", err)
	}
	if _, err := us.CreateSession(ctx, tokenHash(bobToken), bob.ID); err != nil {
		t.Fatalf("create session for %q: %v", bob.Username, err)
	}
	goalReq := httptest.NewRequest(http.MethodPost, "/goals", goalBody)
	goalReq.Header.Set("Content-Type", goalCT)
	goalReq.AddCookie(&http.Cookie{Name: "arborette_session", Value: bobToken})
	goalRec := httptest.NewRecorder()
	submitServer.Routes().ServeHTTP(goalRec, goalReq)
	if goalRec.Code != http.StatusCreated {
		t.Fatalf("bob submit goal status = %d, body %q", goalRec.Code, goalRec.Body.String())
	}
	var submit struct {
		OptimizationFunctionID string `json:"optimization_function_id"`
	}
	if err := json.Unmarshal(goalRec.Body.Bytes(), &submit); err != nil {
		t.Fatalf("decode submit response: %v", err)
	}
	testutil.RegisterGoalCleanup(t, ctx, cfg, submit.OptimizationFunctionID)

	// The goal row records bob as its creator.
	goal, err := store.NewGoalRegistry(p).Get(ctx, submit.OptimizationFunctionID)
	if err != nil {
		t.Fatalf("read back goal: %v", err)
	}
	if goal.CreatedBy != bob.ID {
		t.Fatalf("goal created_by = %q, want the submitter %q", goal.CreatedBy, bob.ID)
	}

	// The goal's accessibility follows the dataset: bob (collaborator) and alice
	// (owner) both reach it; a third party never does.
	charlie := registerUser(t, ctx, cfg, us, "own-create-charlie-"+testutil.NewID(t))
	for _, who := range []struct {
		u      store.User
		status int
	}{
		{bob, http.StatusOK},
		{alice, http.StatusOK},
		{charlie, http.StatusNotFound},
	} {
		rec := httptest.NewRecorder()
		srv.Routes().ServeHTTP(rec, signIn(t, ctx, us, who.u, http.MethodGet, "/goals/"+submit.OptimizationFunctionID+"/outcomes", ""))
		if rec.Code != who.status {
			t.Fatalf("%s goal outcomes status = %d, want %d", who.u.Username, rec.Code, who.status)
		}
	}
}

// TestShareDatasetLifecycle (US4, T028): the owner grants working access to a
// collaborator by username through the real handlers. The recipient joins the
// share list, sees the dataset, and a non-owner is refused share-management
// entirely -- a collaborator answers 403, never the grant list.
func TestShareDatasetLifecycle(t *testing.T) {
	ctx := context.Background()
	cfg := testutil.RequireIntegration(t)
	testutil.SetupPostgres(t, ctx, cfg)
	srv, p := accessServer(t, ctx, cfg)
	us := store.NewUserSessionStore(p)

	owner := registerUser(t, ctx, cfg, us, "own-share-owner-"+testutil.NewID(t))
	collab := registerUser(t, ctx, cfg, us, "own-share-collab-"+testutil.NewID(t))

	ds := store.NewDatasetStore(p)
	datasetID, err := ds.Create(ctx, store.Dataset{
		Name:          "own-share-" + testutil.NewID(t),
		Description:   "fixture",
		Status:        store.DatasetActive,
		DataSourceRef: "datasources/" + testutil.NewID(t) + "/share.csv",
		OwnerID:       owner.ID,
	})
	if err != nil {
		t.Fatalf("create dataset: %v", err)
	}
	testutil.RegisterDatasetCleanup(t, ctx, cfg, datasetID)

	// Before any grant, the share list is empty.
	empty := httptest.NewRecorder()
	srv.Routes().ServeHTTP(empty, signIn(t, ctx, us, owner, http.MethodGet, "/datasets/"+datasetID+"/shares", ""))
	if empty.Code != http.StatusOK || !strings.Contains(empty.Body.String(), "[]") {
		t.Fatalf("initial share list = %d %s, want empty []", empty.Code, empty.Body.String())
	}

	// Owner grants access by (case-insensitive) username: the response carries
	// the recipient and the next list includes them.
	grant := httptest.NewRecorder()
	srv.Routes().ServeHTTP(grant, signIn(t, ctx, us, owner, http.MethodPut, "/datasets/"+datasetID+"/shares/"+strings.ToUpper(collab.Username), ""))
	if grant.Code != http.StatusOK {
		t.Fatalf("share grant status = %d, body %q", grant.Code, grant.Body.String())
	}
	var grantDTO shareGrantDTO
	if err := json.Unmarshal(grant.Body.Bytes(), &grantDTO); err != nil {
		t.Fatalf("decode grant response: %v", err)
	}
	if grantDTO.UserID != collab.ID || grantDTO.Username != collab.Username {
		t.Fatalf("grant response = %+v, want user %q", grantDTO, collab.ID)
	}

	list := httptest.NewRecorder()
	srv.Routes().ServeHTTP(list, signIn(t, ctx, us, owner, http.MethodGet, "/datasets/"+datasetID+"/shares", ""))
	if list.Code != http.StatusOK {
		t.Fatalf("share list status = %d, body %q", list.Code, list.Body.String())
	}
	var shares []shareGrantDTO
	if err := json.Unmarshal(list.Body.Bytes(), &shares); err != nil {
		t.Fatalf("decode share list: %v", err)
	}
	found := false
	for _, g := range shares {
		if g.UserID == collab.ID {
			found = true
			if g.Username != collab.Username {
				t.Fatalf("share list username = %q, want %q", g.Username, collab.Username)
			}
		}
	}
	if !found {
		t.Fatalf("share list omitted the grant: %+v", shares)
	}

	// The collaborator can now reach the dataset; a third party still cannot.
	collabDetail := httptest.NewRecorder()
	srv.Routes().ServeHTTP(collabDetail, signIn(t, ctx, us, collab, http.MethodGet, "/datasets/"+datasetID, ""))
	if collabDetail.Code != http.StatusOK {
		t.Fatalf("collaborator detail after grant = %d, want 200", collabDetail.Code)
	}

	// Share-management is owner-only: the collaborator is refused with 403, and
	// the refusal is not a 404 -- the grant list must never leak to the very
	// accounts it names, but the dataset itself is known to them.
	refusedList := httptest.NewRecorder()
	srv.Routes().ServeHTTP(refusedList, signIn(t, ctx, us, collab, http.MethodGet, "/datasets/"+datasetID+"/shares", ""))
	if refusedList.Code != http.StatusForbidden {
		t.Fatalf("collaborator share list status = %d, want 403", refusedList.Code)
	}
	refusedGrant := httptest.NewRecorder()
	srv.Routes().ServeHTTP(refusedGrant, signIn(t, ctx, us, collab, http.MethodPut, "/datasets/"+datasetID+"/shares/"+owner.Username, ""))
	if refusedGrant.Code != http.StatusForbidden {
		t.Fatalf("collaborator grant status = %d, want 403", refusedGrant.Code)
	}

	// Refusals that never grant: sharing with the owner (self), an unknown
	// username, and a duplicate grant.
	self := httptest.NewRecorder()
	srv.Routes().ServeHTTP(self, signIn(t, ctx, us, owner, http.MethodPut, "/datasets/"+datasetID+"/shares/"+owner.Username, ""))
	if self.Code != http.StatusBadRequest {
		t.Fatalf("share-with-self status = %d, want 400", self.Code)
	}
	unknown := httptest.NewRecorder()
	srv.Routes().ServeHTTP(unknown, signIn(t, ctx, us, owner, http.MethodPut, "/datasets/"+datasetID+"/shares/own-nobody-"+testutil.NewID(t), ""))
	if unknown.Code != http.StatusNotFound {
		t.Fatalf("share-unknown-username status = %d, want 404", unknown.Code)
	}
	duplicate := httptest.NewRecorder()
	srv.Routes().ServeHTTP(duplicate, signIn(t, ctx, us, owner, http.MethodPut, "/datasets/"+datasetID+"/shares/"+collab.Username, ""))
	if duplicate.Code != http.StatusConflict {
		t.Fatalf("duplicate share status = %d, want 409", duplicate.Code)
	}

	// Revoking through the real handler removes the grant: 204, the next list is
	// empty, and the recipient's access is gone again.
	revoked := httptest.NewRecorder()
	srv.Routes().ServeHTTP(revoked, signIn(t, ctx, us, owner, http.MethodDelete, "/datasets/"+datasetID+"/shares/"+collab.Username, ""))
	if revoked.Code != http.StatusNoContent {
		t.Fatalf("revoke status = %d, body %q, want 204", revoked.Code, revoked.Body.String())
	}
	revokedList := httptest.NewRecorder()
	srv.Routes().ServeHTTP(revokedList, signIn(t, ctx, us, owner, http.MethodGet, "/datasets/"+datasetID+"/shares", ""))
	if revokedList.Code != http.StatusOK || !strings.Contains(revokedList.Body.String(), "[]") {
		t.Fatalf("share list after revoke = %d %s, want empty []", revokedList.Code, revokedList.Body.String())
	}
	revokedDetail := httptest.NewRecorder()
	srv.Routes().ServeHTTP(revokedDetail, signIn(t, ctx, us, collab, http.MethodGet, "/datasets/"+datasetID, ""))
	if revokedDetail.Code != http.StatusNotFound {
		t.Fatalf("collaborator detail after revoke = %d, want 404", revokedDetail.Code)
	}
}

// accessSubmitServer wires a server whose DB-backed collaborators hit the live
// stack and whose LLM/sandbox seams are scripted fakes, so a bound goal can be
// registered through the real handler against a real dataset.
func accessSubmitServer(t *testing.T, ctx context.Context, cfg config.Config) (*Server, *store.Pool) {
	t.Helper()
	p, err := store.NewPool(ctx, cfg.Postgres.OrchestratorDSN())
	if err != nil {
		t.Fatalf("open orchestrator pool: %v", err)
	}
	users := store.NewUserSessionStore(p)
	srv := testServer{
		repo:     &fakeRepo{},
		datasets: store.NewDatasetStore(p),
		goals:    store.NewGoalRegistry(p),
		users:    users,
		sessions: users,
		audits:   &fakeAudits{},
		claude:   &fakeClaude{matrix: fittedMatrix()},
		sandbox: &fakeSandbox{
			introspect: revenueSchema(),
			execResps:  []ExecuteResponse{{Value: map[string]any{"avg(revenue)": 10.0}}},
		},
	}.build()
	return srv, p
}
