# Data Model — Authentication Required for All Views

This feature introduces **no new schema**. It is a web-origin gate over the existing
session architecture, so the data model below documents the entities the gate *consults*
and the one piece of web-origin state it introduces (a URL query parameter, not storage).

The session itself is unchanged and orchestrator-owned: Postgres `sessions` + `users`
(migration `0017`, feature 003), with the orchestrator's session middleware re-validating
the session (including the user's `active` flag) on every request. Neo4j, the object store,
pgvector embeddings, and `audit_log` are untouched by this feature.

## Existing entity the gate depends on: Session (unchanged)

**`sessions`** (orchestrator, migration `0017`) — the signed-in session keyed by a SHA-256
token hash; the browser holds only the `arborette_session` cookie, whose presence the web
middleware fast-paths on and whose **validity** the shell gate verifies via `GET /me`.

| Column | Type | Constraints | Meaning |
|--------|------|-------------|---------|
| `id` | `uuid` | PK, default `gen_random_uuid()` | Row identity |
| `user_id` | `uuid` | NOT NULL, FK → `users(id)` ON DELETE CASCADE | Account this session belongs to |
| `token_hash` | `text` | NOT NULL, unique | SHA-256 of the cookie's opaque token — digests, never the raw token |
| `created_at` | `timestamptz` | NOT NULL default `now()` | When signed in |

No `expires_at` (unchanged): a session lasts until sign-out or deauthorization. The gate
does not read this table directly — the orchestrator's `GET /me` is the sole verdict the
gate consumes (status 200 = valid, status 401 = absent/invalid/deactivated).

## Existing entity the gate consults: User (unchanged)

**`users`** (orchestrator, migration `0017`) — the account row whose `active` flag the
orchestrator re-reads per request. A deactivated account's sessions answer `401` at `/me`,
which the shell gate treats as signed-out (FR-002).

## Web-origin gate state (transient, no storage)

The shell gate's state is **per-component and ephemeral** — it lives in `AppShell`'s React
state for the lifetime of a page load and is discarded on navigation. No cookies, no
sessionStorage/localStorage, no server-side web state.

| State | Meaning | Next action |
|-------|---------|-------------|
| `checking` | `getMe()` in flight; nothing console-like rendered | resolve → `signed-in` or `signed-out`, or transport-failure surface |
| `signed-in` | `/me` returned an active user | render `AppNav` + children (current behavior, FR-010) |
| `signed-out` | `/me` answered `401` | redirect to `loginURL(pathname)`; render nothing console-like (FR-001/FR-002/FR-003) |
| `error` | `/me` failed for a non-401 reason (transport, 5xx) | render a retry surface; **never** redirect to login (FR-010) |

## Web-origin carry-through: the `?next=` deep-link parameter

The only cross-request state the feature introduces is a URL query parameter on `/login`,
carried from the redirect (middleware fast-path or shell gate) to the login page:

```
?next=<percent-encoded original path>
```

- Written by: `web/middleware.ts` (cookie-absent fast-path) and the shell gate's
  `loginURL(pathname)`.
- Read and validated by: `web/app/login/page.tsx` via `safeNextPath(raw)`, which accepts
  only same-origin relative paths (starts with `/`, does not start with `//`, contains no
  scheme) and falls back to `/` otherwise (FR-008; open-redirect guard).
- Constraints: no value stored; nothing beyond the URL; validated on read, never trusted.

## Validation rules derived from the spec

- A session is "valid" for the gate iff `GET /me` answers 200 with an **active** user
  (`isSignedIn`); a 401 — no cookie, dead token, or deactivated account — is "signed out".
- A non-401 `getMe()` failure is an *outage*, not a sign-out: the gate must not redirect to
  login on it (FR-010).
- A 401 from `/login` or `/register` is a *credentials* verdict, never a redirect trigger
  (R5; the login page must keep surfacing it inline).
- `?next=` must be relative, same-origin, and scheme-free; any other value resolves to `/`
  (FR-008 safety).
