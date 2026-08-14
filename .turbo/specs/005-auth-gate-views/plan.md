# Implementation Plan: Authentication Required for All Views

**Status: done**

**Branch**: `fix_authorization` | **Date**: 2026-08-14 | **Spec**: [spec.md](spec.md)

**Input**: Feature specification from `.turbo/specs/005-auth-gate-views/spec.md`

## Summary

An unauthenticated visitor can currently reach the console's analyst views (`/datasets`,
`/goals`, `/heuristics`, `/settings`, `/users`, the root inventory, and deep links): the
page frame renders with its header, navigation, and corner chip, the data areas come up
empty, and the corner chip flashes the orchestrator's `unauthorized` 401 as an error
callout. The fix makes the web origin refuse to render anything but the login page for a
visitor without a valid session.

Root cause: the web-origin gate is **presence-based, not validity-based**. `web/middleware.ts`
redirects only when the `arborette_session` cookie is *absent*; a stale, forged, expired, or
revoked cookie passes the middleware, and the client shell (`AppShell`/`UserMenu`) mounts and
calls `getMe()` before any authorization verdict exists — the 401 then surfaces as the
top-right "Unauthorized" flash while the shell and empty data frames stay on screen. The
orchestrator's 401 is and stays the authoritative boundary; the web origin just never
validates against it before rendering.

The fix is web-origin-only (no Go, no schema): a **shell-level session gate** that resolves
`getMe()` before mounting any console chrome and redirects to `/login?next=<requested view>`
on a 401, plus a **centralized 401 → login routing** in the data client so a session that
dies mid-use bounces to login instead of decorating the page with an error. Deep-link
recovery (`?next=`, validated to same-origin relative paths only) carries the analyst back
to the view they were trying to reach, including after sign-in. The login page remains the
single unauthenticated surface; signed-in analysts see no behavior change.

## Technical Context

**Language/Version**: TypeScript on Next.js ≥15 (App Router), Node 22. No Go changes —
the orchestrator's session middleware (feature 003) already makes `/me` return `401` for any
missing/invalid/deactivated session and is the authoritative boundary this gate reflects.

**Primary Dependencies**: None new. Uses the existing `web/lib/session.ts` cookie-name
constant and `isSignedIn` predicates, the `web/lib/orchestrator.ts` typed client (`getMe`,
`requestJSON`/`request`, `OrchestratorError`), `web/lib/proxy.ts` (BFF cookie tunneling —
unchanged), `web/middleware.ts` (presence fast-path — gains `?next=`), and vitest. No
`package.json`/`go.mod` movement.

**Storage**: N/A for new data. The session is already orchestrator-owned (Postgres `sessions`
+ `users`, migration `0017`, feature 003); its validity is re-checked server-side per request.
The web origin adds no persistent state — the `?next=` deep-link destination lives in the
URL only. No migration.

**Testing**: `npm test` (vitest) from `web/` — pure gate logic lives in `web/lib/` and is
unit-covered (safe `?next=` validation, login-URL construction, 401-classification in the
data client), per the project rule that every non-trivial rule lives in `web/lib/` (no
component tests). Live-stack validation via `make up` + `make web-dev` per `quickstart.md`.
No Go tests touched; `make test` and `-race` gates unaffected.

**Target Platform**: The web origin — Next.js standalone image on :8083 (compose) and the
dev server on :3000 (`make web-dev`), both pointed at the orchestrator on :8080 through the
BFF. Same deployment shape as feature 003.

**Project Type**: Web application (frontend-only gate; the Go backend is unchanged).

**Performance Goals**: Spec SC-001 — an unauthenticated visitor is on the login page without
seeing console chrome or data. The gate adds one `getMe()` round trip on first mount per page
load; at single-analyst scale (<10 sessions) this is irrelevant, and it is the same call the
corner chip already makes today.

**Constraints**:
- Only an **authoritative 401** means "signed out"; a transport failure (orchestrator down,
  `502` from the BFF) must NOT log the analyst out (FR-010 — no false sign-outs). The gate
  distinguishes `OrchestratorError` status 401 from transport/other errors.
- A `401` from the sign-in/registration calls (`login`, `registerUser`) is the **expected**
  bad-credentials verdict and must keep surfacing as an inline error on the login page —
  never trigger a redirect (matching the 003 generic-401 contract).
