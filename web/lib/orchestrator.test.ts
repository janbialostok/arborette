import { afterEach, describe, expect, it, vi } from "vitest";
import {
  AuthRedirected,
  OrchestratorError,
  correctCausalEdge,
  errorMessage,
  getCausalGraph,
  getMe,
  getOutcomeExcerpt,
  isExtraction,
  listCausalVerifications,
  listDatasets,
  listGoals,
  listOutcomes,
  listVerifications,
  login,
  registerAuthRedirect,
  registerUser,
  resolveVerification,
  searchHeuristics,
  verifyFinding,
  type CausalGraph,
  type CausalVerification,
  type ExtractionOutcome,
  type GoalListItem,
  type HeuristicMatch,
  type OutcomeExcerpt,
  type TripletPayload,
  type VerificationList,
} from "./orchestrator";

afterEach(() => {
  registerAuthRedirect(null);
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

function unauthorizedResponse(): Response {
  return new Response(JSON.stringify({ error: "unauthorized" }), {
    status: 401,
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

describe("auth-redirect seam", () => {
  it("invokes the registered handler once and throws AuthRedirected on a session-secured 401", async () => {
    const handler = vi.fn();
    registerAuthRedirect(handler);
    mockFetch(unauthorizedResponse());

    await expect(searchHeuristics("x")).rejects.toBeInstanceOf(AuthRedirected);
    expect(handler).toHaveBeenCalledTimes(1);
  });

  it("never surfaces a view error on a redirected 401", async () => {
    registerAuthRedirect(() => {});
    mockFetch(unauthorizedResponse());

    const err = await searchHeuristics("x").catch((e: unknown) => e);
    expect(err).toBeInstanceOf(AuthRedirected);
    expect(err).not.toBeInstanceOf(OrchestratorError);
  });

  it("surfaces the 401 as a plain OrchestratorError when no handler is registered", async () => {
    registerAuthRedirect(null);
    mockFetch(unauthorizedResponse());

    await expect(searchHeuristics("x")).rejects.toMatchObject({
      name: "OrchestratorError",
      status: 401,
    });
  });

  it("registerAuthRedirect(null) clears the handler so a later 401 is not redirected", async () => {
    const handler = vi.fn();
    registerAuthRedirect(handler);
    registerAuthRedirect(null);
    mockFetch(unauthorizedResponse());

    await expect(searchHeuristics("x")).rejects.toMatchObject({ status: 401 });
    expect(handler).not.toHaveBeenCalled();
  });

  it("a non-401 failure never invokes the handler", async () => {
    const handler = vi.fn();
    registerAuthRedirect(handler);
    mockFetch(
      new Response(JSON.stringify({ error: "boom" }), {
        status: 500,
        headers: { "content-type": "application/json" },
      }),
    );

    await expect(searchHeuristics("x")).rejects.toMatchObject({ status: 500 });
    expect(handler).not.toHaveBeenCalled();
  });
});

describe("session-secured 401 routing", () => {
  it("getMe answering 401 invokes the handler and throws AuthRedirected", async () => {
    const handler = vi.fn();
    registerAuthRedirect(handler);
    mockFetch(unauthorizedResponse());

    await expect(getMe()).rejects.toBeInstanceOf(AuthRedirected);
    expect(handler).toHaveBeenCalledTimes(1);
  });

  it("listDatasets answering 401 invokes the handler and throws AuthRedirected", async () => {
    const handler = vi.fn();
    registerAuthRedirect(handler);
    mockFetch(unauthorizedResponse());

    await expect(listDatasets()).rejects.toBeInstanceOf(AuthRedirected);
    expect(handler).toHaveBeenCalledTimes(1);
  });

  it("login answering 401 keeps its inline bad-credentials verdict without invoking the handler", async () => {
    const handler = vi.fn();
    registerAuthRedirect(handler);
    mockFetch(unauthorizedResponse());

    await expect(login("alice", "wrong")).rejects.toMatchObject({
      name: "OrchestratorError",
      status: 401,
    });
    expect(handler).not.toHaveBeenCalled();
  });

  it("registerUser answering 401 keeps its inline verdict without invoking the handler", async () => {
    const handler = vi.fn();
    registerAuthRedirect(handler);
    mockFetch(unauthorizedResponse());

    await expect(registerUser("alice", "weak")).rejects.toMatchObject({
      name: "OrchestratorError",
      status: 401,
    });
    expect(handler).not.toHaveBeenCalled();
  });

  it("a transport failure with no status never invokes the handler (FR-010)", async () => {
    const handler = vi.fn();
    registerAuthRedirect(handler);
    vi.stubGlobal(
      "fetch",
      vi.fn().mockRejectedValue(new TypeError("fetch failed")),
    );

    await expect(getMe()).rejects.toBeInstanceOf(TypeError);
    expect(handler).not.toHaveBeenCalled();
  });

  it("a 200 getMe never invokes the handler (FR-010 false-sign-out guard)", async () => {
    const handler = vi.fn();
    registerAuthRedirect(handler);
    mockFetch(
      jsonResponse({
        id: "u1",
        username: "alice",
        role: "admin",
        active: true,
        created_at: "2026-01-01T00:00:00Z",
        updated_at: "2026-01-01T00:00:00Z",
        admin_notice: false,
      }),
    );

    await expect(getMe()).resolves.toMatchObject({ username: "alice" });
    expect(handler).not.toHaveBeenCalled();
  });

  it("a non-401 failure on a session-secured call never invokes the handler nor throws AuthRedirected", async () => {
    const handler = vi.fn();
    registerAuthRedirect(handler);
    mockFetch(
      new Response(JSON.stringify({ error: "forbidden" }), {
        status: 403,
        headers: { "content-type": "application/json" },
      }),
    );

    await expect(listGoals()).rejects.toMatchObject({
      name: "OrchestratorError",
      status: 403,
    });
    expect(handler).not.toHaveBeenCalled();
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

describe("correctCausalEdge", () => {
  it("posts the analyst's edit as JSON", async () => {
    const fetchMock = mockFetch(
      jsonResponse({ op: "flip", columns: ["Z", "X"], graph_version: 2, redispatched: 0 }),
    );

    await expect(
      correctCausalEdge("g1", { op: "flip", from: "Z", to: "X" }),
    ).resolves.toEqual({
      op: "flip",
      columns: ["Z", "X"],
      graph_version: 2,
      redispatched: 0,
    });

    const [url, init] = fetchMock.mock.calls[0];
    expect(url).toBe("/api/orchestrator/goals/g1/causal-graph/corrections");
    expect(init).toMatchObject({
      method: "POST",
      headers: { "content-type": "application/json" },
    });
    expect(JSON.parse(init.body as string)).toEqual({
      op: "flip",
      from: "Z",
      to: "X",
    });
  });

  // Withdrawing a direction is the one correction that cannot be read off from→to,
  // so the explicit field has to reach the wire.
  it("carries an explicit direction through to the body", async () => {
    const fetchMock = mockFetch(
      jsonResponse({ op: "flip", columns: [], graph_version: 3, redispatched: 0 }),
    );

    await correctCausalEdge("g1", {
      op: "flip",
      from: "X",
      to: "Z",
      direction: "undirected",
    });

    expect(JSON.parse(fetchMock.mock.calls[0][1].body as string)).toEqual({
      op: "flip",
      from: "X",
      to: "Z",
      direction: "undirected",
    });
  });

  it("surfaces the in-progress conflict verbatim, with its status", async () => {
    mockFetch(
      jsonResponse(
        { error: "another correction or discovery is in progress for this goal" },
        409,
      ),
    );

    await expect(
      correctCausalEdge("g1", { op: "delete", from: "X", to: "Z" }),
    ).rejects.toMatchObject({
      name: "OrchestratorError",
      message: "another correction or discovery is in progress for this goal",
      status: 409,
    });
  });
});

describe("verifyFinding", () => {
  // Two ids, two segments: swapping or under-encoding either one dispatches a
  // verification for a finding nobody asked about.
  it("posts to the finding's own path, escaping both ids", async () => {
    const fetchMock = mockFetch(
      jsonResponse({ optimization_function_id: "g 1", intervention_id: "i/1" }, 202),
    );

    await expect(verifyFinding("g 1", "i/1")).resolves.toEqual({
      optimization_function_id: "g 1",
      intervention_id: "i/1",
    });

    const [url, init] = fetchMock.mock.calls[0];
    expect(url).toBe("/api/orchestrator/goals/g%201/findings/i%2F1/verify");
    expect(init).toMatchObject({ method: "POST" });
  });

  it("surfaces an unavailable verifier verbatim", async () => {
    mockFetch(jsonResponse({ error: "verifier unavailable" }, 502));

    await expect(verifyFinding("g1", "i1")).rejects.toMatchObject({
      message: "verifier unavailable",
      status: 502,
    });
  });
});

describe("getCausalGraph", () => {
  it("parses the discovered model", async () => {
    const graph: CausalGraph = {
      columns: [{ name: "X", kind: "numeric" }],
      edges: [
        {
          col_a: "X",
          col_b: "Y",
          direction: "a_to_b",
          provenance: "statistical",
          confidence: 1,
          status: "tested",
        },
      ],
      meta: {
        version: 2,
        excluded_columns: [],
        budget_truncated: false,
        test_count: 57,
        discovered_at: "2026-08-04T22:06:38Z",
      },
    };
    const fetchMock = mockFetch(jsonResponse(graph));

    await expect(getCausalGraph("g 1")).resolves.toEqual(graph);
    expect(fetchMock.mock.calls[0][0]).toBe(
      "/api/orchestrator/goals/g%201/causal-graph",
    );
  });

  // The 404 is this view's empty state, so its status has to survive the throw for
  // the caller to tell "none discovered yet" from a fault.
  it("rejects with a 404 the caller can recognize", async () => {
    mockFetch(
      jsonResponse({ error: "no causal graph discovered for this goal yet" }, 404),
    );

    await expect(getCausalGraph("g1")).rejects.toMatchObject({
      name: "OrchestratorError",
      message: "no causal graph discovered for this goal yet",
      status: 404,
    });
  });
});

describe("listCausalVerifications", () => {
  // The nullable effects are the whole point of the DTO: a record short of a
  // terminal outcome must arrive as null, never as a misleading zero.
  it("parses a record whose effects are not measured yet", async () => {
    const records: CausalVerification[] = [
      {
        id: "v1",
        intervention_id: "i1",
        objective_label: "",
        filters: [],
        graph_version: 2,
        status: "pending",
        naive_effect: null,
        adjusted_effect: null,
        adjustment_set: [],
        refutation_score: null,
        confidence: null,
        stale: false,
        created_at: "2026-08-04T22:06:51Z",
        updated_at: "2026-08-04T22:06:51Z",
      },
    ];
    const fetchMock = mockFetch(jsonResponse(records));

    await expect(listCausalVerifications("g1")).resolves.toEqual(records);
    expect(fetchMock.mock.calls[0][0]).toBe(
      "/api/orchestrator/goals/g1/causal-verifications",
    );
  });
});
