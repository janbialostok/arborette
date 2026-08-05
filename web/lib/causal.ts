// The causal surfaces' domain logic, kept out of the components so the rules an
// analyst reads a verdict through — which findings have earned a causal reading,
// what an outcome means in plain language, what a correction does to the served
// graph, and which stream frames carry a causal transition — are testable on their
// own. The layout is here for the same reason.

import type {
  CausalColumn,
  CausalGraph,
  CausalGraphEdge,
  CausalVerification,
  CausalVerificationStatus,
  CorrectionResult,
  EdgeCorrection,
  EdgeDirection,
  OrchestratorEvent,
} from "./orchestrator";

// How a finding's effect was established. The web UI distinguishes only these
// two: a backdoor-adjusted effect that survived refutation over the discovered
// graph, and everything else — a measured correlation. A value it does not
// recognize, or none at all, reads as observational, which is the honest floor.
export type EpistemicSource = "observational" | "causal_inferred";

// The qualifier a causal reading is never rendered without: the effect is
// supported by adjustment and refutation over a model that was itself discovered,
// not proven by an executed intervention.
export const CAUSAL_INFERRED_CAVEAT =
  "causally supported (given the discovered model)";

// The same limit spelled out where a verdict has room for a sentence.
export const SUFFICIENCY_CAVEAT =
  "given the discovered model; unmeasured confounding cannot be ruled out";

// The mark a correction leaves on what it invalidated, worn by both the edges whose
// verifications were re-dispatched and the records themselves — one label so the two
// surfaces cannot come to describe the same state differently.
export const REVERIFYING_LABEL = "re-verifying";

// latestByIntervention indexes a goal's verification records by the finding they
// verify, keeping the newest per finding. Only the newest speaks for a finding: a
// correction re-verifies it against the corrected graph, and a later confounded
// verdict supersedes an earlier causal one rather than sitting beside it.
export function latestByIntervention(
  verifications: CausalVerification[],
): Map<string, CausalVerification> {
  const latest = new Map<string, CausalVerification>();
  for (const record of verifications) {
    const held = latest.get(record.intervention_id);
    if (!held || isNewer(record, held)) latest.set(record.intervention_id, record);
  }
  return latest;
}

// The list arrives newest-first, so an unparseable timestamp keeps whichever
// record the server ranked first rather than letting a NaN comparison decide.
function isNewer(record: CausalVerification, held: CausalVerification): boolean {
  const at = Date.parse(record.created_at);
  const heldAt = Date.parse(held.created_at);
  if (Number.isNaN(at) || Number.isNaN(heldAt)) return false;
  return at > heldAt;
}

// epistemicSource reads a finding's standing off its newest verification. A stale
// record describes a model the analyst has already corrected, so it grants nothing
// until it is re-verified; an absent one means the finding was measured and never
// verified, which is exactly observational.
export function epistemicSource(
  record: CausalVerification | undefined,
): EpistemicSource {
  const verified = record?.status === "causally_verified" && !record.stale;
  return verified ? "causal_inferred" : "observational";
}

// The badge tones a verdict can wear, mirrored here rather than imported so the
// domain rules stay free of the component layer.
export type OutcomeTone = "positive" | "neutral" | "warn" | "negative";

// OutcomeCopy is one verdict as the analyst reads it: what it concluded, what that
// means for the decision they are about to make, and how loudly to say it. The tone
// travels with the copy so a new status needs one row, not two in two places.
export interface OutcomeCopy {
  headline: string;
  detail: string;
  tone: OutcomeTone;
}

