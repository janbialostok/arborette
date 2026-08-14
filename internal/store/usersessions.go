package store

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// ErrUsernameConflict is returned when a user create or rename would break the
// case-insensitive unique-username rule (the lower(username) index). The
// orchestrator surfaces it as the 409 conflict the contract names.
var ErrUsernameConflict = errors.New("username is already in use")

// Role strings are a single extensible value: a later "groups" feature adds
// values here, not a new table.
const (
	RoleAdmin  = "admin"
	RoleMember = "member"
)

// User is a person's account as persisted in the users table.
type User struct {
	ID                      string
	Username                string
	PasswordHash            string
	Role                    string
	Active                  bool
	AdminNoticeAcknowledged bool
	CreatedAt               time.Time
	UpdatedAt               time.Time
}

// Session is one signed-in session, keyed by a SHA-256 digest of the opaque
// cookie token. The raw token is never stored (the auth.go digest rule).
type Session struct {
	ID        string
	UserID    string
	TokenHash string
	CreatedAt time.Time
}

// userColumns is the read projection every user query shares, so the column
// order and scan order cannot drift apart.
const userColumns = `id, username, password_hash, role, active, ` +
	`admin_notice_acknowledged, created_at, updated_at`

// NewUserSessionStore wires the accounts + sessions access package to a pool.
// Both tables are owned by the orchestrator role, so a single store covers both.
func NewUserSessionStore(pool *Pool) *UserSessionStore {
	return &UserSessionStore{pool: pool}
}

// UserSessionStore is the users/sessions access package. The store is one type
// because the session lifecycle (create on sign-in, delete on sign-out, user
// lookup on every request) is one feature; a narrower seam can still be cut at
// the orchestrator consumer (server.go interfaces).
type UserSessionStore struct {
	pool *Pool
}

// seedLockKey is the fixed advisory-lock sentinel mutualizing the first-registrant
// seed decision. pg_advisory_xact_lock is transaction-scoped: the lock is held
// until the seeding transaction commits, serializing two concurrent first
// registrations so exactly one seeds admin, and it is released automatically on
// commit or rollback -- no unlock bookkeeping.
var seedLockKey = fnvInt64Key("users", "seed-admin")

// Create registers a new account and returns the persisted row. The seed
// decision (first account when no active admin exists becomes admin, otherwise
// member) is resolved inside a transaction under the seed advisory lock: the
// caller proposes seedAdmin from its own CountActiveAdmins pre-check, and the
// store re-derives the role under the lock so a concurrent first registration
// cannot double-seed. A case-insensitive username clash reports
// ErrUsernameConflict; the caller never sees a raw constraint error.
func (s *UserSessionStore) Create(ctx context.Context, username, passwordHash string, seedAdmin bool) (User, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return User{}, fmt.Errorf("begin create user tx: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	// The advice vs decided distinction is the whole point: only the lock holder
	// whose count beats the commit gate may seed. pg_advisory_xact_lock blocks
	// (it is not the try variant), so the second concurrent registration waits
	// for the first to commit and then sees the seeded admin.
	if _, err := tx.Exec(ctx, "SELECT pg_advisory_xact_lock($1)", seedLockKey); err != nil {
		return User{}, fmt.Errorf("lock seed decision: %w", err)
	}

	var activeAdmins int
	if err := tx.QueryRow(ctx,
		"SELECT count(*)::int FROM users WHERE role = 'admin' AND active",
	).Scan(&activeAdmins); err != nil {
		return User{}, fmt.Errorf("count active admins in seed tx: %w", err)
	}

	role := RoleMember
	if seedAdmin && activeAdmins == 0 {
		role = RoleAdmin
	}

	var u User
	err = tx.QueryRow(ctx,
		"INSERT INTO users (username, password_hash, role) VALUES ($1, $2, $3) "+
			"RETURNING "+userColumns,
		username, passwordHash, role,
	).Scan(&u.ID, &u.Username, &u.PasswordHash, &u.Role, &u.Active,
		&u.AdminNoticeAcknowledged, &u.CreatedAt, &u.UpdatedAt)
	if err != nil {
		if isUsernameConflict(err) {
			return User{}, ErrUsernameConflict
		}
		return User{}, fmt.Errorf("insert user: %w", err)
	}

	if err := tx.Commit(ctx); err != nil {
		return User{}, fmt.Errorf("commit create user tx: %w", err)
	}
	return u, nil
}

// GetByUsername looks up an account by its sign-in identifier, matching
// case-insensitively the way registration enforces uniqueness.
func (s *UserSessionStore) GetByUsername(ctx context.Context, username string) (User, error) {
	var u User
	err := s.pool.QueryRow(ctx,
		"SELECT "+userColumns+" FROM users WHERE lower(username) = lower($1)",
		username,
	).Scan(&u.ID, &u.Username, &u.PasswordHash, &u.Role, &u.Active,
		&u.AdminNoticeAcknowledged, &u.CreatedAt, &u.UpdatedAt)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return User{}, pgx.ErrNoRows
		}
		return User{}, fmt.Errorf("get user by username %q: %w", username, err)
	}
	return u, nil
}