- `/api/*` (the BFF) must keep answering signed-out requests with machine-readable failures,
  never HTML login-page redirects (FR-006); the middleware matcher already excludes `/api`.
- `?next=` must be validated to same-origin relative paths only (must start with `/`, must not
  start with `//`, must not contain a scheme) to prevent open-redirect.
- Every console view is gated identically: `/`, `/datasets`, `/goals`, `/heuristics`,
  `/settings`, `/users`, and detail/deep links — no per-route whitelist.

**Scale/Scope**: Single shared workspace, a handful of accounts. Pure web-origin change over
the existing shell, data client, and middleware; no migrations, no Go, no new services, no
worker/service changes. Removes the visible symptom (corner-chip "Unauthorized" flash and
empty frames) by refusing to render any console chrome without a validated session.

## Constitution Check

*GATE: Must pass before Phase 0 research. Re-checked after Phase 1 design.*

The project `.specify/memory/constitution.md` is the unfilled repository template (no custom
principles ratified). The governing conventions come from `AGENTS.md`; all gates pass:

- **Toolchain/dependency discipline** — Go 1.24 pin untouched (no Go edits); web dependency
  set untouched (Next ≥15, Node 22 already pinned; **zero new npm packages**). The gate is
  built from `web/lib/session.ts`, `web/lib/orchestrator.ts`, and the existing shell.
- **Testing discipline** — non-trivial rules (safe `?next=` validation, login-URL building,
  `401`-vs-transport classification) live in `web/lib/` for vitest, matching the "no
  component tests" project rule. No Go tests, no integration-suite rows, no shared-table
  assertions — the suite's shared-DB and cleanup gates are untouched. The live-stack
  quickstart covers the browser-level flows (stale cookie, deep link, sign-out back-nav).
- **Web conventions** — the BFF stays the only browser reach into the orchestrator and keeps
  returning machine-readable authorization failures (never HTML redirects for `/api`); the
  middleware stays a UX fast-path while the orchestrator's 401 remains authoritative — this
  gate is the web origin *reflecting* that verdict before rendering, not a second security
  boundary.
- **No unwarranted complexity** — one shell-level gate (not a per-page guard), one shared
  `?next=` carry-through (middleware → login page), one 401-classification rule in the data
  client; no route whitelist, no session storage at the web layer, no server-side session
  state.

No violations to justify; `Complexity Tracking` below is intentionally empty.

**Post-design re-check (after Phase 1)**: the design artifacts (`research.md`,
`data-model.md`, `contracts/web-auth-gate.md`, `quickstart.md`) preserve every gate. No new
dependencies surface (the gate is built from existing `web/lib` pieces); `?next=` validation
and the 401-classification rule land in `web/lib/` for vitest, keeping the no-component-tests
rule; the data-layer `?next=` carry-through and the `getMe()`-before-chrome shell gate are
the only mechanisms added, both justified in research R2–R5 and beneath the plan; no schema,
Go, or wire changes. Still no violations to justify.

## Project Structure

### Documentation (this feature)

```text
.turbo/specs/005-auth-gate-views/
├── plan.md              # this file
├── research.md          # Phase 0 — root cause, gate placement, ?next contract, 401 taxonomy
├── data-model.md        # Phase 1 — no new schema; existing Session + client gate state
├── quickstart.md        # Phase 1 — end-to-end validation guide
├── contracts/
│   └── web-auth-gate.md # Phase 1 — web-origin gate, 401 handling, deep-link contract
└── tasks.md             # created by /speckit.tasks (next phase)
```

### Source Code (repository root)

