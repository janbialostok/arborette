# Tasks: User Accounts, Sign-In & Profile

**Input**: Design documents from `.turbo/specs/003-user-management/`

**Prerequisites**: plan.md, spec.md, research.md (R1–R10), data-model.md, contracts/rest-api.md, quickstart.md

**Tests**: This repo mandates unit + integration coverage (AGENTS.md: `make test` gates, per-row assertions, fakes returning `ctx.Err()`, `-race` on the orchestrator; `npm test` for `web/lib`). Test tasks are therefore part of each phase rather than optional.

**Organization**: Tasks are grouped by user story. Phases 1–2 are shared infra; Phases 3–7 map one-to-one to spec user stories US1–US5; Phase 8 is polish. IDs ascend in execution order (tests first within a story, per the checklist format). **Do not commit to git until told.**

## Format: `[ID] [P?] [Story] Description`

- **[P]**: can run in parallel (different files, no dependencies)
- **[Story]**: US1–US5 label (Setup/Foundational/Polish carry none)
- Descriptions carry exact file paths.

---

## Phase 1: Setup (Shared Infrastructure)

**Purpose**: Confirm the toolchain ground rules and add the (only) new config knobs.

- [x] T001 Verify the pinned Go 1.24 toolchain ships stdlib `crypto/pbkdf2` (`go doc crypto/pbkdf2` under `GOTOOLCHAIN`; already confirmed present on the local toolchain) and that adding the hashing helper below stays a **no-op `go mod tidy`** (no `go.mod` change) — repo root
- [x] T002 [P] Add session config knobs to `internal/config/config.go` `OrchestratorConfig`: `SessionCookieName` (`ARBORETTE_SESSION_COOKIE`, default `"arborette_session"`) and `SessionSecure` (`ARBORETTE_SESSION_SECURE`, default `false`) using the existing `env()`/`boolEnv()` pattern, plus the standard knob unit-check in `internal/config/` mirroring existing knob tests (`t.Setenv` to clear, per AGENTS.md)

---

## Phase 2: Foundational (Blocking Prerequisites)

**Purpose**: Accounts/sessions storage, password hashing, session middleware + identity wiring, and the web session tunnel/redirect. **No user story can function before this phase.**

**⚠️ CRITICAL**: No user story work begins until this phase is complete.

