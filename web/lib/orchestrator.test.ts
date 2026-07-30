import { afterEach, describe, expect, it, vi } from "vitest";
import {
  OrchestratorError,
  errorMessage,
  getOutcomeExcerpt,
  isExtraction,
  listGoals,
  listOutcomes,
  listVerifications,
  resolveVerification,
  searchHeuristics,
  type ExtractionOutcome,
  type GoalListItem,
  type HeuristicMatch,
  type OutcomeExcerpt,
  type TripletPayload,
  type VerificationList,
} from "./orchestrator";

afterEach(() => {
  vi.unstubAllGlobals();
});

function mockFetch(response: Response) {
  const fetchMock = vi.fn().mockResolvedValue(response);
  vi.stubGlobal("fetch", fetchMock);
  return fetchMock;
}

function jsonResponse(body: unknown, status = 200): Response {
  return new Response(JSON.stringify(body), {
    status,
    headers: { "content-type": "application/json" },
  });
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

// These pin the two triplet wire shapes verbatim.
describe("isExtraction", () => {
  const measured: TripletPayload = {
    state_id: "s1",
    intervention_id: "i1",
    outcome_id: "o1",
    baseline: 10,
    value: 12,
    effect_size: 2,
    objective_label: "avg order value",
    direction: "maximize",
    filters: ["region = NE"],
    new_filters: ["region = NE"],
  };
  const extracted: TripletPayload = {
    state_id: "s1",
    intervention_id: "i1",
    outcome_id: "o1",
    field: "invoice_total",
    method: "Read the document top to bottom.",
    value: "1296.00",
    confidence: 0.99,
    provenance: { page: 0, char_start: 195, char_end: 202 },
  };

  it("tells an extracted field from a measured effect", () => {
    expect(isExtraction(extracted)).toBe(true);
    expect(isExtraction(measured)).toBe(false);
  });

  it("narrows to the extraction shape, including an unlocated value", () => {
    const unlocated = { ...extracted, provenance: null };
    if (!isExtraction(unlocated)) throw new Error("expected an extraction");
    expect(unlocated.field).toBe("invoice_total");
    expect(unlocated.provenance).toBeNull();
  });

  it("does not mistake a measured triplet carrying a zero effect", () => {
    expect(isExtraction({ ...measured, effect_size: 0, filters: [] })).toBe(false);
  });
});

describe("listVerifications", () => {
  it("parses the queue alongside the settings that produced it", async () => {
    const list: VerificationList = {
      effective_threshold: 0.75,
      epoch_mode: "speculative",
      entries: [
        {
          queue_id: "q1",
          outcome_id: "o1",
          field: "total_value",
          extracted_value: "$1,200",
          provenance: { page: 0, char_start: 12, char_end: 18 },
          confidence: 0.42,
          status: "pending",
          created_at: "2026-07-29T09:00:00Z",
        },
        {
          queue_id: "q2",
          outcome_id: "o2",
          field: "total_value",
          extracted_value: "$900",
          provenance: null,
          confidence: 0.6,
          status: "resolved",
          resolution: "corrected",
          corrected_value: "$960",
          created_at: "2026-07-29T09:01:00Z",
          resolved_at: "2026-07-29T09:05:00Z",
        },
      ],
    };
    const fetchMock = mockFetch(jsonResponse(list));
    await expect(listVerifications("g 1")).resolves.toEqual(list);
    expect(fetchMock.mock.calls[0][0]).toBe(
      "/api/orchestrator/goals/g%201/verifications",
    );
  });

  // Unfiltered, the queue returns every row it has ever held, so the work list
  // has to ask for the unresolved ones by name.
  it("narrows to the rows still awaiting a verdict when asked", async () => {
    const fetchMock = mockFetch(
      jsonResponse({ effective_threshold: 0.8, epoch_mode: "speculative", entries: [] }),
    );
    await listVerifications("g1", "pending");
    expect(fetchMock.mock.calls[0][0]).toBe(
      "/api/orchestrator/goals/g1/verifications?status=pending",
    );
  });
});

describe("listOutcomes", () => {
  it("parses an extraction whose value is keyed by field", async () => {
    const outcomes: ExtractionOutcome[] = [
      {
        outcome_id: "o1",
        field: "total_value",
        method: "document_extraction",
        value: { total_value: "$1,200" },
        provenance: { page: 2, char_start: 0, char_end: 6 },
        verification_status: "unverified",
        confidence: 0.91,
      },
    ];
    const fetchMock = mockFetch(jsonResponse(outcomes));
    await expect(listOutcomes("g 1")).resolves.toEqual(outcomes);
    expect(fetchMock.mock.calls[0][0]).toBe(
      "/api/orchestrator/goals/g%201/outcomes",
    );
  });
});

describe("getOutcomeExcerpt", () => {
  it("parses a located excerpt with its page context", async () => {
    const excerpt: OutcomeExcerpt = {
      fallback: false,
      page: 1,
      char_start: 10,
      char_end: 16,
      excerpt: "$1,200",
      page_text: "Total due $1,200 on receipt.",
    };
    const fetchMock = mockFetch(jsonResponse(excerpt));
    await expect(getOutcomeExcerpt("g1", "o/1")).resolves.toEqual(excerpt);
    expect(fetchMock.mock.calls[0][0]).toBe(
      "/api/orchestrator/goals/g1/outcomes/o%2F1/excerpt",
    );
  });

  it("parses the whole-document fallback shape", async () => {
    const excerpt: OutcomeExcerpt = {
      fallback: true,
      page: 0,
      char_start: 0,
      char_end: 0,
      pages: ["page one text", "page two text"],
    };
    mockFetch(jsonResponse(excerpt));
    const got = await getOutcomeExcerpt("g1", "o1");
    expect(got.fallback).toBe(true);
    expect(got.pages).toEqual(["page one text", "page two text"]);
    expect(got.page_text).toBeUndefined();
  });
});

describe("resolveVerification", () => {
  it("posts the verdict and its corrected value as JSON", async () => {
    const fetchMock = mockFetch(
      jsonResponse({
        outcome_id: "o1",
        resolution: "corrected",
        verification_status: "corrected",
        confidence: 1,
      }),
    );
    await expect(
      resolveVerification("g1", "o1", "correct", "$1,250"),
    ).resolves.toEqual({
      outcome_id: "o1",
      resolution: "corrected",
      verification_status: "corrected",
      confidence: 1,
    });
    const [url, init] = fetchMock.mock.calls[0];
    expect(url).toBe("/api/orchestrator/goals/g1/verifications/o1");
    expect(init).toMatchObject({
      method: "POST",
      headers: { "content-type": "application/json" },
    });
    expect(JSON.parse(init.body as string)).toEqual({
      action: "correct",
      corrected_value: "$1,250",
    });
  });

  it("sends an empty corrected value for a verdict that takes none", async () => {
    const fetchMock = mockFetch(
      jsonResponse({
        outcome_id: "o1",
        resolution: "confirmed",
        verification_status: "confirmed",
        confidence: 1,
      }),
    );
    await resolveVerification("g1", "o1", "confirm");
    const [, init] = fetchMock.mock.calls[0];
    expect(JSON.parse(init.body as string)).toEqual({
      action: "confirm",
      corrected_value: "",
    });
  });

  it("surfaces the already-resolved conflict verbatim, with its status", async () => {
    mockFetch(
      jsonResponse(
        {
          error:
            'this outcome was already resolved as corrected with the value "$960"; that resolution was completed and the requested action was not applied',
        },
        409,
      ),
    );
    await expect(
      resolveVerification("g1", "o1", "confirm"),
    ).rejects.toMatchObject({
      name: "OrchestratorError",
      message:
        'this outcome was already resolved as corrected with the value "$960"; that resolution was completed and the requested action was not applied',
      status: 409,
    });
  });

  it("surfaces the missing-corrected-value rejection verbatim", async () => {
    mockFetch(
      jsonResponse(
        { error: "corrected_value is required to correct an extraction" },
        422,
      ),
    );
    await expect(
      resolveVerification("g1", "o1", "correct"),
    ).rejects.toMatchObject({
      message: "corrected_value is required to correct an extraction",
      status: 422,
    });
  });
});
