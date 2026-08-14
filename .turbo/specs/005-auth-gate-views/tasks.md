# Tasks: Authentication Required for All Views

**Input**: Design documents from `.turbo/specs/005-auth-gate-views/`

**Prerequisites**: plan.md (required), spec.md (user stories), research.md (R1–R6), data-model.md, contracts/web-auth-gate.md, quickstart.md (validation scenarios)

**Tests**: TDD is the project norm for the web surfaces (AGENTS.md: vitest suites under `web/lib/`, no component tests). Each user story carries its unit test where the rule is non-trivial; browser-level flows live in `quickstart.md`.

**Organization**: Tasks are grouped by user story so each story can be implemented and tested independently.

## Path Conventions

Web app per plan.md structure decision — the entire change is frontend-only, laid on the
existing `web/` deployable:

```text
web/
├── middleware.ts                        # presence fast-path, gains ?next=
├── components/AppShell.tsx              # shell-level session gate
├── components/UserMenu.tsx              # no longer flashes a signed-out 401
├── app/login/page.tsx                   # ?next= deep-link recovery
├── lib/session.ts                       # + safeNextPath, loginURL
└── lib/orchestrator.ts                  # + registerAuthRedirect, 401 -> login
```

No Go, no schema, no migrations, no new packages.

---

## Phase 1: Setup (Shared Infrastructure)

**Purpose**: Baseline the existing web suite so the change's test deltas are clean.

- [x] T001 Run `npm test` from `web/` to confirm the current vitest suite is green before any change (186 passed before any change)
- [x] T002 Reproduce the bug with `make up` + `make web-dev`: with no `arborette_session` cookie (private window) open `/datasets` and `/goals`, observe the shell + corner-chip "Unauthorized" flash and empty frames, and note the repro in the feature plan's records (code-level repro: middleware passes any cookie present, so with a stale/forged cookie AppShell mounts AppNav→UserMenu whose getMe() 401s and renders the Callout flash while views render empty frames; browser observation deferred to T021 live-stack pass)

---

## Phase 2: Foundational (Blocking Prerequisites)

**Purpose**: The pure helpers and the redirect mechanism every story builds on. Must be complete before any user story.

**⚠️ CRITICAL**: No user story work can begin until this phase is complete

- [x] T003 Add pure helpers `safeNextPath(raw: string | null): string | null` and `loginURL(next?: string): string` to `web/lib/session.ts` — `safeNextPath` accepts only same-origin relative paths (starts with `/`, does not start with `//`, contains no scheme; otherwise returns `null`); `loginURL` builds `/login?next=<percent-encoded path+query>` (or `/login` when empty). Reuse the existing `SESSION_COOKIE` module
- [x] T004 Add unit tests for `safeNextPath` / `loginURL` in `web/lib/session.test.ts` — absolute URL (`https://evil.example`), protocol-relative (`//evil.example`), and scheme-full values → `null` → fall back to `/`; `/datasets/<id>` and a path-with-query survive round-trip through `loginURL`
- [x] T005 Add the injectable auth-redirect mechanism to `web/lib/orchestrator.ts`: `registerAuthRedirect(handler)` / clear, an `AuthRedirected` sentinel, and a `redirectOn401` opt-out flag on `requestJSON`/`request` so session-secured calls can trigger the handler on `OrchestratorError` status 401 while `login`/`registerUser` opt out (research R5)
- [x] T006 Add unit tests for the handler lifecycle in `web/lib/orchestrator.test.ts` — `registerAuthRedirect` installs/clears the handler; a session-secured 401 yields the sentinel and the registered handler is invoked exactly once

**Checkpoint**: Foundation ready — helpers, `?next=` building/validation, and the 401-redirect seam all unit-covered. User story implementation can now begin.

---

## Phase 3: User Story 1 - Only the login page for signed-out visitors (Priority: P1) 🎯 MVP

**Goal**: The shell refuses to mount any console chrome until `getMe()` proves a valid active session; on a 401 the visitor is sent to `/login?next=<requested view>` with nothing console-like ever painted.

