"use client";

import { useState } from "react";
import {
  CAUSAL_INFERRED_CAVEAT,
  REVERIFYING_LABEL,
  addCorrection,
  applyCorrection,
  causeEffect,
  clipLabel,
  deleteCorrection,
  directionGlyph,
  edgeKey,
  flipCorrection,
  graphTone,
  layoutGraph,
  orientCorrection,
  touchesColumns,
  unorientCorrection,
  NODE_HEIGHT,
  NODE_WIDTH,
  type GraphLayout,
  type GraphLine,
  type GraphNode,
  type GraphTone,
} from "@/lib/causal";
import { formatConfidence, formatTimestamp } from "@/lib/format";
import {
  OrchestratorError,
  correctCausalEdge,
  errorMessage,
  type CausalGraph as Graph,
  type CausalGraphEdge,
  type CorrectionResult,
  type EdgeCorrection,
  type EdgeDirection,
} from "@/lib/orchestrator";
import { Badge, Button, Callout, cn, Panel, SectionLabel } from "@/components/ui";

type CorrectionAction =
  | "orient_a"
  | "orient_b"
  | "flip"
  | "unorient"
  | "delete"
  | "add";

// CausalGraph renders the model the verifier reasons over and lets the analyst
// correct it. Corrections are the point of the view, not an afterthought: an
// adjustment set is only as good as the graph it came from, so an analyst who
// knows an edge is backwards has to be able to say so and see the affected
// verifications re-run.
export function CausalGraph({
  goalID,
  graph,
  staleColumns,
  onCorrected,
  onConflict,
}: {
  goalID: string;
  graph: Graph;
  staleColumns: ReadonlySet<string>;
  onCorrected: (next: Graph, result: CorrectionResult) => void;
  onConflict: () => void;
}) {
  const [selectedKey, setSelectedKey] = useState<string | null>(null);
  // Which control is in flight, not which wire op it sends: four of the buttons
  // send `flip`, so keying the spinner on the op spins a control the analyst did
  // not press — the wrong signal entirely for a form whose subject is direction.
  const [pending, setPending] = useState<CorrectionAction | null>(null);
  const [error, setError] = useState<string | null>(null);

  const layout = layoutGraph(graph);
  const selected = graph.edges.find((e) => edgeKey(e) === selectedKey) ?? null;

  async function correct(action: CorrectionAction, correction: EdgeCorrection) {
    setError(null);
    setPending(action);
    try {
      const result = await correctCausalEdge(goalID, correction);
      onCorrected(applyCorrection(graph, correction, result), result);
      setSelectedKey(null);
    } catch (err) {
      setError(errorMessage(err, "Could not apply the correction. Please retry."));
      // A 409 means a discovery sweep or another analyst holds the graph: the
      // edit did not land, and the version this view was reading may already be
      // behind, so it is re-read rather than left looking authoritative.
      if (err instanceof OrchestratorError && err.status === 409) onConflict();
    } finally {
      setPending(null);
    }
  }

  return (
    <section className="flex flex-col gap-4">
      <div className="flex flex-wrap items-baseline justify-between gap-3">
        <SectionLabel>Discovered causal model</SectionLabel>
        <span className="font-mono text-[11px] text-faint">
          version <span className="tabular text-muted">{graph.meta.version}</span> ·{" "}
          <span className="tabular text-muted">{graph.meta.test_count}</span>{" "}
          independence test{graph.meta.test_count === 1 ? "" : "s"}
          {graph.meta.discovered_at &&
            ` · ${formatTimestamp(graph.meta.discovered_at)}`}
        </span>
      </div>

      <p className="max-w-2xl text-sm leading-relaxed text-muted">
        Discovered from observational data, so an effect measured against it reads as{" "}
        {CAUSAL_INFERRED_CAVEAT} — never as proven. Select an edge to correct it;
        verifications that relied on the columns you change are re-run.
      </p>

      <Coverage meta={graph.meta} />

      <Panel className="p-4">
        <Diagram
          layout={layout}
          selected={selected}
          staleColumns={staleColumns}
          onSelect={(edge) => setSelectedKey(edgeKey(edge))}
        />
      </Panel>

      <EdgeList
        edges={graph.edges}
        selectedKey={selectedKey}
        staleColumns={staleColumns}
        onSelect={(edge) =>
          setSelectedKey((held) => (held === edgeKey(edge) ? null : edgeKey(edge)))
        }
      />

      {selected && (
        <SelectedEdge
          edge={selected}
          pending={pending}
          onCorrect={correct}
          onDismiss={() => setSelectedKey(null)}
        />
      )}

      <AddEdge
        columns={graph.columns.map((c) => c.name)}
        pending={pending}
        onAdd={(correction) => correct("add", correction)}
      />

      {error && <Callout tone="error">{error}</Callout>}
    </section>
  );
}

