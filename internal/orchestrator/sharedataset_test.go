package orchestrator

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/arborette/arborette/internal/store"
)

// shareTestServer wires a server whose acting identity is the test analyst and
// whose roster carries the collaborator the share handlers look up. The audits
// fake is the one wired into the server, so the caller can assert audit writes.
func shareTestServer(ds *fakeDatasets) (*Server, *fakeUserStore, *fakeAudits) {
	users := newFakeUserStore()
	audits := &fakeAudits{}
	srv := testServer{datasets: ds, users: users, audits: audits}.build()
	return srv, users, audits
}

// TestShareDatasetOwnerOnly (US4): sharing management answers only to the owner.
// The acting test analyst owns the seeded dataset; a dataset owned by someone
// else, and an unknown id, both refuse with the uniform answers, and the owner
// granting by username lands a Share on the store plus an audit row.
func TestShareDatasetOwnerOnly(t *testing.T) {
	ctx := context.Background()

	t.Run("owner grants by username", func(t *testing.T) {
		ds := &fakeDatasets{}
		ds.seed(store.Dataset{ID: "d-own", Name: "shared", DataSourceRef: "datasources/x.csv", OwnerID: testAnalystID})
		users := newFakeUserStore()
		collab, err := users.Create(ctx, "maya", "hash", false)
		if err != nil {
			t.Fatalf("seed collaborator: %v", err)
		}
		audits := &fakeAudits{}
		srv := testServer{datasets: ds, users: users, audits: audits}.build()

		rec := doReq(t, srv, http.MethodPut, "/datasets/d-own/shares/MAYA", "")
		if rec.Code != http.StatusOK {
			t.Fatalf("grant status = %d, body %q, want 200", rec.Code, rec.Body.String())
		}
		var out shareGrantDTO
		if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
			t.Fatalf("decode grant: %v", err)
		}
		if out.UserID != collab.ID || out.Username != "maya" {
			t.Fatalf("grant = %+v, want user %q", out, collab.ID)
		}
		grants := ds.sharesFor("d-own")
		if len(grants) != 1 || grants[0].UserID != collab.ID {
			t.Fatalf("store shares = %+v, want one grant to %q", grants, collab.ID)
		}
		var shared bool
		for _, r := range audits.records() {
			if r.Action == "dataset_share" {
				shared = true
				if r.Detail["dataset_id"] != "d-own" {
					t.Fatalf("audit detail = %+v, want dataset_id d-own", r.Detail)
				}
			}
		}
		if !shared {
			t.Fatalf("no dataset_share audit: %+v", audits.records())
		}
	})

	t.Run("list answers the owner", func(t *testing.T) {
		ds := &fakeDatasets{}
		ds.seed(store.Dataset{ID: "d-own", Name: "shared", DataSourceRef: "datasources/x.csv", OwnerID: testAnalystID})
		users := newFakeUserStore()
		if _, err := users.Create(ctx, "maya", "hash", false); err != nil {
			t.Fatalf("seed collaborator: %v", err)
		}
		srv := testServer{datasets: ds, users: users, audits: &fakeAudits{}}.build()
		ds.Share(ctx, "d-own", "user-maya", testAnalystID)

		rec := doReq(t, srv, http.MethodGet, "/datasets/d-own/shares", "")
		if rec.Code != http.StatusOK {
			t.Fatalf("list status = %d, body %q", rec.Code, rec.Body.String())
		}
		if !strings.Contains(rec.Body.String(), "user-maya") {
			t.Fatalf("share list omitted the grant: %s", rec.Body.String())
		}
	})

	t.Run("revoke removes the grant idempotently", func(t *testing.T) {
		ds := &fakeDatasets{}
		ds.seed(store.Dataset{ID: "d-own", Name: "shared", DataSourceRef: "datasources/x.csv", OwnerID: testAnalystID})
		users := newFakeUserStore()
		maya, err := users.Create(ctx, "maya", "hash", false)
		if err != nil {
			t.Fatalf("seed collaborator: %v", err)
		}
		audits := &fakeAudits{}
		srv := testServer{datasets: ds, users: users, audits: audits}.build()
		ds.Share(ctx, "d-own", maya.ID, testAnalystID)

		// First revoke: 204 and the grant is gone.
		revoked := doReq(t, srv, http.MethodDelete, "/datasets/d-own/shares/maya", "")
		if revoked.Code != http.StatusNoContent {
			t.Fatalf("revoke status = %d, body %q, want 204", revoked.Code, revoked.Body.String())
		}
		if grants := ds.sharesFor("d-own"); len(grants) != 0 {
			t.Fatalf("revoke left a grant: %+v", grants)
		}
		var unshared bool
		for _, r := range audits.records() {
			if r.Action == "dataset_unshare" && r.Detail["dataset_id"] == "d-own" && r.Detail["user_id"] == maya.ID {
				unshared = true
			}
		}
		if !unshared {
			t.Fatalf("no dataset_unshare audit: %+v", audits.records())
		}

		// A second revoke of the absent grant is the same idempotent 204.
		again := doReq(t, srv, http.MethodDelete, "/datasets/d-own/shares/maya", "")
		if again.Code != http.StatusNoContent {
			t.Fatalf("repeat revoke status = %d, want 204", again.Code)
		}
		// An unknown username is a 404 before any revoke.
		unknown := doReq(t, srv, http.MethodDelete, "/datasets/d-own/shares/nobody", "")
		if unknown.Code != http.StatusNotFound {
			t.Fatalf("revoke-unknown status = %d, want 404", unknown.Code)
		}
	})

	t.Run("revoke is refused to a non-owner", func(t *testing.T) {
		ds := &fakeDatasets{}
		ds.seed(store.Dataset{ID: "d-other", Name: "shared", DataSourceRef: "datasources/x.csv", OwnerID: "someone-else"})
		users := newFakeUserStore()
		maya, err := users.Create(ctx, "maya", "hash", false)
		if err != nil {
			t.Fatalf("seed collaborator: %v", err)
		}
		srv := testServer{datasets: ds, users: users, audits: &fakeAudits{}}.build()
		ds.Share(ctx, "d-other", maya.ID, "someone-else")

		rec := doReq(t, srv, http.MethodDelete, "/datasets/d-other/shares/maya", "")
		if rec.Code != http.StatusForbidden {
			t.Fatalf("non-owner revoke status = %d, want 403", rec.Code)
		}
		if grants := ds.sharesFor("d-other"); len(grants) != 1 {
			t.Fatalf("non-owner revoke removed the grant: %+v", grants)
		}
	})

	t.Run("a non-owner is refused 403 and nothing is granted", func(t *testing.T) {
		ds := &fakeDatasets{}
		ds.seed(store.Dataset{ID: "d-other", Name: "shared", DataSourceRef: "datasources/x.csv", OwnerID: "someone-else"})
		users := newFakeUserStore()
		if _, err := users.Create(ctx, "maya", "hash", false); err != nil {
			t.Fatalf("seed collaborator: %v", err)
		}
		audits := &fakeAudits{}
		srv := testServer{datasets: ds, users: users, audits: audits}.build()

		for _, path := range []string{"/datasets/d-other/shares", "/datasets/d-other/shares/maya"} {
			method := http.MethodGet
			if strings.Contains(path, "/shares/") {
				method = http.MethodPut
			}
			rec := doReq(t, srv, method, path, "")
			if rec.Code != http.StatusForbidden {
				t.Fatalf("%s %s status = %d, want 403", method, path, rec.Code)
			}
		}
		if len(ds.sharesFor("d-other")) != 0 {
			t.Fatalf("non-owner granted shares: %+v", ds.sharesFor("d-other"))
		}
	})

	t.Run("unknown dataset id is the uniform 404", func(t *testing.T) {
		ds := &fakeDatasets{}
		srv, users, _ := shareTestServer(ds)
		users.Create(ctx, "maya", "hash", false)
		for _, path := range []string{"/datasets/nope/shares", "/datasets/nope/shares/maya"} {
			method := http.MethodGet
			if strings.Contains(path, "/shares/") {
				method = http.MethodPut
			}
			rec := doReq(t, srv, method, path, "")
			if rec.Code != http.StatusNotFound {
				t.Fatalf("%s %s status = %d, want 404", method, path, rec.Code)
			}
		}
	})

	t.Run("refusals that never grant", func(t *testing.T) {
		ds := &fakeDatasets{}
		ds.seed(store.Dataset{ID: "d-own", Name: "shared", DataSourceRef: "datasources/x.csv", OwnerID: testAnalystID})
		users := newFakeUserStore()
		// The owner is a real account in the roster, under the same id the
		// acting stub identity resolves to, so sharing with that username is the
		// self-grant the handler refuses.
		users.mu.Lock()
		users.users[testAnalystID] = store.User{ID: testAnalystID, Username: "analyst-test", Active: true}
		users.byName["analyst-test"] = testAnalystID
		users.mu.Unlock()
		if _, err := users.Create(ctx, "maya", "hash", false); err != nil {
			t.Fatalf("seed collaborator: %v", err)
		}
		srv := testServer{datasets: ds, users: users, audits: &fakeAudits{}}.build()

		// Sharing with the owner (self) is refused before any grant.
		self := doReq(t, srv, http.MethodPut, "/datasets/d-own/shares/analyst-test", "")
		if self.Code != http.StatusBadRequest {
			t.Fatalf("share-with-self status = %d, want 400", self.Code)
		}
		// An unknown username is a 404.
		unknown := doReq(t, srv, http.MethodPut, "/datasets/d-own/shares/nobody", "")
		if unknown.Code != http.StatusNotFound {
			t.Fatalf("share-unknown status = %d, want 404", unknown.Code)
		}
		// A second grant to the same user is a 409, and the store never holds two.
		taylor, err := users.Create(ctx, "taylor", "hash", false)
		if err != nil {
			t.Fatalf("seed second collaborator: %v", err)
		}
		ds.Share(ctx, "d-own", taylor.ID, testAnalystID)
		dup := doReq(t, srv, http.MethodPut, "/datasets/d-own/shares/taylor", "")
		if dup.Code != http.StatusConflict {
			t.Fatalf("duplicate share status = %d, want 409", dup.Code)
		}
		if grants := ds.sharesFor("d-own"); len(grants) != 1 {
			t.Fatalf("duplicate grant stored twice: %+v", grants)
		}
	})
}