**Independent Test**: In a private window (no cookie) open every console route — `/`, `/datasets`, `/goals`, `/heuristics`, `/settings`, `/users`, and a dataset deep link — each lands on `/login` with no header, nav, corner chip, or data frame rendered; refreshing and re-navigating never reveal a view.

### Implementation for User Story 1

- [ ] T007 [US1] Add the session gate to `web/components/AppShell.tsx`: on non-`/login` routes resolve `getMe()` before rendering chrome — 200 + active user → render `AppNav` + children unchanged; `OrchestratorError` status 401 → `window.location.assign(loginURL(pathname))` rendering no console chrome; any other failure → a minimal retry surface, never a login redirect (FR-010). Route the redirect through `loginURL` from `web/lib/session.ts`
- [ ] T008 [US1] Update the middleware fast-path in `web/middleware.ts` to redirect a cookie-less request to `loginURL(pathname + search)` instead of bare `/login`, so bookmarked deep links carry through; matcher stays `/((?!login|\\_next|api|.*\\..*).*)`
- [ ] T009 [US1] Adjust `web/components/UserMenu.tsx` so it mounts and calls `getMe()` only after the shell gate has passed (the gate owns sign-out detection); its error surface no longer renders a signed-out 401 flash

**Checkpoint**: US1 fully functional and independently testable — unauthenticated visitors see only the login page.

---

## Phase 4: User Story 2 - No error flashes for signed-out visitors (Priority: P1)

**Goal**: A 401 on any session-secured data call routes to the login page via the registered handler — the "Unauthorized" flash never appears; login/register keep their inline bad-credentials verdict.

**Independent Test**: Signed out, open any console route (stale cookie present or not) — no "Unauthorized" (or equivalent) error callout ever appears in the corner chip or anywhere on the page; the only outcome is the login page. Unit tests in `web/lib/orchestrator.test.ts` hold the rule.

### Tests for User Story 2

> **NOTE: Write these tests FIRST, ensure they FAIL before wiring the redirect**

- [x] T010 [P] [US2] Add unit tests in `web/lib/orchestrator.test.ts`: a session-secured call answering 401 (`getMe`, `listDatasets`) invokes the registered handler and throws `AuthRedirected`; `login`/`registerUser` answering 401 do **not** invoke the handler (inline credentials error preserved); a transport failure with no `status` does **not** invoke the handler (FR-010)

### Implementation for User Story 2

- [x] T011 [US2] Wire the 401→login rule into `requestJSON`/`request` in `web/lib/orchestrator.ts`: on `OrchestratorError` status 401 for a session-secured call, invoke the registered handler (navigating to `loginURL(location.pathname + location.search)`), throw `AuthRedirected`, and never surface a view error; leave `login`/`registerUser` opted out
- [x] T012 [US2] In `web/components/UserMenu.tsx`, drop the 401-as-Callout path (superseded by the data-client redirect); keep only genuine transport/other-error surfacing so no signed-out flash remains in the shell

**Checkpoint**: US1 and US2 both work independently; the flash is gone at the data layer.

---

## Phase 5: User Story 3 - Expired or revoked sessions behave like signed out (Priority: P1)

**Goal**: A session that dies mid-use (expired, revoked, account deactivated) is treated as signed out: the gate and data client route the next navigation or data action to the login page, never leaving a dead page or partial frame.

**Independent Test**: Sign in, then invalidate the session server-side (delete the `sessions` row or deactivate the account via `/users` as admin); the next navigation or data action lands on `/login` with no error flash, and a reload of any console route shows only the login page.

### Tests for User Story 3

- [x] T013 [P] [US3] Add a unit test in `web/lib/orchestrator.test.ts` covering the deactivation path: an identity read (`getMe`) answering 401 (deactivated account) routes to login through the registered handler, and a `200` `getMe` never triggers the handler (FR-010 false-sign-out guard)

### Implementation for User Story 3

- [x] T014 [US3] In `web/components/AppShell.tsx`, enforce gate-before-children ordering for every reload: `getMe()` (with the data-client 401 behavior from T011) resolves before any chrome or data child mounts, so a dead session paints nothing console-like on refresh; a 401 on a later data action independently routes to login

