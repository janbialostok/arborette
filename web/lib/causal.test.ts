import { describe, expect, it } from "vitest";
import {
  CAUSAL_INFERRED_CAVEAT,
  SUFFICIENCY_CAVEAT,
  addCorrection,
  applyCorrection,
  awaitingVerdict,
  causalTransition,
  clipLabel,
  correctedColumns,
  deleteCorrection,
  directionGlyph,
  edgeKey,
  epistemicSource,
  flipCorrection,
  graphTone,
  inFlight,
  latestByIntervention,
  layoutGraph,
  orientCorrection,
  outcomeCopy,
  touchesColumns,
  unorientCorrection,
  verificationView,
  NODE_HEIGHT,
  NODE_WIDTH,
} from "./causal";
import type {
  CausalGraph,
  CausalGraphEdge,
  CausalVerification,
  CausalVerificationStatus,
  OrchestratorEvent,
} from "./orchestrator";

function verification(
  over: Partial<CausalVerification> & { intervention_id: string },
): CausalVerification {
  return {
    id: `v-${over.intervention_id}-${over.created_at ?? "0"}`,
    objective_label: "avg(gpa_change)",
    filters: ["prior_gpa < 3"],
    graph_version: 1,
    status: "causally_verified",
    naive_effect: 2,
    adjusted_effect: 1.5,
    adjustment_set: ["region"],
    refutation_score: 0.9,
    confidence: 0.9,
    stale: false,
    created_at: "2026-08-04T10:00:00Z",
    updated_at: "2026-08-04T10:05:00Z",
    ...over,
  };
}

function edge(over: Partial<CausalGraphEdge> = {}): CausalGraphEdge {
  return {
    col_a: "ad_spend",
    col_b: "revenue",
    direction: "a_to_b",
    provenance: "statistical",
    confidence: 0.8,
    status: "tested",
    ...over,
  };
}

function graph(over: Partial<CausalGraph> = {}): CausalGraph {
  return {
    columns: [
      { name: "ad_spend", kind: "numeric" },
      { name: "revenue", kind: "numeric" },
    ],
    edges: [edge()],
    meta: {
      version: 3,
      excluded_columns: [],
      budget_truncated: false,
      test_count: 12,
      discovered_at: "2026-08-04T09:00:00Z",
    },
    ...over,
  };
}

describe("latestByIntervention", () => {
  it("keeps the newest record per finding", () => {
    const older = verification({
      intervention_id: "i1",
      created_at: "2026-08-04T10:00:00Z",
      status: "causally_verified",
    });
    const newer = verification({
      intervention_id: "i1",
      created_at: "2026-08-04T11:00:00Z",
      status: "confounded",
    });
    const other = verification({ intervention_id: "i2" });

    const latest = latestByIntervention([newer, older, other]);

    expect(latest.get("i1")).toBe(newer);
    expect(latest.get("i2")).toBe(other);
    expect(latest.size).toBe(2);
  });

  // The list arrives newest-first, so an unusable timestamp must not promote an
  // older record over the one the server ranked first.
  it("falls back to the server's ordering when a timestamp is unparseable", () => {
    const first = verification({ intervention_id: "i1", created_at: "not a date" });
    const second = verification({
      intervention_id: "i1",
      created_at: "2026-08-04T09:00:00Z",
      status: "confounded",
    });

    expect(latestByIntervention([first, second]).get("i1")).toBe(first);
  });
});

describe("epistemicSource", () => {
  it("reads a fresh causally_verified record as a causal finding", () => {
    expect(epistemicSource(verification({ intervention_id: "i1" }))).toBe(
      "causal_inferred",
    );
  });

  it("withholds the causal reading from a stale record", () => {
    const stale = verification({ intervention_id: "i1", stale: true });
    expect(epistemicSource(stale)).toBe("observational");
  });

  it("reads every other outcome, and no record at all, as observational", () => {
    const others: CausalVerificationStatus[] = [
      "pending",
      "confounded",
      "not_identifiable",
      "unsupported_objective",
      "failed",
    ];
    for (const status of others) {
      expect(epistemicSource(verification({ intervention_id: "i1", status }))).toBe(
        "observational",
      );
    }
    expect(
      epistemicSource(verification({ intervention_id: "i1", status: "quantum_verified" })),
    ).toBe("observational");
    expect(epistemicSource(undefined)).toBe("observational");
  });
});