const OUTCOME_COPY: Record<CausalVerificationStatus, OutcomeCopy> = {
  pending: {
    headline: "Verifying…",
    detail:
      "Adjusting the effect for the confounders the discovered graph names, then trying to break the result.",
    tone: "neutral",
  },
  causally_verified: {
    headline: "Causally supported (given the discovered model)",
    detail: `The adjusted effect held up under the refutation battery — ${SUFFICIENCY_CAVEAT}.`,
    tone: "positive",
  },
  confounded: {
    headline: "Explained by a shared cause — not causal",
    detail:
      "Adjusting for the confounders moved the effect, or the refutation battery broke it: the segment and the outcome move together because something else drives both. Acting on the segment should not be expected to move the objective.",
    tone: "warn",
  },
  not_identifiable: {
    headline: "Can't be answered from this data (no valid adjustment)",
    detail:
      "No set of measured columns blocks every back-door path, so no amount of adjustment isolates this effect. Correcting the graph, or measuring the column it is missing, is what changes that.",
    tone: "warn",
  },
  unsupported_objective: {
    headline: "This objective type can't be causally verified",
    detail:
      "Causal verification needs a numeric aggregate measured over a segment; this objective does not reduce to one.",
    tone: "warn",
  },
  failed: {
    headline: "Verification failed",
    detail:
      "The run ended without reaching a verdict. Nothing about the finding changed, so re-verifying it is safe.",
    tone: "negative",
  },
};

// outcomeCopy maps a status to its analyst-facing reading, rendering a status this
// build has never heard of verbatim instead of dropping the record: a newer
// verifier must be able to report an outcome without this view hiding it.
export function outcomeCopy(status: string): OutcomeCopy {
  return (
    OUTCOME_COPY[status as CausalVerificationStatus] ?? {
      headline: status || "unknown outcome",
      detail:
        "This verdict came from a newer version of the verifier than this view knows how to explain.",
      tone: "neutral",
    }
  );
}

// VerificationView is how one record reads once the goal's other records are taken
// into account. A finding's newest verification is the one that speaks for it, so a
// record a newer one replaced is history: it never grants a causal reading, and it
// is not "re-verifying" either, because the re-verification it was waiting for is
// the very record that superseded it.
export interface VerificationView {
  superseded: boolean;
  reverifying: boolean;
  source: EpistemicSource;
}

export function verificationView(
  record: CausalVerification,
  latest: Map<string, CausalVerification>,
): VerificationView {
  const superseded = latest.get(record.intervention_id)?.id !== record.id;
  return {
    superseded,
    reverifying: record.stale && !superseded,
    source: superseded ? "observational" : epistemicSource(record),
  };
}

// inFlight reports whether one record still has verification coming for it: a
// record a correction invalidated, or the freshly dispatched replacement, which is
// pending rather than stale — the re-dispatch writes a NEW record at the corrected
// graph version instead of updating the one it invalidates, so reading staleness
// alone would call the work finished the moment it started. An absent record has
// nothing in flight, which is what lets a caller ask about a finding it has never
// seen a verification for.
export function inFlight(record: CausalVerification | undefined): boolean {
  return record != null && (record.stale || record.status === "pending");
}

// awaitingVerdict is inFlight over a goal. It reads the newest record per finding:
// a superseded stale record lives in the list forever.
export function awaitingVerdict(latest: Map<string, CausalVerification>): boolean {
  return [...latest.values()].some(inFlight);
}

// canonicalPair orders two column names the way every edge key and pair
// comparison does, so a correction the analyst states as cause→effect lands on the
// edge the graph holds.
export function canonicalPair(a: string, b: string): [string, string] {
  return a <= b ? [a, b] : [b, a];
}

export function edgeKey(edge: CausalGraphEdge): string {
  return `${edge.col_a}→${edge.col_b}`;
}

// causeEffect resolves an edge's endpoints into cause and effect, or null when the
// pair was left unoriented — which is a claim about the data, not missing data, and
// is drawn as a plain line.
export function causeEffect(edge: CausalGraphEdge): [string, string] | null {
  if (edge.direction === "a_to_b") return [edge.col_a, edge.col_b];
  if (edge.direction === "b_to_a") return [edge.col_b, edge.col_a];
  return null;
}

// directionGlyph is how an edge's orientation reads in a line of text: an arrow when
// the tests (or the analyst) settled it, a dash when nothing did.
export function directionGlyph(direction: EdgeDirection): string {
  if (direction === "a_to_b") return "→";
  if (direction === "b_to_a") return "←";
  return "—";
}