// Coverage states what discovery did not look at. A column it never tested cannot
// appear in an adjustment set, so an analyst reading "no adjustment needed" has to
// be able to see whether that is a finding or a gap.
function Coverage({ meta }: { meta: Graph["meta"] }) {
  const excluded = meta.excluded_columns;
  if (!meta.budget_truncated && excluded.length === 0) return null;
  return (
    <Callout tone="warn">
      {meta.budget_truncated &&
        "Discovery stopped at its test budget, so some column pairs were never tested and stay unoriented. "}
      {excluded.length > 0 && (
        <>
          Outside the model: <span className="font-mono">{excluded.join(", ")}</span>.
          Nothing here can adjust for {excluded.length === 1 ? "it" : "them"}.
        </>
      )}
    </Callout>
  );
}

function Diagram({
  layout,
  selected,
  staleColumns,
  onSelect,
}: {
  layout: GraphLayout;
  selected: CausalGraphEdge | null;
  staleColumns: ReadonlySet<string>;
  onSelect: (edge: CausalGraphEdge) => void;
}) {
  if (layout.nodes.length === 0) {
    return (
      <p className="px-2 py-10 text-center text-sm text-faint">
        The discovered model holds no columns.
      </p>
    );
  }
  const selectedKey = selected ? edgeKey(selected) : null;
  return (
    <div className="overflow-x-auto">
      <svg
        width={layout.width}
        height={layout.height}
        viewBox={`0 0 ${layout.width} ${layout.height}`}
        className="block"
        role="img"
        aria-label="Discovered causal graph. The columns and edges are listed below."
      >
        <defs>
          {Object.values(TONES).map(({ marker, color }) => (
            <marker
              key={marker}
              id={marker}
              viewBox="0 0 8 8"
              refX="7"
              refY="4"
              markerWidth="6"
              markerHeight="6"
              orient="auto-start-reverse"
            >
              <path d="M 0 1 L 7 4 L 0 7 z" fill={color} />
            </marker>
          ))}
        </defs>

        {layout.lines.map((line) => (
          <Edge
            key={line.key}
            line={line}
            selected={line.key === selectedKey}
            stale={touchesColumns(line.edge, staleColumns)}
            onSelect={onSelect}
          />
        ))}
        {layout.nodes.map((node) => (
          <Node
            key={node.name}
            node={node}
            adjacent={
              selected != null &&
              (selected.col_a === node.name || selected.col_b === node.name)
            }
            stale={staleColumns.has(node.name)}
          />
        ))}
      </svg>
    </div>
  );
}

// The paint each tone wears. A marker cannot inherit its line's stroke in every
// browser, so a tone carries its own arrowhead declaration alongside its color; which
// tone applies is the precedence rule in lib, shared by the edges and the nodes.
const TONES: Record<GraphTone, { marker: string; color: string }> = {
  idle: { marker: "arrow-idle", color: "var(--color-line-strong)" },
  selected: { marker: "arrow-selected", color: "var(--color-signal)" },
  stale: { marker: "arrow-stale", color: "var(--color-warn)" },
};

