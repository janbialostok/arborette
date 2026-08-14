# Research Notes — Authentication Required for All Views

Resolves the unknowns in `plan.md` Technical Context. Grounded in the existing codebase:
`web/middleware.ts` (the presence-only gate), `web/components/AppShell.tsx` / `AppNav.tsx` /
`UserMenu.tsx` (the shared chrome that mounts and 401s), `web/lib/orchestrator.ts` (the typed
client whose `requestJSON`/`request` throw `OrchestratorError`), `web/lib/proxy.ts` (the BFF
that relays the orchestrator's 401 verbatim), `web/lib/session.ts` (cookie name + `isSignedIn`
predicates), `web/app/login/page.tsx` (the sign-in surface), and feature 003's session
architecture (orchestrator-owned sessions, `/me` as the identity bootstrap).

## R1 — Why the leak exists: the web-origin gate is presence-based, not validity-based

**Decision**: The bug is the middleware's cookie-presence check plus the shell rendering
before any authorization verdict exists. `web/middleware.ts` redirects to `/login` only when
the `arborette_session` cookie is **absent**; a cookie that is present but stale, forged,
expired, or revoked passes the middleware. The `AppShell` then mounts its full chrome
(`AppNav` → `UserMenu`), `UserMenu`'s `getMe()` gets the orchestrator's 401, and the failure
surfaces as the corner-chip `Callout tone="error"` ("Unauthorized") while the shell and empty
data frames stay on screen. The orchestrator's 401 is authoritative (the middleware's own
comment says so), but the web origin never consults it before rendering.

