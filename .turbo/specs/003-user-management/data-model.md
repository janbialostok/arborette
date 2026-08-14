# Data Model — User Accounts, Sign-In & Profile

Adds the accounts and session stores to Postgres (`internal/store`). Neo4j, the object
store, and pgvector embeddings are **untouched** by this feature. Two new tables, created
by one migration `0017_users_sessions`. The `audit_log` schema is unchanged; it gains new
action strings only (see contracts).

## New entity: User (account)

**`users`** — the person's identity on the console.

| Column | Type | Constraints | Meaning |
|--------|------|-------------|---------|
| `id` | `uuid` | PK, default `gen_random_uuid()` | Stable account id; sessions and audit reference it |
| `username` | `text` | NOT NULL; unique case-insensitively | Sign-in identifier; uniqueness enforced by a function index on `lower(username)` (the `datasets.name` precedent, 0015) |
| `password_hash` | `text` | NOT NULL | Self-describing PBKDF2 envelope `pbkdf2$sha256$<iter>$<saltB64>$<keyB64>`; never the plaintext, never reversible |
| `role` | `text` | NOT NULL `CHECK (role IN ('admin','member'))` | Single extensible role value; a later "groups" feature adds values here, not a new table |
| `active` | `boolean` | NOT NULL default `true` | Deactivated accounts cannot sign in and their sessions are rejected at middleware time |
| `admin_notice_acknowledged` | `boolean` | NOT NULL default `false` | First-sign-in admin-grant notice: false until the seeded admin dismisses it (`PATCH /me`) |
| `created_at` | `timestamptz` | NOT NULL default `now()` | Account created |
| `updated_at` | `timestamptz` | NOT NULL default `now()` | Last mutation (name/password/role/status/a-ack) |

**Seeding rule (FR-003, resolved Q2)**: the seed decision happens inside a transaction
that first takes a **transaction-scoped advisory lock on a fixed seed sentinel**, then
counts `role='admin' AND active`:
`COUNT(*) == 0` → the account is created as `admin` with `admin_notice_acknowledged =
false`; `COUNT(*) ≥ 1` → created as `member`. The lock serializes two concurrent first
registrations so exactly one seeds. This is the codebase's established advisory-lock
discipline (`internal/store/advisorylock.go`).

**Delegation isn't special**: the initial admin promotes/demotes others through the same
`role` column (admin account reads/ops), so no privilege columns or delegate table exist.

## New entity: Session

**`sessions`** — a signed-in session, keyed by an opaque token hash.

| Column | Type | Constraints | Meaning |
|--------|------|-------------|---------|
| `id` | `uuid` | PK, default `gen_random_uuid()` | Row identity |
| `user_id` | `uuid` | NOT NULL, FK → `users(id)` ON DELETE CASCADE | Account this session belongs to |
| `token_hash` | `text` | NOT NULL, unique | SHA-256 of the cookie's opaque token — digests, never the raw token (the `service/auth.go` rule) |
| `created_at` | `timestamptz` | NOT NULL default `now()` | When signed in |

