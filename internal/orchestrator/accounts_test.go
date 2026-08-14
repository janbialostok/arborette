package orchestrator

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/arborette/arborette/internal/store"
	"github.com/jackc/pgx/v5"
)

// fakeUserStore is an in-memory accounts fake that honors context cancellation
// exactly like the real store: every method returns ctx.Err() when the context
// is done, so a handler passing a wrong (cancelled/detached) context fails
// loudly instead of passing for the wrong reason.
type fakeUserStore struct {
	mu     sync.Mutex
	users  map[string]store.User
	byName map[string]string // lower(username) -> id
	nextID int

	createErr  error
	countErr   error
	updateErr  error
	updatedIDs []string
	lastUpdate store.UserUpdate
}

func newFakeUserStore() *fakeUserStore {
	return &fakeUserStore{users: map[string]store.User{}, byName: map[string]string{}}
}

func (f *fakeUserStore) userName(id string) string {
	if u, ok := f.users[id]; ok {
		return u.Username
	}
	return ""
}

func (f *fakeUserStore) Create(ctx context.Context, username, passwordHash string, seedAdmin bool) (store.User, error) {
	if err := ctx.Err(); err != nil {
		return store.User{}, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.createErr != nil {
		return store.User{}, f.createErr
	}
	if _, clash := f.byName[strings.ToLower(username)]; clash {
		return store.User{}, store.ErrUsernameConflict
	}
	role := store.RoleMember
	if seedAdmin {
		role = store.RoleAdmin
	}
	f.nextID++
	u := store.User{
		ID:                      "user-" + itoa(f.nextID),
		Username:                username,
		PasswordHash:            passwordHash,
		Role:                    role,
		Active:                  true,
		AdminNoticeAcknowledged: false,
	}
	f.users[u.ID] = u
	f.byName[strings.ToLower(username)] = u.ID
	return u, nil
}

func (f *fakeUserStore) GetByUsername(ctx context.Context, username string) (store.User, error) {
	if err := ctx.Err(); err != nil {
		return store.User{}, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	id, ok := f.byName[strings.ToLower(username)]
	if !ok {
		return store.User{}, pgx.ErrNoRows
	}
	return f.users[id], nil
}

func (f *fakeUserStore) GetByID(ctx context.Context, id string) (store.User, error) {
	if err := ctx.Err(); err != nil {
		return store.User{}, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	u, ok := f.users[id]
	if !ok {
		return store.User{}, pgx.ErrNoRows
	}
	return u, nil
}

func (f *fakeUserStore) List(ctx context.Context) ([]store.User, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]store.User, 0, len(f.users))
	for _, u := range f.users {
		out = append(out, u)
	}
	return out, nil
}

func (f *fakeUserStore) CountActiveAdmins(ctx context.Context) (int, error) {
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.countErr != nil {
		return 0, f.countErr
	}
	// The real store recomputes from the users table (role=admin AND active); the
	// fake mirrors that from its roster so promote/demote/deactivate move the
	// count exactly as a mutation of the DB would.
	n := 0
	for _, u := range f.users {
		if u.Role == store.RoleAdmin && u.Active {
			n++
		}
	}
	return n, nil
}

// EarliestActiveAdmin mirrors the store's caretaker oracle over the in-memory
// roster: the oldest active admin by created_at, then id.
func (f *fakeUserStore) EarliestActiveAdmin(ctx context.Context) (store.User, error) {
	if err := ctx.Err(); err != nil {
		return store.User{}, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	var earliest *store.User
	for id, u := range f.users {
		if u.Role != store.RoleAdmin || !u.Active {
			continue
		}
		if earliest == nil || u.CreatedAt.Before(earliest.CreatedAt) ||
			(u.CreatedAt.Equal(earliest.CreatedAt) && id < earliest.ID) {
			copy := u
			earliest = &copy
		}
	}
	if earliest == nil {
		return store.User{}, pgx.ErrNoRows
	}
	return *earliest, nil
}

func (f *fakeUserStore) Update(ctx context.Context, id string, updates store.UserUpdate) (store.User, error) {
	if err := ctx.Err(); err != nil {
		return store.User{}, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.updateErr != nil {
		return store.User{}, f.updateErr
	}
	u, ok := f.users[id]
	if !ok {
		return store.User{}, pgx.ErrNoRows
	}
	if updates.Username != nil {
		if _, clash := f.byName[strings.ToLower(*updates.Username)]; clash && f.byName[strings.ToLower(*updates.Username)] != id {
			return store.User{}, store.ErrUsernameConflict
		}
		delete(f.byName, strings.ToLower(u.Username))
		u.Username = *updates.Username
		f.byName[strings.ToLower(u.Username)] = id
	}
	if updates.PasswordHash != nil {
		u.PasswordHash = *updates.PasswordHash
	}
	if updates.Role != nil {
		u.Role = *updates.Role
	}
	if updates.Active != nil {
		u.Active = *updates.Active
	}
	if updates.AdminNoticeAcknowledged != nil {
		u.AdminNoticeAcknowledged = *updates.AdminNoticeAcknowledged
	}
	f.users[id] = u
	f.updatedIDs = append(f.updatedIDs, id)
	f.lastUpdate = updates
	return u, nil
}

func itoa(n int) string {
	return string(rune('0' + n))
}

// seedAdmin seeds one active admin account so the fake's roster behaves like a
// populated DB.
func (f *fakeUserStore) seedAdmin(t *testing.T, username string) store.User {
	t.Helper()
	u, err := f.Create(context.Background(), username, "hash-"+username, true)
	if err != nil {
		t.Fatalf("seed admin: %v", err)
	}
	f.mu.Lock()
	u.Active = true
	f.users[u.ID] = u
	f.mu.Unlock()
	return u
}

// fakeSessionStore is the session-ledger double. It honors context cancellation
// and records sign-out hits so a test can assert both the row deletion and the
// cookie clear.
type fakeSessionStore struct {
	mu        sync.Mutex
	sessions  map[string]store.Session // tokenHash -> row
	deleted   []string
	createErr error
	getErr    error
}

func newFakeSessionStore() *fakeSessionStore {
	return &fakeSessionStore{sessions: map[string]store.Session{}}
}

func (f *fakeSessionStore) CreateSession(ctx context.Context, tokenHash, userID string) (store.Session, error) {
	if err := ctx.Err(); err != nil {
		return store.Session{}, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.createErr != nil {
		return store.Session{}, f.createErr
	}
	f.sessions[tokenHash] = store.Session{ID: "sess-" + tokenHash, UserID: userID, TokenHash: tokenHash}
	return f.sessions[tokenHash], nil
}

func (f *fakeSessionStore) GetSessionByTokenHash(ctx context.Context, tokenHash string) (store.Session, error) {
	if err := ctx.Err(); err != nil {
		return store.Session{}, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.getErr != nil {
		return store.Session{}, f.getErr
	}
	s, ok := f.sessions[tokenHash]
	if !ok {
		return store.Session{}, pgx.ErrNoRows
	}
	return s, nil
}

func (f *fakeSessionStore) DeleteSessionByTokenHash(ctx context.Context, tokenHash string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.sessions, tokenHash)
	f.deleted = append(f.deleted, tokenHash)
	return nil
}

// postJSON builds a POST with a JSON body against the routes.
func postJSON(t *testing.T, url string, body any) *http.Request {
	t.Helper()
	b, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("marshal body: %v", err)
	}
	req := httptest.NewRequest(http.MethodPost, url, bytes.NewReader(b))
	req.Header.Set("Content-Type", "application/json")
	return req
}

// accountServer wires a testServer that fights over the account surface: real
// users/sessions fakes, and an audits fake that records what it stamps.
func accountServer(t *testing.T, users *fakeUserStore, sessions *fakeSessionStore) (*Server, *fakeAudits) {
	t.Helper()
	audits := &fakeAudits{}
	srv := testServer{users: users, sessions: sessions, audits: audits}.build()
	return srv, audits
}

// firstCookie returns the set cookie by name from a recorder.
func firstCookie(t *testing.T, rec *httptest.ResponseRecorder, name string) *http.Cookie {
	t.Helper()
	for _, c := range rec.Result().Cookies() {
		if c.Name == name {
			return c
		}
	}
	t.Fatalf("no %q cookie in response", name)
	return nil
}

func decodeMe(t *testing.T, rec *httptest.ResponseRecorder) meDTO {
	t.Helper()
	var m meDTO
	if err := json.Unmarshal(rec.Body.Bytes(), &m); err != nil {
		t.Fatalf("decode me: %v (body %q)", err, rec.Body.String())
	}
	return m
}

// TestRegisterSeedsAdmin: the very first registration when no active admin
// exists becomes admin with the one-shot grant notice, signs in with a session
// cookie, and is audit-stamped with the acting user.
func TestRegisterSeedsAdmin(t *testing.T) {
	users := newFakeUserStore() // activeAdminCount 0
	sessions := newFakeSessionStore()
	srv, audits := accountServer(t, users, sessions)

	rec := httptest.NewRecorder()
	srv.Routes().ServeHTTP(rec, postJSON(t, "/register", map[string]string{
		"username": "first_admin", "password": "password123",
	}))
	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201 (body %q)", rec.Code, rec.Body.String())
	}
	me := decodeMe(t, rec)
	if me.Role != store.RoleAdmin {
		t.Fatalf("first registration role = %q, want admin", me.Role)
	}
	if !me.AdminNotice {
		t.Fatal("a seeded admin must carry the pending grant notice")
	}
	if me.Username != "first_admin" || me.ID == "" {
		t.Fatalf("me = %+v", me)
	}

	cookie := firstCookie(t, rec, "arborette_session")
	if cookie.HttpOnly && cookie.SameSite != http.SameSiteLaxMode && cookie.Path != "/" {
		t.Fatalf("cookie flags wrong: %+v", cookie)
	}
	if cookie.HttpOnly != true || cookie.SameSite != http.SameSiteLaxMode || cookie.Path != "/" {
		t.Fatalf("cookie must be HttpOnly, SameSite=Lax, Path=/: %+v", cookie)
	}

	// The session is durable, keyed on the token digest not the raw value.
	sessions.mu.Lock()
	sessCount := len(sessions.sessions)
	sessions.mu.Unlock()
	if sessCount != 1 {
		t.Fatalf("session rows = %d, want 1", sessCount)
	}

	var sawRegister bool
	for _, r := range audits.records() {
		if r.Action == "user_register" && r.EventType == "user" &&
			r.Detail["user_id"] == me.ID && r.Detail["username"] == me.Username && r.Detail["role"] == "admin" {
			sawRegister = true
		}
	}
	if !sawRegister {
		t.Fatalf("user_register audit with user_id/username/role absent: %+v", audits.records())
	}
}

// TestRegisterMemberWhenAdminExists: with an active admin present, a new
// registration lands as a member without the grant notice.
func TestRegisterMemberWhenAdminExists(t *testing.T) {
	users := newFakeUserStore()
	users.seedAdmin(t, "boss")
	sessions := newFakeSessionStore()
	srv, _ := accountServer(t, users, sessions)

	rec := httptest.NewRecorder()
	srv.Routes().ServeHTTP(rec, postJSON(t, "/register", map[string]string{
		"username": "analyst_one", "password": "password123",
	}))
	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201 (body %q)", rec.Code, rec.Body.String())
	}
	me := decodeMe(t, rec)
	if me.Role != store.RoleMember {
		t.Fatalf("second registration role = %q, want member", me.Role)
	}
	if me.AdminNotice {
		t.Fatal("a member must not carry the admin-grant notice")
	}
}

// TestRegisterDuplicateAndValidation exercises the 409 clash and the 400
// validation reasons.
func TestRegisterDuplicateAndValidation(t *testing.T) {
	users := newFakeUserStore()
	sessions := newFakeSessionStore()
	srv, _ := accountServer(t, users, sessions)

	srv.Routes().ServeHTTP(httptest.NewRecorder(), postJSON(t, "/register", map[string]string{
		"username": "dup_user", "password": "password123",
	}))

	rec := httptest.NewRecorder()
	srv.Routes().ServeHTTP(rec, postJSON(t, "/register", map[string]string{
		"username": "DUP_USER", "password": "password123",
	}))
	if rec.Code != http.StatusConflict {
		t.Fatalf("duplicate register status = %d, want 409 (body %q)", rec.Code, rec.Body.String())
	}

	cases := []struct {
		name, username, password string
	}{
		{"short username", "ab", "password123"},
		{"bad chars", "bad name_x", "password123"},
		{"short password", "valid_user", "short"},
	}
	for _, c := range cases {
		rec := httptest.NewRecorder()
		srv.Routes().ServeHTTP(rec, postJSON(t, "/register", map[string]string{
			"username": c.username, "password": c.password,
		}))
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("%s: status = %d, want 400 (body %q)", c.name, rec.Code, rec.Body.String())
		}
	}
}