**Rationale**: Tracing the current flow confirms it: middleware matcher
`/((?!login|\\_next|api|.*\\..*).*)` passes `/datasets`, `/goals`, `/` etc. when a cookie
exists; `AppShell` renders `AppNav` unconditionally for non-login paths; `UserMenu` calls
`getMe()` in a `useEffect` and on `OrchestratorError` sets `error`, which renders the
"Unauthorized" flash. So an unauthenticated visitor with any stale/forged cookie gets the
page frame + flash. Even a visitor with **no** cookie can briefly see the shell between the
middleware check and the redirect in dev, and any route the matcher fails to cover (or a
stale cookie the matcher's dot-exclusion passes through on asset-like paths) is browsable.

**Alternatives considered**:
- *Server-side session validation in the middleware (call `/me` upstream per request)* —
  rejected: the middleware is an edge/UX fast-path, the orchestrator's 401 is already
  authoritative, and a network call in middleware per page load duplicates the BFF and adds
  latency; the design instead makes the shell gate consult the same `/me` the app already
  calls, client-side, before mounting chrome.
- *Per-page guards on every route* — rejected: multiplies the same logic across `/datasets`,
  `/goals`, `/heuristics`, `/settings`, `/users`, and detail views; the shell is the single
  mount point, so one gate covers all routes including future and deep-linked ones.
- *Client-side `getMe()` in each page component* — rejected: same duplication; a mid-session
  401 from a data call also needs routing, which the data-client rule (R3) centralizes.

## R2 — Where the authoritative gate lives: the shell-level session gate

**Decision**: `AppShell` becomes the single session gate. On any non-`/login` route it
resolves `getMe()` before mounting console chrome:
- `getMe()` resolves with an active user → render `AppNav` + children exactly as today
  (FR-010: no behavior change for signed-in analysts).
- `getMe()` rejects with `OrchestratorError` status **401** (cookie gone, session dead,
  account deactivated) → `window.location.assign(loginURL(pathname))`, and render nothing
  console-like in the meantime (no nav, no flash, no empty frame) (FR-001/FR-002/FR-003).
- `getMe()` rejects with anything else (transport failure, 5xx) → a minimal retry/error
  surface, **never** a redirect to login (FR-010: no false sign-outs; an orchestrator outage
  must not log the analyst out).

**Rationale**: `AppShell` is the root layout's only shared chrome for every non-login route,
so the gate there covers all views, all deep links, and all future routes in one place. The
login page is already special-cased (bare children, no chrome), so the gate never interferes
with the sign-in surface (FR-007). The `pathname` is available via `usePathname()`, which the
shell already imports. The 401-vs-transport discrimination is exactly what
`OrchestratorError.status` gives us — no new error taxonomy.

**Alternatives considered**:
- *Keep gating in `UserMenu` only* — rejected: that is precisely the current broken behavior
  (the flash), and it still lets the nav/footer and page content render.
- *A route group `(auth)`/`(app)` layout* — rejected: restructures the whole `app/` tree for
  a change one component achieves; the existing tree is flat and the shell is the right seam.

## R3 — Centralized 401 → login routing in the data client

**Decision**: `requestJSON`/`request` in `web/lib/orchestrator.ts` gain a registered
auth-redirect handler: on `OrchestratorError` with status **401** for a session-secured call,
invoke the registered handler (which navigates to `loginURL(currentPath)`) and throw a
sentinel so no view renders an error flash. The registration is injectable
(`registerAuthRedirect(handler)`, cleared in tests) because vitest runs in Node without
`window.location` — the navigation is a seam, not a global side effect.

**Rationale**: This is what removes the "Unauthorized" flash at the source: every analyst
surface funnels its reads through `requestJSON`/`request`, so a mid-session 401 (session
expired, account deactivated, cookie cleared in another tab) routes to login on the **next
data action** (FR-005) instead of decorating the page. The shell gate (R2) handles the
initial-load case; this handles the mid-session case the gate cannot see (a 401 that arrives
after the shell already mounted). It also keeps the BFF contract intact: `/api/*` still
returns machine-readable 401s (FR-006) — the client *acts* on them, it never changes the
wire.

**Alternatives considered**:
- *Redirect inside every view's `catch`* — rejected: duplicates the rule across a dozen
  components and is exactly the kind of non-trivial logic the project rule says belongs in
  `web/lib/` for vitest.
- *Module-level `window.location.assign` directly in `requestJSON`* — rejected: untestable in
  Node; the registered-handler seam keeps it unit-testable and lets the shell own the
  navigation.

## R4 — Deep-link recovery: the `?next=` contract

**Decision**: Every redirect to `/login` carries `?next=<encoded original path>`:
`loginURL(next)` builds it; `safeNextPath(raw)` validates on the login page before use.
`safeNextPath` accepts only same-origin **relative** paths: it must start with `/`, must not
start with `//`, and must not contain a scheme (`:` before any `/`), blocking open-redirect.
After a successful sign-in/registration the login page navigates to the validated `next`
(when present and safe), else `/` (FR-008).

**Rationale**: The spec requires landing the analyst back where they were going (FR-008) —
both for the middleware fast-path and for the shell gate. A cookie absence/expiry should not
strand a bookmarked `/datasets/<id>` deep link on the console home. The safety rules are the
standard open-redirect guard (no absolute URLs, no `//` protocol-relative, no scheme), kept
as pure functions in `web/lib/session.ts` so vitest owns them (project rule).

**Alternatives considered**:
- *Stash the destination in a cookie/sessionStorage* — rejected: a URL query parameter is
  stateless, survives the middleware redirect, and needs no storage lifecycle at the web
  layer.
- *Always land on `/` after login* — rejected: explicitly fails FR-008 and the US4 deep-link
  story.

## R5 — The login/register 401 exemption

**Decision**: `login` and `registerUser` never trigger the auth-redirect handler; their 401
(the orchestrator's single generic bad-credentials verdict) keeps surfacing as the inline
error on the login page, exactly as today. The redirect-on-401 behavior is opt-in at the
`requestJSON`/`request` call site for session-secured calls (or, equivalently, the two auth
calls are the explicit opt-outs).

**Rationale**: A 401 from `/login` means "wrong credentials," not "go sign in" — routing that
to the login page would loop the user in place and destroy the error message. Feature 003
deliberately made login failures one generic 401; that contract must not change (AGENTS.md
and the 003 contract both hold it). Distinguishing by which endpoint answered keeps the rule
simple and the login UX intact.

**Alternatives considered**:
- *Classify 401 by request path inside `requestJSON`* — rejected: implicit string matching;
  an explicit per-call flag at the two auth call sites is readable and testable.
- *Check `isSignedIn` before calling login* — rejected: races and does not fix the surfaced
  401; the endpoint distinction is the correct signal.

## R6 — Testing the gate without component tests

**Decision**: The gate's non-trivial rules are pure functions in `web/lib/` and covered by
vitest: `safeNextPath` / `loginURL` in `session.test.ts`; the 401-redirect behavior of
`requestJSON`/`request` in `orchestrator.test.ts` using `registerAuthRedirect` with a spy
handler — asserting a session-secured call 401s → handler invoked, login/register 401 → no
invocation, and a transport error (no `status`) → no invocation. The `AppShell` gate is a
thin component over `getMe()` + these helpers, so its branches are the three `getMe()`
outcomes, each already unit-covered at the helper layer. Browser-level flows (stale cookie,
deep link, back/forward after sign-out) go in `quickstart.md` against the live stack
(`make up` + `make web-dev`).

**Rationale**: The project writes no component tests and keeps every non-trivial rule in
`web/lib/` (AGENTS.md). The three branch outcomes of the gate are exactly the three outcomes
of `getMe()` (active / 401 / transport error), so unit coverage of the helpers plus the
documented shell contract gives the gate its guarantees without a React test renderer.
`make test` is untouched — no Go, no integration rows, no shared-table assertions.

**Alternatives considered**:
- *Add `@testing-library/react` component tests for the shell* — rejected: violates the
  project's no-component-tests rule and adds a dev dependency for logic that belongs in
  `web/lib/`.
- *E2E-only verification* — rejected: the 401-classification and `?next=` safety rules are
  cheap and valuable to hold in the unit suite.
