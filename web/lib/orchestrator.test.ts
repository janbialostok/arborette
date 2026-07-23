import { afterEach, describe, expect, it, vi } from "vitest";
import {
  OrchestratorError,
  errorMessage,
  listGoals,
  searchHeuristics,
  type GoalListItem,
  type HeuristicMatch,
} from "./orchestrator";

afterEach(() => {
  vi.unstubAllGlobals();
});

function mockFetch(response: Response) {
  vi.stubGlobal("fetch", vi.fn().mockResolvedValue(response));
}

describe("errorMessage", () => {
  it("surfaces an OrchestratorError message verbatim", () => {
    const err = new OrchestratorError("q is required", 400);
    expect(errorMessage(err, "fallback")).toBe("q is required");
  });

  it("masks any non-OrchestratorError behind the fallback", () => {
    expect(errorMessage(new TypeError("fetch failed"), "fallback")).toBe(
      "fallback",
    );
    expect(errorMessage("boom", "fallback")).toBe("fallback");
  });
});

describe("requestJSON via searchHeuristics", () => {
  it("returns the parsed body on a 2xx response", async () => {
    const rows: HeuristicMatch[] = [{ id: "a", definition: "d" }];
    mockFetch(
      new Response(JSON.stringify(rows), {
        status: 200,
        headers: { "content-type": "application/json" },
      }),
    );
    await expect(searchHeuristics("x")).resolves.toEqual(rows);
  });

  it("throws an OrchestratorError carrying the orchestrator's {error} body verbatim", async () => {
    mockFetch(
      new Response(JSON.stringify({ error: "q is required" }), {
        status: 400,
        headers: { "content-type": "application/json" },
      }),
    );
    await expect(searchHeuristics("")).rejects.toMatchObject({
      name: "OrchestratorError",
      message: "q is required",
      status: 400,
    });
  });

  it("masks a non-JSON error body behind a generic message", async () => {
    mockFetch(new Response("upstream boom", { status: 502 }));
    await expect(searchHeuristics("x")).rejects.toMatchObject({
      message: "request failed (502)",
      status: 502,
    });
  });

  it("falls through to the generic message when JSON lacks a usable error field", async () => {
    mockFetch(
      new Response(JSON.stringify({ detail: "nope" }), {
        status: 500,
        headers: { "content-type": "application/json" },
      }),
    );
    await expect(searchHeuristics("x")).rejects.toMatchObject({
      message: "request failed (500)",
      status: 500,
    });
  });
});

describe("listGoals", () => {
  it("returns the parsed objectives on a 2xx response", async () => {
    const rows: GoalListItem[] = [
      {
        optimization_function_id: "g1",
        goal_text: "Maximize order value",
        created_at: "2026-07-23T12:00:00Z",
        status: "completed",
      },
      {
        optimization_function_id: "g2",
        goal_text: "Reduce churn",
        created_at: "2026-07-23T13:00:00Z",
        status: "failed",
        failure_reason: "field not present in schema",
      },
    ];
    mockFetch(
      new Response(JSON.stringify(rows), {
        status: 200,
        headers: { "content-type": "application/json" },
      }),
    );
    await expect(listGoals()).resolves.toEqual(rows);
  });

  it("throws an OrchestratorError carrying the orchestrator's {error} body verbatim", async () => {
    mockFetch(
      new Response(JSON.stringify({ error: "internal error" }), {
        status: 500,
        headers: { "content-type": "application/json" },
      }),
    );
    await expect(listGoals()).rejects.toMatchObject({
      name: "OrchestratorError",
      message: "internal error",
      status: 500,
    });
  });
});
