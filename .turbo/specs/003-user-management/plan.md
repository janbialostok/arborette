# Implementation Plan: User Accounts, Sign-In & Profile

**Status**: done (implemented on 2026-08-13, all phases complete; live-stack quickstart and integration gate verified)

**Branch**: `003-user-management` | **Date**: 2026-08-13 | **Spec**: [spec.md](spec.md)

**Input**: Feature specification from spec.md (username/password account lifecycle — sign-up, sign-in, sign-out, self-service username/password — a role model seeded when no active admin exists, a Google-Docs-style corner identity chip with settings + sign-out, an admin account-management surface, and removal of the Objectives/Heuristics header links).

## Summary

Give the console its first real identity layer. The Go orchestrator, which already owns the `Identity` audit seam and is the only service the web UI talks to, becomes the session authority: it gains Postgres-backed `users` and `sessions` tables (migration `0017`), a cookie-session auth middleware around its analyst-facing routes, and a small REST surface — `POST /register`, `POST /login`, `POST /logout`, `GET /me`, `PATCH /me` (self-service username/password + admin-notice ack), and admin-only `GET /users` / `POST /users` / `PATCH /users/{id}`. Passwords hash with the Go-1.24 stdlib `crypto/pbkdf2` — **no new dependency**. The seed rule (clarified: seed admin whenever no active admin exists) is serialized with a transaction-scoped advisory lock; the last-admin guard refuses any demote/deactivate that would orphan the console. The web BFF proxies gain cookie tunneling (forward `Cookie` upstream, relay `Set-Cookie` back), a `middleware.ts` redirects signed-out visitors to `/login`, the new `/login`, `/settings`, and `/users` pages cover the flows, `AppNav` keeps only the brand mark + **Datasets**, and a `UserMenu` chip shows the username's first letter with settings/sign-out (plus the dismissible first-sign-in admin-grant notice). See `research.md`, `data-model.md`, `contracts/rest-api.md`, `quickstart.md`.

## Technical Context

**Language/Version**: Go 1.24 (pinned `GOTOOLCHAIN`); stdlib `crypto/pbkdf2`
(added 1.24.0 — available under the pinned toolchain, verified present in the local
`go1.26` toolchain docs; re-verify with `go doc crypto/pbkdf2` under 1.24 during build).
Web: TypeScript on Next.js ≥15 (App Router), Node 22.

**Primary Dependencies**: None new. Passwords hash via stdlib PBKDF2-HMAC-SHA256
(`pbkdf2$sha256$<iter>$<salt>$<key>` envelope, constant-time compare), avoiding a
`golang.org/x/crypto` addition and leaving `go.mod` untouched (`pgx/v5` stays ≤ v5.7.x
per AGENTS.md). `google/uuid` (already required) makes session/user ids.

**Storage**: Postgres. Migration `0017_users_sessions` adds `users` and `sessions` with
least-privilege grants for the `arborette_orchestrator` runtime role (SELECT/INSERT/UPDATE
on `users`; SELECT/INSERT/DELETE on `sessions`); the audit-log INSERT grant already
exists. `audit_log` gains no schema change — two new vocabulary actions with established
detail-key discipline (`user_id`, `username`, `role`). Neo4j / S3 / pgvector untouched.

**Testing**: Go `go test ./...` (integration gated `ARBORETTE_INTEGRATION=1`), `make test`
(`-p 1`, localhost overrides), `go test -race ./internal/orchestrator` (sessions/identity
and their fakes), `go vet`. Web `npm test` (vitest) from `web/`. AGENTS.md rules bite this
feature hard: the **seeded-admin rule depends on a table-global count**, which the shared
integration DB forbids asserting — so seed/all-admin-count assertions live in unit tests
against a fake `userStore` (`CountActiveAdmins()==0 → admin`, `≥1 → member`, last-admin
guard), while integration tests create **their own uniquely-named users** and assert only
per-row effects (register→member under a populated DB, login, self-service change, admin
ops that never touch the global admin count). A scripted-fake negotiation rule from
AGENTS.md applies to login failure paths (one scripted 401 per attempt).

**Target Platform**: Local docker compose stack (orchestrator :8080, web :3000 dev /
:8083 standalone, sleepcycle-serve :8084, verifier-serve :8085) — an internal
analyst-facing web app. Plain-HTTP localhost means the session cookie omits `Secure`
(config `ARBORETTE_SESSION_SECURE`, default false in compose).

**Project Type**: Web application (Go orchestrator backend + Next.js App Router frontend
with a server-side BFF proxy).