// GetByID looks up an account by id. It is the session middleware's per-request
// read: the row is re-fetched so a deactivated account's sessions are rejected
// at the next request without any deletion.
func (s *UserSessionStore) GetByID(ctx context.Context, id string) (User, error) {
	var u User
	err := s.pool.QueryRow(ctx,
		"SELECT "+userColumns+" FROM users WHERE id = $1", id,
	).Scan(&u.ID, &u.Username, &u.PasswordHash, &u.Role, &u.Active,
		&u.AdminNoticeAcknowledged, &u.CreatedAt, &u.UpdatedAt)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return User{}, pgx.ErrNoRows
		}
		return User{}, fmt.Errorf("get user by id %q: %w", id, err)
	}
	return u, nil
}

// List returns every account, ordered by creation so the admin roster is stable
// across reloads.
func (s *UserSessionStore) List(ctx context.Context) ([]User, error) {
	rows, err := s.pool.Query(ctx,
		"SELECT "+userColumns+" FROM users ORDER BY created_at ASC, username ASC")
	if err != nil {
		return nil, fmt.Errorf("list users: %w", err)
	}
	defer rows.Close()

	var users []User
	for rows.Next() {
		var u User
		if err := rows.Scan(&u.ID, &u.Username, &u.PasswordHash, &u.Role, &u.Active,
			&u.AdminNoticeAcknowledged, &u.CreatedAt, &u.UpdatedAt); err != nil {
			return nil, fmt.Errorf("scan user row: %w", err)
		}
		users = append(users, u)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate user rows: %w", err)
	}
	return users, nil
}

// CountActiveAdmins is the seed and last-admin-guard oracle: how many
// role=admin accounts are active right now. The orchestrator consults it both to
// propose seedAdmin on registration and to refuse a demotion/deactivation that
// would leave zero active admins.
func (s *UserSessionStore) CountActiveAdmins(ctx context.Context) (int, error) {
	var n int
	err := s.pool.QueryRow(ctx,
		"SELECT count(*)::int FROM users WHERE role = 'admin' AND active").Scan(&n)
	if err != nil {
		return 0, fmt.Errorf("count active admins: %w", err)
	}
	return n, nil
}

// EarliestActiveAdmin resolves the admin caretaker: the oldest active admin
// account, the deterministic owner every un-owned pre-ownership dataset and
// legacy goal is attributed to. The migration backfill and the runtime
// self-heal use the same ordering (created_at, then id) so attribution is
// stable and convergent. An empty id signals that no active admin exists (the
// seed admin is still the first registrant's concern).
func (s *UserSessionStore) EarliestActiveAdmin(ctx context.Context) (User, error) {
	var u User
	err := s.pool.QueryRow(ctx,
		"SELECT "+userColumns+" FROM users WHERE role = 'admin' AND active "+
			"ORDER BY created_at ASC, id ASC LIMIT 1",
	).Scan(&u.ID, &u.Username, &u.PasswordHash, &u.Role, &u.Active,
		&u.AdminNoticeAcknowledged, &u.CreatedAt, &u.UpdatedAt)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return User{}, pgx.ErrNoRows
		}
		return User{}, fmt.Errorf("earliest active admin: %w", err)
	}
	return u, nil
}

// UserUpdate is the narrow field-mutation slice the account and admin handlers
// use. Pointer fields keep PATCH semantics: nil means "leave unchanged", so a
// consumer updates exactly the columns it means to. Zero fields yields a no-op
// RowsAffected 0 (pgx.ErrNoRows is not returned for a no-op update -- the row
// still matched, it just did not change), which callers treat as an unchanged
// success.
type UserUpdate struct {
	Username                *string
	PasswordHash            *string
	Role                    *string
	Active                  *bool
	AdminNoticeAcknowledged *bool
}

