"use client";

import { useEffect, useState } from "react";
import Link from "next/link";
import { errorMessage, listGoals, type GoalListItem } from "@/lib/orchestrator";
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
  const [goals, setGoals] = useState<GoalListItem[] | null>(null);
  const [loading, setLoading] = useState(true);
  const [error, setError] = useState<string | null>(null);

  useEffect(() => {
    let cancelled = false;
    listGoals()
      .then((rows) => {
        if (!cancelled) setGoals(rows);
      })
      .catch((err: unknown) => {
        if (cancelled) return;
        setError(errorMessage(err, "Could not load objectives. Please retry."));
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
        <SectionLabel>Objectives</SectionLabel>
        <h1 className="text-2xl font-semibold tracking-tight">
          Registered optimization objectives
        </h1>
        <p className="max-w-xl text-sm leading-relaxed text-muted">
          Return to a past or currently-running objective and see its latest run
          status. Open one to view its live run or settled outcome.
        </p>
      </div>

      {error && <Callout tone="error">{error}</Callout>}

      {!error && <List goals={goals} loading={loading} />}
    </div>
  );
}

function List({
  goals,
  loading,
}: {
  goals: GoalListItem[] | null;
  loading: boolean;
}) {
  if (loading && !goals) {
    return (
      <Panel className="flex items-center justify-center px-6 py-16 text-sm text-muted">
        <Spinner className="mr-2" /> Loading objectives…
      </Panel>
    );
  }
  if (!goals || goals.length === 0) {
    return (
      <Panel className="px-6 py-16 text-center text-sm text-faint">
        No objectives registered yet.{" "}
        <Link href="/" className="text-signal underline-offset-2 hover:underline">
          Submit a goal
        </Link>{" "}
        to get started.
      </Panel>
    );
  }
  return (
    <ul className="flex flex-col gap-2">
      {goals.map((g) => (
        <li key={g.optimization_function_id}>
          <ObjectiveRow goal={g} />
        </li>
      ))}
    </ul>
  );
}

function ObjectiveRow({ goal }: { goal: GoalListItem }) {
  return (
    <Link
      href={`/goals/${goal.optimization_function_id}`}
      className="flex flex-col gap-3 rounded-lg border border-line bg-surface/60 px-4 py-3.5 transition-colors hover:border-line-strong"
    >
      <div className="flex items-start justify-between gap-4">
        <span className="text-sm leading-relaxed text-fg">{goal.goal_text}</span>
        <StatusBadge status={goal.status} />
      </div>
      {goal.status === "failed" && goal.failure_reason && (
        <span className="text-xs leading-relaxed text-warn">
          {goal.failure_reason}
        </span>
      )}
      <span className="font-mono text-[11px] text-faint">
        {formatTimestamp(goal.created_at)}
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
