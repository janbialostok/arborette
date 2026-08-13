"use client";

import { useParams, useRouter } from "next/navigation";
import { useEffect, useState } from "react";
import Link from "next/link";
import { errorMessage, listGoals, triggerSleepCycle, type GoalListItem as DatasetListItem } from "@/lib/orchestrator";
import { formatTimestamp } from "@/lib/format";
import {
  Button,
  Callout,
  Panel,
  SectionLabel,
  Spinner,
} from "@/components/ui";

function splitGoalText(text: string): { name: string; question: string } {
  const idx = text.indexOf("\n\n");
  if (idx === -1) return { name: text, question: "" };
  return { name: text.slice(0, idx), question: text.slice(idx + 2) };
}

export default function DatasetQuestionsPage() {
  const { id } = useParams<{ id: string }>();
  const router = useRouter();
  const [dataset, setDataset] = useState<DatasetListItem | null>(null);
  const [loading, setLoading] = useState(true);
  const [error, setError] = useState<string | null>(null);

  useEffect(() => {
    let cancelled = false;
    listGoals()
      .then((rows) => {
        if (cancelled) return;
        const found = rows.find((g) => g.optimization_function_id === id);
        if (found) setDataset(found);
        else setError("Dataset not found.");
      })
      .catch((err: unknown) => {
        if (cancelled) return;
        setError(errorMessage(err, "Could not load dataset."));
      })
      .finally(() => { if (!cancelled) setLoading(false); });
    return () => { cancelled = true; };
  }, [id]);

  if (loading) {
    return (
      <Panel className="flex items-center justify-center px-6 py-16 text-sm text-muted">
        <Spinner className="mr-2" /> Loading dataset…
      </Panel>
    );
  }

  if (error && !dataset) {
    return (
      <div className="flex flex-col gap-4 pt-4">
        <Callout tone="error">{error}</Callout>
        <Link href="/datasets" className="text-sm text-signal underline-offset-2 hover:underline">← Back to datasets</Link>
      </div>
    );
  }

  const { name, question } = dataset ? splitGoalText(dataset.goal_text) : { name: "", question: "" };

  return (
    <div className="flex flex-col gap-8 pt-4">
      <div className="flex flex-col gap-1 border-b border-line pb-6">
        <Link href="/datasets" className="text-xs text-muted underline-offset-2 hover:underline">← Datasets</Link>
        <SectionLabel>Dataset</SectionLabel>
        <h1 className="text-2xl font-semibold tracking-tight">{name}</h1>
        {question && <p className="text-sm text-muted">{question}</p>}
        <p className="text-xs text-faint">Registered {dataset ? formatTimestamp(dataset.created_at) : ""}</p>
      </div>

      <div className="flex flex-col gap-4">
        <SectionLabel>Questions</SectionLabel>
        <button
          onClick={async () => {
            try {
              const res = await fetch(`/api/orchestrator/goals/${encodeURIComponent(id)}/hypothesis-loop`, { method: "POST" });
              if (res.ok) router.refresh();
            } catch {}
          }}
          className="inline-flex w-fit items-center gap-2 rounded-md bg-signal px-4 py-2 text-sm font-medium text-bg transition-opacity hover:opacity-90"
        >
          Ask a new question
        </button>

        {dataset?.status === "no run" ? (
          <Panel className="px-6 py-8 text-center text-sm text-faint">
            No questions asked yet. Ask a question to begin exploring this dataset.
          </Panel>
        ) : dataset?.status === "running" ? (
          <Panel className="flex items-center gap-2 px-6 py-8 text-sm text-muted">
            <Spinner className="mr-1" /> Analysis in progress…
          </Panel>
        ) : (
          <Link
            href={`/datasets/${encodeURIComponent(id)}/questions/latest`}
            className="flex items-center justify-between rounded-lg border border-line bg-surface/60 px-4 py-3 transition-colors hover:border-line-strong"
          >
            <div>
              <span className="text-sm font-medium text-fg">{question || "Latest analysis"}</span>
              <p className="text-xs text-muted mt-0.5">Click to browse insights</p>
            </div>
            <span className="text-sm text-signal">View insights →</span>
          </Link>
        )}
      </div>
    </div>
  );
}
