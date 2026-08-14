# Quickstart — Validate User Accounts, Sign-In & Profile

Runnable end-to-end validation for the feature. Implementation bodies live in plan/tasks,
not here. Requires the live stack (`make up`) per the repo's integration conventions.

## Prerequisites

- `make up` (compose: postgres, minio, neo4j, orchestrator :8080, verifier, web :8083).
- Sourced `.env`; integration runs use `ARBORETTE_INTEGRATION=1` and localhost overrides.
- The shared integration DB persists rows between runs — **including accounts**. A real
  run leaves at least one admin behind, so the "first user becomes admin" path is only
  verifiable on a truly fresh stack (`docker compose down -v`, discuss with the operator
  before wiping). Against a populated stack, verify the seed rule the other way: registering
  a new user creates a **member** (an admin already exists).
- Cookies: the browser holds `arborette_session`; the BFF must relay it (contracts).

## Backend validation (Go, against the live stack)

Smoke-verify with a local orchestrator binary: `go build ./cmd/orchestrator`, run with
`.env` + localhost overrides (`S3_ENDPOINT=http://localhost:9000`, `ORCHESTRATOR_PORT=<free>`,
`ARBORETTE_SESSION_COOKIE=arborette_session`), use `curl -c/-b` jars to carry the cookie.

1. **Registration seeds admin (fresh stack only)**
   `curl -c jar -X POST -d '{"username":"first","password":"correctHorse99"}' \
   localhost:<port>/register` → `201` with `role:"admin"` and `admin_notice:true`; the jar
   now holds `arborette_session`. On a populated stack this same body returns
   `role:"member"` and `admin_notice:false` (an admin exists).
2. **Sign-in round trip**
   `curl -c jar -X POST -d '{"username":"first","password":"correctHorse99"}' .../login`
   → `200` + `Set-Cookie`. Wrong password → `401 {"error":"invalid username or password"}`
   — identical message for a nonexistent username (FR-004).
3. **Protected by default**
   `curl .../me` with no cookie → `401 {"error":"unauthorized"}`; with the jar → `200`
   `MeDto`. A request with the jar to any pre-existing route (`/datasets`, `/goals`) works —
   session middleware hands the old surface through unchanged.
4. **Self-service change (FR-009)**
   `PATCH /me -d '{"password":"newPass123","current_password":"correctHorse99"}'` → `200`;
   old password now `401`s at login, new signs in. `-d '{"username":"first2"}'` renames;
   the chip letter follows on the next `/me`.
5. **Admin list, create, reset, delegate (US4)**
   `GET /users` (as admin) → the account list. `POST /users -d '{"username":"member-a",
   "password":"..."}'` → `201` member. `PATCH /users/<id> -d '{"role":"admin"}'` → promoted
   (delegation); sign that account in and `GET /users` works for it too.
   `PATCH /users/<id> -d '{"new_password":"..."}'` → reset works on next login (FR-010).
6. **Last-admin guard (FR-013)**
   Create a second admin, then as the acting admin `PATCH /users/<own-id> -d '{"role":
   "member"}'` → `409` refused only while it is the last active admin; with a second admin
   active the same body succeeds.
7. **Deactivate takes effect now (edge)**
   `PATCH /users/<id> -d '{"active":false}'` → the target's next request returns `401`;
   its login is refused with the generic message.
8. **Admin-notice acknowledgment**
   Seeded admin: `/me` shows `admin_notice:true`; `PATCH /me -d '{"acknowledge_admin_notice":
   true}'` → `/me` now `false`, never shown again.

## Integration tests

- `make test` (sets `ARBORETTE_INTEGRATION=1`, `-p 1`, localhost overrides). New coverage
  is **per-row by design**: each test registers its own uniquely-named user and asserts that
  user's registration (member under a populated DB), login, self-service change, and admin
  visibility — never `COUNT(users)` or a global first-user claim (AGENTS.md). The seed rule
  and last-admin guard are deterministic in the **unit** suite (fake `userStore` with
  `CountActiveAdmins()` 0/≥1), which is where the table-global logic is asserted.
- `go test -race ./internal/orchestrator` covers the session middleware, register/login
  generic-401 path, and self/admin handlers; fakes return `ctx.Err()` on a done context
  (AGENTS.md) so a handler passing the wrong context fails loudly.
- `go vet ./...` and `go build ./...` stay green without infra; `go mod tidy` is a
  **no-op** (stdlib PBKDF2, no new dependency — verify `go doc crypto/pbkdf2` under the
  pinned Go 1.24 toolchain).

## Frontend validation (web/)

- `make web-dev` (dev server on :3000 against :8080), or the standalone :8083 after rebuild.
- **Signed-out redirect** — visiting `/` with no `arborette_session` lands on `/login`;
  the sign-in + registration fill one screen. Registering signs in and lands on the
  inventory immediately (SC-001).
- **Corner chip (US2)** — after sign-in, every page shows the avatar chip with the first
  letter of the username. Clicking it opens the menu: username + role, **Settings**,
  **Sign out**; an admin also sees **Users**. **Sign out** returns to `/login` and the
  page redirects apply again.
- **First-sign-in admin notice (US1.7, SC-009)** — the seeded admin's first sign-in shows
  the one-line dismissible notice under the chip ("You're the first user; this account is
  admin — you can grant admin access to others"). Choosing **Users** reaches the account
  list; dismissing hides the notice for good.
- **Settings (US3)** — change username (chip updates immediately) and password
  (current required). The new credentials sign in, the old ones don't.
- **Admin Users (US4)** — list, create, one-click promote/demote, reset password,
  deactivate; a member never sees the entry and a direct `/users` visit gets the server's
  403 (entry points hidden, boundary enforced).
- **Header cleanup (US5, SC-008)** — the header shows brand mark, **Datasets**, and the
  chip only; no **Objectives** or **Heuristics** links on any page. Datasets reaches the
  inventory; a dataset's objectives and the goal/h-details remain reachable through it —
  no dead routes, `/goals`-style URLs handled gracefully per the spec assumption.
- **Unit suites** — `npm test` from `web/` (vitest): the BFF proxy cookie round-trip
  (`Set-Cookie` relayed, `Cookie` forwarded), the typed client's new auth DTOs/helpers, and
  the `lib/session` role-gating/cookie-name logic. No infrastructure needed.

## Expected outcomes (mapping to success criteria)

- SC-001/SC-009: sign-up to signed-in admin in well under 2 minutes and at most 2 screens,
  with the one-line dismissible notice on first sign-in (fresh stack).
- SC-002: generic-401 login failures are identical for wrong-password and unknown-username.
- SC-003: signed-out visitors are redirected to /login on every app address; signed-in
  users pass through.
- SC-004: the chip shows the username's first letter and exposes Settings + Sign out on
  every authenticated page.
- SC-005: username/password changes take effect immediately.
- SC-006: an admin password reset lets the target sign in immediately.
- SC-007: the last-admin demote/deactivate is refused 100% of the time (unit- and
  handler-asserted).
- SC-008: Objectives/Heuristics links appear 0 times in the header; every remaining link
  works.