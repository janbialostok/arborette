// Session helpers for the App Router: the cookie name the orchestrator issues
// and the frontend tracks, and the pure predicates the header/chip and route
// gating use. The cookie value itself never reaches this bundle -- it is
// HttpOnly and read only by the BFF proxy and the web origin's middleware.

// SESSION_COOKIE must track the orchestrator's ARBORETTE_SESSION_COOKIE
// default ("arborette_session") and the value internal/orchestrator/session.go
// falls back to. web/middleware.ts and the fetch wrapper read this one name, so
// a platform change is one edit, not a hunt.
export const SESSION_COOKIE = "arborette_session";

// SessionUser is the shape of the identity the /me endpoint returns (plus the
// admin-notice flag, which the chip reads separately in MeDto). Keeping the
// type here lets the client helpers and tests share the discriminating fields
// role/active without importing a DTO from the orchestrator client.
export interface SessionUser {
  id: string;
  username: string;
  role: "admin" | "member";
  active: boolean;
}

// isSignedIn is the one gate every page's fetch wrapper relies on: a null or
// deactivated user is signed out even if a cookie exists (deactivation is
// authoritative server-side; the client reflects the /me read).
export function isSignedIn(user: SessionUser | null): boolean {
  return user !== null && user.active;
}

// isAdmin is the view-gating predicate for the admin Users entry: only an
// active admin may see the roster. It is a convenience for the UI; the
// orchestrator's role check on /users* is authoritative.
export function isAdmin(user: SessionUser | null): boolean {
  return user !== null && user.role === "admin" && user.active;
}