package orchestrator

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/arborette/arborette/internal/store"
)

// The admin account surface tests (T034): the role gate (member -> 403 on every
// /users* route), create-as-member, promote/demote, password reset, and the
// last-active-admin guard (409).

// seedAdminMember seeds an active member account in the fake roster.
func seedMember(t *testing.T, users *fakeUserStore, username string) store.User {
	t.Helper()
	u, err := users.Create(context.Background(), username, "hash-"+username, false)
	if err != nil {
		t.Fatalf("seed member: %v", err)
	}
	return u
}

// signInMember signs in as an existing (seeded) member and returns the cookie.
func signInMember(t *testing.T, srv *Server, users *fakeUserStore, username, password string) string {
	t.Helper()
	var id string
	users.mu.Lock()
	for _, u := range users.users {
		if u.Username == username {
			id = u.ID
		}
	}
	users.mu.Unlock()
	if id == "" {
		t.Fatalf("no such member %q", username)
	}
	hashFor(t, users, id, password)
	rec := httptest.NewRecorder()
	srv.Routes().ServeHTTP(rec, postJSON(t, "/login", map[string]string{
		"username": username, "password": password,
	}))
	if rec.Code != http.StatusOK {
		t.Fatalf("member login status = %d (body %q)", rec.Code, rec.Body.String())
	}
	return "arborette_session=" + firstCookie(t, rec, "arborette_session").Value + ";"
}

// adminUserServer builds a server with one seeded admin (boss) and returns it.
func adminUserServer(t *testing.T) (*Server, *fakeUserStore, *fakeSessionStore, *fakeAudits) {
	t.Helper()
	users := newFakeUserStore()
	users.seedAdmin(t, "boss")
	sessions := newFakeSessionStore()
	srv, audits := accountServer(t, users, sessions)
	return srv, users, sessions, audits
}

// userNameBy resolves a user id from a username in the fake roster.
func (f *fakeUserStore) userNameBy(username string) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.byName[strings.ToLower(username)]
}

// TestUsersMemberGate (T034/FR-014): a member gets 403 on every /users* route,
// the same as a missing cookie gets 401.
func TestUsersMemberGate(t *testing.T) {
	srv, users, _, _ := adminUserServer(t)
	seedMember(t, users, "analyst_one")
	prefix := signInMember(t, srv, users, "analyst_one", "member-pass")

	requests := []struct {
		method, path, body string
	}{
		{http.MethodGet, "/users", ""},
		{http.MethodPost, "/users", `{"username":"nope","password":"password123"}`},
		{http.MethodPatch, "/users/user-1", `{"role":"member"}`},
	}
	for _, c := range requests {
		req := httptest.NewRequest(c.method, c.path, strings.NewReader(c.body))
		req.Header.Set("Cookie", prefix)
		if c.body != "" {
			req.Header.Set("Content-Type", "application/json")
		}
		rec := httptest.NewRecorder()
		srv.Routes().ServeHTTP(rec, req)
		if rec.Code != http.StatusForbidden {
			t.Fatalf("%s %s as member = %d, want 403 (body %q)", c.method, c.path, rec.Code, rec.Body.String())
		}
	}
}

// TestUsersAdminListAndCreate (T034): an admin lists the roster and creates a
// member that is always active, role=member, and signs in immediately.
func TestUsersAdminListAndCreate(t *testing.T) {
	srv, users, _, _ := adminUserServer(t)
	prefix := signInAs(t, srv, users)

	// A seeded member shows up in the roster.
	seedMember(t, users, "analyst_one")

	req := httptest.NewRequest(http.MethodGet, "/users", nil)
	req.Header.Set("Cookie", prefix)
	rec := httptest.NewRecorder()
	srv.Routes().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("list users = %d, want 200 (body %q)", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), `"analyst_one"`) {
		t.Fatalf("roster missing seeded member: %q", rec.Body.String())
	}

	// Admin creates a member.
	create := httptest.NewRequest(http.MethodPost, "/users", strings.NewReader(`{"username":"new_member","password":"password123"}`))
	create.Header.Set("Cookie", prefix)
	create.Header.Set("Content-Type", "application/json")
	crec := httptest.NewRecorder()
	srv.Routes().ServeHTTP(crec, create)
	if crec.Code != http.StatusCreated {
		t.Fatalf("create user = %d, want 201 (body %q)", crec.Code, crec.Body.String())
	}
	// Admin-created accounts are always an active member.
	if !strings.Contains(crec.Body.String(), `"role":"member"`) || strings.Contains(crec.Body.String(), `"active":false`) {
		t.Fatalf("created account must be active member: %q", crec.Body.String())
	}

	// The new member signs in immediately.
	if loginStatus(t, srv, "new_member", "password123") != http.StatusOK {
		t.Fatal("admin-created member must sign in on first attempt")
	}
}