**Checkpoint**: US1–US3 route every signed-out state (absent, stale, expired, revoked, deactivated) to the login page.

---

## Phase 6: User Story 4 - Sign-in returns the analyst to where they were going (Priority: P2)

**Goal**: A signed-out analyst who opens a bookmarked deep link lands back on that exact view after signing in, instead of the console home.

**Independent Test**: Signed out, open a deep link such as `/datasets/<id>` (the URL becomes `/login?next=%2Fdatasets%2F<id>`); sign in → you land on that dataset's detail view; sign in from `/login` directly (no `next`) → you land on `/`.

### Tests for User Story 4

> **NOTE: Write these tests FIRST, ensure they FAIL before implementing the page wiring**

- [x] T015 [US4] Add a round-trip unit test in `web/lib/session.test.ts`: `loginURL` + `safeNextPath` round-trip preserves a same-origin relative path (`/datasets/<id>`) while an absolute or protocol-relative `next` resolves to the `/` fallback

### Implementation for User Story 4

- [x] T016 [US4] Update `web/app/login/page.tsx` to read `next` via `useSearchParams`, validate it with `safeNextPath`, and after a successful sign-in/registration navigate to the validated path (or `/`); keep both the sign-in and create-account modes on the same post-auth path

**Checkpoint**: US1–US4 together give the full gate + deep-link recovery loop.

---

## Phase 7: User Story 5 - Signed-in analysts use the console unchanged (Priority: P2)

**Goal**: The gate never false-bounces a legitimate user; every view works exactly as before for a signed-in analyst.

**Independent Test**: Sign in and exercise the full console — browse datasets, open a dataset detail, goals, heuristics, settings, and (as admin) users; refresh each view. Everything renders and every action succeeds with no redirect to login, confirmed across the browser flow.

### Tests for User Story 5

- [x] T017 [P] [US5] Add a regression unit test in `web/lib/orchestrator.test.ts`: session-secured calls answering `200` (and any non-401 status) never invoke the registered auth-redirect handler and never throw `AuthRedirected`

### Implementation for User Story 5

- [ ] T018 [US5] Execute the signed-in regression pass from `quickstart.md` section 5 against `make up` + `make web-dev`, verifying no view regresses and no false bounce occurs on navigate or refresh; fix any regression surfaced by the pass
  - HTTP-verifiable portion done (2026-08-14): signed-in session registered through the BFF; `/api/orchestrator/me` and `/api/orchestrator/datasets` answer 200 with the session cookie; `/datasets` renders 200 (no redirect). Browser navigate/refresh click-through of every view (quickstart §5) remains for the manual browser pass.

**Checkpoint**: All five user stories independently functional; the signed-in experience is unchanged.

---

## Phase 8: Polish & Cross-Cutting Concerns

**Purpose**: Hardening and documentation that touch the whole feature.

- [x] T019 Run `npm test` and `npm run lint` from `web/`, resolving any type/lint fallout from the session-helper and data-client changes
- [ ] T020 Update the feature plan (`.turbo/specs/005-auth-gate-views/plan.md`) to `status: done` and record any implementation departures from the plan, per AGENTS.md ("a commit that implements a shell carries its own plan at `status: done`")
- [ ] T021 Run the full `quickstart.md` validation end-to-end — unit suite plus every browser-level scenario (signed-out, stale cookie, deep-link recovery, mid-session expiry, sign-out back-nav, unsigned `/api` curl check) — and confirm each passes
  - Automated portion done (2026-08-14): unit suite green (`npx vitest run` = 206 passing, incl. the `safeNextPath`/`loginURL` and data-client 401-routing rules); `next build` + `tsc` + lint clean; live-stack HTTP checks against `make web-dev` pass — every console route redirects to `/login?next=<encoded path+query>` with no cookie, `/login` stays 200, `/api/*` answers 401 JSON to a signed-out caller (never a redirect), a valid session cookie gets 200 on pages and `/me`, and a forged cookie passes the presence-only middleware (the AppShell validity gate owns that verdict client-side). Remaining browser-only scenarios: stale-cookie client-side redirect, deep-link recovery after sign-in, mid-session expiry, sign-out back-nav.

