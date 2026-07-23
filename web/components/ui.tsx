import type { ButtonHTMLAttributes, ReactNode } from "react";

export function cn(...parts: Array<string | false | null | undefined>): string {
  return parts.filter(Boolean).join(" ");
}

// SectionLabel titles a working region without adding a card or a divider.
export function SectionLabel({
  children,
  className,
}: {
  children: ReactNode;
  className?: string;
}) {
  return (
    <span
      className={cn(
        "font-mono text-[11px] uppercase tracking-[0.18em] text-faint",
        className,
      )}
    >
      {children}
    </span>
  );
}

type ButtonProps = ButtonHTMLAttributes<HTMLButtonElement> & {
  variant?: "primary" | "ghost" | "subtle";
  loading?: boolean;
};

export function Button({
  variant = "primary",
  loading = false,
  className,
  children,
  disabled,
  ...rest
}: ButtonProps) {
  const base =
    "inline-flex items-center justify-center gap-2 rounded-lg px-4 py-2.5 text-sm font-medium transition-all duration-150 focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-signal/60 disabled:cursor-not-allowed disabled:opacity-45";
  const variants = {
    primary:
      "bg-signal text-signal-ink hover:brightness-110 active:brightness-95 shadow-[0_0_0_1px_rgba(123,241,168,0.2)]",
    ghost:
      "border border-line-strong text-fg hover:border-signal/50 hover:text-signal",
    subtle: "text-muted hover:text-fg",
  };
  return (
    <button
      className={cn(base, variants[variant], className)}
      disabled={disabled || loading}
      {...rest}
    >
      {loading && <Spinner />}
      {children}
    </button>
  );
}

export function Spinner({ className }: { className?: string }) {
  return (
    <span
      className={cn(
        "inline-block h-3.5 w-3.5 animate-spin rounded-full border-[1.5px] border-current border-t-transparent",
        className,
      )}
      aria-hidden
    />
  );
}

// Callout is the single inline-message primitive, used for surfaced orchestrator
// messages and status notices.
export function Callout({
  tone = "info",
  children,
  className,
}: {
  tone?: "error" | "warn" | "info";
  children: ReactNode;
  className?: string;
}) {
  const tones = {
    error: "border-neg/40 bg-neg/10 text-neg",
    warn: "border-warn/40 bg-warn/10 text-warn",
    info: "border-line-strong bg-surface-2 text-muted",
  };
  return (
    <div
      role={tone === "error" ? "alert" : "status"}
      className={cn(
        "rounded-lg border px-4 py-3 text-sm leading-relaxed",
        tones[tone],
        className,
      )}
    >
      {children}
    </div>
  );
}

export type BadgeTone = "positive" | "neutral" | "warn" | "negative";

// Badge is the rounded-full status pill; callers pass a semantic tone.
export function Badge({
  tone = "neutral",
  children,
  className,
}: {
  tone?: BadgeTone;
  children: ReactNode;
  className?: string;
}) {
  const tones: Record<BadgeTone, string> = {
    positive: "border-signal/40 text-signal",
    neutral: "border-line-strong text-muted",
    warn: "border-warn/40 text-warn",
    negative: "border-neg/40 text-neg",
  };
  return (
    <span
      className={cn(
        "rounded-full border px-2 py-0.5 font-mono text-[10px] uppercase tracking-wider",
        tones[tone],
        className,
      )}
    >
      {children}
    </span>
  );
}

export function Panel({
  children,
  className,
}: {
  children: ReactNode;
  className?: string;
}) {
  return (
    <div
      className={cn(
        "rounded-[var(--radius-panel)] border border-line bg-surface/70",
        className,
      )}
    >
      {children}
    </div>
  );
}