- [x] T003 Write migration `internal/store/migrations/0017_users_sessions.up.sql`: `users` (id uuid PK, username text, password_hash text, role text CHECK in ('admin','member'), active boolean default true, admin_notice_acknowledged boolean default false, created_at/updated_at) with a unique `lower(username)` function index; `sessions` (id uuid PK, user_id uuid FK→users ON DELETE CASCADE, token_hash text UNIQUE, created_at); grants per data-model.md (`SELECT, INSERT, UPDATE, DELETE ON users` and `SELECT, INSERT, DELETE ON sessions` to `arborette_orchestrator`, no service-role grants)
- [x] T004 Write migration `internal/store/migrations/0017_users_sessions.down.sql` dropping `sessions` then `users` (cascade order; `//go:embed *.sql` picks both up untouched)
- [x] T005 [P] Implement password helpers `internal/store/passwords.go`: `HashPassword`/`VerifyPassword` over stdlib `crypto/pbkdf2` HMAC-SHA256 (600k iterations, 16-byte salt, 32-byte key) with a self-describing `pbkdf2$sha256$<iter>$<saltB64>$<keyB64>` envelope, constant-time compare, and a `dummyHash` for uniform login timing (R5)
- [x] T006 [P] Implement `internal/store/usersessions.go`: `User`/`Session` types per data-model.md; `UserStore` (`Create(ctx, username, passwordHash string, seedAdmin bool)` returning `ErrUsernameConflict` on the lower(username) clash, `GetByUsername`, `GetByID`, `List`, `CountActiveAdmins`, `Update*` field mutations for username/password_hash/role/active/admin_notice_acknowledged) and `SessionStore` (`Create`, `GetByTokenHash`, `DeleteByTokenHash`); the seed decision runs inside a transaction-scoped advisory lock on a fixed sentinel (`pg_advisory_xact_lock`) so concurrent first registrations can't double-seed (R4)
- [x] T007 Implement session-auth middleware `internal/orchestrator/session.go`: read cookie by configured name, sha256 the value, look up the session, load the user, **re-check `active`**, stash the `Analyst` in the request context; missing/invalid/inactive → `401 {"error":"unauthorized"}` with `WWW-Authenticate`; a `Guard{exempt: set{"/register","/login"}}` wraps all analyst-facing routes while `/internal/*` stay Bearer-only (R3)
- [x] T008 Extend `internal/orchestrator/identity.go` with `SessionIdentity`: `Current(ctx)` reads the middleware stash, falling back to a `StubIdentity` for the `/internal/audit` and boot-reconciliation paths (R3/R6) — audit actors become the real user for human actions
- [x] T009 Add `userStore`/`sessionStore` narrow interfaces + cookie config to `internal/orchestrator/server.go` `Server`/`NewServer`, register the public/guarded route surfaces, and wire `store.NewUserSessionStore(pool)` + `SessionIdentity{Fallback: StubIdentity{...}}` in `cmd/orchestrator/main.go` (`ReconcileDatasets` keeps the plain `StubIdentity`)
- [x] T010 [P] Implement the BFF cookie tunnel in `web/lib/proxy.ts`: forward the inbound `Cookie` header upstream and relay the upstream `Set-Cookie` through `passThrough` and `eventStream` across `forward`, `forwardStream`, and `forwardStreamSubmit` (R2 — today only `content-type` is copied)
- [x] T011 [P] Add `web/middleware.ts`: matcher excluding `/login`, `/_next`, `/api`, and static assets; missing `arborette_session` cookie → redirect to `/login` (FR-005; the orchestrator 401, not this, is authoritative)
- [x] T012 Unit-test the password envelope in `internal/store/passwords_test.go`: round-trip hashes/verifies, wrong password rejects, malformed envelope fails closed, dummy hash always verifies false
- [x] T013 Add storage integration coverage `internal/store/usersessions_integration_test.go` (gated on `ARBORETTE_INTEGRATION`): per-row effects only — uniquely-named user registers as **member** under a populated DB (never a table-global count), `ErrUsernameConflict` on a duplicate lower(username), session create/lookup/delete keyed on the token hash

**Checkpoint**: Sessions are durable, password hashing works, every analyst route is guarded, and the web BFF can carry the session both ways. User stories may begin.

---

## Phase 3: User Story 1 - Create an account and sign in (Priority: P1) 🎯 MVP

**Goal**: Username + password sign-up and sign-in with no email, the seeded-first-admin rule, the generic login failure, and a sign-out that ends the session.

**Independent Test**: On a fresh stack, register an account and confirm the first one is `admin`; sign out; sign back in with the same credentials; a wrong password fails with the single generic message. In the web, `/login` registers and lands on the inventory. (Spec US1, FR-001–FR-005.)

### Tests for User Story 1

- [x] T014 [US1] Unit-test the register/login/logout handlers in `internal/orchestrator/accounts_test.go`: fake `userStore` with `CountActiveAdmins()` 0→seeded admin + `admin_notice`, ≥1→member; duplicate username → 409; short password → 400; wrong-password login → identical 401 message; logout deletes the session and clears the cookie; the fake returns `ctx.Err()` when the context is done (AGENTS.md)
- [x] T015 [US1] [P] Add BFF cookie-round-trip vitest in `web/lib/proxy.test.ts`: a canned upstream `Set-Cookie` is relayed to the browser and a request carrying a `Cookie` header forwards it upstream (R2)

### Implementation for User Story 1

