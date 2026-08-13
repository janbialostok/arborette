"use client";

import { useCallback, useEffect, useRef, useState } from "react";
import {
  errorMessage,
  isExtraction,
  listGoals,
  triggerHypothesisLoop,
  triggerSleepCycle,
  type BranchFailurePayload,
  type ConfidenceDistribution,
  type OrchestratorEvent,
  type TripletPayload,
} from "@/lib/orchestrator";
import { applyReopenEvent, newReopenState } from "@/lib/reopen";
import { getRunRecord, markCompleted, markTriggered } from "@/lib/runState";
import { consumeRun } from "@/lib/sse";
import { ConfidenceHistogram } from "@/components/ConfidenceHistogram";
import { EffectReadout } from "@/components/EffectReadout";
import { ExtractionReadout } from "@/components/ExtractionReadout";
import { Button, Callout, cn, Panel, SectionLabel } from "@/components/ui";

// No event has arrived this long after connecting → decide what the silence
// means (see runState). A triggered run may just be slow to start (introspect +
// a ProposeInterventionTree call before the first triplet), so it waits longer;
// an un-triggered goal has no run going, so it resolves to neutral quickly —
// long enough only to catch the replay of a run started elsewhere.
const IDLE_TRIGGERED_MS = 8000;
const IDLE_UNTRIGGERED_MS = 2500;

type Phase =
  | "connecting"
  | "starting"
  | "live"
  | "reconnecting"
  | "complete"
  | "failed"
  | "neutral";

type FeedItem =
  | { kind: "triplet"; seq: number; payload: TripletPayload }
  | { kind: "failure"; seq: number; payload: BranchFailurePayload };

type SleepState =
  | { status: "idle" }
  | { status: "launching" }
  | { status: "launched" }
  | { status: "error"; message: string };

