# Quickstart — Validate the Authentication Gate

Runnable end-to-end validation for the feature. Implementation bodies live in plan/tasks,
not here. Requires the live stack (`make up`) plus the frontend dev server (`make web-dev`)
per the repo's web conventions — the compose `web` service (:8083) serves the pre-built
image and will **not** reflect local `web/` edits.

## Prerequisites

- `make up` (compose: postgres, minio, neo4j, orchestrator :8080, verifier, web).
- `make web-dev` in a second terminal — the Next.js dev server on :3000 with your local
  edits, pointed at the orchestrator on :8080.
- At least one account exists (register one on `/login` if the stack is fresh — the first
  account becomes admin). The shared integration DB persists accounts between runs.
- Tests: `cd web && npm test` (vitest) — no infrastructure needed.

## Unit suite (the gate's rules, per project convention)

Run `npm test` from `web/`:

1. **`safeNextPath` / `loginURL`** (`lib/session.test.ts`): a same-origin relative path
   (`/datasets/abc`) is accepted; an absolute URL (`https://evil.example`), a
   protocol-relative path (`//evil.example`), and a scheme-full value are rejected (fall back
   to `/`); `loginURL` produces `/login?next=...` with the path percent-encoded.
2. **Data-client 401 routing** (`lib/orchestrator.test.ts`, using `registerAuthRedirect` with
   a spy handler):
   - a session-secured call (`getMe`, `listDatasets`, …) answering 401 → handler invoked,
     sentinel thrown (no flash);
   - `login`/`registerUser` answering 401 → handler **not** invoked (credentials verdict
     surfaces inline);
   - a transport failure (no `status`) → handler **not** invoked (no false sign-out).

## Browser-level validation (against the live stack)

Use a browser against http://localhost:3000. For the cookie-absent and stale-cookie cases,
use a private window or a fresh browser profile so no `arborette_session` cookie exists.

### 1. Signed-out visitor sees only the login page (FR-001/FR-004)

- In a private window, open `/datasets`, `/goals`, `/heuristics`, `/settings`, `/users`, `/`,
  and a deep link like `/datasets/<any-id>`.
- **Expected**: every URL lands on `/login`; the console header, navigation, corner chip, and
  any data frame **never** appear; the address bar shows the login page. Refreshing or
  re-navigating does not reveal a view.

### 2. No "Unauthorized" flash (FR-003)

- While still signed out, watch the top-right corner across the same set of routes.
- **Expected**: no "Unauthorized" error flash ever appears — the outcome is uniformly the
  login page.

### 3. Stale cookie is treated as signed out (FR-002)

- Sign in on the live stack, then make the session stale server-side (delete the row, or
  deactivate the account via `/users` as an admin — or just close the browser and clear the
  cookie). Revisit any console route with the stale cookie present.
- **Expected**: the shell gate's `getMe()` answers 401 → you land on `/login`; no console
  chrome, no flash, no empty frame.

### 4. Deep-link recovery after sign-in (FR-008)

- Signed out, open a bookmarked deep link, e.g. `http://localhost:3000/datasets/<id>`.
- **Expected**: redirected to `/login?next=%2Fdatasets%2F<id>`. Sign in → you land back on
  that same dataset's detail view, not the console home.
- Sign out, open the root URL, sign in again → you land on `/` (no `next`).

### 5. Signed-in analysts are unaffected (FR-010)

- Sign in and exercise the full console: browse datasets, open a dataset detail, goals,
  heuristics, settings, and (as admin) users. Refresh each view.
- **Expected**: everything renders and behaves exactly as before — no false bounce to login,
  no changed flows.

### 6. Mid-session expiry routes to login (FR-005)

- Signed in with the console open, invalidate the session without logging out (deactivate the
  account server-side as an admin, or delete the session row in Postgres).
- **Expected**: on the **next navigation or data action** you are returned to `/login` rather
  than left on a page with an error or a dead frame.

### 7. Sign-out sticks (FR-009)

- Sign out. Press back/forward and refresh on a console URL.
- **Expected**: you land on the login page; no console view is restored to the signed-out
  visitor by history traversal.

## Non-goals to verify unchanged

- `/api/*` still returns machine-readable JSON (`{"error": ...}`) to a signed-out caller —
  never an HTML login redirect (FR-006). Spot-check with `curl -i` against
  `http://localhost:3000/api/orchestrator/datasets` (no cookie) → `401` JSON body.
- Bad credentials on `/login` still show the single generic inline error (never a redirect
  loop).
- An orchestrator outage shows a retry surface to a signed-in analyst — it does **not** log
  them out (FR-010).