- [x] T016 [US1] Implement `handleRegister` in `internal/orchestrator/accounts.go`: validate username (3–32, letters/digits `_`/`-`) and password (≥ 8), seed-decision via `CountActiveAdmins` then `store.Create(seedAdmin)`, mint a session token + cookie (`HttpOnly`, `SameSite=Lax`, `/`), `201` `MeDto`, audit `user_register` (`user_id`, `username`, `role`)
- [x] T017 [US1] Implement `handleLogin` in `internal/orchestrator/accounts.go`: PBKDF2 verify with the uniform dummy-compare path for unknown/deactivated users; `200` `MeDto` + `Set-Cookie`, else single generic `401 {"error":"invalid username or password"}`
- [x] T018 [US1] Implement `handleLogout` in `internal/orchestrator/accounts.go`: delete the session row by token hash and clear the cookie (`204`); login/sign-out do **not** audit (R6)
- [x] T019 [US1] Register the new routes in `internal/orchestrator/server.go` `Routes()`: `POST /register` and `POST /login` public, `POST /logout` behind the guard; add the `toUserDTO`/`toMeDTO` mappers in `accounts.go`
- [x] T020 [US1] [P] Add the BFF route handlers `web/app/api/orchestrator/register/route.ts`, `login/route.ts`, `logout/route.ts` (thin `forward*`, `force-dynamic`, nodejs runtime — mirror `datasets/route.ts`)
- [x] T021 [US1] [P] Add `AccountDto`/`MeDto` types and `registerUser`, `login`, `logout` helpers to `web/lib/orchestrator.ts` (same-origin cookies flow automatically; `OrchestratorError` verbatim `{error}`)
- [x] T022 [US1] Build `web/app/login/page.tsx`: sign-in and registration as one screen; on success `router.push("/")`; surface the returned `{error}` verbatim

**Checkpoint**: Register→admin-seed→sign-out→sign-in round trip works end-to-end through the cookie-tunnelled BFF (fresh stack). MVP is demonstrable.

---

## Phase 4: User Story 2 - See who I am and sign out from the corner chip (Priority: P1)

**Goal**: The Google-Docs-style corner chip shows the username's first letter; its menu exposes Settings, a dismissible first-sign-in admin-grant notice, and Sign out.

**Independent Test**: Sign in and, on any page, the top-right chip shows the first letter; the menu lists username/role, Settings, (admin) Users, Sign out; choosing Sign out returns to `/login` (Spec US2, FR-006–FR-008; depends on US1 for the session). The one-line admin notice shows once and stays dismissed.

### Tests for User Story 2

- [x] T023 [US2] Unit-test the middleware + `/me` in `internal/orchestrator/session_test.go`/`accounts_test.go`: cookie replay via `httptest` `Set-Cookie`, invalid token → 401, **deactivated user → 401 on next request** (R3), `GET /me` returns `MeDto` with `admin_notice` true for an unacknowledged seeded admin and false after `PATCH /me` ack
- [x] T024 [US2] [P] Add vitest for `web/lib/session.ts` (cookie-name constant, `isSignedIn`, admin view gating) in `web/lib/session.test.ts`

### Implementation for User Story 2

- [x] T025 [US2] Implement `GET /me` and the minimal `PATCH /me` (`acknowledge_admin_notice: true` only) in `internal/orchestrator/accounts.go` — the identity bootstrap every signed-in page and the chip use
- [x] T026 [US2] [P] Add the BFF route handler `web/app/api/orchestrator/me/route.ts` (GET + PATCH through `forward`)
- [x] T027 [US2] [P] Add `getMe`/`updateMe` typing and the `admin_notice` field to `web/lib/orchestrator.ts`, plus `web/lib/session.ts` pure helpers (cookie name default matching `ARBORETTE_SESSION_COOKIE`, `isSignedIn`, admin view gating) used by the menu and views
- [x] T028 [US2] Build `web/components/UserMenu.tsx`: avatar chip of the username's first letter; dropdown with username + role, **Settings**, admin-only **Users**, **Sign out**, and the one-line dismissible admin-grant notice (dismiss → `updateMe({acknowledge_admin_notice: true})`) surfaced while `admin_notice` is true
- [x] T029 [US2] Render `<UserMenu />` on the right edge of `web/components/AppNav.tsx` **keeping the existing LINKS** for this story (the removals happen in US5)

