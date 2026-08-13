"use client";

import { useEffect, useState } from "react";
import Link from "next/link";
import { errorMessage, listGoals, type GoalListItem as DatasetListItem } from "@/lib/orchestrator";
import { formatTimestamp } from "@/lib/format";
import {
  Badge,
  Callout,
  Panel,
  SectionLabel,
  Spinner,
  type BadgeTone,
} from "@/components/ui";

export function ObjectivesList() {
  const [datasets, setDatasets] = useState<DatasetListItem[] | null>(null);
  const [loading, setLoading] = useState(true);
  const [error, setError] = useState<string | null>(null);

  useEffect(() => {
    let cancelled = false;
    listGoals()
      .then((rows) => {
        if (!cancelled) setDatasets(rows);
      })
      .catch((err: unknown) => {
        if (cancelled) return;
        setError(errorMessage(err, "Could not load datasets. Please retry."));
      })
      .finally(() => {
        if (!cancelled) setLoading(false);
      });
    return () => {
      cancelled = true;
    };
  }, []);

  return (
    <div className="flex flex-col gap-8 pt-4">
      <div className="flex flex-col gap-3 border-b border-line pb-6">
        <SectionLabel>Datasets</SectionLabel>
        <h1 className="text-2xl font-semibold tracking-tight">
          Registered datasets
        </h1>
        <p className="max-w-xl text-sm leading-relaxed text-muted">
          Return to a dataset and see its latest run status. Open one to view its
          live run or settled outcome.
        </p>
      </div>

      {error && <Callout tone="error">{error}</Callout>}

      {!error && <List datasets={datasets} loading={loading} />}
    </div>
  );
}

function List({
  datasets,
  loading,
}: {
  datasets: DatasetListItem[] | null;
  loading: boolean;
}) {
  if (loading && !datasets) {
    return (
      <Panel className="flex items-center justify-center px-6 py-16 text-sm text-muted">
        <Spinner className="mr-2" /> Loading datasets…
      </Panel>
    );
  }
  if (!datasets || datasets.length === 0) {
    return (
      <Panel className="px-6 py-16 text-center text-sm text-faint">
        No datasets registered yet.{" "}
        <Link href="/" className="text-signal underline-offset-2 hover:underline">
          Register a dataset
        </Link>{" "}
        to get started.
      </Panel>
    );
  }
  return (
    <ul className="flex flex-col gap-2">
      {datasets.map((g) => (
        <li key={g.optimization_function_id}>
          <DatasetRow dataset={g} />
        </li>
      ))}
    </ul>
  );
}

function DatasetRow({ dataset }: { dataset: DatasetListItem }) {
  return (
    <Link
      href={`/datasets/${dataset.optimization_function_id}`}
      className="flex flex-col gap-3 rounded-lg border border-line bg-surface/60 px-4 py-3.5 transition-colors hover:border-line-strong"
    >
      <div className="flex items-start justify-between gap-4">
        <span className="text-sm leading-relaxed text-fg">{dataset.goal_text}</span>
        <span className="text-xs text-muted">View questions →</span>
      </div>
      {dataset.status === "failed" && dataset.failure_reason && (
        <span className="text-xs leading-relaxed text-warn">
          {dataset.failure_reason}
        </span>
      )}
      <span className="font-mono text-[11px] text-faint">
        {formatTimestamp(dataset.created_at)}
      </span>
    </Link>
  );
}

function StatusBadge({ status }: { status: string }) {
  const tone: Record<string, BadgeTone> = {
    running: "positive",
    completed: "positive",
    failed: "negative",
    "no run": "neutral",
  };
  return (
    <Badge tone={tone[status] ?? "neutral"} className="shrink-0">
      {status || "unknown"}
    </Badge>
  );
}
