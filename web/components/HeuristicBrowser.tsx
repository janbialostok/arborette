"use client";

import { useEffect, useState } from "react";
import {
  errorMessage,
  listCausalVerifications,
  listGoals,
  searchInsights,
  traceInsight,
  type CausalVerification,
  type GoalListItem,
  type InsightMatch,
  type TraceTriplet,
} from "@/lib/orchestrator";
import { epistemicSource, inFlight, latestByIntervention } from "@/lib/causal";
import { renderValue } from "@/lib/format";
import { EpistemicBadge } from "@/components/EpistemicBadge";
import { VerifyFindingButton } from "@/components/VerifyFindingButton";
import {
  Badge,
  Button,
  Callout,
  cn,
  Panel,
  SectionLabel,
  Spinner,
  type BadgeTone,
} from "@/components/ui";

// A dataset's causal verdicts, joined onto the evidence rows of a search scoped to
// that dataset. Only a scoped search can carry them: a trace triplet names no
// dataset, and a finding belonging to another dataset cannot be verified against
// this one — so cross-dataset browsing gets neither the badge nor the button rather
// than an affordance that answers "finding not found".
interface GoalScope {
  goalID: string;
  // null when the verdicts could not be read: the rows then offer verification
  // without claiming anything about evidence, rather than showing every finding as
  // observational on the strength of a failed request.
  latest: Map<string, CausalVerification> | null;
}

async function loadScope(goalID: string): Promise<GoalScope> {
  try {
    const records = await listCausalVerifications(goalID);
    return { goalID, latest: latestByIntervention(records) };
  } catch {
    return { goalID, latest: null };
  }
}

export function HeuristicBrowser({ datasetID }: { datasetID?: string } = {}) {
  const [q, setQ] = useState("");
  const [results, setResults] = useState<InsightMatch[] | null>(null);
  const [searching, setSearching] = useState(false);
  const [searchError, setSearchError] = useState<string | null>(null);
  const [selected, setSelected] = useState<InsightMatch | null>(null);
  const [goals, setGoals] = useState<GoalListItem[]>([]);
  const [goalId, setGoalId] = useState(datasetID ?? "");
  const [scope, setScope] = useState<GoalScope | null>(null);

  // Populate the scope selector. The corpus is browsable across all datasets by
  // default; scoping to a dataset surfaces that dataset's own insights instead of
  // the whole (legacy-heavy) corpus. A load failure just leaves the "All datasets"
  // option.
  useEffect(() => {
    let cancelled = false;
    listGoals()
      .then((gs) => {
        if (!cancelled) setGoals(gs);
      })
      .catch(() => {});
    return () => {
      cancelled = true;
    };
  }, []);

  async function onSearch(e: React.FormEvent) {
    e.preventDefault();
    if (!q.trim()) {
      // Guard the empty-q 400 client-side — no request.
      setSearchError("Enter a search query.");
      return;
    }
    setSearchError(null);
    setSearching(true);
    setSelected(null);
    try {
      // The verdicts depend on the dataset filter, not on what the search returns, so
      // the two round trips run together rather than one behind the other.
      const [matches, next] = await Promise.all([
        searchInsights(q.trim(), undefined, goalId || undefined),
        goalId ? loadScope(goalId) : Promise.resolve(null),
      ]);
      setResults(matches);
      setScope(next);
    } catch (err) {
      setResults(null);
      setScope(null);
      setSearchError(errorMessage(err, "Search failed. Please retry."));
    } finally {
      setSearching(false);
    }
  }

  return (
    <div className="flex flex-col gap-8 pt-4">
      <div className="flex flex-col gap-3 border-b border-line pb-6">
        <SectionLabel>Insight browser</SectionLabel>
        <h1 className="text-2xl font-semibold tracking-tight">
          Discovered insights
        </h1>
        <p className="max-w-xl text-sm leading-relaxed text-muted">
          Search insights the system discovered. Open one to trace the
          evidence it was abstracted from. Scope to a dataset to see only that
          question&rsquo;s insights — abstracted definitions rank poorly across
          the whole corpus by domain query — and to check each supporting finding
          for whether its effect is causal or merely correlated.
        </p>
      </div>

      <form onSubmit={onSearch} className="flex flex-col gap-2 sm:flex-row">
        <select
          value={goalId}
          onChange={(e) => setGoalId(e.target.value)}
          aria-label="Dataset scope"
          className="rounded-lg border border-line bg-surface px-3 py-3 text-sm text-fg outline-hidden transition-colors focus:border-signal/60 focus:ring-2 focus:ring-signal/20 sm:w-56 sm:shrink-0"
        >
          <option value="">All datasets</option>
          {goals.map((g) => (
            <option key={g.optimization_function_id} value={g.optimization_function_id}>
              {g.goal_text}
            </option>
          ))}
        </select>
        <input
          type="text"
          value={q}
          onChange={(e) => setQ(e.target.value)}
          placeholder="Describe a state or question, e.g. high-value repeat customers"
          className="w-full rounded-lg border border-line bg-surface px-4 py-3 text-sm text-fg outline-hidden transition-colors placeholder:text-faint focus:border-signal/60 focus:ring-2 focus:ring-signal/20"
        />
        <Button type="submit" loading={searching}>
          Search
        </Button>
      </form>

      {searchError && <Callout tone="error">{searchError}</Callout>}

      <div className="grid gap-6 lg:grid-cols-[minmax(0,0.9fr)_minmax(0,1.1fr)]">
        <ResultList
          results={results}
          searching={searching}
          selectedId={selected?.id ?? null}
          onSelect={setSelected}
        />
        <TracePanel
          key={selected?.id ?? "none"}
          insight={selected}
          scope={scope}
        />
      </div>
    </div>
  );
}