**Checkpoint**: With the cookie set, every page shows the chip, the admin notice is dismissible once, and sign-out returns to `/login`.

---

## Phase 5: User Story 3 - Manage my username and password (Priority: P2)

**Goal**: Self-service account management from the chip's Settings — rename the username and change the password (current password required).

**Independent Test**: Open Settings from the chip: change the username → chip updates immediately and only the new username signs in; change the password with the correct current password → the old password stops working; a wrong current password is refused (Spec US3, FR-009–FR-010; depends on US1 session + US2 Settings entry).

### Tests for User Story 3

- [x] T030 [US3] Unit-test self-service changes in `internal/orchestrator/accounts_test.go`: username rename reflected on next `/me`, username conflict → 409, password change with wrong `current_password` → 403, correct → old password rejected at login, audits `user_username_change`/`user_password_change` written

### Implementation for User Story 3

- [x] T031 [US3] Extend `PATCH /me` in `internal/orchestrator/accounts.go` with `username` (validated, unique-checked → 409) and `password` (requires `current_password`, wrong → 403; no session rotation — the session keys on user id)
- [x] T032 [US3] Build `web/app/settings/page.tsx`: username form + password form (current + new), reflecting the returned `{error}` verbatim; the chip reacts on the next `/me`
- [x] T033 [US3] [P] Finalize `updateMe` typing in `web/lib/orchestrator.ts` for all `PATCH /me` fields

**Checkpoint**: A signed-in user changes username/password from Settings with immediate effect and correct refusal cases.

---

## Phase 6: User Story 4 - Administer accounts (Priority: P2)

**Goal**: Admins list accounts, create members, reset forgotten passwords, and delegate/revoke admin — through the account list reached from the chip.

**Independent Test**: As an admin, open Users: the list shows every account; create a member that signs in immediately; reset a password that works at the next login; promote/demote changes access at once; the last-admin demote/deactivate is refused with a 409; a member gets no entry and a direct `/users` request is 403 (Spec US4 / FR-011–FR-014; depends on US1 + US2 for session/role and the Users entry).

### Tests for User Story 4

- [x] T034 [US4] Unit-test the admin surface in `internal/orchestrator/accountadmin_test.go`: member → 403 on every `/users*` route; create produces `role=member`; promote/demote flips `role`; `new_password` reset signs in; **last-active-admin guard refuses demote/deactivate of the final admin (409) and allows it once a second admin exists** (per-row via fake `CountActiveAdmins` — never a global integration count)

### Implementation for User Story 4

- [x] T035 [US4] Implement `handleListUsers` / `handleCreateUser` / `handleUpdateUser` in `internal/orchestrator/accountadmin.go`: `GET /users`, `POST /users` (always member), `PATCH /users/{id}` (`role`/`active`/`new_password`) with the last-admin guard 409 pre-check and audits `user_role_change`/`user_status_change`/`user_password_reset`
- [x] T036 [US4] Register `/users` routes behind the admin role check in `internal/orchestrator/server.go` (role gate server-side; the UI entry is convenience only)
- [x] T037 [US4] [P] Add the BFF route handlers `web/app/api/orchestrator/users/route.ts` (GET/POST) and `users/[id]/route.ts` (PATCH)
- [x] T038 [US4] [P] Add `listUsers`, `createUser`, `updateUser` helpers + `UserAdminDto` to `web/lib/orchestrator.ts`
- [x] T039 [US4] Build `web/app/users/page.tsx`: account list (username, role, active), create form, one-click promote/demote, reset-password, deactivate/reactivate; member visits surface the server 403

**Checkpoint**: The account list is live; delegation, reset, and deactivation work and the last-admin guard holds.

