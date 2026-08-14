# Contracts — Web-Origin Authentication Gate

This contract defines how the web origin (`web/`, the Next.js App Router app) enforces the
spec's hard gate: *a visitor must be signed in to see anything other than the login page.*
It sits on top of the orchestrator's session contract (feature 003) and the BFF proxy
contract — neither changes. The orchestrator's 401 remains the authoritative verdict; this
document is the contract for how the web origin **reflects that verdict before rendering**.

## Layering

```
Browser ──→ web origin (Next.js) ──BFF──→ orchestrator (Go, authoritative)
             │  middleware.ts  : presence fast-path (cookie absent → /login?next=)
             │  AppShell gate  : validity check (getMe() → 200/401/transport)
             │  orchestrator.ts: data client (mid-session 401 → login, no flash)
             └  login page     : ?next= deep-link recovery after sign-in
```

## 1. Middleware fast-path (unchanged role, adds `?next=`)

- **Matcher** (unchanged): `/((?!login|\\_next|api|.*\\..*).*)` — excludes the sign-in
  surface, Next internals, the BFF (`/api/*` must keep answering JSON, never a login-page
  redirect), and static-asset-looking paths.
- **Behavior**: when the `arborette_session` cookie is **absent**, redirect to
  `/login?next=<encoded original path+query>`. When the cookie is **present**, pass through
  — the middleware is deliberately presence-only; **validity** is the shell gate's job.
- **Contract**: the middleware never calls the orchestrator and never decides "valid"; it is
  a cheap fast-path and a UX guard, not the security boundary.

## 2. Shell session gate (`AppShell`, every non-login route)

The gate resolves `getMe()` before mounting console chrome. Its three outcomes are the
contract:

| `getMe()` outcome | Web-origin action |
|-------------------|-------------------|
| 200, active user (`isSignedIn`) | Render `AppNav` + page content exactly as today (FR-010 — zero behavior change for signed-in analysts) |
| `OrchestratorError` **status 401** | Render **nothing console-like** (no nav, no flash, no empty frame) and navigate to `loginURL(pathname)` (FR-001/FR-002/FR-003) |
| any other failure (transport, 5xx, no `status`) | Render a minimal retry/error surface; **never** navigate to login (FR-010 — an orchestrator outage must not log anyone out) |

- The `/login` route stays bare chrome (current special-case in `AppShell`), so the gate
  never intercepts the sign-in surface (FR-007).
- Every console route is covered by this single gate — `/`, `/datasets`,
  `/goals[/:id]`, `/heuristics`, `/settings`, `/users`, and any future route — because the
  shell is the one shared mount point. No per-route whitelist.

## 3. Data-client 401 handling (`web/lib/orchestrator.ts`)

- **Session-secured calls** (`requestJSON`/`request` for datasets, goals, heuristics,
  settings, users, `/me`, streams): an `OrchestratorError` with **status 401** invokes the
  registered auth-redirect handler (navigates to `loginURL(currentPath)`) and throws a
  sentinel so no view renders an error flash. This covers a session dying **after** the
  shell mounted (expiry, deactivation, another tab signing out) — FR-005.
- **Sign-in calls** (`login`, `registerUser`): **exempt** — their 401 is the orchestrator's
  generic bad-credentials verdict and must keep surfacing inline on the login page (never a
  redirect; the single-generic-401 contract from feature 003 holds).
- **Transport failures**: no `status` on the error → no redirect, surfaced as a normal error
  (FR-010).
- The redirect handler is **registered** (`registerAuthRedirect(handler)`), injectable for
  vitest; the shell installs it, tests substitute a spy.

## 4. BFF wire contract (unchanged, relied upon)

`web/lib/proxy.ts` keeps forwarding the inbound `Cookie` header and relaying the upstream
`Set-Cookie` back; `/api/*` responses are machine-readable (`{"error": ...}` with the
orchestrator's status) — never HTML login-page redirects (FR-006). The client *acts* on a
401; it never changes the wire shape.

## 5. Deep-link recovery (`?next=`)

- **Writer**: `loginURL(next)` used by both the middleware fast-path and the shell gate;
  format `/login?next=<percent-encoded path>`.
- **Reader**: `web/app/login/page.tsx` reads the `next` query parameter and passes it through
  `safeNextPath(raw)` **before** navigating after a successful sign-in/registration.
- **Validation** (`safeNextPath`, pure, in `web/lib/session.ts`): accepts only same-origin
  relative paths — MUST start with `/`, MUST NOT start with `//`, MUST NOT contain a scheme
  (`:` before any `/`). Any other value (absent, absolute URL, protocol-relative, scheme-full)
  resolves to `/`.
- **Behavior**: valid `next` → navigate there after sign-in (FR-008); invalid/absent → `/`.

## 6. Sign-out and browser history

- Sign-out (`UserMenu` → `POST /logout` via BFF, cookie cleared) lands on `/login`; a
  subsequent back/forward or refresh of a console view re-runs the middleware fast-path and
  the shell gate, both of which route a signed-out visitor back to `/login` (FR-009).
- No console view is ever restored to a signed-out visitor by history traversal because every
  page load re-validates through the gate.

## Data transfer types (unchanged, for reference)

```
MeDto {                       // GET /me — the gate's sole verdict source
  id: string
  username: string
  role: "admin" | "member"
  active: boolean
  created_at: string
  updated_at: string
  admin_notice: boolean
}
```