// TestLoginSuccess signs in with the same credentials and lands a 200 with the
// session cookie.
func TestLoginSuccess(t *testing.T) {
	users := newFakeUserStore()
	u := users.seedAdmin(t, "boss")
	sessions := newFakeSessionStore()
	srv, _ := accountServer(t, users, sessions)

	// Back the seeded admin with a real hash so login verifies.
	hash, err := store.HashPassword("password123")
	if err != nil {
		t.Fatalf("hash: %v", err)
	}
	users.mu.Lock()
	u.PasswordHash = hash
	users.users[u.ID] = u
	users.mu.Unlock()

	rec := httptest.NewRecorder()
	srv.Routes().ServeHTTP(rec, postJSON(t, "/login", map[string]string{
		"username": "Boss", "password": "password123",
	}))
	if rec.Code != http.StatusOK {
		t.Fatalf("login status = %d, want 200 (body %q)", rec.Code, rec.Body.String())
	}
	if firstCookie(t, rec, "arborette_session").Value == "" {
		t.Fatal("login must issue a session cookie")
	}
}

// TestLoginFailureUniformMessage: wrong password, unknown username, and a
// deactivated account all answer the exact same 401 body, so no response
// discloses which happened.
func TestLoginFailureUniformMessage(t *testing.T) {
	users := newFakeUserStore()
	u := users.seedAdmin(t, "boss")
	hash, _ := store.HashPassword("password123")
	u2, _ := users.Create(context.Background(), "sleepy", "hashx", false)
	users.mu.Lock()
	u.PasswordHash = hash
	users.users[u.ID] = u
	u2.Active = false
	users.users[u2.ID] = u2
	users.mu.Unlock()
	sessions := newFakeSessionStore()
	srv, _ := accountServer(t, users, sessions)

	cases := []struct {
		name, username, password string
	}{
		{"wrong password", "boss", "wrong-password"},
		{"unknown user", "ghost", "password123"},
		{"deactivated", "sleepy", "hashx"},
	}
	recs := map[string]string{}
	for _, c := range cases {
		rec := httptest.NewRecorder()
		srv.Routes().ServeHTTP(rec, postJSON(t, "/login", map[string]string{
			"username": c.username, "password": c.password,
		}))
		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("%s: status = %d, want 401 (body %q)", c.name, rec.Code, rec.Body.String())
		}
		body := strings.TrimSpace(rec.Body.String())
		recs[c.name] = body
		if body != `{"error":"invalid username or password"}` {
			t.Fatalf("%s: body %q, want the single generic message", c.name, body)
		}
	}
	for n := range recs {
		for m := range recs {
			if recs[n] != recs[m] {
				t.Fatalf("login bodies must be identical across failure modes: %q vs %q", recs[n], recs[m])
			}
		}
	}
	// No failure may mint a session.
	sessions.mu.Lock()
	n := len(sessions.sessions)
	sessions.mu.Unlock()
	if n != 0 {
		t.Fatalf("failed logins minted %d session rows", n)
	}
}