No `expires_at`: the spec declines an idle timeout ("a session lasts until sign-out or
deauthorization"). Sign-out deletes the row; deactivation doesn't delete rows but the
middleware rejects the user's sessions by re-reading `active` every request.

## Grants (0017, matching the `0005` discipline)

```
GRANT SELECT, INSERT, UPDATE, DELETE ON users    TO arborette_orchestrator;
GRANT SELECT, INSERT, DELETE ON sessions         TO arborette_orchestrator;
```

- No service-role grants: Sleep-Cycle/Verifier never touch accounts or sessions — they
  reach audit only, and their `POST /internal/audit` path needs no user row.
- No delete of `users` in the v1 feature (FR-012 ships deactivate, not delete), but the
  DELETE grant is symmetric with the sessions CASCADE so an operator-level cleanup is
  possible; the handler surface simply doesn't expose it.
- `audit_log` INSERT grant already exists (0005); no new sequence/identity grants.

## Store shapes (`internal/store/usersessions.go`)

- `type User struct { ID, Username, PasswordHash, Role string; Active bool;
  AdminNoticeAcknowledged bool; CreatedAt, UpdatedAt time.Time }`
- `type Session struct { ID, UserID, TokenHash string; CreatedAt time.Time }`
- `UserStore` methods (the narrow consumer slice lives in `server.go` interface):
  - `Create(ctx, username, passwordHash string, seedAdmin bool) (User, error)` — insert
    honoring the seed decision made by the caller-supplied advisory-lock transaction;
    returns `ErrUsernameConflict` on the unique-lower-name clash.
  - `GetByUsername(ctx, username) (User, error)` — case-insensitive (`lower(username)=lower($1)`).
  - `GetByID(ctx, id) (User, error)` / `List(ctx) ([]User, error)` — admin view surface.
  - `CountActiveAdmins(ctx) (int, error)` — the seed and last-admin guard oracle.
  - `ActivateSeededAdmin(ctx, ...)` — not needed: the a-ack flag rides `Update` (below).
  - `Update(ctx, id, fields…)` — a narrow field-mutation method covering `username`
    (re-checking uniqueness), `password_hash`, `role`, `active`,
    `admin_notice_acknowledged`.
- `SessionStore` methods: `Create(ctx, tokenHash, userID) (Session, error)`,
  `GetByTokenHash(ctx, tokenHash) (Session, error)`, `DeleteByTokenHash(ctx, tokenHash) error`.

## Validation rules (from requirements)

| Rule | Source | Enforced where |
|------|--------|----------------|
| Username 3–32 chars, letters/digits `_` `-` | Assumption / FR-002 | Orchestrator handler (shared `validateUsername`) |
| Username unique case-insensitively | FR-001/FR-002 | DB unique index + `ErrUsernameConflict` → 409 |
| Password ≥ 8 chars at register and change | Assumption / FR-002/FR-009 | Orchestrator handler |
| Password change requires current password | FR-009 | Orchestrator handler (`verify` before `Update`) |
| First registrant when no active admin → admin; else member | FR-003 (Q2) | Store transaction under advisory lock |
| Last active admin cannot be demoted/deactivated | FR-013 | Orchestrator `handleUserUpdate`: `CountActiveAdmins` pre-check → 409 |
| Admin-only: list/create/reset/grant/deactivate | FR-012/FR-014 | Middleware role check on `/users*` routes (authoritative), UI entry gated for convenience |
| Login failure: one generic message, uniform timing | FR-004 (Q-sec) | Orchestrator `handleLogin` (dummy-hash compare on unknown usernames) |
| Dismissible admin notice until acknowledged | FR-003 (Q1) | `admin_notice_acknowledged` returned by `/me`; `PATCH /me` sets it |

## State transitions

- **User lifecycle**: `active=true → (deactivate) → active=false`, and `active=false →
  (reactivate) → active=true`. Deactivate rejects the account's live sessions at the next
  request (middleware `active` re-read) and blocks sign-in. A deactivated member keeps its
  row (data/audit attribution preserved — no delete path in v1).
- **Role transitions**: `member → admin` (promote), `admin → member` (demote), each
  subject to the FR-013 guard against leaving zero active admins.
- **Session lifecycle**: create on register/login; delete on logout; effectively dead on
  deactivate (middleware). No rotation policy in v1 (session keys on `user_id`, so username
  and password changes don't invalidate it).

## Ordered/derived concepts (not stored)

- **Admin-grant notice** — derived from `admin_notice_acknowledged = false` for a
  role=admin account; the UI shows it once and flips the flag.
- **Actor on audit rows** — derived from the context-stashed `Analyst` (the session user),
  replacing the stub for human actions; the stub remains the actor for `/internal/audit`
  and boot-time reconciliation.

## Side effects of this feature

- `audit_log` gains new actions (`user_register`, `user_role_change`, …) with detail keys
  `user_id`, `username`, `role` — new vocabulary under existing discipline, no migration.
- Every orchestrator request now carries a session lookup (one PK/hash-index read).
- `/register` and `/login` become the only unauthenticated analyst routes in the app.
- The BFF proxy starts carrying `Cookie`/`Set-Cookie` (a web-side behavioral change, not a
  data-model one).