// Update applies the non-nil fields of upd to the account and returns the
// refreshed row. A rename onto an existing name reports ErrUsernameConflict
// (case-insensitive); an unknown id reports pgx.ErrNoRows. Fields are validated
// at the handler before reaching this write.
func (s *UserSessionStore) Update(ctx context.Context, id string, upd UserUpdate) (User, error) {
	var sets []string
	var args []any
	arg := func(v any) string {
		args = append(args, v)
		return fmt.Sprintf("$%d", len(args))
	}
	if upd.Username != nil {
		sets = append(sets, "username = "+arg(*upd.Username))
	}
	if upd.PasswordHash != nil {
		sets = append(sets, "password_hash = "+arg(*upd.PasswordHash))
	}
	if upd.Role != nil {
		sets = append(sets, "role = "+arg(*upd.Role))
	}
	if upd.Active != nil {
		sets = append(sets, "active = "+arg(*upd.Active))
	}
	if upd.AdminNoticeAcknowledged != nil {
		sets = append(sets, "admin_notice_acknowledged = "+arg(*upd.AdminNoticeAcknowledged))
	}
	sets = append(sets, "updated_at = now()")
	idArg := arg(id)

	var u User
	err := s.pool.QueryRow(ctx,
		"UPDATE users SET "+strings.Join(sets, ", ")+" WHERE id = "+idArg+
			" RETURNING "+userColumns,
		args...,
	).Scan(&u.ID, &u.Username, &u.PasswordHash, &u.Role, &u.Active,
		&u.AdminNoticeAcknowledged, &u.CreatedAt, &u.UpdatedAt)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return User{}, pgx.ErrNoRows
		}
		if isUsernameConflict(err) {
			return User{}, ErrUsernameConflict
		}
		return User{}, fmt.Errorf("update user %q: %w", id, err)
	}
	return u, nil
}

// CreateSession records a signed-in session, keyed by the token's SHA-256. The
// caller mints the opaque token and stores only its digest here.
func (s *UserSessionStore) CreateSession(ctx context.Context, tokenHash, userID string) (Session, error) {
	var sess Session
	err := s.pool.QueryRow(ctx,
		"INSERT INTO sessions (token_hash, user_id) VALUES ($1, $2) "+
			"RETURNING id, user_id, token_hash, created_at",
		tokenHash, userID,
	).Scan(&sess.ID, &sess.UserID, &sess.TokenHash, &sess.CreatedAt)
	if err != nil {
		return Session{}, fmt.Errorf("create session: %w", err)
	}
	return sess, nil
}

// GetSessionByTokenHash resolves a session from the cookie's token digest. The
// middleware then loads the user and re-checks active -- deactivation is
// enforced by that read, not by deleting session rows.
func (s *UserSessionStore) GetSessionByTokenHash(ctx context.Context, tokenHash string) (Session, error) {
	var sess Session
	err := s.pool.QueryRow(ctx,
		"SELECT id, user_id, token_hash, created_at FROM sessions WHERE token_hash = $1",
		tokenHash,
	).Scan(&sess.ID, &sess.UserID, &sess.TokenHash, &sess.CreatedAt)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return Session{}, pgx.ErrNoRows
		}
		return Session{}, fmt.Errorf("get session by token hash: %w", err)
	}
	return sess, nil
}

// DeleteSessionByTokenHash ends a session (sign-out). An absent row is a no-op,
// not an error: a session that was already deleted server-side or whose account
// was removed leaves nothing left to delete.
func (s *UserSessionStore) DeleteSessionByTokenHash(ctx context.Context, tokenHash string) error {
	if _, err := s.pool.Exec(ctx,
		"DELETE FROM sessions WHERE token_hash = $1", tokenHash); err != nil {
		return fmt.Errorf("delete session by token hash: %w", err)
	}
	return nil
}

// isUsernameConflict classifies a constraint violation from the lower(username)
// unique index as the case-insensitive username conflict it is.
func isUsernameConflict(err error) bool {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		return pgErr.Code == "23505" && strings.Contains(pgErr.ConstraintName, "users_username_lower")
	}
	return false
}