describe("verificationView", () => {
  const older = verification({
    intervention_id: "i1",
    id: "old",
    created_at: "2026-08-04T10:00:00Z",
    stale: true,
  });
  const newer = verification({
    intervention_id: "i1",
    id: "new",
    created_at: "2026-08-04T11:00:00Z",
  });

  // The record a re-verification replaced is history, not work in flight: the
  // re-verification it was waiting for IS the record that superseded it.
  it("reads a superseded record as history rather than as re-verifying", () => {
    const latest = latestByIntervention([newer, older]);

    expect(verificationView(older, latest)).toEqual({
      superseded: true,
      reverifying: false,
      source: "observational",
    });
  });

  it("reads the newest record as the one that speaks for the finding", () => {
    const latest = latestByIntervention([newer, older]);

    expect(verificationView(newer, latest)).toEqual({
      superseded: false,
      reverifying: false,
      source: "causal_inferred",
    });
  });

  // A causally_verified record with no confounder to adjust for is never marked
  // stale (staleness matches on adjustment-set overlap), yet any correction bumps
  // the graph version and a later verify writes a record that supersedes it. Being
  // superseded alone has to withdraw the causal reading: otherwise that card keeps
  // the green badge, the accented effect and the "read as causal" footer for a
  // number measured against a model the analyst has already replaced.
  it("withdraws the causal reading from a fresh record a newer one replaced", () => {
    const replaced = verification({
      intervention_id: "i1",
      id: "replaced",
      created_at: "2026-08-04T10:00:00Z",
      status: "causally_verified",
      adjustment_set: [],
      stale: false,
    });
    const replacement = verification({
      intervention_id: "i1",
      id: "replacement",
      created_at: "2026-08-04T12:00:00Z",
      status: "confounded",
    });
    const latest = latestByIntervention([replacement, replaced]);

    // Read on its own the record still looks causal — only the newer sibling says
    // otherwise, which is exactly what this view exists to apply.
    expect(epistemicSource(replaced)).toBe("causal_inferred");
    expect(verificationView(replaced, latest)).toEqual({
      superseded: true,
      reverifying: false,
      source: "observational",
    });
  });

  it("marks a stale record with nothing newer as re-verifying", () => {
    const latest = latestByIntervention([older]);

    expect(verificationView(older, latest)).toEqual({
      superseded: false,
      reverifying: true,
      source: "observational",
    });
  });
});

describe("inFlight", () => {
  // Reading "absent" as in flight would disable Verify on every finding nobody has
  // verified yet — the surface's only action.
  it("reports nothing in flight for a finding with no verification", () => {
    expect(inFlight(undefined)).toBe(false);
  });

  it("reports a stale or pending record as in flight, and a settled one as not", () => {
    expect(inFlight(verification({ intervention_id: "i1", stale: true }))).toBe(true);
    expect(inFlight(verification({ intervention_id: "i1", status: "pending" }))).toBe(
      true,
    );
    expect(inFlight(verification({ intervention_id: "i1" }))).toBe(false);
    expect(
      inFlight(verification({ intervention_id: "i1", status: "confounded" })),
    ).toBe(false);
  });
});

describe("awaitingVerdict", () => {
  // A correction's re-dispatch writes a NEW pending record rather than updating the
  // stale one, so reading staleness alone would call the work finished the instant
  // it started — and clear the re-verifying marks while the run is still going.
  it("counts a freshly dispatched replacement as still waiting", () => {
    const pending = verification({ intervention_id: "i1", status: "pending" });
    expect(awaitingVerdict(latestByIntervention([pending]))).toBe(true);
  });

  it("counts a stale record as still waiting", () => {
    const stale = verification({ intervention_id: "i1", stale: true });
    expect(awaitingVerdict(latestByIntervention([stale]))).toBe(true);
  });

  it("reports nothing waiting once every finding has a settled verdict", () => {
    const settled = [
      verification({ intervention_id: "i1" }),
      verification({ intervention_id: "i2", status: "confounded" }),
    ];
    expect(awaitingVerdict(latestByIntervention(settled))).toBe(false);
    expect(awaitingVerdict(new Map())).toBe(false);
  });
});

