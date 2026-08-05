import { describe, expect, it } from "vitest";
import {
  formatConfidence,
  formatNumber,
  formatTimestamp,
  renderValue,
  shortId,
} from "./format";

describe("renderValue", () => {
  it("renders an absent value as an em dash and an object as its JSON", () => {
    expect(renderValue(null)).toBe("—");
    expect(renderValue(undefined)).toBe("—");
    expect(renderValue({ a: 1 })).toBe('{"a":1}');
    expect(renderValue(12)).toBe("12");
    expect(renderValue(false)).toBe("false");
  });
});

describe("formatNumber", () => {
  // The orchestrator sends null for an unmeasured effect; a measured zero is a
  // finding of its own.
  it("separates an unmeasured value from a measured zero", () => {
    expect(formatNumber(null)).toBe("—");
    expect(formatNumber(undefined)).toBe("—");
    expect(formatNumber(0)).toBe("0");
  });

  it("renders a non-finite result as an em dash", () => {
    expect(formatNumber(Number.NaN)).toBe("—");
    expect(formatNumber(Number.POSITIVE_INFINITY)).toBe("—");
    expect(formatNumber(Number.NEGATIVE_INFINITY)).toBe("—");
  });

  it("holds more precision the smaller the magnitude", () => {
    expect(formatNumber(1234.567)).toBe("1,234.6");
    expect(formatNumber(12.3456)).toBe("12.35");
    expect(formatNumber(0.123456)).toBe("0.1235");
    expect(formatNumber(-0.123456)).toBe("-0.1235");
  });

  it("renders an integer without inventing decimals", () => {
    expect(formatNumber(42)).toBe("42");
    expect(formatNumber(1234567)).toBe("1,234,567");
  });
});

describe("formatConfidence", () => {
  it("pins every score to two decimals", () => {
    expect(formatConfidence(0.9)).toBe("0.90");
    expect(formatConfidence(0.9513219660557305)).toBe("0.95");
    expect(formatConfidence(1)).toBe("1.00");
    expect(formatConfidence(0)).toBe("0.00");
  });

  it("renders an unmeasured score as an em dash", () => {
    expect(formatConfidence(null)).toBe("—");
    expect(formatConfidence(undefined)).toBe("—");
    expect(formatConfidence(Number.NaN)).toBe("—");
  });
});

describe("shortId", () => {
  it("trims a uuid to the prefix that tells two records apart", () => {
    expect(shortId("2a0ca15c-bb25-48c0-9e97-7d9576d8f14c")).toBe("2a0ca15c");
    expect(shortId("123456789")).toBe("12345678");
  });

  it("leaves an id no longer than the prefix alone", () => {
    expect(shortId("12345678")).toBe("12345678");
    expect(shortId("abc")).toBe("abc");
    expect(shortId("")).toBe("");
  });
});

describe("formatTimestamp", () => {
  it("renders a server timestamp in the reader's locale", () => {
    const rendered = formatTimestamp("2026-08-04T22:06:38Z");
    expect(rendered).not.toBe("2026-08-04T22:06:38Z");
    expect(rendered).toBe(new Date("2026-08-04T22:06:38Z").toLocaleString());
  });

  it("falls back to the raw string it could not parse", () => {
    expect(formatTimestamp("not a date")).toBe("not a date");
    expect(formatTimestamp("")).toBe("");
  });
});