export function LiveRun({ id }: { id: string }) {
  const initial = getRunRecord(id);
  // Start from the SSR-stable "connecting" for every run. Reading initial.completed
  // here would diverge server (localStorage-blind) from client hydration and throw
  // a mismatch on a revisited finished run; the subscribe effect below promotes a
  // completed run to "complete" right after mount from completedRef instead.
  const [phase, setPhase] = useState<Phase>("connecting");
  const [items, setItems] = useState<FeedItem[]>([]);
  const [generation, setGeneration] = useState(0);
  const [triggering, setTriggering] = useState(false);
  const [triggerError, setTriggerError] = useState<string | null>(null);
  const [sleep, setSleep] = useState<SleepState>({ status: "idle" });
  const [failureReason, setFailureReason] = useState<string | null>(null);
  // Every run publishes a distribution — a measuring run counts its outcomes at
  // certainty — so this being non-null says nothing about which kind is running;
  // the render site decides whose distribution is worth plotting.
  const [distribution, setDistribution] =
    useState<ConfidenceDistribution | null>(null);

  const triggeredRef = useRef(initial.triggered);
  const completedRef = useRef(initial.completed);
  const failedRef = useRef(false);
  const receivedRef = useRef(false);
  const seqRef = useRef(0);
  const idleTimerRef = useRef<ReturnType<typeof setTimeout> | null>(null);
  const reconnectTimerRef = useRef<ReturnType<typeof setTimeout> | null>(null);
  // Server-authoritative run status from GET /goals, fetched once on mount. The
  // live stream still wins the moment it delivers an event; this only seeds the
  // initial resolution for a run reopened without a local runState record.
  const persistedStatusRef = useRef<string | null>(null);
  const statusFetchedRef = useRef(false);
  const mountedAtRef = useRef<number | null>(null);
  if (mountedAtRef.current === null) mountedAtRef.current = Date.now();

  const clearIdle = useCallback(() => {
    if (idleTimerRef.current) {
      clearTimeout(idleTimerRef.current);
      idleTimerRef.current = null;
    }
  }, []);

  const resolveSilence = useCallback(() => {
    if (completedRef.current) return setPhase("complete");
    if (failedRef.current) return setPhase("failed");
    if (receivedRef.current) return; // A live run in a lull — stay live.
    if (triggeredRef.current) return setPhase("starting"); // Slow start.
    // Untriggered silence would resolve to neutral, but the persisted-status
    // fetch may still be in flight — committing to neutral now could flash a
    // "Run Phase 1" button over a run that is really running. Defer until the
    // fetch settles, bounded so a hung fetch can't wait forever.
    if (!statusFetchedRef.current) {
      const remaining =
        (mountedAtRef.current ?? 0) + IDLE_TRIGGERED_MS - Date.now();
      if (remaining > 0) {
        clearIdle();
        idleTimerRef.current = setTimeout(resolveSilence, remaining);
        return;
      }
    }
    if (persistedStatusRef.current === "running") return setPhase("starting");
    setPhase("neutral"); // Never run, finished and evicted, or fetch unavailable.
  }, [clearIdle]);

  const handleEvent = useCallback(
    (ev: OrchestratorEvent) => {
      // Resetting the backoff belongs here: a delivered frame is the only evidence
      // the stream works.
      reopenRef.current = applyReopenEvent(reopenRef.current, "frame").state;
      const append = (item: FeedItem) => {
        clearIdle();
        receivedRef.current = true;
        setItems((prev) => [...prev, item]);
      };
      switch (ev.type) {
        case "triplet":
          append({ kind: "triplet", seq: seqRef.current++, payload: ev.payload });
          setPhase("live");
          break;
        case "branch_failure":
          append({ kind: "failure", seq: seqRef.current++, payload: ev.payload });
          setPhase((p) => (p === "complete" ? p : "live"));
          break;
        case "confidence_distribution":
          // Each frame is a whole snapshot, so the newest one replaces the last
          // rather than accumulating. It carries no liveness on its own — a
          // resolution publishes one from outside the run — so the phase is left
          // to the triplets it always arrives alongside.
          setDistribution(ev.payload);
          break;
        case "loop_complete":
          clearIdle();
          completedRef.current = true;
          markCompleted(id);
          setPhase("complete");
          break;
      }
    },
    [id, clearIdle],
  );

  // Backoff rather than a fixed delay: a run believed to still be going would
  // otherwise pin the browser to a constant request rate for the whole page view.
  const reopenRef = useRef(newReopenState());
  const scheduleReconnect = useCallback(() => {
    if (reconnectTimerRef.current) clearTimeout(reconnectTimerRef.current);
    const { state, reopenAfter } = applyReopenEvent(reopenRef.current, "end");
    reopenRef.current = state;
    reconnectTimerRef.current = setTimeout(
      () => setGeneration((g) => g + 1),
      reopenAfter,
    );
  }, []);

  const onStreamEnd = useCallback(() => {
    if (completedRef.current) return setPhase("complete");
    if (failedRef.current) return setPhase("failed");
    if (triggeredRef.current) {
      // The loop keeps running server-side; the replay buffer will catch us up.
      setPhase("reconnecting");
      scheduleReconnect();
      return;
    }
    resolveSilence();
  }, [scheduleReconnect, resolveSilence]);

  // Seed the initial phase from the server-authoritative status once the mount
  // fetch resolves. A live stream event overrides this at any time.
  const reconcileStatus = useCallback(
    (status: string, reason?: string) => {
      statusFetchedRef.current = true;
      persistedStatusRef.current = status;
      setFailureReason(status === "failed" ? reason ?? null : null);
      if (receivedRef.current || completedRef.current) return; // Stream owns it.
      // For a terminal status, seed the phase and bump `generation` so the
      // subscribe effect re-runs, early-returns (its guard now honors both refs),
      // and its cleanup aborts the SSE connection opened at mount — which would
      // otherwise stay open replaying nothing for the whole page view.
      if (status === "completed") {
        clearIdle();
        completedRef.current = true;
        setPhase("complete");
        setGeneration((g) => g + 1);
        return;
      }
      if (status === "failed") {
        clearIdle();
        failedRef.current = true;
        setPhase("failed");
        setGeneration((g) => g + 1);
        return;
      }
      if (status === "running" && !triggeredRef.current) {
        // A run in progress in another session: wait for its stream rather than
        // flashing a duplicate-run prompt. But a crashed run's status is stale,
        // so degrade to the recoverable neutral state if no event arrives within
        // the startup budget instead of spinning or reconnecting forever.
        clearIdle();
        setPhase("starting");
        idleTimerRef.current = setTimeout(() => {
          if (!receivedRef.current && !completedRef.current && !failedRef.current) {
            setPhase("neutral");
          }
        }, IDLE_TRIGGERED_MS);
        return;
      }
      // "no run", unknown, or a locally-triggered run: release the gate and let
      // the standard silence resolution proceed.
      resolveSilence();
    },
    [clearIdle, resolveSilence],
  );

  // Fetch the persisted status once on mount. The orchestrator exposes only the
  // objectives list, so this run is found within it. A failure is non-fatal —
  // the view falls back to the localStorage/idle resolution.
  useEffect(() => {
    let cancelled = false;
    listGoals()
      .then((rows) => {
        if (cancelled) return;
        const row = rows.find((g) => g.optimization_function_id === id);
        reconcileStatus(row?.status ?? "no run", row?.failure_reason);
      })
      .catch(() => {
        if (!cancelled) reconcileStatus("no run");
      });
    return () => {
      cancelled = true;
    };
  }, [id, reconcileStatus]);

  // Subscribe to the SSE stream. Re-runs whenever `generation` bumps (reconnect,
  // re-trigger, or a persisted terminal status resolving). A settled run isn't
  // worth connecting to — the stream would replay nothing and block open.
  useEffect(() => {
    if (completedRef.current || failedRef.current) {
      setPhase(completedRef.current ? "complete" : "failed");
      return;
    }
    const controller = new AbortController();
    let cancelled = false;

    clearIdle();
    const idleMs = triggeredRef.current
      ? IDLE_TRIGGERED_MS
      : IDLE_UNTRIGGERED_MS;
    idleTimerRef.current = setTimeout(resolveSilence, idleMs);

    (async () => {
      try {
        await consumeRun(id, handleEvent, controller.signal);
      } catch {
        // A drop and a clean end are the same decision here: onStreamEnd reads the
        // run's own terminal state to tell "finished" from "reconnect".
      }
      if (cancelled || controller.signal.aborted) return;
      onStreamEnd();
    })();

    return () => {
      cancelled = true;
      controller.abort();
      clearIdle();
      // Drop a pending reconnect so re-subscribing (a re-trigger, or the next
      // generation) can't be torn down ~RECONNECT_MS later by a stale timer.
      if (reconnectTimerRef.current) {
        clearTimeout(reconnectTimerRef.current);
        reconnectTimerRef.current = null;
      }
    };
  }, [id, generation, handleEvent, onStreamEnd, resolveSilence, clearIdle]);

  useEffect(
    () => () => {
      if (reconnectTimerRef.current) clearTimeout(reconnectTimerRef.current);
    },
    [],
  );

  async function runPhase1() {
    setTriggering(true);
    setTriggerError(null);
    try {
      await triggerHypothesisLoop(id);
      markTriggered(id, false);
      triggeredRef.current = true;
      completedRef.current = false;
      failedRef.current = false;
      receivedRef.current = false;
      seqRef.current = 0;
      // A fresh local run supersedes the persisted status; drop it so the stale
      // failure reason and its Callout don't linger over the new run's triplets.
      persistedStatusRef.current = null;
      statusFetchedRef.current = true;
      setFailureReason(null);
      setItems([]);
      setDistribution(null);
      setSleep({ status: "idle" });
      setPhase("connecting");
      setGeneration((g) => g + 1); // Force a fresh subscription for the new run.
    } catch (err) {
      setTriggerError(
        errorMessage(err, "Could not start the hypothesis loop. Please retry."),
      );
    } finally {
      setTriggering(false);
    }
  }

  async function runSleepCycle() {
    setSleep({ status: "launching" });
    try {
      await triggerSleepCycle(id);
      setSleep({ status: "launched" });
    } catch (err) {
      setSleep({
        status: "error",
        message: errorMessage(err, "Could not launch the sleep cycle. Please retry."),
      });
    }
  }

  const triplets = items.filter(
    (i): i is Extract<FeedItem, { kind: "triplet" }> => i.kind === "triplet",
  );
  const failures = items.filter((i) => i.kind === "failure").length;
  // Only a measured triplet has an effect to normalize against; an extraction
  // run contributes none, which leaves the scale at zero and unused.
  const scale = triplets.reduce(
    (max, t) =>
      isExtraction(t.payload) ? max : Math.max(max, Math.abs(t.payload.effect_size)),
    0,
  );
  // A run's stream is homogeneous, so one extraction identifies the whole run.
  const extracting = triplets.some((t) => isExtraction(t.payload));
  const isRunning = phase === "connecting" || phase === "starting" || phase === "live";
  // "Settled" = the run is finished (completed or failed) or was never started —
  // the states that expose the sleep-cycle action and the re-run hint.
  const settled = phase === "complete" || phase === "failed" || phase === "neutral";

  return (
    <div className="flex flex-col gap-8 pt-4">
      <RunHeader
        id={id}
        phase={phase}
        tripletCount={triplets.length}
        failureCount={failures}
      />

      <div className="flex flex-wrap items-center gap-3">
        <Button onClick={runPhase1} loading={triggering} disabled={isRunning}>
          {settled ? "Re-run Phase 1" : "Run Phase 1"}
        </Button>
        {settled && (
          <Button
            variant="ghost"
            onClick={runSleepCycle}
            loading={sleep.status === "launching"}
            disabled={sleep.status === "launched"}
          >
            {sleep.status === "launched"
              ? "Sleep cycle launched"
              : "Run Sleep Cycle"}
          </Button>
        )}
        {settled && (
          <span className="text-xs text-faint">
            Re-running starts a brand-new run — triplets are appended, not
            resumed.
          </span>
        )}
      </div>

      {phase === "failed" && failureReason && (
        <Callout tone="error">{failureReason}</Callout>
      )}
      {triggerError && <Callout tone="error">{triggerError}</Callout>}
      {sleep.status === "error" && (
        <Callout tone="error">{sleep.message}</Callout>
      )}
      {sleep.status === "launched" && (
        <Callout tone="info">
          Sleep cycle launched. It runs asynchronously with no completion
          signal — new meta-heuristics appear on a later search in the{" "}
          <a href="/heuristics" className="text-signal underline-offset-2 hover:underline">
            heuristic browser
          </a>
          .
        </Callout>
      )}

      {/* A measuring run's outcomes are all at certainty, so its plot would be
          one full bar saying nothing. */}
      {extracting && distribution && (
        <ConfidenceHistogram distribution={distribution} />
      )}

      <Feed
        id={id}
        items={items}
        scale={scale}
        extracting={extracting}
        phase={phase}
        onRun={runPhase1}
        running={triggering}
      />
    </div>
  );
}

