package store_test

import (
	"context"
	"errors"
	"testing"

	"github.com/arborette/arborette/internal/store"
	"github.com/arborette/arborette/internal/testutil"
	"github.com/jackc/pgx/v5"
)

// seedUser creates an account through the store and returns the row, selecting
// the member role (or honoring the seed proposition) without ever touching the
// shared database's admin count.
func seedUser(t *testing.T, ctx context.Context, us *store.UserSessionStore, username string, seedAdmin bool) store.User {
	t.Helper()
	hash, err := store.HashPassword("integration-password-123")
	if err != nil {
		t.Fatalf("hash password: %v", err)
	}
	u, err := us.Create(ctx, username, hash, seedAdmin)
	if err != nil {
		t.Fatalf("create user %q: %v", username, err)
	}
	return u
}

// TestUserAccountLifecycle covers create-vs-conflict, case-insensitive lookup,
// the field-mutation Update, and the admin list. Per-row assertions only: the
// shared database accumulates prior runs' users, so the admin roster's width is
// not ours to assert -- every user this test creates carries a fresh unique
// username.
func TestUserAccountLifecycle(t *testing.T) {
	ctx := context.Background()
	cfg := setup(t, ctx)
	p := pool(t, ctx, cfg.Postgres.OrchestratorDSN())
	us := store.NewUserSessionStore(p)

	username := "inv-" + testutil.NewID(t)
	created := seedUser(t, ctx, us, username, false)
	if created.Role != store.RoleMember {
		t.Fatalf("seed decision without the seed proposition = %q, want member", created.Role)
	}
	if !created.Active {
		t.Fatal("a fresh account must be active")
	}

	// Duplicate username conflicts case-insensitively (the lower(username) index).
	if _, err := us.Create(ctx, testutilMixedCase(username), "x", false); !errors.Is(err, store.ErrUsernameConflict) {
		t.Fatalf("duplicate (case-insensitive) username: err=%v, want ErrUsernameConflict", err)
	}

	// Case-insensitive sign-in lookup.
	byName, err := us.GetByUsername(ctx, username)
	if err != nil {
		t.Fatalf("get by username: %v", err)
	}
	if byName.ID != created.ID {
		t.Fatalf("get by username returned %q, want %q", byName.ID, created.ID)
	}
	byNameUpper, err := us.GetByUsername(ctx, testutilMixedCase(username))
	if err != nil || byNameUpper.ID != created.ID {
		t.Fatalf("case-insensitive lookup: id=%q err=%v, want %q", byNameUpper.ID, err, created.ID)
	}
	if _, err := us.GetByUsername(ctx, "no-such-"+testutil.NewID(t)); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("unknown username: err=%v, want pgx.ErrNoRows", err)
	}

	// Field-mutation Update: role change rides the same path as deactivation.
	rol := store.RoleAdmin
	active := false
	if _, err := us.Update(ctx, created.ID, store.UserUpdate{Role: &rol}); err != nil {
		t.Fatalf("update role: %v", err)
	}
	if _, err := us.Update(ctx, created.ID, store.UserUpdate{Active: &active}); err != nil {
		t.Fatalf("update active: %v", err)
	}
	got, err := us.GetByID(ctx, created.ID)
	if err != nil {
		t.Fatalf("get by id: %v", err)
	}
	if got.Role != store.RoleAdmin || got.Active {
		t.Fatalf("mutation did not persist: %+v", got)
	}

	// Unknown-user updates and reads refuse rather than no-op silently.
	if _, err := us.Update(ctx, testutil.NewID(t), store.UserUpdate{Active: &active}); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("update unknown id: err=%v, want pgx.ErrNoRows", err)
	}
	if _, err := us.GetByID(ctx, testutil.NewID(t)); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("get unknown id: err=%v, want pgx.ErrNoRows", err)
	}

	// The admin roster lists the account (never a table-global width assertion).
	roster, err := us.List(ctx)
	if err != nil {
		t.Fatalf("list users: %v", err)
	}
	found := false
	for _, u := range roster {
		if u.ID == created.ID {
			found = true
		}
	}
	if !found {
		t.Fatalf("created user %q absent from the roster", created.ID)
	}
}