```text
web/
├── middleware.ts                        # + ?next=<path> on the /login redirect (fast path);
│                                        #   matcher unchanged (excludes /login, /_next,
│                                        #   /api, static) — presence gate, not authority
├── components/AppShell.tsx              # shell-level session gate: resolve getMe() before
│                                        #   mounting chrome on non-/login routes; 401 →
│                                        #   redirect /login?next=<path>; transport error →
│                                        #   retry surface (never a logout); signed-in →
│                                        #   render children unchanged; /login stays bare
├── components/UserMenu.tsx              # no longer the place a signed-out 401 flashes
│                                        #   (it only mounts after the shell gate passes);
│                                        #   its transport-error surface stays
├── app/login/page.tsx                   # reads & validates ?next= (useSearchParams +
│                                        #   safeNextPath), navigates there after a
│                                        #   successful sign-in/registration, else "/"
├── lib/session.ts                       # + safeNextPath(raw) and loginURL(next?) pure
│                                        #   helpers (same-origin relative-only ?next);
│                                        #   isSignedIn/isAdmin unchanged (vitest-covered)
├── lib/orchestrator.ts                  # + 401 handling in requestJSON/request: session-
│                                        #   secured calls invoke the registered auth-
│                                        #   redirect handler and throw a sentinel (no
│                                        #   flash); login/registerUser opt out so their
│                                        #   401 surfaces inline on the login page;
│                                        #   + registerAuthRedirect(handler) / clear it in
│                                        #   tests; OrchestratorError.status drives the
│                                        #   401-vs-transport decision
└── lib/
    ├── session.test.ts                  # + safeNextPath / loginURL unit coverage
    └── orchestrator.test.ts             # + 401-redirect on session-secured calls,
                                         #   no-redirect on login/register, transport
                                         #   failures never redirect (registerAuthRedirect)
```

**Structure Decision**: The repo is one Go module plus the `web/` deployable; this feature
lives entirely in the frontend, layered onto the existing pieces rather than adding any. The
shell (`AppShell`) is already the single shared chrome mount point for every non-login route,
so it is the one place a gate covers all views including deep links — no per-page guards. The
data client (`web/lib/orchestrator.ts`) already funnels every session-secured read through
`requestJSON`/`request`, so the 401→login rule centralizes there. `middleware.ts` keeps its
presence fast-path (it must stay cheap and never gate `/api`); the shell gate supplies the
validity check the middleware deliberately doesn't. No re-architecture, no backend change.

## Implementation Record (post-implementation, per AGENTS.md)

Shipped as planned; all five user stories landed. Status set to `done`. The following are
records rather than scope changes — the plan's mechanism, layering, and no-Go/no-schema
constraints held exactly:

- **Log-in page Suspense boundary (structural, not behavioral)**: `app/login/page.tsx` reads
  `?next=` via `useSearchParams`. Because the page is prerendered statically (Next ≥15), the
  form moved into a `LoginForm` wrapped in a `<Suspense>` boundary — the standard escape for
  the "`useSearchParams()` must be wrapped in a suspense boundary" prerender rule. Behavior is
  unchanged: validated `next` wins, everything else lands on `/`.
- **AppShell installs the registered handler, not the plan's implied "shell wires it"**: a
  mount effect calls `registerAuthRedirect(() => window.location.assign(loginURL(location.pathname + location.search)))`
  and clears it on unmount; the data client's `requestJSON`/`request` invoke it on a
  session-secured 401 and throw `AuthRedirected`, with `login`/`registerUser` opted out via
  `redirectOn401: false`. This is the contract §3 behavior the plan described.
- **Task consolidation (execution, no design change)**: T009's single UserMenu edit already
  dropped the 401-as-Callout path T012 called for; T013's 401-deactivation half was already
  pinned by T010's `getMe`-401 test (only the 200-never-triggers guard was added); T014 was
  satisfied by T007's gate ordering (getMe resolves before any chrome mounts; "checking"
  renders null); T015's round-trip test already existed from T004. Overlapping checkboxes were
  marked together rather than duplicated.
- **Middleware stays presence-only and `/api` stays machine-readable**: zero change to the
  security boundary; the 401 verdict remains the orchestrator's, reflected client-side.

## Complexity Tracking

> No `AGENTS.md` / constitution violations justified — no new projects, no added repository
> pattern, no schema re-architecture. The only mechanism that deserves a note is the
> **registered auth-redirect handler** in the data client: a module-level callback the shell
> installs so `requestJSON` can route a mid-session 401 to login without every view owning the
> redirect. It exists because vitest runs in Node (no `window.location`), so the navigation
> must be injectable to be testable — and because login/register need a way to opt out of the
> redirect while every other call inherits it. The table is therefore intentionally empty.
