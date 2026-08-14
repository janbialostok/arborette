"use client";

import { usePathname } from "next/navigation";
import { AppNav } from "@/components/AppNav";

// AppShell is the shared chrome the root layout composes. Sign-in and account
// creation live on /login, which is standalone: the same root <html>/<body>
// applies (fonts, colors) but no header/nav/footer so the in-app chrome --
// whose UserMenu 401s without a session -- never mounts on the auth screen.
export function AppShell({ children }: { children: React.ReactNode }) {
  const pathname = usePathname();

  if (pathname === "/login") {
    return <>{children}</>;
  }

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