---

## Dependencies & Execution Order

### Phase Dependencies

- **Setup (Phase 1)**: No dependencies — starts immediately
- **Foundational (Phase 2)**: Depends on Setup — BLOCKS all user stories (US1 → US4 depend on `safeNextPath`/`loginURL` T003–T004; US2/US3/5 depend on the redirect seam T005–T006)
- **User Stories (Phase 3+)**: All depend on Foundational completion
  - US1 (T007–T009) is the MVP; US3's T014 requires US2's T011 (the 401 behavior it leans on); US4's T016 needs T003 (safeNextPath) but can otherwise run parallel
  - US3 and US4 can be built simultaneously once US1/US2 wiring exists
- **Polish (Final Phase)**: Depends on all desired user stories being complete

### User Story Dependencies

- **US1 (P1)**: After Foundational — no story dependencies (MVP)
- **US2 (P1)**: After Foundational — independent; consumes T005/T006
- **US3 (P1)**: After US2's T011 (needs the wired data-client 401 routing)
- **US4 (P2)**: After Foundational — independent; consumes T003/T004
- **US5 (P2)**: After US1–US4 (regression of the whole gate)

### Within Each User Story

- Unit tests (where included) are written first and fail before implementation (T010, T013, T015, T017)
- Helpers/mechanisms before wiring; wiring before verification
- Each story is complete and verified before moving to the next priority

### Parallel Opportunities

- Phase 2 tasks T003/T004 and T005/T006 are [P], but note T004 depends on T003 and T006 on T005 (they sequence within the foundational slice)
- T010, T013, T015, T017 can each run in parallel inside their own story phases (distinct test files or distinct `describe` blocks)
- Once Foundational is done, US1 (T007–T009) and US4 (T015–T016) can start in parallel; US2 (T010–T012) runs beside them; US3 joins after T011
- T018 and T019 can run together after the stories land

---

## Parallel Example: User Story 2

```bash
# Launch the test suite for US2 first (must fail before wiring):
Task: "Add 401-routing unit tests in web/lib/orchestrator.test.ts (T010)"

# After T010 passes into red, implement the redirect (T011), then clean the flash (T012):
Task: "Wire 401->login into requestJSON/request in web/lib/orchestrator.ts (T011)"
Task: "Drop the 401-as-Callout path in web/components/UserMenu.tsx (T012)"
```

---

## Implementation Strategy

### MVP First (User Story 1 Only)

1. Complete Phase 1: Setup (baseline + bug repro)
2. Complete Phase 2: Foundational (`safeNextPath`/`loginURL`, redirect seam + tests) — CRITICAL, blocks all stories
3. Complete Phase 3: US1 (shell gate + middleware `?next=` + UserMenu mount guard)
4. **STOP and VALIDATE**: private-window pass over every console route → all land on `/login`, no chrome/flash
5. Deploy/demo if ready — this alone fixes the reported bug for the cookie-absent and initial-load cases

### Incremental Delivery

1. Setup + Foundational → gate primitives unit-covered
2. US1 → signed-out visitors see only login (MVP) → demo
3. US2 → no flash on any data 401; mid-session sessions route to login
4. US3 → expired/revoked/deactivated uniformly treated as signed out
5. US4 → deep-link recovery after sign-in
6. US5 → signed-in regression confirmed unchanged
7. Polish → lint, doc sync, full quickstart gate

### Parallel Team Strategy

1. Team completes Setup + Foundational together
2. Developer A: US1; Developer B: US2; Developer C: US4 (all after Foundational)
3. US3 lands once T011 is in (Developer A or B freely)
4. US5 and Polish come last, integrating the whole gate

---

## Notes

- [P] tasks = different files, no dependencies (or dependency-sequenced within their slice)
- [Story] label maps each task to its user story for traceability
- The whole feature is frontend-only: no Go, no migrations, no new packages — `make test` and the integration suite are untouched
- Verify the new tests fail before implementing (T010/T013/T015/T017)
- Stop at any checkpoint to validate the story independently via `quickstart.md`
- After the final phase, the plan file carries `status: done` per AGENTS.md