// GraphTone is what a drawn edge or node is currently saying about itself, ordered by
// precedence: a selection outranks a pending re-verification, which outranks nothing
// in particular. The paint each tone wears belongs to the renderer; the precedence is
// here because both the edges and the nodes apply it, and the nodes apply it to
// adjacency rather than selection — one rule serving two notions of "highlighted".
export type GraphTone = "idle" | "selected" | "stale";

export function graphTone(highlighted: boolean, stale: boolean): GraphTone {
  if (highlighted) return "selected";
  if (stale) return "stale";
  return "idle";
}

// directionForCause encodes cause→effect over the pair's canonical order.
export function directionForCause(cause: string, effect: string): EdgeDirection {
  const [colA] = canonicalPair(cause, effect);
  return colA === cause ? "a_to_b" : "b_to_a";
}

// orientCorrection asserts cause→effect on a pair the graph already holds. This is
// the correction that unblocks an unidentifiable effect: an unoriented edge blocks
// every adjustment across it, and only the analyst can say which way it runs.
export function orientCorrection(cause: string, effect: string): EdgeCorrection {
  return { op: "flip", from: cause, to: effect };
}

// flipCorrection asserts the opposite of what an edge currently claims. An edge the
// statistics left unoriented has no opposite, so a flip on one is the analyst's
// first orientation of that pair rather than a reversal.
export function flipCorrection(edge: CausalGraphEdge): EdgeCorrection {
  const oriented = causeEffect(edge);
  if (!oriented) return orientCorrection(edge.col_a, edge.col_b);
  const [cause, effect] = oriented;
  return orientCorrection(effect, cause);
}

// unorientCorrection returns an edge to unoriented: the analyst rejecting an
// orientation without rejecting the association behind it. Nothing can adjust
// across an unoriented edge, so this is deliberately the conservative edit.
export function unorientCorrection(edge: CausalGraphEdge): EdgeCorrection {
  return {
    op: "flip",
    from: edge.col_a,
    to: edge.col_b,
    direction: "undirected",
  };
}

// deleteCorrection removes an edge the data suggested and the analyst knows is not
// there. Direction is irrelevant to a delete, so the pair is named as it is held.
export function deleteCorrection(edge: CausalGraphEdge): EdgeCorrection {
  return { op: "delete", from: edge.col_a, to: edge.col_b };
}

// addCorrection states an edge discovery missed — typically the confounder that
// explains a spurious finding.
export function addCorrection(cause: string, effect: string): EdgeCorrection {
  return { op: "add", from: cause, to: effect };
}

// correctedDirection is the orientation a correction asserts: the explicit one when
// the analyst chose it, otherwise the one from→to encodes.
export function correctedDirection(correction: EdgeCorrection): EdgeDirection {
  return correction.direction || directionForCause(correction.from, correction.to);
}

// applyCorrection is the correction's effect on the served graph, applied locally
// so the edge moves under the analyst's hand instead of after a round trip. It
// stamps the same analyst/tested/full-confidence edge the write does — an analyst
// asserting an edge must not leave it fail-closed — and the refetch that follows
// is still authoritative.
export function applyCorrection(
  graph: CausalGraph,
  correction: EdgeCorrection,
  result: CorrectionResult,
): CausalGraph {
  const [colA, colB] = canonicalPair(correction.from, correction.to);
  const meta = { ...graph.meta, version: result.graph_version };
  const held = graph.edges.findIndex((e) => e.col_a === colA && e.col_b === colB);

  if (correction.op === "delete") {
    return { ...graph, edges: graph.edges.filter((_, i) => i !== held), meta };
  }
  const corrected = {
    direction: correctedDirection(correction),
    provenance: "analyst",
    status: "tested",
    confidence: 1,
  };
  const edges =
    held === -1
      ? [...graph.edges, { col_a: colA, col_b: colB, ...corrected }]
      : graph.edges.map((e, i) => (i === held ? { ...e, ...corrected } : e));
  return { ...graph, edges, meta };
}

