"use client";

import { Suspense, useState } from "react";
import Link from "next/link";
import { useSearchParams } from "next/navigation";
import { login, registerUser, errorMessage } from "@/lib/orchestrator";
import { safeNextPath } from "@/lib/session";
import { Button, Callout, Panel, SectionLabel } from "@/components/ui";

// LoginPage is sign-in and account creation as one screen. The first account on
// a seedless stack becomes admin (server-side rule), so this single form covers
// both a fresh install and an analyst signing back in. A returned {error} body
// is surfaced verbatim -- the login failure is deliberately one generic message,
// and registration validation reasons are analyst-safe to show.
//
// The middleware and the shell gate bounce a signed-out visitor here with a
// ?next=<encoded destination> deep link; after a successful sign-in or
// registration the analyst is returned to that validated same-origin view (or
// the console root when there is none). useSearchParams is wrapped in Suspense
// because the page is prerendered: the search params only resolve on the client.
export default function LoginPage() {
  return (
    <Suspense fallback={null}>
      <LoginForm />
    </Suspense>
  );
}

function LoginForm() {
  const searchParams = useSearchParams();
  const [mode, setMode] = useState<"signin" | "register">("signin");
  const [username, setUsername] = useState("");
  const [password, setPassword] = useState("");
  const [loading, setLoading] = useState(false);
  const [error, setError] = useState<string | null>(null);

  // The deep-link destination is validated to a same-origin relative path (an
  // absolute URL, protocol-relative path, or scheme-full value resolves to the
  // root); an absent next is the plain /login visit, which lands on "/".
  const destination = safeNextPath(searchParams.get("next")) ?? "/";

  async function submit() {
    setLoading(true);
    setError(null);
    try {
      if (mode === "signin") {
        await login(username, password);
      } else {
        await registerUser(username, password);
      }
      window.location.assign(destination);
    } catch (err) {
      setError(errorMessage(err, "Could not complete the request. Please retry."));
      setLoading(false);
    }
  }

  return (
    <main className="flex min-h-dvh flex-col items-center justify-center gap-8 px-6">
      <Link href="/login" className="group flex items-center gap-2.5">
        <span className="inline-block h-2.5 w-2.5 rounded-[3px] bg-signal shadow-[0_0_12px_rgba(123,241,168,0.6)] transition-transform group-hover:rotate-45" />
        <span className="font-mono text-sm font-semibold tracking-[0.14em] text-fg">
          ARBORETTE
        </span>
      </Link>

      <Panel className="w-full max-w-sm p-6">
        <div className="mb-5 flex flex-col gap-3">
          <SectionLabel>
            {mode === "signin" ? "Sign in" : "Create account"}
          </SectionLabel>
          <h1 className="text-xl font-semibold tracking-tight">
            {mode === "signin" ? "Welcome back" : "Join the console"}
          </h1>
          <p className="text-sm leading-relaxed text-muted">
            {mode === "signin"
              ? "Sign in with your username and password."
              : "The first account on a fresh stack becomes the administrator."}
          </p>
        </div>

        <form
          className="flex flex-col gap-4"
          onSubmit={(e) => {
            e.preventDefault();
            void submit();
          }}
        >
          <label className="flex flex-col gap-1.5">
            <span className="font-mono text-[11px] uppercase tracking-wider text-faint">
              Username
            </span>
            <input
              value={username}
              onChange={(e) => setUsername(e.target.value)}
              autoComplete="username"
              className="rounded-lg border border-line-strong bg-surface-2 px-3.5 py-2.5 text-sm text-fg outline-none transition-colors placeholder:text-faint focus:border-signal/60"
              placeholder="analyst"
              required
            />
          </label>

          <label className="flex flex-col gap-1.5">
            <span className="font-mono text-[11px] uppercase tracking-wider text-faint">
              Password
            </span>
            <input
              type="password"
              value={password}
              onChange={(e) => setPassword(e.target.value)}
              autoComplete={
                mode === "signin" ? "current-password" : "new-password"
              }
              className="rounded-lg border border-line-strong bg-surface-2 px-3.5 py-2.5 text-sm text-fg outline-none transition-colors placeholder:text-faint focus:border-signal/60"
              placeholder="••••••••"
              required
            />
          </label>

          {error && (
            <Callout tone="error" className="text-sm">
              {error}
            </Callout>
          )}

          <Button
            type="submit"
            loading={loading}
            disabled={loading}
            className="w-full"
          >
            {mode === "signin" ? "Sign in" : "Create account"}
          </Button>
        </form>

        <button
          type="button"
          className="mt-5 w-full text-center text-sm text-muted transition-colors hover:text-signal"
          onClick={() => {
            setMode(mode === "signin" ? "register" : "signin");
            setError(null);
          }}
        >
          {mode === "signin"
            ? "No account yet? Create one"
            : "Have an account? Sign in"}
        </button>
      </Panel>
    </main>
  );
}