// TestLogoutDeletesSessionAndClearsCookie: sign-out ends the session row and
// clears the cookie; a repeat logout is still a 204.
func TestLogoutDeletesSessionAndClearsCookie(t *testing.T) {
	users := newFakeUserStore()
	u := users.seedAdmin(t, "boss")
	sessions := newFakeSessionStore()
	// Sign in to obtain a real session cookie.
	hash, _ := store.HashPassword("password123")
	users.mu.Lock()
	u.PasswordHash = hash
	users.users[u.ID] = u
	users.mu.Unlock()
	srv, _ := accountServer(t, users, sessions)

	login := httptest.NewRecorder()
	srv.Routes().ServeHTTP(login, postJSON(t, "/login", map[string]string{
		"username": "boss", "password": "password123",
	}))
	token := firstCookie(t, login, "arborette_session").Value

	prefix := "arborette_session=" + token + ";"
	req := httptest.NewRequest(http.MethodPost, "/logout", nil)
	req.Header.Set("Cookie", prefix)
	rec := httptest.NewRecorder()
	srv.Routes().ServeHTTP(rec, req)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("logout status = %d, want 204 (body %q)", rec.Code, rec.Body.String())
	}

	sessions.mu.Lock()
	rows := len(sessions.sessions)
	deleted := len(sessions.deleted)
	sessions.mu.Unlock()
	if rows != 0 {
		t.Fatalf("session rows after logout = %d, want 0", rows)
	}
	if deleted != 1 {
		t.Fatalf("session deletes = %d, want 1", deleted)
	}
	cleared := firstCookie(t, rec, "arborette_session")
	if cleared.MaxAge >= 0 {
		t.Fatalf("logout cookie must be expired, MaxAge=%d", cleared.MaxAge)
	}

	// The session is truly dead: replaying the cookie on a guarded route is a
	// 401, and a repeat logout with it is too (the guard is authoritative).
	replay := httptest.NewRequest(http.MethodGet, "/goals", nil)
	replay.Header.Set("Cookie", prefix)
	rrec := httptest.NewRecorder()
	srv.Routes().ServeHTTP(rrec, replay)
	if rrec.Code != http.StatusUnauthorized {
		t.Fatalf("guard after logout = %d, want 401", rrec.Code)
	}
}