// touchesColumns reports whether a correction's columns include either end of an
// edge — the edges whose verifications that correction just invalidated.
export function touchesColumns(
  edge: CausalGraphEdge,
  columns: ReadonlySet<string>,
): boolean {
  return columns.has(edge.col_a) || columns.has(edge.col_b);
}

// causalTransition names the transition a stream frame carries for these surfaces,
// or null for a frame they have no stake in. The Verifier's own transitions arrive
// wrapped, so a consumer reading only the outer type would miss every one of them —
// including the outcome that fills a card in.
export function causalTransition(ev: OrchestratorEvent): string | null {
  switch (ev.type) {
    case "verification": {
      const name = ev.payload?.type;
      return typeof name === "string" && name !== "" ? name : null;
    }
    case "causal_verification_dispatched":
    case "causal_graph_corrected":
      return ev.type;
    default:
      return null;
  }
}

// correctedColumns pulls the touched columns out of a correction frame, which is
// how a correction made in another session marks edges here as re-verifying.
export function correctedColumns(ev: OrchestratorEvent): string[] {
  if (ev.type !== "causal_graph_corrected") return [];
  const columns = ev.payload?.columns;
  if (!Array.isArray(columns)) return [];
  return columns.filter((c): c is string => typeof c === "string");
}

// Node geometry, in the SVG's own units.
export const NODE_WIDTH = 132;
export const NODE_HEIGHT = 34;
// How many characters of a column name the box holds at its mono label size.
const NODE_LABEL_CHARS = 18;

// clipLabel cuts a column name to what a node box can hold. The box is fixed-width,
// so a longer name is read in full from the edge list and the hover title instead.
export function clipLabel(name: string): string {
  return name.length > NODE_LABEL_CHARS
    ? `${name.slice(0, NODE_LABEL_CHARS - 1)}…`
    : name;
}
const LAYER_GAP = 84;
const ROW_GAP = 16;
const PADDING = 20;
// Lines stop short of a node so an arrowhead lands beside the box, not on it.
const EDGE_GAP = 7;

// A column at its rendered position. x/y are the box's center, which is what the
// edge geometry is measured from.
export interface GraphNode {
  name: string;
  kind: string;
  layer: number;
  x: number;
  y: number;
}

// One edge as a drawn segment, already clipped to both boxes. `directed` says
// whether it earns an arrowhead; `from`/`to` are cause and effect when it does.
export interface GraphLine {
  edge: CausalGraphEdge;
  key: string;
  from: string;
  to: string;
  directed: boolean;
  x1: number;
  y1: number;
  x2: number;
  y2: number;
}

export interface GraphLayout {
  width: number;
  height: number;
  nodes: GraphNode[];
  lines: GraphLine[];
}

// layoutGraph places the columns in causal layers — every column to the right of
// its causes — and clips each edge to the two boxes it joins. Unoriented edges
// constrain nothing, so a graph the statistics left mostly undirected lays out as
// one column, which is an honest picture of it.
export function layoutGraph(graph: CausalGraph): GraphLayout {
  const layers = layerColumns(graph);
  const rows = Math.max(1, ...layers.map((l) => l.length));
  const contentHeight = rows * NODE_HEIGHT + (rows - 1) * ROW_GAP;
  const width =
    PADDING * 2 + layers.length * NODE_WIDTH + (layers.length - 1) * LAYER_GAP;
  const height = PADDING * 2 + contentHeight;

  const nodes: GraphNode[] = [];
  layers.forEach((names, layer) => {
    const span = names.length * NODE_HEIGHT + (names.length - 1) * ROW_GAP;
    const top = PADDING + (contentHeight - span) / 2;
    names.forEach((column, row) => {
      nodes.push({
        name: column.name,
        kind: column.kind,
        layer,
        x: PADDING + layer * (NODE_WIDTH + LAYER_GAP) + NODE_WIDTH / 2,
        y: top + row * (NODE_HEIGHT + ROW_GAP) + NODE_HEIGHT / 2,
      });
    });
  });

  const placed = new Map(nodes.map((n) => [n.name, n]));
  const lines: GraphLine[] = [];
  for (const edge of graph.edges) {
    const oriented = causeEffect(edge);
    const [fromName, toName] = oriented ?? [edge.col_a, edge.col_b];
    const from = placed.get(fromName);
    const to = placed.get(toName);
    if (!from || !to) continue;
    const start = boundaryPoint(from, to);
    const end = boundaryPoint(to, from);
    lines.push({
      edge,
      key: edgeKey(edge),
      from: fromName,
      to: toName,
      directed: oriented !== null,
      x1: start.x,
      y1: start.y,
      x2: end.x,
      y2: end.y,
    });
  }
  return { width: Math.max(width, PADDING * 2 + NODE_WIDTH), height, nodes, lines };
}