describe("outcomeCopy", () => {
  it("reads every status in analyst language", () => {
    expect(outcomeCopy("causally_verified").headline).toBe(
      "Causally supported (given the discovered model)",
    );
    expect(outcomeCopy("causally_verified").detail).toContain(SUFFICIENCY_CAVEAT);
    expect(outcomeCopy("confounded").headline).toContain("shared cause");
    expect(outcomeCopy("not_identifiable").headline).toContain(
      "Can't be answered from this data",
    );
    expect(outcomeCopy("unsupported_objective").headline).toContain(
      "can't be causally verified",
    );
    expect(outcomeCopy("failed").headline).toBe("Verification failed");
    expect(outcomeCopy("pending").headline).toBe("Verifying…");
  });

  // The tone is what the badge wears, and it is the difference between a confounded
  // verdict reading as a warning and reading as an endorsement. It lives beside the
  // copy so one table decides both; that only helps if the table is asserted.
  it("carries a tone that matches what the verdict says", () => {
    expect(outcomeCopy("causally_verified").tone).toBe("positive");
    expect(outcomeCopy("confounded").tone).toBe("warn");
    expect(outcomeCopy("not_identifiable").tone).toBe("warn");
    expect(outcomeCopy("unsupported_objective").tone).toBe("warn");
    expect(outcomeCopy("failed").tone).toBe("negative");
    expect(outcomeCopy("pending").tone).toBe("neutral");
    expect(outcomeCopy("mechanistically_verified").tone).toBe("neutral");
  });

  // A verifier release can add an outcome; the card must still render, naming what
  // it was told rather than hiding the record.
  it("renders an unknown status verbatim", () => {
    expect(outcomeCopy("mechanistically_verified").headline).toBe(
      "mechanistically_verified",
    );
    expect(outcomeCopy("").headline).toBe("unknown outcome");
  });

  it("keeps the caveat the causal badge carries", () => {
    expect(CAUSAL_INFERRED_CAVEAT).toBe(
      "causally supported (given the discovered model)",
    );
  });
});

describe("correction builders", () => {
  it("flips an oriented edge to the opposite claim", () => {
    expect(flipCorrection(edge({ direction: "a_to_b" }))).toEqual({
      op: "flip",
      from: "revenue",
      to: "ad_spend",
    });
    expect(flipCorrection(edge({ direction: "b_to_a" }))).toEqual({
      op: "flip",
      from: "ad_spend",
      to: "revenue",
    });
  });

  // An unoriented edge has no opposite, so the flip is the analyst's first
  // orientation of the pair rather than a reversal of nothing.
  it("orients an unoriented edge along the canonical pair", () => {
    for (const direction of ["undirected", "unknown"] as const) {
      expect(flipCorrection(edge({ direction }))).toEqual({
        op: "flip",
        from: "ad_spend",
        to: "revenue",
      });
    }
  });

  // Nothing can be adjusted across an unoriented edge, so orienting one is the
  // correction that makes a not-identifiable effect answerable — in either
  // direction, since only the analyst knows which way it runs.
  it("orients a pair either way", () => {
    expect(orientCorrection("Z", "X")).toEqual({ op: "flip", from: "Z", to: "X" });
    expect(orientCorrection("X", "Z")).toEqual({ op: "flip", from: "X", to: "Z" });
  });

  // Withdrawing a direction keeps the association and blocks adjustment across it,
  // which an explicit direction is the only way to express.
  it("returns an edge to unoriented explicitly", () => {
    expect(unorientCorrection(edge({ direction: "a_to_b" }))).toEqual({
      op: "flip",
      from: "ad_spend",
      to: "revenue",
      direction: "undirected",
    });
  });

  it("names the pair as held for a delete and as cause→effect for an add", () => {
    expect(deleteCorrection(edge({ direction: "b_to_a" }))).toEqual({
      op: "delete",
      from: "ad_spend",
      to: "revenue",
    });
    expect(addCorrection("season", "revenue")).toEqual({
      op: "add",
      from: "season",
      to: "revenue",
    });
  });
});