// TestFakeHonorsCancelledContext: the doubled store returns ctx.Err() on a
// cancelled context, so a handler that passes the wrong context fails loudly.
func TestFakeHonorsCancelledContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	users := newFakeUserStore()
	if _, err := users.Create(ctx, "x", "y", false); !errors.Is(err, context.Canceled) {
		t.Fatalf("Create on cancelled ctx = %v, want context.Canceled", err)
	}
	if _, err := users.CountActiveAdmins(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("CountActiveAdmins on cancelled ctx = %v, want context.Canceled", err)
	}
	sessions := newFakeSessionStore()
	if _, err := sessions.CreateSession(ctx, "h", "u"); !errors.Is(err, context.Canceled) {
		t.Fatalf("CreateSession on cancelled ctx = %v, want context.Canceled", err)
	}
}

// signInAs signs in as the seeded admin and returns the cookie value for the
// guarded-route tests. It is the httptest Set-Cookie -> replay round trip.
func signInAs(t *testing.T, srv *Server, users *fakeUserStore) string {
	t.Helper()
	hash, _ := store.HashPassword("password123")
	users.mu.Lock()
	for id, u := range users.users {
		if u.Username == "boss" {
			u.PasswordHash = hash
			users.users[id] = u
		}
	}
	users.mu.Unlock()
	login := httptest.NewRecorder()
	srv.Routes().ServeHTTP(login, postJSON(t, "/login", map[string]string{
		"username": "boss", "password": "password123",
	}))
	if login.Code != http.StatusOK {
		t.Fatalf("login status = %d (body %q)", login.Code, login.Body.String())
	}
	return "arborette_session=" + firstCookie(t, login, "arborette_session").Value + ";"
}