// layerColumns assigns each column the layer one past its deepest cause, so an
// edge always points rightward and a chain reads as one. Cycles are reachable —
// an analyst can flip an edge into one — so the columns the sweep cannot settle
// are placed after the ones it did rather than dropped or stacked at the origin.
function layerColumns(graph: CausalGraph): CausalColumn[][] {
  const columns = graph.columns;
  const effects = new Map<string, string[]>();
  const causeCount = new Map<string, number>();
  for (const column of columns) {
    effects.set(column.name, []);
    causeCount.set(column.name, 0);
  }
  for (const edge of graph.edges) {
    const oriented = causeEffect(edge);
    if (!oriented) continue;
    const [cause, effect] = oriented;
    if (!effects.has(cause) || !causeCount.has(effect)) continue;
    effects.get(cause)!.push(effect);
    causeCount.set(effect, causeCount.get(effect)! + 1);
  }

  const layer = new Map(columns.map((c) => [c.name, 0]));
  const settled = columns.filter((c) => causeCount.get(c.name) === 0).map((c) => c.name);
  const reached = new Set(settled);
  for (let i = 0; i < settled.length; i++) {
    const name = settled[i];
    for (const effect of effects.get(name) ?? []) {
      // Longest path, not last writer: the layering must not depend on the order the
      // sweep happens to settle a column's causes in.
      layer.set(effect, Math.max(layer.get(effect) ?? 0, (layer.get(name) ?? 0) + 1));
      const remaining = (causeCount.get(effect) ?? 0) - 1;
      causeCount.set(effect, remaining);
      if (remaining === 0 && !reached.has(effect)) {
        reached.add(effect);
        settled.push(effect);
      }
    }
  }
  const deepest = Math.max(0, ...[...reached].map((name) => layer.get(name) ?? 0));
  for (const column of columns) {
    if (!reached.has(column.name)) layer.set(column.name, deepest + 1);
  }

  const rows: CausalColumn[][] = [];
  for (const column of columns) {
    const index = layer.get(column.name) ?? 0;
    (rows[index] ??= []).push(column);
  }
  // A graph that is one whole cycle settles nothing, so every column lands on the
  // fallback layer and the layers before it are empty — dropping them is what keeps
  // the diagram from opening with a column's width of blank canvas.
  return rows.filter((row) => row !== undefined && row.length > 0);
}

// boundaryPoint is where the center-to-center line leaves the `from` box, pulled
// out by the edge gap. Scaling the offset by whichever axis reaches the boundary
// first is what keeps a line from cutting a corner.
function boundaryPoint(from: GraphNode, to: GraphNode): { x: number; y: number } {
  const dx = to.x - from.x;
  const dy = to.y - from.y;
  const halfWidth = NODE_WIDTH / 2 + EDGE_GAP;
  const halfHeight = NODE_HEIGHT / 2 + EDGE_GAP;
  const reach = Math.max(Math.abs(dx) / halfWidth, Math.abs(dy) / halfHeight);
  if (reach === 0) return { x: from.x, y: from.y };
  return { x: from.x + dx / reach, y: from.y + dy / reach };
}