**Performance Goals**: Spec SC-001/SC-009 — sign-up-to-signed-in admin under 2 min / at
most 2 screens. The auth middleware adds one indexed session→user lookup per request; at
single-analyst scale (< 10 sessions, few users) this is irrelevant. Login/registration
hash cost dominates (600k PBKDF2 iterations, ~200 ms): inside the SC-001 budget.

**Constraints**: Every analyst-facing orchestrator route except `/register` and `/login`
now requires a valid session (deactivation is honored the next request — the middleware
re-reads the user's `active` flag per request). Cookie tunneling through the BFF is
mandatory (browsers only ever talk to the web origin). The login failure is a single
generic 401, never revealing username existence or timing-detectable difference. The
last-admin guard and the seed rule are enforced server-side, never client-side. `GET
/me` intentionally bypasses no gate but is the only cheap route the UI uses to bootstrap
identity. Removed header links must not break any live route (goals/heuristics pages stay;
only the header entries go).

**Scale/Scope**: Single shared workspace, a handful of accounts. New full stack surface
(store migrations + accounts/sessions store, orchestrator session middleware + 9 routes,
web BFF cookie handling + 3 pages + UserMenu + middleware), but no change to goals /
datasets / heuristics behavior, intra-cluster auth (`/internal/*` stay Bearer-guarded;
the Sleep-Cycle Worker and Verifier keep their shared-token seam), or any worker service.

## Constitution Check

*GATE: Must pass before Phase 0 research. Re-checked after Phase 1 design.*

The project `.specify/memory/constitution.md` is the unfilled repository template (no
custom principles ratified). The governing conventions come from `AGENTS.md`; all gates
pass:

- **Toolchain/dependency discipline** — Go 1.24 pin kept; **zero new Go dependencies**
  (stdlib `crypto/pbkdf2`); no `go.mod` movement. Web dependencies untouched (Next ≥15
  already pinned; no new npm packages).
- **Migrate pattern** — additive migration `0017` for the two new tables with per-table
  least-privilege grants matching `0005`/`0015` style; `audit_log` schema untouched; event
  vocabulary extended with the codebase-wide detail-key discipline.
- **Testability** — the seed rule's table-global dependency is defused by construction:
  unit tests own the global-count assertions against fakes; integration tests assert
  per-row effects on uniquely-named users and never `COUNT(users)`-global numbers. The
  session/auth path is unit-tested with `httptest` + cookie replay and `-race` where the
  run lifecycle's detached audit goroutines can touch shared fakes.
- **Web conventions** — the BFF stays the only browser reach into the orchestrator; the
  proxy gains explicit cookie tunneling (a documented contract, not hidden behavior);
  non-trivial rules (cookie name, /me state, role gating) live in `web/lib/` for vitest,
  matching the "no component tests" project rule.
- **No unwarranted complexity** — one store package for accounts+sessions, one cookie
  scheme reused for every session-secured route, one `PATCH /users/{id}` for admin
  operations; no per-group per-row tenancy (explicitly out of scope), no email, no
  self-service password reset, no deletion of users (deactivate only — FR-012).

No violations to justify; `Complexity Tracking` below is intentionally empty.

## Project Structure

### Documentation (this feature)

```text
.turbo/specs/003-user-management/
├── plan.md              # this file
├── research.md          # Phase 0 — accounts/sessions, cookie tunneling, seed, hashing
├── data-model.md        # Phase 1 — users/sessions schema, grants, seed & guard rules
├── quickstart.md        # Phase 1 — end-to-end validation guide
├── contracts/
│   └── rest-api.md      # Phase 1 — auth/session/admin endpoints + cookie contract
└── tasks.md             # created by /speckit.tasks (next phase)
```

### Source Code (repository root)

```text
internal/store/
├── migrations/
│   ├── 0017_users_sessions.up.sql        # users + sessions + grants + seed-guard indexes
│   └── 0017_users_sessions.down.sql
├── usersessions.go                      # User, Session types; UserStore + SessionStore:
│                                        #   InsertUser, GetUserByUsername, ListUsers,
│                                        #   CountActiveAdmins, SetPassword, SetUsername,
│                                        #   SetRoleStatus, UpdateUsername; Session:
│                                        #   CreateSession(tokenHash,userID), GetByToken,
│                                        #   DeleteSession. ErrUsernameConflict sentinel.
├── usersessions_integration_test.go     # per-row integration coverage (unique usernames)
├── roles.go / pool.go                   # unchanged (runtime-role LOGINs, pool)
└── migrations.go                        # unchanged (//go:embed *.sql)

internal/orchestrator/
├── session.go                           # session-auth middleware: parse cookie by name,
│                                        #   lookup token_hash, load user (active check),
│                                        #   stash Analyst in ctx, 401 otherwise; marks
│                                        #   public routes (/register, /login)
├── accounts.go                          # POST /register, /login, /logout, GET /me,
│                                        #   PATCH /me (+ ack admin notice)
├── accountadmin.go                      # admin-only: GET /users, POST /users,
│                                        #   PATCH /users/{id} (role/active/password reset)
│                                        #   + last-admin guard (409)
├── identity.go                          # + SessionIdentity: Current() reads ctx stash,
│                                        #   falls back to StubIdentity (internal/boot paths)
├── server.go                            # + userStore/sessionStore interfaces, cookie name;
│                                        #   Routes() wraps analyst routes in the middleware,
│                                        #   keeps /internal/* under BearerAuth
├── accounts_test.go                     # unit: seed rule, generic-401 login, self-service,
│                                        #   admin guard (fake userStore; ctx.Err() doubles)
├── session_test.go                      # unit: cookie replay, inactive user → 401
└── server_test.go                       # fakes: fakeUserStore, fakeSessionStore

cmd/orchestrator/main.go                 # wire UserStore+SessionStore; SessionIdentity{
                                         #   Fallback: StubIdentity{...}} passed to NewServer;
                                         #   ReconcileDatasets keeps StubIdentity
internal/config/config.go                # OrchestratorConfig += SessionCookieName
                                         #   (ARBORETTE_SESSION_COOKIE, "arborette_session"),
                                         #   SessionSecure (ARBORETTE_SESSION_SECURE, false)
```

```text
web/
├── middleware.ts                        # matcher excludes /login, /_next, /api, static;
│                                        #   no arborette_session cookie → redirect /login
├── app/login/page.tsx                   # sign-in + registration in one screen
├── app/settings/page.tsx                # self-service username / password forms
├── app/users/page.tsx                   # admin: list, create, promote/demote, reset,
│                                        #   deactivate (server-enforced)
├── components/AppNav.tsx                # LINKS = Datasets only; right side renders
│                                        #   <UserMenu />, keeps brand mark → "/"
├── components/UserMenu.tsx              # avatar chip (first letter) → menu: username +
│                                        #   role, Settings, (admin) Users, Sign out;
│                                        #   dismissible first-sign-in admin notice
├── components/AuthedForm.tsx            # (opt.) shared login/password field shell
├── app/api/orchestrator/
│   ├── register/route.ts                # NEW BFF route handlers (forward*)
│   ├── login/route.ts
│   ├── logout/route.ts
│   ├── me/route.ts
│   └── users/route.ts, users/[id]/route.ts
├── lib/proxy.ts                         # forward* + passThrough/eventStream relay
│                                        #   Cookie upstream and Set-Cookie back
├── lib/orchestrator.ts                  # + AccountDto, MeDto, UserAdminDto, SessionState;
│                                        #   registerUser, login, logout, getMe, updateMe,
│                                        #   listUsers, createUser, updateUser (credentials
│                                        #   same-origin: cookies flow automatically)
├── lib/session.ts                       # pure helpers: cookie name const, isLoggedIn, role
│                                        #   gating for views (vitest-covered)
└── lib/orchestrator.test.ts             # new DTO/client typings; proxy cookie tests
```

**Structure Decision**: The repo is one Go module plus the `web/` deployable; this feature
maps onto the existing layers with no new service: accounts+sessions live in `internal/
store` (the orchestrator already holds the Postgres DSN and the audit-writing identity
seam), the session middleware is orchestrator-local (`identity.go` already owns the
identity concept; `service/` stays generic plumbing), and the web surface is App-Router
pages plus BFF route handlers exactly like the existing datasets/goals surfaces. Cookies
tunnel through the existing BFF rather than opening a second browser→orchestrator channel,
which is why the proxy cookie relay is the one cross-cutting change. No re-architecture.

## Complexity Tracking

> No `AGENTS.md` / constitution violations justified — no new projects, no added
> repository pattern, no schema re-architecture. The two genuinely new mechanisms are a
> **cookie-tunnel through the BFF** (mandated by the deployment shape: browsers only reach
> the web origin; the alternative — opening orchestrator :8080 to the browser with CORS —
> was rejected in research R2) and the **transaction-scoped seed advisory lock**
> (mandated by correctness: two simultaneous first registrations must not double-seed an
> admin; the lock is the same Postgres advisory-lock discipline the discovery loop already
> runs). The table is therefore intentionally empty.