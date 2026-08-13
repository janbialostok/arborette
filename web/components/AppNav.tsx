"use client";

import Link from "next/link";
import { usePathname } from "next/navigation";
import { cn } from "@/components/ui";

const LINKS = [
  { href: "/", label: "New dataset" },
  { href: "/datasets", label: "Datasets" },
];

export function AppNav() {
  const pathname = usePathname();
  return (
    <header className="flex items-center justify-between py-6">
      <Link href="/" className="group flex items-center gap-2.5">
        <span className="inline-block h-2.5 w-2.5 rounded-[3px] bg-signal shadow-[0_0_12px_rgba(123,241,168,0.6)] transition-transform group-hover:rotate-45" />
        <span className="font-mono text-sm font-semibold tracking-[0.14em] text-fg">
          ARBORETTE
        </span>
      </Link>
      <nav className="flex items-center gap-1">
        {LINKS.map((link) => {
          const active =
            link.href === "/"
              ? pathname === "/"
              : pathname.startsWith(link.href);
          return (
            <Link
              key={link.href}
              href={link.href}
              className={cn(
                "rounded-md px-3 py-1.5 text-sm transition-colors",
                active
                  ? "text-signal"
                  : "text-muted hover:text-fg",
              )}
            >
              {link.label}
            </Link>
          );
        })}
      </nav>
    </header>
  );
}
