"use client";

import { useEffect, useState } from "react";
import { usePathname } from "next/navigation";
import { AppNav } from "@/components/AppNav";
import { Button } from "@/components/ui";
import {
  AuthRedirected,
  OrchestratorError,
  getMe,
  registerAuthRedirect,
} from "@/lib/orchestrator";
import { isSignedIn, loginURL } from "@/lib/session";

// AppShell is the shared chrome the root layout composes. Sign-in and account
// creation live on /login, which is standalone: the same root <html>/<body>
// applies (fonts, colors) but no header/nav/footer so the in-app chrome --
// whose UserMenu 401s without a session -- never mounts on the auth screen.
//
// On every other route the shell is the session gate (contract §2): it resolves
// getMe() before mounting any console chrome. A 401 -- no session, a stale or
// forged cookie, an expired or revoked session, a deactivated account -- sends
// the visitor to /login?next=<current view> with nothing console-like painted
// (FR-001..FR-003); any other failure (a transport or upstream 5xx, no status)
// renders a retry surface and NEVER redirects to login (FR-010, no false
// sign-outs). Signed-in analysts render exactly as before.
type Gate = "checking" | "signed-in" | "signed-out" | "error";

export function AppShell({ children }: { children: React.ReactNode }) {
  const pathname = usePathname();
  const [gate, setGate] = useState<Gate>("checking");
  const [attempt, setAttempt] = useState(0);

  // Install the data client's 401-redirect handler for the lifetime of the
  // shell: any session-secured call that answers 401 after the gate passed
  // (session expired, revoked, or deactivated mid-use) routes the analyst to
  // /login?next=<current view> instead of surfacing an error flash. /login
  // makes no session-secured calls -- its sign-in actions opt out -- so
  // registering here is harmless on the auth screen.
  useEffect(() => {
    registerAuthRedirect(() => {
      window.location.assign(loginURL(location.pathname + location.search));
    });
    return () => registerAuthRedirect(null);
  }, []);

  useEffect(() => {
    if (pathname === "/login") {
      setGate("signed-in");
      return;
    }
    let cancelled = false;
    setGate("checking");
    getMe()
      .then((me) => {
        if (cancelled) return;
        setGate(isSignedIn(me) ? "signed-in" : "signed-out");
      })
      .catch((err: unknown) => {
        if (cancelled) return;
        if (err instanceof AuthRedirected) return;
        if (err instanceof OrchestratorError && err.status === 401) {
          window.location.assign(loginURL(pathname));
          setGate("signed-out");
          return;
        }
        setGate("error");
      });
    return () => {
      cancelled = true;
    };
  }, [pathname, attempt]);

  if (pathname === "/login") {
    return <>{children}</>;
  }

  if (gate === "signed-in") {
    return (
      <div className="mx-auto flex min-h-dvh max-w-6xl flex-col px-5 md:px-8">
        <AppNav />
        <main className="flex-1 pb-20">{children}</main>
        <footer className="border-t border-line py-6 text-xs text-faint">
          Arborette · analyst console
        </footer>
      </div>
    );
  }

  if (gate === "error") {
    return (
      <div className="flex min-h-dvh items-center justify-center px-5">
        <div className="w-full max-w-sm rounded-lg border border-line bg-surface-2 p-6 text-center">
          <p className="text-sm text-muted">
            Could not verify your session. The console may be temporarily
            unavailable.
          </p>
          <div className="mt-4 flex justify-center">
            <Button variant="primary" onClick={() => setAttempt((n) => n + 1)}>
              Retry
            </Button>
          </div>
        </div>
      </div>
    );
  }

  // "checking" (the gate is still resolving) and "signed-out" (the login
  // redirect is in flight) both render nothing console-like.
  return null;
}
