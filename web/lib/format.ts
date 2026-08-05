// The display forms every view shares. These live outside the components because
// what a number, a score, an id, or a timestamp looks like is a property of the
// whole console rather than of one card — and because the difference between an
// unmeasured value and a zero is a distinction the product cannot afford to make
// twice, differently.

// renderValue turns an orchestrator-supplied value of unknown shape into the one
// display form every view uses for graph values.
export function renderValue(val: unknown): string {
  if (val == null) return "—";
  if (typeof val === "object") return JSON.stringify(val);
  return String(val);
}

// formatTimestamp renders a server timestamp in the reader's own locale, falling
// back to the raw string for anything unparseable — an odd-looking timestamp is
// better than a card that reads "Invalid Date".
export function formatTimestamp(iso: string): string {
  const date = new Date(iso);
  return Number.isNaN(date.getTime()) ? iso : date.toLocaleString();
}

// shortId trims a uuid to the prefix that identifies a record on screen. The full
// id is never the point of a card — it is how an analyst tells two of them apart.
export function shortId(id: string): string {
  return id.slice(0, 8);
}

// formatNumber renders a measured number, holding precision where the magnitude
// needs it. A number the run has no value for reads as an em dash rather than a
// zero: the orchestrator sends null for an effect it has not measured yet, and a
// zero effect is a finding of its own that must not be confused with an absent one.
export function formatNumber(n: number | null | undefined): string {
  if (n == null || !Number.isFinite(n)) return "—";
  if (Number.isInteger(n)) return n.toLocaleString("en-US");
  const abs = Math.abs(n);
  const digits = abs >= 100 ? 1 : abs >= 1 ? 2 : 4;
  return n.toLocaleString("en-US", { maximumFractionDigits: digits });
}

// formatConfidence renders a 0–1 score. Confidence gets two fixed decimals
// everywhere it appears — an effect's precision follows its magnitude, but two
// scores are only comparable at a glance when they are written to the same width.
export function formatConfidence(n: number | null | undefined): string {
  if (n == null || !Number.isFinite(n)) return "—";
  return n.toFixed(2);
}
