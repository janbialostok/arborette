"use client";

import { useEffect, useRef, useState } from "react";
import {
  OrchestratorError,
  errorMessage,
  getOutcomeExcerpt,
  listOutcomes,
  listVerifications,
  resolveVerification,
  type OutcomeExcerpt,
  type ResolveAction,
  type ResolveResult,
} from "@/lib/orchestrator";
import { formatConfidence } from "@/lib/format";
import {
  fromEntry,
  fromOutcome,
  isAwaiting,
  nextAwaiting,
  splitByteRange,
  type ReviewItem,
} from "@/lib/review";
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

// The queue lists what fell below the confidence threshold; the full list adds
// every extraction that cleared it, so an analyst can pull one up and review it
// anyway. The two endpoints answer structurally different shapes, normalized
// into ReviewItem by lib/review.
type Source = "queue" | "all";

interface Review {
  items: ReviewItem[];
  // The settings the queue was produced under. Absent for the full list, which
  // is not a product of the threshold.
  threshold: number | null;
  epochMode: string | null;
}

export function VerificationQueue({ id }: { id: string }) {
  const [source, setSource] = useState<Source>("queue");
  const [review, setReview] = useState<Review | null>(null);
  const [loading, setLoading] = useState(true);
  const [error, setError] = useState<string | null>(null);
  const [selectedID, setSelectedID] = useState<string | null>(null);
  const [generation, setGeneration] = useState(0);

  useEffect(() => {
    let cancelled = false;
    setLoading(true);
    setError(null);
    loadReview(id, source)
      .then((r) => {
        if (!cancelled) setReview(r);
      })
      .catch((err: unknown) => {
        if (cancelled) return;
        setReview(null);
        setError(errorMessage(err, "Could not load the review queue. Please retry."));
      })
      .finally(() => {
        if (!cancelled) setLoading(false);
      });
    return () => {
      cancelled = true;
    };
  }, [id, source, generation]);

  const items = review?.items ?? [];
  const selected = items.find((i) => i.outcomeID === selectedID) ?? null;

  // Recording a verdict awaits the orchestrator, and a source toggle or a
  // conflict refetch can replace the list during that await. This tracks the
  // committed list so the update and the advance both read what is on screen
  // now, rather than what was when the callback was created.
  const latestItems = useRef<ReviewItem[] | null>(null);
  useEffect(() => {
    latestItems.current = review?.items ?? null;
  }, [review]);

  // A recorded verdict is reflected in place rather than re-fetched: the
  // resolution response is authoritative for the outcome it names, and a
  // refetch would reorder the list under the analyst mid-review. Confidence
  // comes from that response too — resolving rewrites it — so this row and the
  // run's confidence plot cannot end up disagreeing about the same outcome.
  //
  // Advancing from a stale list would select an id the new list has never heard
  // of and strand the analyst on an empty detail pane.
  function onResolved(outcomeID: string, result: ResolveResult, value?: string) {
    const updated = (latestItems.current ?? items).map((i) =>
      i.outcomeID === outcomeID
        ? {
            ...i,
            status: result.verification_status,
            confidence: result.confidence,
            value: value ?? i.value,
          }
        : i,
    );
    setReview((r) => (r ? { ...r, items: updated } : r));
    setSelectedID(nextAwaiting(updated, outcomeID) ?? outcomeID);
  }

  return (
    <div className="flex flex-col gap-6 pt-4">
      <div className="flex flex-wrap items-center justify-between gap-3">
        <SectionLabel>
          {source === "queue"
            ? queueCaption(review)
            : "every extraction · resolved and unresolved"}
        </SectionLabel>
        <SourceToggle source={source} onChange={setSource} />
      </div>

      {error && <Callout tone="error">{error}</Callout>}

      <div className="grid gap-6 lg:grid-cols-[minmax(0,0.85fr)_minmax(0,1.15fr)]">
        <ReviewList
          items={items}
          loading={loading}
          source={source}
          selectedID={selectedID}
          onSelect={(item) => setSelectedID(item.outcomeID)}
        />
        <OutcomeDetail
          key={selected?.outcomeID ?? "none"}
          goalID={id}
          item={selected}
          onResolved={onResolved}
          onConflict={() => setGeneration((g) => g + 1)}
        />
      </div>
    </div>
  );
}

async function loadReview(id: string, source: Source): Promise<Review> {
  if (source === "all") {
    const outcomes = await listOutcomes(id);
    return { items: outcomes.map(fromOutcome), threshold: null, epochMode: null };
  }
  // Narrowed to the unresolved rows: this pane is a work list, and the queue
  // keeps every row it has ever held, so an unfiltered read turns into a history
  // of past verdicts the moment reviewing starts. The resolved ones stay
  // reachable under the full-extraction source, alongside their outcomes.
  const list = await listVerifications(id, "pending");
  return {
    items: list.entries.map(fromEntry),
    threshold: list.effective_threshold,
    epochMode: list.epoch_mode,
  };
}

function queueCaption(review: Review | null): string {
  if (!review || review.threshold == null) return "review queue";
  return `review queue · below ${formatConfidence(review.threshold)} confidence · ${review.epochMode} epoch`;
}