function RunHeader({
  id,
  phase,
  tripletCount,
  failureCount,
}: {
  id: string;
  phase: Phase;
  tripletCount: number;
  failureCount: number;
}) {
  return (
    <div className="flex flex-col gap-4 border-b border-line pb-6 md:flex-row md:items-end md:justify-between">
      <div className="flex flex-col gap-2">
        <SectionLabel>Hypothesis run</SectionLabel>
        <code className="font-mono text-sm text-muted">{id}</code>
      </div>
      <div className="flex items-center gap-6">
        <Stat label="triplets" value={tripletCount} />
        <Stat label="failures" value={failureCount} tone={failureCount ? "warn" : "muted"} />
        <StatusPill phase={phase} />
      </div>
    </div>
  );
}

function Stat({
  label,
  value,
  tone = "muted",
}: {
  label: string;
  value: number;
  tone?: "muted" | "warn";
}) {
  return (
    <div className="flex flex-col">
      <span
        className={cn(
          "font-mono text-2xl font-semibold tabular",
          tone === "warn" && value > 0 ? "text-warn" : "text-fg",
        )}
      >
        {value}
      </span>
      <SectionLabel>{label}</SectionLabel>
    </div>
  );
}

function StatusPill({ phase }: { phase: Phase }) {
  const map: Record<Phase, { label: string; dot: string; text: string; live?: boolean }> = {
    connecting: { label: "Connecting", dot: "bg-muted", text: "text-muted" },
    starting: { label: "Starting", dot: "bg-signal", text: "text-signal", live: true },
    live: { label: "Live", dot: "bg-signal", text: "text-signal", live: true },
    reconnecting: { label: "Reconnecting", dot: "bg-warn", text: "text-warn", live: true },
    complete: { label: "Complete", dot: "bg-muted", text: "text-muted" },
    failed: { label: "Failed", dot: "bg-neg", text: "text-neg" },
    neutral: { label: "Idle", dot: "bg-faint", text: "text-faint" },
  };
  const s = map[phase];
  return (
    <div className="flex items-center gap-2 rounded-full border border-line bg-surface px-3 py-1.5">
      <span
        className={cn("h-2 w-2 rounded-full", s.dot, s.live && "pulse-live")}
      />
      <span className={cn("text-xs font-medium", s.text)}>{s.label}</span>
    </div>
  );
}

