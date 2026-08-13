"use client";

import { useEffect, useState } from "react";
import Link from "next/link";
import { useRouter } from "next/navigation";
import { errorMessage, listGoals, deleteDataset, type GoalListItem as DatasetListItem } from "@/lib/orchestrator";
import { formatTimestamp } from "@/lib/format";
import {
  Button,
  Callout,
  Panel,
  SectionLabel,
  Spinner,
} from "@/components/ui";

function splitGoalText(text: string): { name: string; question?: string } {
  const idx = text.indexOf("\n\n");
  if (idx === -1) return { name: text };
  return { name: text.slice(0, idx), question: text.slice(idx + 2) };
}

export function DatasetsList() {
  const router = useRouter();
  const [datasets, setDatasets] = useState<DatasetListItem[] | null>(null);
  const [loading, setLoading] = useState(true);
  const [error, setError] = useState<string | null>(null);
  const [deleting, setDeleting] = useState<string | null>(null);

  useEffect(() => {
    let cancelled = false;
    listGoals()
      .then((rows) => { if (!cancelled) setDatasets(rows); })
      .catch((err: unknown) => {
        if (cancelled) return;
        setError(errorMessage(err, "Could not load datasets."));
      })
      .finally(() => { if (!cancelled) setLoading(false); });
    return () => { cancelled = true; };
  }, []);

  async function handleDelete(id: string, e: React.MouseEvent) {
    e.preventDefault();
    e.stopPropagation();
    if (!confirm("Delete this dataset and all its questions and insights?")) return;
    setDeleting(id);
    try {
      await deleteDataset(id);
      setDatasets((prev) => prev?.filter((d) => d.optimization_function_id !== id) ?? null);
    } catch (err) {
      setError(errorMessage(err, "Failed to delete dataset."));
    } finally {
      setDeleting(null);
    }
  }

  return (
    <div className="flex flex-col gap-8 pt-4">
      <div className="flex flex-col gap-3 border-b border-line pb-6">
        <SectionLabel>Datasets</SectionLabel>
        <h1 className="text-2xl font-semibold tracking-tight">
          Registered datasets
        </h1>
        <p className="max-w-xl text-sm leading-relaxed text-muted">
          Click a dataset to view its questions and insights.
        </p>
      </div>

      {error && <Callout tone="error">{error}</Callout>}

      {loading && !datasets && (
        <Panel className="flex items-center justify-center px-6 py-16 text-sm text-muted">
          <Spinner className="mr-2" /> Loading datasets…
        </Panel>
      )}

      {!loading && datasets?.length === 0 && (
        <Panel className="px-6 py-16 text-center text-sm text-faint">
          No datasets registered yet.{" "}
          <Link href="/" className="text-signal underline-offset-2 hover:underline">
            Register a dataset
          </Link>{" "}
          to get started.
        </Panel>
      )}

      {datasets && datasets.length > 0 && (
        <ul className="flex flex-col gap-2">
          {datasets.map((g) => {
            const { name, question } = splitGoalText(g.goal_text);
            return (
              <li key={g.optimization_function_id}>
                <div className="group relative flex items-start justify-between rounded-lg border border-line bg-surface/60 px-4 py-3.5 transition-colors hover:border-line-strong">
                  <Link
                    href={`/datasets/${g.optimization_function_id}`}
                    className="flex-1"
                  >
                    <div className="flex flex-col gap-0.5">
                      <span className="text-sm font-medium text-fg">{name}</span>
                      {question && (
                        <span className="text-xs leading-relaxed text-muted line-clamp-1">
                          {question}
                        </span>
                      )}
                    </div>
                    {g.status === "failed" && g.failure_reason && (
                      <span className="text-xs leading-relaxed text-warn">{g.failure_reason}</span>
                    )}
                    <div className="mt-1.5 flex items-center gap-3">
                      <span className="font-mono text-[11px] text-faint">
                        {formatTimestamp(g.created_at)}
                      </span>
                    </div>
                  </Link>
                  <button
                    onClick={(e) => handleDelete(g.optimization_function_id, e)}
                    disabled={deleting === g.optimization_function_id}
                    className="ml-3 shrink-0 self-start rounded px-2 py-1 text-[11px] text-warn transition-colors hover:bg-warn/10 disabled:opacity-30"
                  >
                    {deleting === g.optimization_function_id ? "Deleting…" : "Delete"}
                  </button>
                </div>
              </li>
            );
          })}
        </ul>
      )}
    </div>
  );
}