describe("applyCorrection", () => {
  const result = { op: "flip", columns: [], graph_version: 4, redispatched: 1 };

  it("re-orients the edge in place and stamps it as the analyst's", () => {
    const next = applyCorrection(graph(), flipCorrection(edge()), result);

    expect(next.edges).toEqual([
      {
        col_a: "ad_spend",
        col_b: "revenue",
        direction: "b_to_a",
        provenance: "analyst",
        confidence: 1,
        status: "tested",
      },
    ]);
    expect(next.meta.version).toBe(4);
  });

  it("removes the edge a delete names", () => {
    const next = applyCorrection(graph(), deleteCorrection(edge()), result);
    expect(next.edges).toEqual([]);
  });

  it("appends an added edge under its canonical pair", () => {
    const withSeason = graph({
      columns: [
        { name: "ad_spend", kind: "numeric" },
        { name: "revenue", kind: "numeric" },
        { name: "season", kind: "categorical" },
      ],
    });

    const next = applyCorrection(
      withSeason,
      addCorrection("season", "revenue"),
      result,
    );

    expect(next.edges).toHaveLength(2);
    expect(next.edges[1]).toEqual({
      col_a: "revenue",
      col_b: "season",
      direction: "b_to_a",
      provenance: "analyst",
      confidence: 1,
      status: "tested",
    });
  });

  // An add naming a pair that already exists is a re-orientation of it, which is
  // what the write does too — appending a second edge for the pair would double it.
  it("re-orients rather than duplicating when the added pair already exists", () => {
    const next = applyCorrection(
      graph(),
      addCorrection("revenue", "ad_spend"),
      result,
    );
    expect(next.edges).toHaveLength(1);
    expect(next.edges[0].direction).toBe("b_to_a");
  });

  // Withdrawing a direction is the one correction that cannot be read off from→to,
  // so an explicit direction must win over the cause→effect encoding. Without this
  // the optimistic graph would draw the opposite of what the analyst asserted.
  it("honors an explicit direction over the from→to reading", () => {
    const next = applyCorrection(graph(), unorientCorrection(edge()), result);

    expect(next.edges[0].direction).toBe("undirected");
    expect(next.edges[0].provenance).toBe("analyst");
  });

  // Both halves of the canonical-pair encoding decide which way an edge is drawn,
  // and a correction stated in either order has to land on the right one.
  it("encodes cause→effect in both directions of the canonical pair", () => {
    const forward = applyCorrection(
      graph(),
      orientCorrection("ad_spend", "revenue"),
      result,
    );
    const backward = applyCorrection(
      graph(),
      orientCorrection("revenue", "ad_spend"),
      result,
    );

    expect(forward.edges[0].direction).toBe("a_to_b");
    expect(backward.edges[0].direction).toBe("b_to_a");
  });

  it("leaves an unaffected edge untouched", () => {
    const two = graph({
      columns: [
        { name: "ad_spend", kind: "numeric" },
        { name: "revenue", kind: "numeric" },
        { name: "season", kind: "categorical" },
      ],
      edges: [edge(), edge({ col_a: "revenue", col_b: "season", provenance: "llm_prior" })],
    });

    const next = applyCorrection(two, deleteCorrection(edge()), result);

    expect(next.edges).toEqual([two.edges[1]]);
  });
});

describe("edgeKey", () => {
  // The key is the selection identity behind flip and delete: a colliding key applies
  // an edit to an edge nobody selected.
  it("identifies an edge by its canonical pair", () => {
    expect(edgeKey(edge())).toBe("ad_spend→revenue");
    expect(edgeKey(edge({ col_a: "revenue", col_b: "season" }))).toBe("revenue→season");
  });

  // Direction is a property of the edge, not part of its identity, so flipping one
  // must not move the analyst's selection off it.
  it("does not change when the pair is re-oriented", () => {
    expect(edgeKey(edge({ direction: "a_to_b" }))).toBe(
      edgeKey(edge({ direction: "b_to_a" })),
    );
  });
});

describe("directionGlyph", () => {
  it("reads an oriented pair as an arrow and an unsettled one as a dash", () => {
    expect(directionGlyph("a_to_b")).toBe("→");
    expect(directionGlyph("b_to_a")).toBe("←");
    expect(directionGlyph("undirected")).toBe("—");
    expect(directionGlyph("unknown")).toBe("—");
  });
});

describe("clipLabel", () => {
  it("leaves a name the box can hold alone", () => {
    expect(clipLabel("revenue")).toBe("revenue");
    expect(clipLabel("eighteen_chars_abc")).toBe("eighteen_chars_abc");
  });

  // The ellipsis costs a character, so the cut has to make room for it rather than
  // pushing the label past the width it was measured against.
  it("cuts a longer name to the width, ellipsis included", () => {
    const clipped = clipLabel("nineteen_chars_abcd");
    expect(clipped).toBe("nineteen_chars_ab…");
    expect(clipped).toHaveLength(18);
  });
});

describe("graphTone", () => {
  // One precedence rule serves two notions of "highlighted": an edge asks about
  // selection, a node about adjacency to the selected edge.
  it("lets a highlight outrank a pending re-verification", () => {
    expect(graphTone(true, true)).toBe("selected");
    expect(graphTone(true, false)).toBe("selected");
    expect(graphTone(false, true)).toBe("stale");
    expect(graphTone(false, false)).toBe("idle");
  });
});

