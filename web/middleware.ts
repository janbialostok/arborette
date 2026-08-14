import { NextRequest, NextResponse } from "next/server";
import { SESSION_COOKIE } from "@/lib/session";

// Session gate on the web origin. A visitor with no arborette_session cookie is
// bounced to /login before any analyst-facing page renders. This is a UX guard,
// not the security boundary: the orchestrator's 401 is authoritative (and the
// BFF forwards every /api request regardless, so a stale or forged cookie still
// answers the orchestrator's real verdict).
//
// The matcher excludes the sign-in surface (/login), Next's internals, the BFF
// (/api — its responses must reach the client as JSON, never as a redirect to a
// login page), and paths that look like static assets (a dot in the pathname).
export function middleware(req: NextRequest) {
  if (!req.cookies.get(SESSION_COOKIE)) {
    const login = new URL("/login", req.url);
    return NextResponse.redirect(login);
  }
  return NextResponse.next();
}

export const config = {
  matcher: ["/((?!login|\\_next|api|.*\\..*).*)"],
};