// TestSessionLifecycle covers the session ledger: create keyed on a token
// digest, look up by that digest, delete by it, and the deactivation read-path
// (a deactivated user still owns sessions, but the middleware rejects them by
// re-reading active -- that decision is the middleware's, the store only keeps
// the rows).
func TestSessionLifecycle(t *testing.T) {
	ctx := context.Background()
	cfg := setup(t, ctx)
	p := pool(t, ctx, cfg.Postgres.OrchestratorDSN())
	us := store.NewUserSessionStore(p)

	u := seedUser(t, ctx, us, "sess-"+testutil.NewID(t), false)

	tokenHash := "h" + testutil.NewID(t) // stand-in for sha256(token); see usersessions_test
	sess, err := us.CreateSession(ctx, tokenHash, u.ID)
	if err != nil {
		t.Fatalf("create session: %v", err)
	}
	if sess.UserID != u.ID {
		t.Fatalf("session user = %q, want %q", sess.UserID, u.ID)
	}

	got, err := us.GetSessionByTokenHash(ctx, tokenHash)
	if err != nil {
		t.Fatalf("get session by token hash: %v", err)
	}
	if got.ID != sess.ID {
		t.Fatalf("session round-trip mismatch: got %q want %q", got.ID, sess.ID)
	}
	if _, err := us.GetSessionByTokenHash(ctx, "absent"); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("unknown token hash: err=%v, want pgx.ErrNoRows", err)
	}

	// Sign-out deletes only this session; deleting again is a no-op, not an
	// error (an already-signed-out cookie must not fail a repeat logout).
	if err := us.DeleteSessionByTokenHash(ctx, tokenHash); err != nil {
		t.Fatalf("delete session: %v", err)
	}
	if _, err := us.GetSessionByTokenHash(ctx, tokenHash); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("session still resolvable after delete: err=%v, want pgx.ErrNoRows", err)
	}
	if err := us.DeleteSessionByTokenHash(ctx, tokenHash); err != nil {
		t.Fatalf("repeat delete session must be a no-op: %v", err)
	}
}

// testutilMixedCase flips one letter of a mostly-lowercase identifier to prove
// the case-insensitive index/lookup, without risking a collision on the shared
// database.
func testutilMixedCase(s string) string {
	runes := []rune(s)
	for i := range runes {
		if runes[i] >= 'a' && runes[i] <= 'z' {
			runes[i] -= 'a' - 'A'
			break
		}
	}
	return string(runes)
}

// TestUsersGrants pins the least-privilege boundary: orchestrator owns accounts
// and sessions; service role touches neither.
func TestUsersGrants(t *testing.T) {
	ctx := context.Background()
	cfg := setup(t, ctx)

	orch := pool(t, ctx, cfg.Postgres.OrchestratorDSN())
	svc := pool(t, ctx, cfg.Postgres.ServiceDSN())

	username := "grants-" + testutil.NewID(t)
	u := seedUser(t, ctx, store.NewUserSessionStore(orch), username, false)

	if _, err := svc.Exec(ctx, "INSERT INTO users (username, password_hash, role) VALUES ($1,'x','member')", "svc-"+testutil.NewID(t)); err == nil {
		t.Fatal("expected service INSERT on users to be denied")
	}
	if _, err := svc.Exec(ctx, "INSERT INTO sessions (token_hash, user_id) VALUES ('x', $1)", u.ID); err == nil {
		t.Fatal("expected service INSERT on sessions to be denied")
	}
	if _, err := store.NewUserSessionStore(svc).GetByUsername(ctx, username); err == nil {
		t.Fatal("expected service SELECT on users to be denied")
	}
}
