# Research Notes — User Accounts, Sign-In & Profile

Resolves the unknowns in `plan.md` Technical Context. Grounded in the existing
codebase: the orchestrator's identity seam (`internal/orchestrator/identity.go`),
route surface (`server.go`), the store/migration/grants pattern (`0005`-style grants,
`0015`/`0016` migrations), the bearer-auth precedent (`internal/service/auth.go`), the
BFF proxy (`web/lib/proxy.ts`), and the typed client (`web/lib/orchestrator.ts`).

## R1 — Where accounts and sessions live

**Decision**: New Postgres tables `users` and `sessions` (migration `0017`), backed by one
`internal/store` package (`usersessions.go`). Passwords hash with the Go-1.24 **stdlib**
`crypto/pbkdf2`; sessions store only a SHA-256 **token hash** (digests, never raw tokens —
the same rule `service/auth.go` establishes for bearer tokens).

**Rationale**: The orchestrator is already the sole owner of the audit-writing identity
seam and the only service the web UI calls; it already holds the Postgres connection roles.
Postgres-backed sessions make authorization-loss durable (an admin's deactivate takes
effect on the next request, and restarting the orchestrator does not silently relogin an
ex-deactivated user), and they integrate with the existing integration suite. Storing
`token_hash` instead of the token keeps a DB leak from becoming session-theft.

**Alternatives considered**:
- *In-memory session map in the orchestrator process* — rejected: sessions would not
  survive a restart, would defeat the deactivate-takes-effect edge case across reboots,
  and add per-instance state to a service that is otherwise stateless Postgres-bound.
- *JWT/signed cookies (stateless)* — rejected: revocation (sign-out, deactivate) cannot
  take effect until an expiry the spec deliberately doesn't want (no idle timeout); a
  validating lookup is required anyway, so a random token + row is simpler than a signature
  scheme plus revocation list.
- *golang.org/x/crypto bcrypt for hashing* — rejected: adds a dependency the repo
  otherwise does not need; stdlib PBKDF2 (since Go 1.24) is a first-class KDF and avoids
  `go mod tidy` churn under the pinned toolchain. (bcrypt's 72-byte truncation footgun is
  avoided entirely.)

## R2 — Session cookie mechanics and the BFF tunnel

**Decision**: An opaque random token in an `HttpOnly`, `SameSite=Lax`, `Path=/` cookie
named `arborette_session` (default; `ARBORETTE_SESSION_COOKIE` override), set by the
orchestrator on register/login, cleared on logout. **The web BFF becomes an explicit
cookie tunnel**: `web/lib/proxy.ts` must forward the browser's `Cookie` header upstream
and relay any upstream `Set-Cookie` back through `passThrough` and `eventStream` — today
it copies only `content-type` and would silently drop both.

**Rationale**: The browser only ever talks to the web origin (`:3000`/`:8083`); the
orchestrator's `:8080` is internal. The BFF therefore must carry the session both ways or
the feature cannot exist; `forward`, `forwardStream`, and `forwardStreamSubmit` all need
the relay since the HTTP surface of authorization (`401`) shows up on JSON and SSE alike.
`SameSite=Lax` is sufficient (same-origin app, no cross-site embedding), and `Secure` is
config-controlled (`ARBORETTE_SESSION_SECURE`, default off) because the compose stack
serves plain HTTP on localhost.

**Alternatives considered**:
- *Expose the orchestrator to the browser with CORS and direct cookie* — rejected: breaks
  the existing BFF architecture, the internal network boundary, and every current route's
  pass-through; the cookie would belong to a different origin anyway.
- *Keep the session at the web layer (Cookie in Next, session secret in the BFF)* —
  rejected: splits identity between two systems when the orchestrator already owns the
  audit identity and the user row; FR-008 (real identity on activity) is the orchestrator's
  job.

## R3 — Authenticating and protecting the routes

**Decision**: A cookie-session middleware wraps every analyst-facing orchestrator route
except `POST /register` and `POST /login` (and the always-bearer `/internal/*`). It reads
the cookie, hashes it, looks up the session row, loads the user, **re-checks `active`**,
then stashes the resolved `Analyst` into the request context. `Identity.Current` reads the
context; `SessionIdentity` uses it and falls back to the configured stub for the internal
audit/boot paths. A missing/invalid cookie answers `401 {"error":"unauthorized"}` with the
`WWW-Authenticate` discipline `service/auth.go` establishes on its 401.