function SourceToggle({
  source,
  onChange,
}: {
  source: Source;
  onChange: (s: Source) => void;
}) {
  return (
    <div className="flex gap-1 rounded-lg border border-line bg-surface p-1">
      {(["queue", "all"] as const).map((value) => (
        <button
          key={value}
          type="button"
          onClick={() => onChange(value)}
          aria-pressed={source === value}
          className={cn(
            "rounded-md px-3 py-1.5 text-sm font-medium transition-colors",
            source === value
              ? "bg-surface-2 text-signal shadow-[0_0_0_1px_rgba(123,241,168,0.25)]"
              : "text-muted hover:text-fg",
          )}
        >
          {value === "queue" ? "Awaiting review" : "All extractions"}
        </button>
      ))}
    </div>
  );
}

function ReviewList({
  items,
  loading,
  source,
  selectedID,
  onSelect,
}: {
  items: ReviewItem[];
  loading: boolean;
  source: Source;
  selectedID: string | null;
  onSelect: (item: ReviewItem) => void;
}) {
  if (loading && items.length === 0) {
    return (
      <Panel className="flex items-center justify-center px-6 py-16 text-sm text-muted">
        <Spinner className="mr-2" /> Loading extractions…
      </Panel>
    );
  }
  if (items.length === 0) {
    return (
      <Panel className="px-6 py-16 text-center text-sm leading-relaxed text-faint">
        {source === "queue"
          ? "Nothing is waiting for review. Extractions that land below the confidence threshold appear here."
          : "This run has extracted nothing yet. Only a goal reading from a document produces extractions."}
      </Panel>
    );
  }
  return (
    <ul className="flex flex-col gap-2">
      {items.map((item) => {
        const active = item.outcomeID === selectedID;
        return (
          <li key={item.outcomeID}>
            <button
              onClick={() => onSelect(item)}
              className={cn(
                "w-full rounded-lg border px-4 py-3.5 text-left transition-colors",
                active
                  ? "border-signal/50 bg-surface-2"
                  : "border-line bg-surface/60 hover:border-line-strong",
              )}
            >
              <div className="flex items-start justify-between gap-3">
                <span className="line-clamp-2 text-sm leading-relaxed text-fg">
                  {item.value || "—"}
                </span>
                <StatusBadge status={item.status} />
              </div>
              <div className="mt-2 flex items-center gap-3 font-mono text-[11px] text-faint">
                <span>{item.field}</span>
                {/* A queue row keeps the confidence that sent it to review; the
                    graph holds what the verdict rewrote it to. Naming which one
                    this is keeps the two panes from looking contradictory. */}
                <span className="tabular">
                  {source === "queue" ? "queued at" : "confidence"}{" "}
                  {formatConfidence(item.confidence)}
                </span>
              </div>
            </button>
          </li>
        );
      })}
    </ul>
  );
}

function OutcomeDetail({
  goalID,
  item,
  onResolved,
  onConflict,
}: {
  goalID: string;
  item: ReviewItem | null;
  onResolved: (outcomeID: string, result: ResolveResult, value?: string) => void;
  onConflict: () => void;
}) {
  const [excerpt, setExcerpt] = useState<OutcomeExcerpt | null>(null);
  const [loading, setLoading] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const outcomeID = item?.outcomeID ?? null;

  // The parent keys this component by outcome id, so it remounts per selection
  // and this effect runs once with the id fixed.
  useEffect(() => {
    if (!outcomeID) return;
    let cancelled = false;
    setLoading(true);
    setError(null);
    getOutcomeExcerpt(goalID, outcomeID)
      .then((e) => {
        if (!cancelled) setExcerpt(e);
      })
      .catch((err: unknown) => {
        if (cancelled) return;
        setError(errorMessage(err, "Could not load the source text."));
      })
      .finally(() => {
        if (!cancelled) setLoading(false);
      });
    return () => {
      cancelled = true;
    };
  }, [goalID, outcomeID]);

  if (!item) {
    return (
      <Panel className="hidden px-6 py-16 text-center text-sm text-faint lg:block">
        Select an extraction to check it against the source document.
      </Panel>
    );
  }

  return (
    <Panel className="flex flex-col gap-5 p-5">
      <div className="flex flex-col gap-2 border-b border-line pb-4">
        <div className="flex items-center justify-between gap-3">
          <SectionLabel>{item.field}</SectionLabel>
          <div className="flex items-center gap-2">
            <span className="font-mono text-[11px] tabular text-faint">
              confidence {formatConfidence(item.confidence)}
            </span>
            <StatusBadge status={item.status} />
          </div>
        </div>
        <p className="text-lg leading-snug text-fg">{item.value || "—"}</p>
      </div>

      <VerdictRow
        goalID={goalID}
        item={item}
        onResolved={onResolved}
        onConflict={onConflict}
      />

      {loading && (
        <div className="flex items-center gap-2 py-8 text-sm text-muted">
          <Spinner /> Loading source text…
        </div>
      )}
      {error && <Callout tone="error">{error}</Callout>}
      {excerpt && <ExcerptView excerpt={excerpt} />}
    </Panel>
  );
}