describe("touchesColumns", () => {
  it("matches an edge on either end", () => {
    expect(touchesColumns(edge(), new Set(["revenue"]))).toBe(true);
    expect(touchesColumns(edge(), new Set(["ad_spend"]))).toBe(true);
    expect(touchesColumns(edge(), new Set(["season"]))).toBe(false);
    expect(touchesColumns(edge(), new Set())).toBe(false);
  });
});

describe("causalTransition", () => {
  it("passes the frames the orchestrator emits itself", () => {
    expect(
      causalTransition({
        type: "causal_verification_dispatched",
        payload: { intervention_id: "i1" },
      }),
    ).toBe("causal_verification_dispatched");
    expect(
      causalTransition({
        type: "causal_graph_corrected",
        payload: { op: "flip", columns: ["revenue"], graph_version: 4, redispatched: 2 },
      }),
    ).toBe("causal_graph_corrected");
  });

  // A relayed Verifier frame carries the transition one level down; reading the
  // outer type alone would miss the outcome that fills a card in.
  it("resolves a wrapped verification frame to its own transition", () => {
    for (const name of [
      "verification_dispatched",
      "verification_refutation",
      "verification_outcome",
      "verification_rejected",
      "verification_error",
      "claim_reified",
      "claim_skipped",
      "claim_construction_failed",
    ]) {
      expect(
        causalTransition({ type: "verification", payload: { type: name } }),
      ).toBe(name);
    }
  });

  it("ignores a run frame and a wrapper carrying no transition", () => {
    expect(causalTransition({ type: "loop_complete" })).toBeNull();
    expect(
      causalTransition({
        type: "branch_failure",
        payload: { error: "sandbox timed out" },
      }),
    ).toBeNull();
    expect(
      causalTransition({ type: "verification", payload: { type: "" } }),
    ).toBeNull();
    expect(
      causalTransition({ type: "verification" } as OrchestratorEvent),
    ).toBeNull();
  });
});

describe("correctedColumns", () => {
  it("pulls the touched columns off a correction frame only", () => {
    expect(
      correctedColumns({
        type: "causal_graph_corrected",
        payload: { op: "add", columns: ["season", "revenue"], graph_version: 5, redispatched: 1 },
      }),
    ).toEqual(["season", "revenue"]);
    expect(
      correctedColumns({
        type: "verification",
        payload: { type: "verification_outcome" },
      }),
    ).toEqual([]);
  });

  it("tolerates a frame whose columns are missing or malformed", () => {
    const frame = {
      type: "causal_graph_corrected",
      payload: { op: "flip", graph_version: 5, redispatched: 0 },
    } as unknown as OrchestratorEvent;
    expect(correctedColumns(frame)).toEqual([]);

    const mixed = {
      type: "causal_graph_corrected",
      payload: { op: "flip", columns: ["revenue", 7, null], graph_version: 5, redispatched: 0 },
    } as unknown as OrchestratorEvent;
    expect(correctedColumns(mixed)).toEqual(["revenue"]);
  });
});

