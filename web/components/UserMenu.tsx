"use client";

import { useEffect, useRef, useState } from "react";
import Link from "next/link";
import {
  AuthRedirected,
  OrchestratorError,
  getMe,
  logout,
  updateMe,
  errorMessage,
  type MeDto,
} from "@/lib/orchestrator";
import { isSignedIn } from "@/lib/session";
import { Callout, SectionLabel } from "@/components/ui";

// UserMenu is the top-right identity surface: an avatar chip of the username's
// first letter that opens a dropdown with the username + role, Settings, the
// admin-only Users entry, and Sign out. While a seeded admin hasn't dismissed
// the grant notice yet (MeDto.admin_notice) a one-line banner is shown under
// the chip. Signing out POSTs the BFF /logout, which clears the session cookie,
// then lands on /login.
//
// It mounts only after the AppShell session gate has passed, and the gate owns
// sign-out detection: a 401 here (a session that died mid-use) is routed to the
// login page by the data client's registered auth-redirect handler and must
// never flash as an error. Only genuine failures -- a transport or upstream
// error with no 401 -- surface a message.
export function UserMenu() {
  const [me, setMe] = useState<MeDto | null>(null);
  const [error, setError] = useState<string | null>(null);
  const [open, setOpen] = useState(false);
  const menuRef = useRef<HTMLDivElement>(null);

  useEffect(() => {
    let cancelled = false;
    getMe()
      .then((m) => {
        if (!cancelled) setMe(m);
      })
      .catch((err: unknown) => {
        if (cancelled) return;
        if (err instanceof AuthRedirected) return;
        if (err instanceof OrchestratorError && err.status === 401) return;
        setError(errorMessage(err, "Could not load your account."));
      });
    return () => {
      cancelled = true;
    };
  }, []);

  // Close the dropdown on an outside click.
  useEffect(() => {
    if (!open) return;
    function onDocClick(e: MouseEvent) {
      if (menuRef.current && !menuRef.current.contains(e.target as Node)) {
        setOpen(false);
      }
    }
    document.addEventListener("mousedown", onDocClick);
    return () => document.removeEventListener("mousedown", onDocClick);
  }, [open]);

  const signedIn = isSignedIn(me);

  async function dismissNotice() {
    setError(null);
    try {
      setMe(await updateMe({ acknowledge_admin_notice: true }));
    } catch (err) {
      setError(errorMessage(err, "Could not dismiss the notice."));
    }
  }

  async function signOut() {
    setError(null);
    try {
      await logout();
    } catch (err) {
      setError(errorMessage(err, "Could not sign out."));
      return;
    }
    window.location.assign("/login");
  }

  if (!signedIn && !error) {
    return null;
  }

  return (
    <div className="relative flex flex-col items-end gap-2" ref={menuRef}>
      {me?.admin_notice && (
        <div className="flex items-center gap-2 rounded-lg border border-warn/40 bg-warn/10 px-3 py-2 text-xs text-warn">
          <span>You were granted the administrator role.</span>
          <button
            onClick={() => void dismissNotice()}
            className="rounded px-1.5 py-0.5 text-warn underline-offset-2 transition-colors hover:underline"
          >
            Dismiss
          </button>
        </div>
      )}

      {error && <Callout tone="error">{error}</Callout>}

      <button
        type="button"
        onClick={() => setOpen((o) => !o)}
        className="flex h-9 w-9 items-center justify-center rounded-full border border-line-strong bg-surface-2 text-sm font-semibold text-signal transition-colors hover:border-signal/50"
        aria-haspopup="menu"
        aria-expanded={open}
      >
        {signedIn ? me!.username.charAt(0).toUpperCase() : "?"}
      </button>

      {open && signedIn && (
        <div
          role="menu"
          className="absolute right-0 top-12 z-20 w-56 rounded-lg border border-line bg-surface/95 p-1.5 shadow-lg backdrop-blur"
        >
          <div className="flex flex-col gap-0.5 border-b border-line px-3 py-2.5">
            <span className="text-sm font-medium text-fg">{me!.username}</span>
            <span className="font-mono text-[10px] uppercase tracking-wider text-faint">
              {me!.role}
            </span>
          </div>

          <div className="flex flex-col gap-0.5 py-1">
            <MenuItem href="/settings">Settings</MenuItem>
            {me!.role === "admin" && me!.active && (
              <MenuItem href="/users">Users</MenuItem>
            )}
            <button
              onClick={() => void signOut()}
              className="px-3 py-2 text-left text-sm text-muted transition-colors hover:text-neg"
            >
              Sign out
            </button>
          </div>
        </div>
      )}
    </div>
  );
}

function MenuItem({ href, children }: { href: string; children: React.ReactNode }) {
  return (
    <Link
      href={href}
      className="px-3 py-2 text-sm text-muted transition-colors hover:text-fg"
    >
      {children}
    </Link>
  );
}