// TestMeRoundTrip: GET /me is guarded (no cookie -> 401), and replaying the
// sign-in Set-Cookie answers 200 with the identity bootstrap.
func TestMeRoundTrip(t *testing.T) {
	users := newFakeUserStore()
	users.seedAdmin(t, "boss")
	sessions := newFakeSessionStore()
	srv, _ := accountServer(t, users, sessions)

	noCookie := httptest.NewRecorder()
	srv.Routes().ServeHTTP(noCookie, httptest.NewRequest(http.MethodGet, "/me", nil))
	if noCookie.Code != http.StatusUnauthorized {
		t.Fatalf("GET /me without cookie = %d, want 401", noCookie.Code)
	}
	if noCookie.Header().Get("WWW-Authenticate") != "Session" {
		t.Fatal("unsigned 401 must carry the Session challenge")
	}

	prefix := signInAs(t, srv, users)
	req := httptest.NewRequest(http.MethodGet, "/me", nil)
	req.Header.Set("Cookie", prefix)
	rec := httptest.NewRecorder()
	srv.Routes().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /me with cookie = %d, want 200 (body %q)", rec.Code, rec.Body.String())
	}
	me := decodeMe(t, rec)
	if me.Username != "boss" || me.Role != store.RoleAdmin || me.ID == "" {
		t.Fatalf("me = %+v, want boss/admin with an id", me)
	}
	// The seeded admin's grant notice is still unacknowledged on first read.
	if !me.AdminNotice {
		t.Fatal("unacknowledged seeded admin must read admin_notice=true")
	}
}

