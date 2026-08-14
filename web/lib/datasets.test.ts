import { describe, expect, it } from "vitest";
import { canAdminister, canRegisterGoal, filterDatasets, isInUse, isOwner, isSharedToMe } from "./datasets";
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
    owner_id: "u1",
    access: "owner",
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

describe("ownership vocabulary", () => {
  const u1 = "u1";
  const u2 = "u2";

  it("isOwner is true only for the acting user's own row", () => {
    expect(isOwner(ds({ access: "owner", owner_id: u1 }), u1)).toBe(true);
    expect(isOwner(ds({ access: "owner", owner_id: u2 }), u1)).toBe(false);
    expect(isOwner(ds({ access: "shared", owner_id: u1 }), u1)).toBe(false);
    expect(isOwner(ds({ access: "none", owner_id: u1 }), u1)).toBe(false);
  });

  it("isSharedToMe distinguishes the collaborator relationship", () => {
    expect(isSharedToMe(ds({ access: "shared" }))).toBe(true);
    expect(isSharedToMe(ds({ access: "owner" }))).toBe(false);
    expect(isSharedToMe(ds({ access: "none" }))).toBe(false);
  });

  it("canAdminister requires an attributed owner identity", () => {
    expect(canAdminister(ds({ access: "owner", owner_id: u1 }), u1)).toBe(true);
    expect(canAdminister(ds({ access: "owner", owner_id: u2 }), u1)).toBe(false);
    expect(canAdminister(ds({ access: "shared", owner_id: u1 }), u1)).toBe(false);
    expect(canAdminister(ds({ access: "owner", owner_id: null }), u1)).toBe(false);
  });

  it("canRegisterGoal opens explicitly shared rows to collaborators but never to outsiders", () => {
    expect(canRegisterGoal(ds({ access: "owner", owner_id: u1 }), u1)).toBe(true);
    expect(canRegisterGoal(ds({ access: "shared" }), u1)).toBe(true);
    expect(canRegisterGoal(ds({ access: "none" }), u1)).toBe(false);
    expect(canRegisterGoal(ds({ access: "shared" }), u2)).toBe(true);
  });
});