// TestUsersPromoteDemote (T034): an admin promotes a member to admin and demotes
// a demotable admin back to member, with the role reflected on their /me and a
// role_change audit written.
func TestUsersPromoteDemote(t *testing.T) {
	srv, users, _, audits := adminUserServer(t)
	prefix := signInAs(t, srv, users)
	member := seedMember(t, users, "analyst_one")

	// Promote member -> admin.
	patch := httptest.NewRequest(http.MethodPatch, "/users/"+member.ID, strings.NewReader(`{"role":"admin"}`))
	patch.Header.Set("Cookie", prefix)
	patch.Header.Set("Content-Type", "application/json")
	prec := httptest.NewRecorder()
	srv.Routes().ServeHTTP(prec, patch)
	if prec.Code != http.StatusOK {
		t.Fatalf("promote = %d, want 200 (body %q)", prec.Code, prec.Body.String())
	}
	if !strings.Contains(prec.Body.String(), `"role":"admin"`) {
		t.Fatalf("promoted account must read admin: %q", prec.Body.String())
	}

	// The promoted account's own /me reflects the new role (it signs in).
	memberPrefix := signInMember(t, srv, users, "analyst_one", "member-pass")
	meRec := httptest.NewRecorder()
	me := httptest.NewRequest(http.MethodGet, "/me", nil)
	me.Header.Set("Cookie", memberPrefix)
	srv.Routes().ServeHTTP(meRec, me)
	if decodeMe(t, meRec).Role != store.RoleAdmin {
		t.Fatalf("promoted /me role = %q, want admin", decodeMe(t, meRec).Role)
	}

	// With a second admin present, demoting boss (now not the last) succeeds.
	bossID := users.userNameBy("boss")
	demote := httptest.NewRequest(http.MethodPatch, "/users/"+bossID, strings.NewReader(`{"role":"member"}`))
	demote.Header.Set("Cookie", prefix)
	demote.Header.Set("Content-Type", "application/json")
	drec := httptest.NewRecorder()
	srv.Routes().ServeHTTP(drec, demote)
	if drec.Code != http.StatusOK {
		t.Fatalf("demote (not last) = %d, want 200 (body %q)", drec.Code, drec.Body.String())
	}

	var sawRole bool
	for _, r := range audits.records() {
		if r.Action == "user_role_change" && r.EventType == "user" && r.Detail["user_id"] == member.ID {
			sawRole = true
		}
	}
	if !sawRole {
		t.Fatalf("user_role_change audit absent: %+v", audits.records())
	}
}

// TestUsersPasswordReset (T034): an admin resets a password; the new value works
// at the next sign-in and the old one stops.
func TestUsersPasswordReset(t *testing.T) {
	srv, users, _, audits := adminUserServer(t)
	prefix := signInAs(t, srv, users)
	member := seedMember(t, users, "analyst_one")
	hashFor(t, users, member.ID, "original-pass")

	if loginStatus(t, srv, "analyst_one", "original-pass") != http.StatusOK {
		t.Fatal("member must sign in before the reset")
	}

	reset := httptest.NewRequest(http.MethodPatch, "/users/"+member.ID, strings.NewReader(`{"new_password":"reset-pass-1"}`))
	reset.Header.Set("Cookie", prefix)
	reset.Header.Set("Content-Type", "application/json")
	rrec := httptest.NewRecorder()
	srv.Routes().ServeHTTP(rrec, reset)
	if rrec.Code != http.StatusOK {
		t.Fatalf("password reset = %d, want 200 (body %q)", rrec.Code, rrec.Body.String())
	}

	if loginStatus(t, srv, "analyst_one", "original-pass") != http.StatusUnauthorized {
		t.Fatal("old password must stop working after the reset")
	}
	if loginStatus(t, srv, "analyst_one", "reset-pass-1") != http.StatusOK {
		t.Fatal("reset password must work at the next sign-in")
	}

	var sawReset bool
	for _, r := range audits.records() {
		if r.Action == "user_password_reset" && r.EventType == "user" && r.Detail["user_id"] == member.ID {
			sawReset = true
		}
	}
	if !sawReset {
		t.Fatalf("user_password_reset audit absent: %+v", audits.records())
	}
}

// TestUsersLastAdminGuard (T034/FR-013): with a single active admin the guard
// refuses a demote (409) and a deactivate (409); once a second admin exists the
// same demote succeeds.
func TestUsersLastAdminGuard(t *testing.T) {
	srv, users, _, _ := adminUserServer(t)
	prefix := signInAs(t, srv, users)
	bossID := users.userNameBy("boss")

	// Demote the only admin -> 409, unchanged.
	for _, body := range []string{`{"role":"member"}`, `{"active":false}`} {
		patch := httptest.NewRequest(http.MethodPatch, "/users/"+bossID, strings.NewReader(body))
		patch.Header.Set("Cookie", prefix)
		patch.Header.Set("Content-Type", "application/json")
		rec := httptest.NewRecorder()
		srv.Routes().ServeHTTP(rec, patch)
		if rec.Code != http.StatusConflict {
			t.Fatalf("last-admin %s = %d, want 409", body, rec.Code)
		}
	}
	// The admin survives the refusals.
	if loginStatus(t, srv, "boss", "password123") != http.StatusOK {
		t.Fatal("boss must still sign in after the refused demote/deactivate")
	}

	// Promote member -> second admin, then boss is no longer last.
	member := seedMember(t, users, "analyst_one")
	promote := httptest.NewRequest(http.MethodPatch, "/users/"+member.ID, strings.NewReader(`{"role":"admin"}`))
	promote.Header.Set("Cookie", prefix)
	promote.Header.Set("Content-Type", "application/json")
	prec := httptest.NewRecorder()
	srv.Routes().ServeHTTP(prec, promote)
	if prec.Code != http.StatusOK {
		t.Fatalf("promote to second admin = %d, want 200", prec.Code)
	}

	demote := httptest.NewRequest(http.MethodPatch, "/users/"+bossID, strings.NewReader(`{"role":"member"}`))
	demote.Header.Set("Cookie", prefix)
	demote.Header.Set("Content-Type", "application/json")
	drec := httptest.NewRecorder()
	srv.Routes().ServeHTTP(drec, demote)
	if drec.Code != http.StatusOK {
		t.Fatalf("demote after second admin = %d, want 200 (body %q)", drec.Code, drec.Body.String())
	}
}