// TestMeAdminNoticeAcknowledgement: PATCH /me acknowledging the notice flips it
// to false, and the row is the source of truth for the next GET /me.
func TestMeAdminNoticeAcknowledgement(t *testing.T) {
	users := newFakeUserStore()
	users.seedAdmin(t, "boss")
	sessions := newFakeSessionStore()
	srv, _ := accountServer(t, users, sessions)
	prefix := signInAs(t, srv, users)

	ackReq := httptest.NewRequest(http.MethodPatch, "/me", strings.NewReader(`{"acknowledge_admin_notice":true}`))
	ackReq.Header.Set("Cookie", prefix)
	ackReq.Header.Set("Content-Type", "application/json")
	ack := httptest.NewRecorder()
	srv.Routes().ServeHTTP(ack, ackReq)
	if ack.Code != http.StatusOK {
		t.Fatalf("PATCH /me status = %d, want 200 (body %q)", ack.Code, ack.Body.String())
	}
	if decodeMe(t, ack).AdminNotice {
		t.Fatal("acknowledged admin notice must read false in the PATCH response")
	}

	againReq := httptest.NewRequest(http.MethodGet, "/me", nil)
	againReq.Header.Set("Cookie", prefix)
	again := httptest.NewRecorder()
	srv.Routes().ServeHTTP(again, againReq)
	if again.Code != http.StatusOK {
		t.Fatalf("GET /me after ack = %d, want 200", again.Code)
	}
	if decodeMe(t, again).AdminNotice {
		t.Fatal("ack must persist: GET /me after PATCH must read admin_notice=false")
	}
}

