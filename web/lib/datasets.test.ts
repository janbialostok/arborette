import { describe, expect, it } from "vitest";
import { filterDatasets, isInUse } from "./datasets";
import type { DatasetSummary } from "./orchestrator";

function ds(overrides: Partial<DatasetSummary> = {}): DatasetSummary {
  return {
    id: "d1",
    name: "sales-2024",
    description: "",
    status: "active",
    usage: "empty",
    objective_count: 0,
    created_at: "2026-08-04T22:06:38Z",
    updated_at: "2026-08-04T22:06:38Z",
    data_source_ref: "datasources/u/data.csv",
    last_accessed_at: null,
    ...overrides,
  };
}

describe("isInUse", () => {
  it("reads an in-use dataset from the derived usage", () => {
    expect(isInUse(ds({ usage: "in_use" }))).toBe(true);
    expect(isInUse(ds({ usage: "empty" }))).toBe(false);
  });

  it("treats a nonzero objective count as in use even if usage is stale", () => {
    expect(isInUse(ds({ usage: "empty", objective_count: 3 }))).toBe(true);
  });
});

describe("filterDatasets", () => {
  it("is case-insensitive over the name substring", () => {
    const rows = [ds({ name: "Sales 2024" }), ds({ name: "contracts" })];
    expect(filterDatasets(rows, "SALES")).toEqual([rows[0]]);
    expect(filterDatasets(rows, " 2024 ")).toEqual([rows[0]]);
  });

  it("returns the list unchanged for an empty or whitespace query", () => {
    const rows = [ds(), ds({ name: "other" })];
    expect(filterDatasets(rows, "")).toBe(rows);
    expect(filterDatasets(rows, "   ")).toBe(rows);
  });

  it("returns no rows when nothing matches", () => {
    expect(filterDatasets([ds()], "nothing")).toEqual([]);
  });
});
