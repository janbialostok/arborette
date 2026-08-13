"use client";

import { useParams } from "next/navigation";
import { useEffect, useState } from "react";
import Link from "next/link";
import { HeuristicBrowser } from "@/components/HeuristicBrowser";
import { errorMessage, triggerSleepCycle } from "@/lib/orchestrator";
import {
  Button,
  Callout,
  Panel,
  SectionLabel,
  Spinner,
} from "@/components/ui";

export default function QuestionInsightsPage() {
  const { id, qid } = useParams<{ id: string; qid: string }>();
  const [generating, setGenerating] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const [started, setStarted] = useState(false);

  async function handleGenerate() {
    setGenerating(true);
    setError(null);
    try {
      await triggerSleepCycle(id);
      setStarted(true);
    } catch (err) {
      setError(errorMessage(err, "Failed to start insight generation."));
    } finally {
      setGenerating(false);
    }
  }

  return (
    <div className="flex flex-col gap-6 pt-4">
      <div className="flex flex-col gap-1 border-b border-line pb-4">
        <Link href={`/datasets/${encodeURIComponent(id)}`} className="text-xs text-muted underline-offset-2 hover:underline">
          ← Back to dataset
        </Link>
        <SectionLabel>Question insights</SectionLabel>
        <h1 className="text-lg font-semibold tracking-tight">
          {qid === "latest" ? "Latest analysis" : `Question ${qid.slice(0, 8)}`}
        </h1>
        <p className="text-sm text-muted">
          Browse generated insights below, or generate new ones from the accumulated findings.
        </p>
      </div>

      <div className="flex items-center gap-3">
        <Button onClick={handleGenerate} loading={generating}>
          {generating ? "Generating insights…" : "Generate insights"}
        </Button>
        {started && (
          <span className="text-xs text-signal">Insight generation launched</span>
        )}
        {error && <Callout tone="error">{error}</Callout>}
      </div>

      <HeuristicBrowser datasetID={id} />
    </div>
  );
}