---

## Phase 7: User Story 5 - Simplify the header navigation (Priority: P2)

**Goal**: The header shows only the brand mark, **Datasets**, and the corner chip — no **Objectives** or **Heuristics** links — with no dead routes.

**Independent Test**: On any signed-in page the header has exactly brand + Datasets + chip; `Objectives`/`Heuristics` appear 0 times; Datasets resolves to the inventory and the removed views stay reachable through the dataset flow (Spec US5 / FR-015; depends on US2 for the chip in the header).

### Tests for User Story 5

- [x] T040 [US5] [P] Add vitest in `web/lib/nav.test.ts` for the exported nav/links data: exactly one entry (Datasets), each href maps to a real route — enabling the "no Objectives/Heuristics, no dead frontmatter" assertion without component tests

### Implementation for User Story 5

- [x] T041 [US5] Extract the links list into `web/lib/nav.ts` and reduce `web/components/AppNav.tsx` LINKS to **Datasets** only (drop Objectives/Heuristics), keeping the brand mark → `/` and the `<UserMenu />` right edge
- [x] T042 [US5] Walk `web/app/goals`, `web/app/heuristics`, `web/app/datasets/**` to confirm the removed links' targets remain reachable through existing paths (dataset flow), per the spec assumption — adjust nothing that serves content, only anything that references a dead header entry

**Checkpoint**: Header is brand + Datasets + chip everywhere; no Objective/Heuristic header links and no dead frontmatter links.

---

## Phase 8: Polish & Cross-Cutting Concerns

**Purpose**: Whole-feature verification, gate runs, and hygiene.

- [x] T043 Run the full gate and confirm green with infra-less unit/build coverage: `go build ./...`, `go vet ./...`, `go test ./...` (no infra), `go test -race ./internal/orchestrator`, then `make test` against the live stack (`-p 1`, localhost overrides) and `npm test` from `web/`; confirm `go mod tidy` is a no-op
- [x] T044 Execute `quickstart.md` end-to-end: on a fresh stack verify the seed-admin path and the admin-notice dismissal; on the populated stack verify the member-registration path, cookie-jar curl flows (register/login/logout/me, admin list/create/reset/promote, last-admin 409, deactivate-takes-effect), and the web flows (`/login`, chip menu, Settings, Users, header); record results
- [x] T045 Reconcile the feature record: mark the plan `Status` and confirm `spec.md` needs no "Implementation departures" additions; ensure audit actions and detail keys in `contracts/rest-api.md` match the implementation; verify `.turbo/improvements.md` backlog is untouched by this feature
- [x] T046 [P] Security/UX hardening sweep: `WWW-Authenticate` on every 401, cookie flags (`HttpOnly`/`SameSite=Lax`, `Secure` honored via `ARBORETTE_SESSION_SECURE`), no username-enumeration messages anywhere, admin guard not reachable from any member path, and no leftover `StubIdentity`-as-user code except the documented fallback

---

## Dependencies & Execution Order

### Phase Dependencies

- **Setup (Phase 1)**: none — starts immediately.
- **Foundational (Phase 2)**: depends on Setup; **blocks all user stories**.
- **User Stories (Phase 3–7)**:
  - **US1 (P1)**: starts after Foundational — first story, no story deps.
  - **US2 (P1)**: depends on US1 (needs an established session + `/login` to be signed in).
  - **US3 (P2)**: depends on US2 (Settings entry lives in the chip menu; `PATCH /me` skeleton).
  - **US4 (P2)**: depends on US1 + US2 (session + role from `/me` for the entry gate).
  - **US5 (P2)**: depends on US2 (chip already in `AppNav`; only the LINKS edit remains).
- **Polish (Phase 8)**: depends on Phases 3–7 (full-suite validation).

### Within a Story

Tests first (write and watch them fail where a TDD seam is meaningful), then handlers/endpoints, then web surfaces after their BFF/lib plumbing, tests alongside the code they verify. No [P] task spans the same file.