function Edge({
  line,
  selected,
  stale,
  onSelect,
}: {
  line: GraphLine;
  selected: boolean;
  stale: boolean;
  onSelect: (edge: CausalGraphEdge) => void;
}) {
  const { marker, color } = TONES[graphTone(selected, stale)];
  return (
    <g
      onClick={() => onSelect(line.edge)}
      className="cursor-pointer"
      role="presentation"
    >
      <title>
        {line.directed
          ? `${line.from} causes ${line.to}`
          : `${line.from} and ${line.to} are related, direction unresolved`}
      </title>
      <line
        x1={line.x1}
        y1={line.y1}
        x2={line.x2}
        y2={line.y2}
        stroke={color}
        strokeWidth={2}
        strokeDasharray={line.directed ? undefined : "5 4"}
        markerEnd={line.directed ? `url(#${marker})` : undefined}
      />
      {/* A 2px line is far thinner than a pointer is accurate. */}
      <line
        x1={line.x1}
        y1={line.y1}
        x2={line.x2}
        y2={line.y2}
        stroke="transparent"
        strokeWidth={14}
      />
    </g>
  );
}

function Node({
  node,
  adjacent,
  stale,
}: {
  node: GraphNode;
  adjacent: boolean;
  stale: boolean;
}) {
  const { color } = TONES[graphTone(adjacent, stale)];
  return (
    <g>
      <title>
        {node.name} · {node.kind}
      </title>
      <rect
        x={node.x - NODE_WIDTH / 2}
        y={node.y - NODE_HEIGHT / 2}
        width={NODE_WIDTH}
        height={NODE_HEIGHT}
        rx={9}
        fill="var(--color-surface-2)"
        stroke={color}
        strokeWidth={adjacent || stale ? 1.5 : 1}
      />
      <text
        x={node.x}
        y={node.y}
        textAnchor="middle"
        dominantBaseline="central"
        fill="var(--color-fg)"
        className="font-mono text-[11px]"
      >
        {clipLabel(node.name)}
      </text>
    </g>
  );
}

function EdgeList({
  edges,
  selectedKey,
  staleColumns,
  onSelect,
}: {
  edges: CausalGraphEdge[];
  selectedKey: string | null;
  staleColumns: ReadonlySet<string>;
  onSelect: (edge: CausalGraphEdge) => void;
}) {
  if (edges.length === 0) {
    return (
      <p className="text-sm text-faint">
        No edges were established between these columns, so no effect measured here
        can be adjusted — every verification will report that it is not identifiable.
      </p>
    );
  }
  return (
    <div className="flex flex-col gap-2">
      <SectionLabel>
        Edges · {edges.length} · select one to correct it
      </SectionLabel>
      <ul className="flex flex-col gap-1.5">
        {edges.map((edge) => {
          const key = edgeKey(edge);
          const active = key === selectedKey;
          return (
            <li key={key}>
              <button
                type="button"
                onClick={() => onSelect(edge)}
                aria-pressed={active}
                className={cn(
                  "flex w-full flex-wrap items-center gap-x-3 gap-y-2 rounded-lg border px-4 py-2.5 text-left transition-colors",
                  active
                    ? "border-signal/50 bg-surface-2"
                    : "border-line bg-surface/60 hover:border-line-strong",
                )}
              >
                <span className="font-mono text-xs text-fg">
                  {edge.col_a} <span className="text-muted">{directionGlyph(edge.direction)}</span>{" "}
                  {edge.col_b}
                </span>
                <Badge tone={edge.provenance === "analyst" ? "positive" : "neutral"}>
                  {edge.provenance}
                </Badge>
                {edge.status !== "tested" && (
                  <Badge tone="warn">{edge.status}</Badge>
                )}
                {touchesColumns(edge, staleColumns) && (
                  <Badge tone="warn">{REVERIFYING_LABEL}</Badge>
                )}
                <span className="ml-auto font-mono text-[11px] tabular text-faint">
                  confidence {formatConfidence(edge.confidence)}
                </span>
              </button>
            </li>
          );
        })}
      </ul>
    </div>
  );
}

