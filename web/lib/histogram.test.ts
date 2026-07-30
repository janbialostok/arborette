import { describe, expect, it } from "vitest";
import { histogramColumns } from "./histogram";

describe("histogramColumns", () => {
  it("normalizes every column against the fullest bucket", () => {
    const columns = histogramColumns([0, 5, 10, 0]);
    expect(columns.map((c) => c.pct)).toEqual([0, 44, 88, 0]);
  });

  it("labels only the fullest bucket, and the first one on a tie", () => {
    const columns = histogramColumns([3, 7, 7]);
    expect(columns.map((c) => c.labeled)).toEqual([false, true, false]);
  });

  it("gives a bucket holding one outcome a visible floor", () => {
    const [small] = histogramColumns([1, 400]);
    expect(small.pct).toBe(3);
  });

  it("renders an all-zero distribution without dividing by zero", () => {
    const columns = histogramColumns(Array(10).fill(0));
    expect(columns.every((c) => c.pct === 0)).toBe(true);
    expect(columns.every((c) => !c.labeled)).toBe(true);
    expect(columns.every((c) => Number.isFinite(c.pct))).toBe(true);
  });

  // The bucket count is the orchestrator's to choose; deriving the width from
  // what arrived keeps the labels honest if it ever changes.
  it("derives bucket ranges from how many buckets arrived", () => {
    expect(histogramColumns(Array(10).fill(0)).map((c) => c.range)).toEqual([
      "0.0–0.1", "0.1–0.2", "0.2–0.3", "0.3–0.4", "0.4–0.5",
      "0.5–0.6", "0.6–0.7", "0.7–0.8", "0.8–0.9", "0.9–1.0",
    ]);
    expect(histogramColumns([1, 2, 3, 4]).map((c) => c.range)).toEqual([
      "0.0–0.3", "0.3–0.5", "0.5–0.8", "0.8–1.0",
    ]);
  });

  it("returns nothing for an empty distribution", () => {
    expect(histogramColumns([])).toEqual([]);
  });
});