function VerdictRow({
  goalID,
  item,
  onResolved,
  onConflict,
}: {
  goalID: string;
  item: ReviewItem;
  onResolved: (outcomeID: string, result: ResolveResult, value?: string) => void;
  onConflict: () => void;
}) {
  const [correcting, setCorrecting] = useState(false);
  const [corrected, setCorrected] = useState(item.value);
  const [pending, setPending] = useState<ResolveAction | null>(null);
  const [error, setError] = useState<string | null>(null);

  if (!isAwaiting(item.status)) {
    return (
      <Callout tone="info">
        This extraction was resolved as {item.status}. Its verdict is recorded in
        the graph and cannot be replaced.
      </Callout>
    );
  }

  async function resolve(action: ResolveAction) {
    const value = action === "correct" ? corrected.trim() : "";
    // Pre-empt the 422 the orchestrator answers for a correction with no value.
    if (action === "correct" && !value) {
      setError("Enter the corrected value before saving.");
      return;
    }
    setError(null);
    setPending(action);
    try {
      const result = await resolveVerification(
        goalID,
        item.outcomeID,
        action,
        value,
      );
      onResolved(item.outcomeID, result, value || undefined);
    } catch (err) {
      setError(errorMessage(err, "Could not record the verdict. Please retry."));
      // A 409 means someone else resolved this outcome; the list is stale, so
      // reload it rather than leaving a row that invites the same rejection.
      if (err instanceof OrchestratorError && err.status === 409) onConflict();
    } finally {
      setPending(null);
    }
  }

  return (
    <div className="flex flex-col gap-3">
      <div className="flex flex-wrap items-center gap-2">
        <Button
          onClick={() => resolve("confirm")}
          loading={pending === "confirm"}
          disabled={pending !== null}
        >
          Confirm
        </Button>
        <Button
          variant="ghost"
          onClick={() => setCorrecting((c) => !c)}
          disabled={pending !== null}
          aria-expanded={correcting}
        >
          Correct
        </Button>
        <Button
          variant="ghost"
          onClick={() => resolve("reject")}
          loading={pending === "reject"}
          disabled={pending !== null}
        >
          Reject
        </Button>
      </div>

      {correcting && (
        <div className="flex flex-col gap-2 sm:flex-row">
          <input
            type="text"
            value={corrected}
            onChange={(e) => setCorrected(e.target.value)}
            placeholder="The value as the document states it"
            className="w-full rounded-lg border border-line bg-surface px-4 py-2.5 text-sm text-fg outline-hidden transition-colors placeholder:text-faint focus:border-signal/60 focus:ring-2 focus:ring-signal/20"
          />
          <Button
            onClick={() => resolve("correct")}
            loading={pending === "correct"}
            disabled={pending !== null}
            className="shrink-0"
          >
            Save correction
          </Button>
        </div>
      )}

      {error && <Callout tone="error">{error}</Callout>}
    </div>
  );
}

function ExcerptView({ excerpt }: { excerpt: OutcomeExcerpt }) {
  if (!excerpt.fallback) {
    return (
      <PageBlock
        page={excerpt.page}
        text={excerpt.page_text ?? ""}
        highlight={[excerpt.char_start, excerpt.char_end]}
      />
    );
  }
  const pages = excerpt.pages ?? [];
  return (
    <div className="flex flex-col gap-3">
      <Callout tone="warn">
        No location was recorded for this value — the model reformatted it, or the
        source changed — so the whole document is shown instead of the span.
      </Callout>
      {pages.length === 0 ? (
        <p className="text-sm text-faint">
          No readable text came back for this document.
        </p>
      ) : (
        pages.map((text, i) => <PageBlock key={i} page={i} text={text} />)
      )}
    </div>
  );
}

function PageBlock({
  page,
  text,
  highlight,
}: {
  page: number;
  text: string;
  highlight?: [number, number];
}) {
  return (
    <div className="flex flex-col gap-1.5">
      <SectionLabel>page {page + 1}</SectionLabel>
      <pre className="max-h-80 overflow-auto whitespace-pre-wrap break-words rounded-lg border border-line bg-surface px-4 py-3 font-mono text-xs leading-relaxed text-muted">
        {highlight ? <Highlighted text={text} range={highlight} /> : text}
      </pre>
    </div>
  );
}

function Highlighted({
  text,
  range,
}: {
  text: string;
  range: [number, number];
}) {
  const [before, span, after] = splitByteRange(text, range[0], range[1]);
  return (
    <>
      {before}
      <mark className="rounded-[3px] bg-signal/15 px-0.5 text-signal">
        {span}
      </mark>
      {after}
    </>
  );
}

function StatusBadge({ status }: { status: string }) {
  const tone: Record<string, BadgeTone> = {
    pending: "warn",
    unverified: "neutral",
    verified: "positive",
    confirmed: "positive",
    corrected: "warn",
    rejected: "negative",
  };
  return (
    <Badge tone={tone[status] ?? "neutral"} className="shrink-0">
      {status || "unknown"}
    </Badge>
  );
}