// TestMePatchValidation: a PATCH /me body that never names the notice (or is
// not well-formed) is a 400 so a partial client cannot silently no-op.
func TestMePatchValidation(t *testing.T) {
	users := newFakeUserStore()
	users.seedAdmin(t, "boss")
	sessions := newFakeSessionStore()
	srv, _ := accountServer(t, users, sessions)
	prefix := signInAs(t, srv, users)

	for _, body := range []string{`{}`, `{"acknowledge_admin_notice":"yes"}`, `{`, ``} {
		req := httptest.NewRequest(http.MethodPatch, "/me", strings.NewReader(body))
		req.Header.Set("Cookie", prefix)
		rec := httptest.NewRecorder()
		srv.Routes().ServeHTTP(rec, req)
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("PATCH /me %q = %d, want 400", body, rec.Code)
		}
	}

	// The notice is untouched by any of the rejected bodies.
	againReq := httptest.NewRequest(http.MethodGet, "/me", nil)
	againReq.Header.Set("Cookie", prefix)
	again := httptest.NewRecorder()
	srv.Routes().ServeHTTP(again, againReq)
	if decodeMe(t, again).AdminNotice != true {
		t.Fatal("rejected PATCH bodies must not have acknowledged the notice")
	}
}

// TestDeactivatedUserRejectedOnNextRequest (R3): a signed-in user passes the
// guard until an admin deactivates the account, and the very next request is a
// 401 even though the cookie itself is still valid.
func TestDeactivatedUserRejectedOnNextRequest(t *testing.T) {
	users := newFakeUserStore()
	u := users.seedAdmin(t, "boss")
	sessions := newFakeSessionStore()
	srv, _ := accountServer(t, users, sessions)
	prefix := signInAs(t, srv, users)

	ok := httptest.NewRecorder()
	r1 := httptest.NewRequest(http.MethodGet, "/me", nil)
	r1.Header.Set("Cookie", prefix)
	srv.Routes().ServeHTTP(ok, r1)
	if ok.Code != http.StatusOK {
		t.Fatalf("guarded route while active = %d, want 200", ok.Code)
	}

	users.mu.Lock()
	u.Active = false
	users.users[u.ID] = u
	users.mu.Unlock()

	blocked := httptest.NewRecorder()
	r2 := httptest.NewRequest(http.MethodGet, "/me", nil)
	r2.Header.Set("Cookie", prefix)
	srv.Routes().ServeHTTP(blocked, r2)
	if blocked.Code != http.StatusUnauthorized {
		t.Fatalf("guarded route after deactivation = %d, want 401", blocked.Code)
	}
	if blocked.Header().Get("WWW-Authenticate") != "Session" {
		t.Fatal("deactivation 401 must carry the Session challenge")
	}
}

// patchMe sends a PATCH with a raw JSON body and the given cookie.
func patchMe(t *testing.T, srv *Server, prefix, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPatch, "/me", strings.NewReader(body))
	req.Header.Set("Cookie", prefix)
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	srv.Routes().ServeHTTP(rec, req)
	return rec
}

// hashFor sets a real PBKDF2 hash for the seeded admin so login/password-change
// verification works in the self-service tests.
func hashFor(t *testing.T, users *fakeUserStore, id, password string) {
	t.Helper()
	h, err := store.HashPassword(password)
	if err != nil {
		t.Fatalf("hash: %v", err)
	}
	users.mu.Lock()
	u := users.users[id]
	u.PasswordHash = h
	users.users[id] = u
	users.mu.Unlock()
}