describe("layoutGraph", () => {
  it("layers a chain left to right", () => {
    const chain = graph({
      columns: [
        { name: "revenue", kind: "numeric" },
        { name: "ad_spend", kind: "numeric" },
        { name: "visits", kind: "numeric" },
      ],
      edges: [
        edge({ col_a: "ad_spend", col_b: "visits", direction: "a_to_b" }),
        edge({ col_a: "revenue", col_b: "visits", direction: "b_to_a" }),
      ],
    });

    const layout = layoutGraph(chain);
    const layerOf = new Map(layout.nodes.map((n) => [n.name, n.layer]));

    expect(layerOf.get("ad_spend")).toBe(0);
    expect(layerOf.get("visits")).toBe(1);
    expect(layerOf.get("revenue")).toBe(2);
    expect(layout.width).toBeGreaterThan(NODE_WIDTH * 3);
  });

  it("leaves an unoriented graph as one column", () => {
    const flat = graph({ edges: [edge({ direction: "undirected" })] });
    const layout = layoutGraph(flat);

    expect(layout.nodes.every((n) => n.layer === 0)).toBe(true);
    expect(layout.lines[0].directed).toBe(false);
  });

  // An effect belongs to the right of its DEEPEST cause, not merely to the right of
  // whichever cause the sweep reached first — otherwise a diamond draws an arrow
  // pointing backwards, the one thing this layout exists to prevent.
  it("places an effect past its deepest cause", () => {
    const diamond = graph({
      columns: [
        { name: "spend", kind: "numeric" },
        { name: "visits", kind: "numeric" },
        { name: "signups", kind: "numeric" },
        { name: "revenue", kind: "numeric" },
      ],
      edges: [
        edge({ col_a: "spend", col_b: "visits", direction: "a_to_b" }),
        edge({ col_a: "revenue", col_b: "spend", direction: "b_to_a" }),
        edge({ col_a: "signups", col_b: "visits", direction: "b_to_a" }),
        edge({ col_a: "revenue", col_b: "signups", direction: "b_to_a" }),
      ],
    });

    const layerOf = new Map(
      layoutGraph(diamond).nodes.map((n) => [n.name, n.layer]),
    );

    expect(layerOf.get("spend")).toBe(0);
    expect(layerOf.get("visits")).toBe(1);
    expect(layerOf.get("signups")).toBe(2);
    expect(layerOf.get("revenue")).toBe(3);
  });

  // The columns a cycle strands are placed AFTER everything the sweep settled, so
  // they never render on top of the chain that does have an order.
  it("places a cycle's columns past the chain the sweep settled", () => {
    const mixed = graph({
      columns: [
        { name: "spend", kind: "numeric" },
        { name: "visits", kind: "numeric" },
        { name: "loop_a", kind: "numeric" },
        { name: "loop_b", kind: "numeric" },
      ],
      edges: [
        edge({ col_a: "spend", col_b: "visits", direction: "a_to_b" }),
        edge({ col_a: "loop_a", col_b: "loop_b", direction: "a_to_b" }),
        edge({ col_a: "loop_a", col_b: "loop_b", direction: "b_to_a" }),
      ],
    });

    const layerOf = new Map(layoutGraph(mixed).nodes.map((n) => [n.name, n.layer]));

    expect(layerOf.get("spend")).toBe(0);
    expect(layerOf.get("visits")).toBe(1);
    expect(layerOf.get("loop_a")).toBe(2);
    expect(layerOf.get("loop_b")).toBe(2);
  });

  it("spaces the nodes within a layer", () => {
    const layout = layoutGraph(
      graph({ edges: [edge({ direction: "undirected" })] }),
    );
    const [first, second] = layout.nodes;

    expect(first.x).toBe(second.x);
    expect(second.y - first.y).toBeGreaterThanOrEqual(NODE_HEIGHT);
  });

  it("places the columns a cycle leaves unsettled", () => {
    const cyclic = graph({
      edges: [
        edge({ col_a: "ad_spend", col_b: "revenue", direction: "a_to_b" }),
        edge({ col_a: "ad_spend", col_b: "revenue", direction: "b_to_a" }),
      ],
    });

    const layout = layoutGraph(cyclic);

    expect(layout.nodes).toHaveLength(2);
    expect(layout.nodes.every((n) => Number.isFinite(n.x) && Number.isFinite(n.y))).toBe(
      true,
    );
  });

  // The segment is clipped to both boxes, so an arrowhead lands beside a node
  // rather than inside it.
  it("clips each edge to the boxes it joins", () => {
    const layout = layoutGraph(graph());
    const [line] = layout.lines;
    const [from, to] = layout.nodes;

    expect(line.from).toBe("ad_spend");
    expect(line.to).toBe("revenue");
    expect(line.directed).toBe(true);
    expect(line.x1).toBeGreaterThan(from.x + NODE_WIDTH / 2);
    expect(line.x2).toBeLessThan(to.x - NODE_WIDTH / 2);
    expect(line.y1).toBeCloseTo(from.y);
  });

  it("drops an edge naming a column the graph does not list", () => {
    const dangling = graph({
      edges: [edge(), edge({ col_a: "ad_spend", col_b: "ghost_column" })],
    });

    expect(layoutGraph(dangling).lines).toHaveLength(1);
  });

  it("lays out an empty graph without collapsing", () => {
    const empty = graph({ columns: [], edges: [] });
    const layout = layoutGraph(empty);

    expect(layout.nodes).toEqual([]);
    expect(layout.lines).toEqual([]);
    expect(layout.width).toBeGreaterThanOrEqual(NODE_WIDTH);
    expect(layout.height).toBeGreaterThanOrEqual(NODE_HEIGHT);
  });
});
