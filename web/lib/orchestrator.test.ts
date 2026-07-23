import { afterEach, describe, expect, it, vi } from "vitest";
import {
  OrchestratorError,
  errorMessage,
  searchHeuristics,
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