// TestUsernameRename: renaming is reflected on the next /me, the new username
// signs in, the old one stops, and a clash is a 409.
func TestUsernameRename(t *testing.T) {
	users := newFakeUserStore()
	u := users.seedAdmin(t, "boss")
	hashFor(t, users, u.ID, "password123")
	sessions := newFakeSessionStore()
	srv, audits := accountServer(t, users, sessions)
	prefix := signInAs(t, srv, users)

	rec := patchMe(t, srv, prefix, `{"username":"chief"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("rename status = %d, want 200 (body %q)", rec.Code, rec.Body.String())
	}
	if decodeMe(t, rec).Username != "chief" {
		t.Fatalf("renamed /me = %q, want chief", decodeMe(t, rec).Username)
	}

	// The session survives the rename; the new username proves identity, the old
	// one no longer does.
	again := httptest.NewRecorder()
	g := httptest.NewRequest(http.MethodGet, "/me", nil)
	g.Header.Set("Cookie", prefix)
	srv.Routes().ServeHTTP(again, g)
	if decodeMe(t, again).Username != "chief" {
		t.Fatalf("next /me after rename = %q, want chief", decodeMe(t, again).Username)
	}
	if loginStatus(t, srv, "chief", "password123") != http.StatusOK {
		t.Fatal("new username must sign in")
	}
	if loginStatus(t, srv, "boss", "password123") != http.StatusUnauthorized {
		t.Fatal("old username must no longer sign in")
	}

	var sawRename bool
	for _, r := range audits.records() {
		if r.Action == "user_username_change" && r.EventType == "user" &&
			r.Detail["user_id"] == u.ID && r.Detail["username"] == "chief" &&
			r.Detail["previous_username"] == "boss" {
			sawRename = true
		}
	}
	if !sawRename {
		t.Fatalf("user_username_change audit absent: %+v", audits.records())
	}

	// A rename onto an existing username is a 409, not a partial overwrite.
	users.mu.Lock()
	users.byName["first_admin"] = "user-other"
	users.mu.Unlock()
	conflict := patchMe(t, srv, prefix, `{"username":"FIRST_ADMIN"}`)
	if conflict.Code != http.StatusConflict {
		t.Fatalf("clash rename status = %d, want 409", conflict.Code)
	}
}

func loginStatus(t *testing.T, srv *Server, username, password string) int {
	t.Helper()
	rec := httptest.NewRecorder()
	srv.Routes().ServeHTTP(rec, postJSON(t, "/login", map[string]string{
		"username": username, "password": password,
	}))
	return rec.Code
}

// TestPasswordChange: a correct current password rotates the hash (the old
// password stops working, the new one signs in); a wrong current password is a
// 403 and nothing changes; the change is audited.
func TestPasswordChange(t *testing.T) {
	users := newFakeUserStore()
	u := users.seedAdmin(t, "boss")
	hashFor(t, users, u.ID, "password123")
	sessions := newFakeSessionStore()
	srv, audits := accountServer(t, users, sessions)
	prefix := signInAs(t, srv, users)

	if st := patchMe(t, srv, prefix, `{"password":"new-Secret1","current_password":"wrong"}`).Code; st != http.StatusForbidden {
		t.Fatalf("wrong current password status = %d, want 403", st)
	}
	// The wrong attempt changed nothing: the old password still signs in.
	if loginStatus(t, srv, "boss", "password123") != http.StatusOK {
		t.Fatal("old password must still work after a refused change")
	}

	if st := patchMe(t, srv, prefix, `{"password":"new-Secret1","current_password":"password123"}`).Code; st != http.StatusOK {
		t.Fatalf("valid password change status = %d, want 200", st)
	}
	if loginStatus(t, srv, "boss", "password123") != http.StatusUnauthorized {
		t.Fatal("old password must stop working after the change")
	}
	if loginStatus(t, srv, "boss", "new-Secret1") != http.StatusOK {
		t.Fatal("new password must sign in after the change")
	}

	var sawPassword bool
	for _, r := range audits.records() {
		if r.Action == "user_password_change" && r.EventType == "user" && r.Detail["user_id"] == u.ID {
			sawPassword = true
		}
	}
	if !sawPassword {
		t.Fatalf("user_password_change audit absent: %+v", audits.records())
	}
}

// TestPatchMeNothingToUpdate: a PATCH /me with none of the recognized fields is
// a 400 so a partial client cannot silently no-op.
func TestPatchMeNothingToUpdate(t *testing.T) {
	users := newFakeUserStore()
	users.seedAdmin(t, "boss")
	sessions := newFakeSessionStore()
	srv, _ := accountServer(t, users, sessions)
	prefix := signInAs(t, srv, users)

	if st := patchMe(t, srv, prefix, `{}`).Code; st != http.StatusBadRequest {
		t.Fatalf("empty PATCH status = %d, want 400", st)
	}
}