// An unoriented pair has two orientations worth asserting, and the analyst is the
// only one who knows which — offering just one of them would leave the other
// reachable only by adding an edge that already exists.
function SelectedEdge({
  edge,
  pending,
  onCorrect,
  onDismiss,
}: {
  edge: CausalGraphEdge;
  pending: CorrectionAction | null;
  onCorrect: (action: CorrectionAction, correction: EdgeCorrection) => void;
  onDismiss: () => void;
}) {
  const unoriented = causeEffect(edge) === null;
  const flip = flipCorrection(edge);
  const busy = pending !== null;
  return (
    <Panel className="flex flex-col gap-3 border-signal/30 p-4">
      <div className="flex items-baseline justify-between gap-3">
        <SectionLabel>
          Correct {edge.col_a} / {edge.col_b}
        </SectionLabel>
        <Button variant="subtle" onClick={onDismiss}>
          Done
        </Button>
      </div>
      <p className="max-w-2xl text-sm leading-relaxed text-muted">
        {unoriented
          ? "The tests left this pair unoriented, and nothing can be adjusted across an unoriented edge — say which way it runs and the effects that depend on it become answerable."
          : "Flipping re-orients the edge; unorienting keeps the association but withdraws the direction, which blocks adjustment across it. Deleting removes the edge entirely."}
      </p>
      <div className="flex flex-wrap items-center gap-2">
        {unoriented ? (
          <>
            <Button
              onClick={() =>
                onCorrect("orient_a", orientCorrection(edge.col_a, edge.col_b))
              }
              loading={pending === "orient_a"}
              disabled={busy}
            >
              {edge.col_a} → {edge.col_b}
            </Button>
            <Button
              onClick={() =>
                onCorrect("orient_b", orientCorrection(edge.col_b, edge.col_a))
              }
              loading={pending === "orient_b"}
              disabled={busy}
            >
              {edge.col_b} → {edge.col_a}
            </Button>
          </>
        ) : (
          <>
            <Button
              onClick={() => onCorrect("flip", flip)}
              loading={pending === "flip"}
              disabled={busy}
            >
              Flip to {flip.from} → {flip.to}
            </Button>
            <Button
              variant="ghost"
              onClick={() => onCorrect("unorient", unorientCorrection(edge))}
              loading={pending === "unorient"}
              disabled={busy}
            >
              Unorient
            </Button>
          </>
        )}
        <Button
          variant="ghost"
          onClick={() => onCorrect("delete", deleteCorrection(edge))}
          loading={pending === "delete"}
          disabled={busy}
        >
          Delete edge
        </Button>
      </div>
    </Panel>
  );
}

// AddEdge is how the confounder discovery missed gets into the model — the edit
// that turns a spurious finding into an explained one.
function AddEdge({
  columns,
  pending,
  onAdd,
}: {
  columns: string[];
  pending: CorrectionAction | null;
  onAdd: (correction: EdgeCorrection) => void;
}) {
  const [cause, setCause] = useState("");
  const [effect, setEffect] = useState("");
  const ready = cause !== "" && effect !== "" && cause !== effect;

  return (
    <div className="flex flex-col gap-2 border-t border-line pt-5">
      <SectionLabel>Add a missing edge</SectionLabel>
      <div className="flex flex-col gap-2 sm:flex-row sm:items-center">
        <ColumnSelect
          label="Cause"
          value={cause}
          columns={columns}
          onChange={setCause}
        />
        <span className="hidden text-muted sm:inline" aria-hidden>
          →
        </span>
        <ColumnSelect
          label="Effect"
          value={effect}
          columns={columns}
          onChange={setEffect}
        />
        <Button
          onClick={() => onAdd(addCorrection(cause, effect))}
          loading={pending === "add"}
          disabled={pending !== null || !ready}
          className="sm:shrink-0"
        >
          Add edge
        </Button>
      </div>
    </div>
  );
}

function ColumnSelect({
  label,
  value,
  columns,
  onChange,
}: {
  label: string;
  value: string;
  columns: string[];
  onChange: (next: string) => void;
}) {
  return (
    <select
      value={value}
      onChange={(e) => onChange(e.target.value)}
      aria-label={label}
      className="rounded-lg border border-line bg-surface px-3 py-2.5 text-sm text-fg outline-hidden transition-colors focus:border-signal/60 focus:ring-2 focus:ring-signal/20 sm:w-52"
    >
      <option value="">{label}…</option>
      {columns.map((name) => (
        <option key={name} value={name}>
          {name}
        </option>
      ))}
    </select>
  );
}
