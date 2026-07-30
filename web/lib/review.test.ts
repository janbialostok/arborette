import { describe, expect, it } from "vitest";
import {
  fromEntry,
  fromOutcome,
  isAwaiting,
  nextAwaiting,
  splitByteRange,
  type ReviewItem,
} from "./review";
import type { ExtractionOutcome, VerificationEntry } from "./orchestrator";

function entry(over: Partial<VerificationEntry> = {}): VerificationEntry {
  return {
    queue_id: "q1",
    outcome_id: "o1",
    field: "invoice_total",
    extracted_value: "1296.00",
    provenance: null,
    confidence: 0.42,
    status: "pending",
    created_at: "2026-07-30T09:00:00Z",
    ...over,
  };
}

function outcome(over: Partial<ExtractionOutcome> = {}): ExtractionOutcome {
  return {
    outcome_id: "o1",
    field: "invoice_total",
    method: "extraction",
    value: { invoice_total: "1296.00" },
    provenance: null,
    verification_status: "unverified",
    confidence: 0.9,
    ...over,
  };
}

function item(over: Partial<ReviewItem> = {}): ReviewItem {
  return {
    outcomeID: "o1",
    field: "f",
    value: "v",
    confidence: 0.5,
    status: "pending",
    ...over,
  };
}

describe("isAwaiting", () => {
  it("admits a verdict for a queued or never-queued extraction", () => {
    expect(isAwaiting("pending")).toBe(true);
    expect(isAwaiting("unverified")).toBe(true);
  });

  it("withholds the verdict row for anything already resolved", () => {
    for (const status of ["confirmed", "corrected", "rejected", "verified", ""]) {
      expect(isAwaiting(status)).toBe(false);
    }
  });
});

describe("fromEntry", () => {
  it("reports a pending row as awaiting review", () => {
    expect(fromEntry(entry())).toEqual({
      outcomeID: "o1",
      field: "invoice_total",
      value: "1296.00",
      confidence: 0.42,
      status: "pending",
    });
  });

  it("shows the value that stands and the verdict, not the bare resolved", () => {
    const got = fromEntry(
      entry({ status: "resolved", resolution: "corrected", corrected_value: "1300.00" }),
    );
    expect(got.value).toBe("1300.00");
    expect(got.status).toBe("corrected");
    expect(isAwaiting(got.status)).toBe(false);
  });

  it("keeps the extracted value when a verdict replaced nothing", () => {
    const got = fromEntry(entry({ status: "resolved", resolution: "confirmed" }));
    expect(got.value).toBe("1296.00");
    expect(got.status).toBe("confirmed");
  });

  it("falls back to the raw status when a resolved row names no resolution", () => {
    expect(fromEntry(entry({ status: "resolved" })).status).toBe("resolved");
  });
});

describe("fromOutcome", () => {
  it("digs the value out of the field-keyed map", () => {
    expect(fromOutcome(outcome())).toEqual({
      outcomeID: "o1",
      field: "invoice_total",
      value: "1296.00",
      confidence: 0.9,
      status: "unverified",
    });
  });

  it("renders a missing key and a nested value without throwing", () => {
    expect(fromOutcome(outcome({ value: {} })).value).toBe("—");
    expect(fromOutcome(outcome({ value: { invoice_total: null } })).value).toBe("—");
    expect(fromOutcome(outcome({ value: { invoice_total: { a: 1 } } })).value).toBe(
      '{"a":1}',
    );
  });
});

describe("nextAwaiting", () => {
  it("advances to the next item still awaiting review", () => {
    const items = [
      item({ outcomeID: "a", status: "confirmed" }),
      item({ outcomeID: "b" }),
      item({ outcomeID: "c" }),
    ];
    expect(nextAwaiting(items, "b")).toBe("c");
  });

  it("wraps to the top rather than dead-ending on the last item", () => {
    const items = [
      item({ outcomeID: "a" }),
      item({ outcomeID: "b", status: "rejected" }),
      item({ outcomeID: "c" }),
    ];
    expect(nextAwaiting(items, "c")).toBe("a");
  });

  it("skips resolved items on the way round", () => {
    const items = [
      item({ outcomeID: "a", status: "corrected" }),
      item({ outcomeID: "b", status: "confirmed" }),
      item({ outcomeID: "c" }),
      item({ outcomeID: "d" }),
    ];
    expect(nextAwaiting(items, "c")).toBe("d");
  });

  it("returns null when nothing else is open, so the caller stays put", () => {
    const items = [
      item({ outcomeID: "a", status: "confirmed" }),
      item({ outcomeID: "b" }),
    ];
    expect(nextAwaiting(items, "b")).toBeNull();
  });

  it("returns null for a single-item list and for an empty one", () => {
    expect(nextAwaiting([item({ outcomeID: "a" })], "a")).toBeNull();
    expect(nextAwaiting([], "a")).toBeNull();
  });
});

describe("splitByteRange", () => {
  it("splits an ASCII page exactly where the locator says", () => {
    const text = "Invoice Total: 1296.00";
    expect(splitByteRange(text, 15, 22)).toEqual(["Invoice Total: ", "1296.00", ""]);
  });

  // The orchestrator locates values with Go's strings.Index, which counts bytes;
  // slicing the JS string by those numbers drifts once the page holds any
  // multi-byte character, which invoices routinely do.
  it("honours byte offsets when earlier characters are multi-byte", () => {
    const text = "Total — €1,200 due";
    const start = new TextEncoder().encode("Total — ").length;
    const end = start + new TextEncoder().encode("€1,200").length;
    expect(splitByteRange(text, start, end)).toEqual(["Total — ", "€1,200", " due"]);
    // The naive code-unit slice picks up the wrong span entirely.
    expect(text.slice(start, end)).not.toBe("€1,200");
  });

  it("clamps offsets that no longer fit the page", () => {
    const text = "short";
    expect(splitByteRange(text, 2, 999)).toEqual(["sh", "ort", ""]);
    expect(splitByteRange(text, -5, 2)).toEqual(["", "sh", "ort"]);
  });

  it("yields an empty span when the range is inverted or empty", () => {
    expect(splitByteRange("abcdef", 4, 2)).toEqual(["abcd", "", "ef"]);
    expect(splitByteRange("abcdef", 3, 3)).toEqual(["abc", "", "def"]);
  });
});