function ResultList({
  results,
  searching,
  selectedId,
  onSelect,
}: {
  results: InsightMatch[] | null;
  searching: boolean;
  selectedId: string | null;
  onSelect: (m: InsightMatch) => void;
}) {
  if (searching && !results) {
    return (
      <Panel className="flex items-center justify-center px-6 py-16 text-sm text-muted">
        <Spinner className="mr-2" /> Searching…
      </Panel>
    );
  }
  if (!results) {
    return (
      <Panel className="px-6 py-16 text-center text-sm text-faint">
        Run a search to list matching insights.
      </Panel>
    );
  }
  if (results.length === 0) {
    return (
      <Panel className="px-6 py-16 text-center text-sm text-faint">
        No insights matched. Run insight generation after a hypothesis run to
        discover some.
      </Panel>
    );
  }
  return (
    <ul className="flex flex-col gap-2">
      {results.map((m) => {
        const active = m.id === selectedId;
        return (
          <li key={m.id}>
            <button
              onClick={() => onSelect(m)}
              className={cn(
                "w-full rounded-lg border px-4 py-3.5 text-left transition-colors",
                active
                  ? "border-signal/50 bg-surface-2"
                  : "border-line bg-surface/60 hover:border-line-strong",
              )}
            >
              <span className="line-clamp-3 text-sm leading-relaxed text-fg">
                {m.definition}
              </span>
              <span className="mt-2 block font-mono text-[11px] text-faint">
                {m.id}
              </span>
            </button>
          </li>
        );
      })}
    </ul>
  );
}