### Parallel Opportunities

- Phase 1: T001, T002 [P].
- Phase 2: hashing (T005) ‖ store (T006) ‖ middleware (T007) ‖ identity (T008) ‖ web tunnel (T010) ‖ web middleware (T011) — all separate files; T009 (server/main wiring) follows T006/T007/T008; migrations T003→T004 sequential.
- Phase 3: handler tests (T014) ‖ proxy vitest (T015); BFF routes (T020) ‖ typed client (T021).
- Phase 4: `/me` tests (T023) ‖ vitest (T024); BFF `/me` (T026) ‖ lib/session (T027).
- Phase 5: `updateMe` typing (T033) [P] alongside the page (T032) once the handler (T031) exists.
- Phase 6: BFF users routes (T037) ‖ typed client (T038); admin handler tests (T034) after T035.
- Phase 7: nav vitest (T040) [P] with the AppNav edit (T041).
- Phase 8: T046 [P] alongside gate runs once implementation settles.

### Parallel Example: Foundational Phase

```bash
# Launch the file-disjoint Go foundations together:
Task: "Password helpers in internal/store/passwords.go"
Task: "UserStore/SessionStore in internal/store/usersessions.go"
Task: "Session middleware in internal/orchestrator/session.go"
Task: "SessionIdentity in internal/orchestrator/identity.go"

# Launch the web foundations together (separate from Go):
Task: "Cookie tunnel in web/lib/proxy.ts"
Task: "web/middleware.ts"
```

### Parallel Example: User Story 1

```bash
# BFF + typed client are file-disjoint:
Task: "register/login/logout BFF handlers in web/app/api/orchestrator/*"
Task: "typed auth client in web/lib/orchestrator.ts"

# 'web/lib/proxy.test.ts' cookie vitest can run alongside handler unit tests:
Task: "proxy cookie round-trip vitest"
```

---

## Implementation Strategy

### MVP First (User Story 1 Only)

1. Complete Phase 1 (Setup) — 2 tasks.
2. Complete Phase 2 (Foundational) — migrations, hashing, store, middleware, identity, wiring, web tunnel/redirect.
3. Complete Phase 3 (US1) — register/login/logout + `/login` page.
4. **STOP and VALIDATE**: fresh-stack register→admin, sign-out, sign-in, generic-401; web `/login` round trip.
5. Deploy/demo the guarded console with accounts — everything after this is additive.

### Incremental Delivery

1. Setup + Foundational → sessions, hashing, guard, tunnel working.
2. US1 → accounts work (MVP). 3. US2 → identity visible + sign-out in the corner + admin notice.
4. US3 → self-service username/password. 5. US4 → admin delegation and reset.
6. US5 → header cleanup. 7. Polish → gates, quickstart, hardening. Each story lands and validates independently.

### Parallel Team Strategy

Developer A owns the Go foundation + US1/US3; Developer B owns web foundation + US2/US5; Developer C owns US4 (and its BFF/lib) once the Go foundation lands. US1 gates nothing about US4's shape — the admin surface depends only on sessions/roles, which all ship in Foundational.

---

## Notes

- **[P]** tasks touch different files with no unfinished dependencies; never claim [P] on two tasks editing the same file.
- Story labels map to spec user stories (`US1`…`US5`) for traceability (FR numbers in the descriptions reference spec FR-001…FR-015).
- The seeded-admin path is fresh-stack-validated in quickstart, not integration (shared DB rule); the seed rule and last-admin guard are unit-asserted against fakes (research R4/R7).
- Verify the BFF cookie tunnel exists before judging any US1 web flow — a dropped `Set-Cookie` makes login "pass" while the chip never appears.
- Commit only when the user says so; the repo convention commits each task or logical group when committing is authorised.
- After commit: mark the plan/spec `Status` per the repo's `.turbo` conventions and log any implementation departures in `spec.md`'s Clarifications-departures section.