# Contracts — Orchestrator REST API (User Accounts & Sessions)

The orchestrator (`internal/orchestrator`) is the sole API the web UI talks to, and it
also owns the session. This feature **adds** the account/session surface and wraps the
existing analyst-facing routes behind the session middleware; no pre-existing endpoint's
request shape changes. Every route keeps (or gains) its BFF proxy under
`web/app/api/orchestrator/...`.

## Session & cookie contract

- **Cookie**: `arborette_session` (config `ARBORETTE_SESSION_COOKIE`), `HttpOnly`,
  `SameSite=Lax`, `Path=/`. `Secure` only when `ARBORETTE_SESSION_SECURE=true` (off in
  local HTTP compose). Value = opaque random token; only its SHA-256 hash is stored.
- **Lifecycle**: set on `POST /register` and `POST /login`; cleared (deleted row + expired
  `Set-Cookie`) on `POST /logout`; the account's own `password`/`username`/role changes do
  **not** rotate it. Deactivation invalidates it at the next request.
- **Protected routes**: every analyst-facing route except `POST /register` and
  `POST /login` requires a valid, active session. No cookie / bad token / deactivated
  user → `401` `{"error":"unauthorized"}` with `WWW-Authenticate` set to the session scheme.
- **BFF tunneling (mandatory)**: `web/lib/proxy.ts` MUST forward the inbound `Cookie`
  header to the orchestrator and relay the upstream `Set-Cookie` header back through
  `passThrough` and `eventStream` (every `forward*` helper). Without it the browser's
  session never reaches the orchestrator and login's cookie never reaches the browser.
- `web/middleware.ts` (web origin) redirects signed-out visitors to `/login` on the app
  routes; this is a UX guard, **not** the security boundary — the orchestrator's 401 is
  authoritative.

## Data transfer types

```
AccountDto {
  id: string                    // uuid
  username: string
  role: "admin" | "member"
  active: boolean
  created_at: string            // RFC3339
}

MeDto extends AccountDto {
  admin_notice: boolean         // seeded-admin grant notice pending acknowledgment
}

UserAdminDto extends AccountDto {
  // same shape as AccountDto; the admin list returns these rows
}
```

`GET /me` is the identity bootstrap every signed-in page uses: username (for the chip's
first letter), role (for the admin **Users** entry), `active`, and the one-shot
`admin_notice` flag. Actions (change password, reset by admin, promote/demote,
deactivate, acknowledge notice) do not add separate DTOs — responses return the refreshed
shape.

## Endpoints

### `POST /register`
- **Request**: `{ "username": "...", "password": "..." }`
- Creates an account: the store seeds it `admin` when **no active admin exists** (FR-003),
  otherwise `member`; grants the admin-grant notice flag only on the seeded path.
- **201** → `MeDto` + `Set-Cookie` (signed in). Username taken → **409**
  `{"error":"username is already in use"}`. Validation (username shape, password ≥ 8) →
  **400** with a clear reason. The registration succeeds with no email/confirmation step.

### `POST /login`
- **Request**: `{ "username": "...", "password": "..." }`
- **200** → `MeDto` + `Set-Cookie`. **401** → the single generic
  `{"error":"invalid username or password"}` whether the username exists and the password
  is wrong, the username is absent, or the account is deactivated — uniform message and
  uniform compare timing (dummy-hash compare on unknown usernames, FR-004).

### `POST /logout`
- Requires a session. Deletes the session row and clears the cookie. **204**.

### `GET /me`
- Requires a session. **200** → `MeDto`. The `admin_notice` field is `true` exactly when a
  seeded admin hasn't dismissed the notice yet.

### `PATCH /me`
- Requires a session. Self-service fields (at least one required):
  - `username` — rename (validated, unique); **409** on a clash; **400** on shape.
  - `password` — the new value; requires `current_password` in the same body (**403** if
    the current password doesn't verify). No session rotation.
  - `acknowledge_admin_notice: true` — clears the one-shot admin-grant notice.
- **200** → refreshed `MeDto`.

### `GET /users` (admin)
- Requires a session with `role="admin"` (**403** for members; entry is hidden in the UI
  but the server enforces the rule, FR-014). **200** → `UserAdminDto[]` (username, role,
  active, created/updated).

### `POST /users` (admin)
- **Request**: `{ "username": "...", "password": "..." }`
- Admin-created accounts are always `active, role=member`; only the seed path ever grants
  admin. **201** → `UserAdminDto`. **404/403/409** as above.

### `PATCH /users/{id}` (admin)
- **Request** (at least one field):
  - `role: "admin" | "member"` — promote/demote, carried by the same field.
  - `active: boolean` — deactivate/reactivate. Deactivate rejects the account's existing
    sessions at their next request.
  - `new_password: "..."` — admin password reset (FR-010); takes effect on the account's
    next sign-in.
- Guard (FR-013): a change that would leave **zero active admins** — demoting or
  deactivating the last one — is refused with **409**
  `{"error":"the last active admin cannot be removed"}` before any write. A member body is
  **403**. Unknown id → **404**.
- **200** → refreshed `UserAdminDto`. The acting admin's own row is manipulated through the
  same endpoint (self-demote hits the same guard).

## Error shape

Unchanged: JSON `{ "error": "<reason>" }`. Auth/session failures use
`401 {"error":"unauthorized"}`; role failures use `403 {"error":"admin access required"}`;
conflicts use `409` with a specific reason. No message ever reveals whether a username
exists (login), nor leaks the cookie value.

## Audit detail keys (new vocabulary under existing discipline)

Management actions are audited with the acting user resolving from the session (FR-008;
boot-time and `/internal/audit` paths keep the stub fallback):

| Action | Detail keys |
|--------|-------------|
| `user_register` | `user_id`, `username`, `role` |
| `user_password_change` (self) | `user_id`, `username` |
| `user_username_change` (self) | `user_id`, `username` (new), `previous_username` |
| `user_password_reset` (admin) | `user_id`, `username` |
| `user_role_change` (admin) | `user_id`, `username`, `role` |
| `user_status_change` (admin) | `user_id`, `username`, `active` |

Login/logout are session transitions, deliberately **not** audited (R6). `audit_log`
schema is unchanged; only action strings + established key discipline are added.