function Feed({
  id,
  items,
  scale,
  extracting,
  phase,
  onRun,
  running,
}: {
  id: string;
  items: FeedItem[];
  scale: number;
  extracting: boolean;
  phase: Phase;
  onRun: () => void;
  running: boolean;
}) {
  if (items.length === 0) {
    if (phase === "neutral") {
      return (
        <EmptyState
          title="No active run"
          body="This run has finished or was never started. Its live triplets aren't retained after completion — trigger Phase 1 to generate a fresh run."
          action={
            <Button onClick={onRun} loading={running}>
              Run Phase 1
            </Button>
          }
        />
      );
    }
    if (phase === "complete") {
      return (
        <EmptyState
          title="Run complete"
          body="Phase 1 finished. Its triplets streamed live and were persisted to the graph; run the sleep cycle to distill them into meta-heuristics, then browse the results."
        />
      );
    }
    if (phase === "failed") {
      return (
        <EmptyState
          title="Run failed"
          body="This run ended with a failure — the reason is shown above. Re-running starts a brand-new run."
        />
      );
    }
    return (
      <EmptyState
        title={phase === "reconnecting" ? "Reconnecting…" : "Waiting for the loop"}
        body={
          phase === "reconnecting"
            ? "The stream dropped. The loop keeps running server-side; catching back up."
            : "Introspecting the data source and proposing the first interventions. The first triplet lands in a few seconds."
        }
        pending
      />
    );
  }

  const ordered = [...items].reverse();
  let tripletIndex = 0;
  return (
    <div className="flex flex-col gap-3">
      <div className="flex items-center justify-between">
        <SectionLabel>
          {extracting ? "Extracted fields" : "Causal triplets"} · newest first
        </SectionLabel>
        {phase === "reconnecting" && (
          <span className="text-xs text-warn">reconnecting…</span>
        )}
      </div>
      <div className="grid gap-3 sm:grid-cols-2">
        {ordered.map((item) => {
          if (item.kind === "failure") {
            return (
              <div key={item.seq} className="sm:col-span-2">
                <div className="animate-flash rounded-lg border border-warn/40 px-4 py-3 text-sm text-warn">
                  <span className="font-mono text-[11px] uppercase tracking-wider">
                    branch failure ·{" "}
                  </span>
                  {item.payload.error}
                </div>
              </div>
            );
          }
          // Index from oldest so animation delays are stable as newer items prepend.
          const idx = tripletIndex++;
          return isExtraction(item.payload) ? (
            <ExtractionReadout
              key={item.seq}
              triplet={item.payload}
              index={idx}
            />
          ) : (
            <EffectReadout
              key={item.seq}
              goalID={id}
              triplet={item.payload}
              scale={scale}
              index={idx}
            />
          );
        })}
      </div>
    </div>
  );
}

function EmptyState({
  title,
  body,
  action,
  pending,
}: {
  title: string;
  body: string;
  action?: React.ReactNode;
  pending?: boolean;
}) {
  return (
    <Panel className="flex flex-col items-center gap-4 px-6 py-16 text-center">
      {pending && (
        <span className="h-6 w-6 animate-spin rounded-full border-2 border-line-strong border-t-signal" />
      )}
      <div className="flex flex-col gap-2">
        <h2 className="text-lg font-semibold">{title}</h2>
        <p className="mx-auto max-w-md text-sm leading-relaxed text-muted">
          {body}
        </p>
      </div>
      {action}
    </Panel>
  );
}