**Rationale**: This is the single seam the identity docstring anticipates ("a future
SSO/per-department implementation replaces" — `identity.go`). Wrapping once in `Routes()`
(rather than per-handler) keeps every existing handler unchanged, including the read-heavy
ones the UI reaches through the tunneled SSE paths. Per-request `active` re-check is what
makes the "admin deactivates a signed-in user" edge case true instead of token-expiry
faith.

**Alternatives considered**:
- *Check `active` only at login* — rejected: that exact edge case (deactivate takes effect
  next request) is in the spec and would fail.
- *A custom context-keyed identity injection per handler* — rejected: a middleware is the
  established Go pattern and keeps call sites untouched.

## R4 — The seed rule without a table-global integration assertion

**Decision**: `RegisterUser` serializes the seed decision with a **transaction-scoped
advisory lock** on a fixed sentinel (`pg_advisory_xact_lock`) before
`SELECT COUNT(*) FROM users WHERE role='admin' AND active`; count 0 → seeded admin with
`admin_notice_acknowledged = false`; count ≥ 1 → member. **Testing**: the global-count
logic is asserted in **unit tests against a fake `userStore`** (`CountActiveAdmins()==0`
→ admin, `==5` → member, register-error → 500, race is the lock's job); integration tests
never assert "the very first user" because the shared DB already holds an admin from every
prior run (AGENTS.md: never assert table-global counts). Integration instead creates
uniquely-named users and asserts they register as members — deterministic in any residue.

**Rationale**: The clarified seed rule ("whenever no active admin exists") is inherently a
table-global query, which the shared integration DB makes untestable-as-global. The
unit/fake split is exactly the AGENTS.md pattern for such rules, and the advisory lock is
the same discipline `internal/store/advisorylock.go` already proves for the discovery
loop — two concurrent first-registrations cannot double-seed. `.env`-sourced defaults get
explicitly cleared in tests (`t.Setenv`) per AGENTS.md, so a host operator's
`ARBORETTE_SESSION_COOKIE` cannot mask a default-name assertion.

**Alternatives considered**: *A dedicated `bootstrap_admin` table/flag* — rejected: a
whole second concept to express "count active admins" that a filtered query already
answers; C was also rejected in clarify because an operator-admin step slows setup.

## R5 — Password storage and the login failure

**Decision**: `crypto/pbkdf2.Key(sha256.New, password, salt(16B), 600_000, 32)` store as a
self-describing string `pbkdf2$sha256$600000$<saltB64>$<keyB64>`; verify by constant-time
`hmac.Equal` (or `sha256.Sum256` compare on the derived keys). Login compares against the
stored hash **when the user exists**, and against a fixed dummy hash + identical
iteration/compare flow **when it doesn't**, so the response path is uniform (no
username enumeration via timing or message). Return one generic
`401 {"error":"invalid username or password"}` either way.

**Rationale**: OWASP-baseline PBKDF2-HMAC-SHA256 at 600k iterations for an internal
tool; the self-describing envelope survives algorithm bumps without a schema migration.
The dummy-compare branch is the standard enumeration-defense for an app with no email and
no silent fallback (FR-002/FR-004).

**Alternatives considered**: *x/crypto bcrypt* — rejected (R1). *Lower iterations* —
rejected: login is infrequent and one analyst-scale console can afford ~200 ms.

## R6 — Audit vocabulary for account management

**Decision**: New orchestrator audit actions under existing event vocabulary discipline:
`user_register`, `user_password_change`, `user_username_change`, `user_password_reset`
(admin), `user_role_change` (admin promote/demote), `user_status_change` (admin
activate/deactivate). Detail keys follow the codebase-wide vocabulary: `user_id`,
`username`, `role`; the acting identity comes from the context stash (FR-008), so the
recorded `actor` is the real signed-in user. Login/logout are session transitions, not
management actions — deliberately **not** audited (YAGNI, consistent with the "access is
not an accountability action" precedent).

**Rationale**: The audit table (`0003`) and its append-only writer are untouched; only
new action strings + established key discipline are added. The spec-side decision to
attribute the real user replaces the stub only for human actions — the worker's
`handleAudit` and boot-time `ReconcileDatasets` keep the stub fallback (R3).

**Alternatives considered**: *Audit login/logout* — rejected as noise at internal-tool
scale and contrary to the surviving "access is not an action" precedent.

## R7 — Admin self-service and the last-admin guard

**Decision**: `PATCH /users/{id}` is the single admin mutation endpoint: JSON fields
`role` (admin|member), `active` (bool), `new_password` (reset). Guard rules live in the
handler, not the client: a change that would leave zero **active admins** (demoting or
deactivating the last one) returns `409`, before any write. Self-service `PATCH /me`
mutates `username` (uniqueness-checked) or `password` (current-password-confirmed, FR-009)
and carries the admin-notice acknowledgment flag. `POST /users` (admin-created accounts)
always creates a member — only the seed path grants admin.

**Rationale**: One patch verb keeps the surface small (spec FR-012's "view, create,
reset, grant/revoke, deactivate" are all single-field mutations), and concentrating the
guard in one place — the handler that owns the count — is the only way to make FR-013
provably server-enforced (a UI toggle can lie). Username changes don't rotate the session
(the session keys on user id, not username); password changes don't either.

**Alternatives considered**: *Separate grant/revoke/reset endpoints* — rejected: seven
nearly-identical routes for five field mutations; a versioned patch with explicit fields is
fewer moving parts, matching how `PATCH /datasets/{id}` already mutates status/name.

## R8 — Web surfaces and header cleanup

**Decision**: New App-Router pages `/login`, `/settings`, `/users`; a
`web/middleware.ts` that reads the `arborette_session` cookie and redirects signed-out
visitors to `/login` (matcher excludes `/login`, `/_next`, `/api`, static assets — `/api`
is the only path excluded unconditionally); `AppNav` LINKS reduce to **Datasets** only and
its right edge hosts the client `UserMenu` chip (avatar = first letter of the signed-in
username; dropdown = username + role, Settings, Sign out, and an admin-only **Users**
entry); the dismissible first-sign-in admin notice rides in `getMe()` as
`admin_notice: true` and is acknowledged via `PATCH /me` (`acknowledge_admin_notice`).
Server enforcement stays authoritative; the role-gated **Users** entry is a UI convenience
that a member's 403 would already reject.

**Rationale**: Matches the project's "no component tests; pure logic in `web/lib/`" rule:
the cookie name, `/me` state shape, and role-gating helpers live in `lib/session.ts` and
`lib/orchestrator.ts` (vitest-covered), while the components stay view-thin. Objectives and
Heuristics remain reachable the spec way — through the dataset flow — since only the
header entries are removed; no routes are deleted (spec FR-015 and its assumption).

**Alternatives considered**: *Client-only auth gate (every page fetches `/me`)* —
rejected: server middleware is the standard App-Router redirect mechanism and avoids a
spinner-then-redirect on every page load. *Remove the goals/heuristics pages* — rejected:
the spec says links are removed but the views stay reachable through the dataset flow.

## R9 — Config and wiring

**Decision**: `OrchestratorConfig` gains `SessionCookieName` (`ARBORETTE_SESSION_COOKIE`,
default `arborette_session`) and `SessionSecure` (`ARBORETTE_SESSION_SECURE`, default
`false`). `main.go` constructs `store.NewUserSessionStore(pool)`, passes it plus the cookie
config into `NewServer`, and passes `SessionIdentity{Fallback: StubIdentity{cfg.Orchestrator.AnalystID}}`
as the identity. `ReconcileDatasets` keeps the pure `StubIdentity` (a boot-time
system actor). The web middleware reads `process.env.ARBORETTE_SESSION_COOKIE ??
"arborette_session"` — the same default, so compose and make-test need no new env invals.

**Rationale**: Two knobs, both with the repo's `env(key, default)` pattern; everything
else is code-constant. Keeping the internal stub as the fallback preserves the Sleep-Cycle
Worker's `/internal/audit` path and boot reconciliation without a second auth scheme.

**Alternatives considered**: *New config struct for the cookie* — rejected: two scalar
fields have no reason to form a struct until a third appears.

## R10 — Integration/test discipline (shared DB, cookies, races)

**Decision**: Go integration: uniquely-named users (`gov_<hex>` suffix) collide never;
assert per-row (login succeeds, self-service change reflected, admin list returns the rows
the test created, promote-`member`→`admin` flips `role`), never `COUNT(users)`/global
first-user claims. Cookies are replayed from `Set-Cookie` in `httptest` chains. Unit
fakes (`fakeUserStore`, `fakeSessionStore`) return `ctx.Err()` when the context is done
(AGENTS.md) so the middleware/register handler reads as correct. The web BFF cookie
relay gets a vitest over `proxy.ts` (a canned upstream with `Set-Cookie` round-trips the
header; a request with a `Cookie` header forwards it). The run-lifecycle's detached-audit
goroutines that consult the identity stash keep `go test -race ./internal/orchestrator`
green — no shared-mutable fakes there.

**Rationale**: The two table-global hazards are the seed admin (R4) and the last-admin
guard (R7, also fake-asserted). Everything else follows the AGENTS.md per-row rule that
`make test`'s shared database and the `meta_heuristic_embeddings` truncation already force.

**Alternatives considered**: *Truncate `users`/`sessions` in `internal/testutil` like
`meta_heuristic_embeddings`* — rejected: that would destroy a real operator's accounts on
a gate run and hides the residue-sensitivity that uniquely-named users already solve.