// The review queue's domain logic, kept out of the component so the rules an
// analyst's verdict depends on — which extractions still admit one, which value
// stands, where the source span falls — are testable on their own.

import { renderValue } from "./format";
import type { ExtractionOutcome, VerificationEntry } from "./orchestrator";

// ReviewItem is the one shape the review view renders, whichever endpoint it
// came from. Both the excerpt and the verdict are addressed by outcome id, so
// the detail panel works unchanged across the two sources.
export interface ReviewItem {
  outcomeID: string;
  field: string;
  value: string;
  confidence: number;
  status: string;
}

// awaiting names the statuses that still admit a verdict — the queue's own
// pending, and the graph's unverified for an outcome that was never queued.
// Anything else has been resolved, and re-resolving it answers a 409, so the
// verdict row is withheld rather than offered and rejected.
const AWAITING = ["pending", "unverified"];

export function isAwaiting(status: string): boolean {
  return AWAITING.includes(status);
}

// fromEntry normalizes a queue row. A resolved row carries the analyst's
// replacement text, so the list shows the value that stands rather than the one
// it replaced, and reports the verdict in place of the bare "resolved".
export function fromEntry(e: VerificationEntry): ReviewItem {
  return {
    outcomeID: e.outcome_id,
    field: e.field,
    value: e.corrected_value || e.extracted_value,
    confidence: e.confidence,
    status: e.status === "resolved" ? e.resolution || e.status : e.status,
  };
}

// fromOutcome normalizes an extraction as the graph holds it, whose value is a
// map keyed by field rather than the queue's flat string.
export function fromOutcome(o: ExtractionOutcome): ReviewItem {
  return {
    outcomeID: o.outcome_id,
    field: o.field,
    value: renderValue(o.value[o.field]),
    confidence: o.confidence,
    status: o.verification_status,
  };
}

// nextAwaiting is the item a reviewer moves to after recording a verdict: the
// next one still awaiting review, wrapping to the top of the list so the last
// item does not dead-end while earlier ones are still open. It returns null when
// nothing else is open, which leaves the caller on the item just resolved.
export function nextAwaiting(items: ReviewItem[], fromID: string): string | null {
  if (items.length === 0) return null;
  const start = items.findIndex((i) => i.outcomeID === fromID);
  for (let step = 1; step <= items.length; step++) {
    const item = items[(start + step) % items.length];
    if (item.outcomeID !== fromID && isAwaiting(item.status)) return item.outcomeID;
  }
  return null;
}

// splitByteRange cuts a page into the text before a located span, the span, and
// the text after it.
//
// The orchestrator's locators are BYTE offsets: it finds a value with Go's
// strings.Index and ends it at idx + len(needle), both of which count bytes,
// then slices the page by those same byte offsets. A JS string indexes UTF-16
// code units instead, so slicing it directly drifts by one position for every
// non-ASCII byte earlier in the page — a currency sign, an em dash, an accent,
// all routine in the documents under review — and lands the highlight on the
// wrong text. Splitting the encoded bytes and decoding each piece keeps the span
// on exactly the text the locator names.
export function splitByteRange(
  text: string,
  start: number,
  end: number,
): [string, string, string] {
  const bytes = new TextEncoder().encode(text);
  const clamp = (n: number) => Math.max(0, Math.min(n, bytes.length));
  const from = clamp(start);
  const to = Math.max(from, clamp(end));
  const decoder = new TextDecoder();
  return [
    decoder.decode(bytes.subarray(0, from)),
    decoder.decode(bytes.subarray(from, to)),
    decoder.decode(bytes.subarray(to)),
  ];
}