function TracePanel({
  insight,
  scope,
}: {
  insight: InsightMatch | null;
  scope: GoalScope | null;
}) {
  const [trace, setTrace] = useState<TraceTriplet[] | null>(null);
  const [loading, setLoading] = useState(false);
  const [error, setError] = useState<string | null>(null);

  // Fetch the trace when an insight is selected. The parent keys this component
  // by id, so it remounts per selection and this effect runs once with the id
  // fixed; the cancelled guard drops a late resolve after unmount.
  useEffect(() => {
    if (!insight) return;
    let cancelled = false;
    setLoading(true);
    setError(null);
    traceInsight(insight.id)
      .then((t) => {
        if (!cancelled) setTrace(t);
      })
      .catch((err: unknown) => {
        if (cancelled) return;
        setError(errorMessage(err, "Could not load the trace."));
      })
      .finally(() => {
        if (!cancelled) setLoading(false);
      });
    return () => {
      cancelled = true;
    };
  }, [insight]);

  if (!insight) {
    return (
      <Panel className="hidden px-6 py-16 text-center text-sm text-faint lg:block">
        Select an insight to trace its supporting evidence.
      </Panel>
    );
  }

  return (
    <Panel className="flex flex-col gap-4 p-5">
      <div className="flex flex-col gap-2 border-b border-line pb-4">
        <SectionLabel>Definition</SectionLabel>
        <p className="text-sm leading-relaxed text-fg">{insight.definition}</p>
      </div>

      {loading && (
        <div className="flex items-center gap-2 py-8 text-sm text-muted">
          <Spinner /> Loading causal chain…
        </div>
      )}
      {error && <Callout tone="error">{error}</Callout>}
      {trace && trace.length === 0 && (
        <p className="py-6 text-sm text-faint">
          No supporting triplets recorded for this insight.
        </p>
      )}
      {trace && trace.length > 0 && (
        <div className="flex flex-col gap-3">
          <SectionLabel>
            Supporting evidence · {trace.length} triplet
            {trace.length === 1 ? "" : "s"}
          </SectionLabel>
          {trace.map((t, i) => (
            <EvidenceRow
              key={`${t.intervention.id}-${i}`}
              triplet={t}
              scope={scope}
            />
          ))}
        </div>
      )}
    </Panel>
  );
}

function EvidenceRow({
  triplet,
  scope,
}: {
  triplet: TraceTriplet;
  scope: GoalScope | null;
}) {
  const record = scope?.latest?.get(triplet.intervention.id);
  return (
    <div className="rounded-lg border border-line bg-surface p-4">
      <div className="grid gap-3 md:grid-cols-3">
        <Facet label="State">
          <PropertyList props={triplet.state.properties} />
        </Facet>
        <Facet label={`Intervention · ${triplet.intervention.type}`}>
          <PropertyList props={triplet.intervention.properties} />
        </Facet>
        <Facet
          label="Outcome"
          badge={<VerificationBadge status={triplet.outcome.verification_status} />}
        >
          <PropertyList props={triplet.outcome.value} />
        </Facet>
      </div>

      {scope && (
        <div className="mt-4 flex flex-wrap items-center gap-3 border-t border-line pt-3">
          {scope.latest && <EpistemicBadge source={epistemicSource(record)} />}
          <div className="ml-auto">
            <VerifyFindingButton
              goalID={scope.goalID}
              interventionID={triplet.intervention.id}
              pending={inFlight(record)}
            />
          </div>
        </div>
      )}
    </div>
  );
}

function Facet({
  label,
  badge,
  children,
}: {
  label: string;
  badge?: React.ReactNode;
  children: React.ReactNode;
}) {
  return (
    <div className="flex flex-col gap-2">
      <div className="flex items-center justify-between gap-2">
        <SectionLabel className="truncate">{label}</SectionLabel>
        {badge}
      </div>
      {children}
    </div>
  );
}

function PropertyList({ props }: { props: Record<string, unknown> }) {
  const entries = Object.entries(props ?? {});
  if (entries.length === 0) {
    return <span className="font-mono text-xs text-faint">—</span>;
  }
  return (
    <dl className="flex flex-col gap-1.5">
      {entries.map(([key, val]) => (
        <div key={key} className="flex flex-col">
          <dt className="font-mono text-[10px] uppercase tracking-wider text-faint">
            {key}
          </dt>
          <dd className="break-words font-mono text-xs text-muted">
            {renderValue(val)}
          </dd>
        </div>
      ))}
    </dl>
  );
}

function VerificationBadge({ status }: { status: string }) {
  const tone: Record<string, BadgeTone> = {
    verified: "positive",
    confirmed: "positive",
    unverified: "neutral",
    corrected: "warn",
    rejected: "negative",
  };
  return <Badge tone={tone[status] ?? "neutral"}>{status || "unknown"}</Badge>;
}
