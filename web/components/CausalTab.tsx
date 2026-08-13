"use client";

import { useCallback, useEffect, useRef, useState } from "react";
import {
  OrchestratorError,
  errorMessage,
  getCausalGraph,
  listCausalVerifications,
  type CausalGraph as Graph,
  type CausalVerification,
  type CorrectionResult,
  type OrchestratorEvent,
} from "@/lib/orchestrator";
import {
  awaitingVerdict,
  causalTransition,
  correctedColumns,
  latestByIntervention,
} from "@/lib/causal";
import { applyReopenEvent, newReopenState, type ReopenEvent } from "@/lib/reopen";
import { consumeRun } from "@/lib/sse";
import { CausalGraph } from "@/components/CausalGraph";
import { CausalVerifications } from "@/components/CausalVerifications";
import { Callout, Panel, Spinner } from "@/components/ui";

// CausalTab owns the causal surface's reads and its live subscription. It holds
// its own stream rather than sharing the run's: verification runs against findings
// that exist only after a run completes, and the hub drops an event published to a
// dataset nobody is subscribed to — so without a subscription of its own, every
// verdict the analyst asked for would arrive nowhere.
export function CausalTab({ id }: { id: string }) {
  // undefined until a read has answered; null is "the dataset has no model", which is a
  // claim only a completed read can make.
  const [graph, setGraph] = useState<Graph | null | undefined>(undefined);
  const [verifications, setVerifications] = useState<CausalVerification[] | null>(
    null,
  );
  // One error per read. Sharing a slot would let whichever request settled last
  // decide what the analyst is told, and a success would erase the other's failure.
  const [graphError, setGraphError] = useState<string | null>(null);
  const [listError, setListError] = useState<string | null>(null);
  const [staleColumns, setStaleColumns] = useState<ReadonlySet<string>>(new Set());
  // One trigger per resource. A verification transition cannot change the graph,
  // and a single verify dispatch emits four of them, so a shared trigger would
  // spend eight requests to learn one thing.
  const [listGen, setListGen] = useState(0);
  const [graphGen, setGraphGen] = useState(0);

  // Whether a graph has been served yet, read from the stream handler, which must
  // stay identity-stable so the subscription is not torn down per frame.
  const hasGraph = useRef(false);

  const refetch = useCallback((withGraph: boolean) => {
    setListGen((g) => g + 1);
    if (withGraph) setGraphGen((g) => g + 1);
  }, []);

  const markStale = useCallback((columns: string[]) => {
    if (columns.length === 0) return;
    setStaleColumns((held) => new Set([...held, ...columns]));
  }, []);

  useEffect(() => {
    let cancelled = false;
    getCausalGraph(id)
      .catch((err: unknown) => {
        // A dataset with no discovered graph answers 404. That is this view's empty
        // state, not a failure: the first verification runs discovery inline.
        if (err instanceof OrchestratorError && err.status === 404) return null;
        throw err;
      })
      .then((next) => {
        if (cancelled) return;
        hasGraph.current = next !== null;
        setGraph(next);
        setGraphError(null);
      })
      .catch((err: unknown) => {
        if (cancelled) return;
        setGraphError(
          errorMessage(err, "Could not load the causal model. Please retry."),
        );
      });
    return () => {
      cancelled = true;
    };
  }, [id, graphGen]);

  useEffect(() => {
    let cancelled = false;
    listCausalVerifications(id)
      .then((next) => {
        if (cancelled) return;
        setVerifications(next);
        setListError(null);
        // The re-verifying marks clear once no finding is waiting on a verdict —
        // which a stale record and its freshly dispatched replacement both are.
        if (!awaitingVerdict(latestByIntervention(next))) setStaleColumns(new Set());
      })
      .catch((err: unknown) => {
        if (cancelled) return;
        setListError(
          errorMessage(err, "Could not load the causal verifications. Please retry."),
        );
      });
    return () => {
      cancelled = true;
    };
  }, [id, listGen]);

  const handleEvent = useCallback(
    (ev: OrchestratorEvent) => {
      const transition = causalTransition(ev);
      if (!transition) return;
      markStale(correctedColumns(ev));
      // A correction changes the graph; so does the first verification, which runs
      // discovery inline and commits the model this view exists to show. That
      // transition happens once and announces itself through no frame of its own,
      // so any transition re-reads the graph until one has been served.
      refetch(transition === "causal_graph_corrected" || !hasGraph.current);
    },
    [markStale, refetch],
  );

  // Subscribe for as long as this tab is mounted — which is until the page is left,
  // since the tab strip hides a visited tab rather than unmounting it. The reopen
  // policy decides when to try again and when a read is owed.
  useEffect(() => {
    const controller = new AbortController();
    let cancelled = false;
    let timer: ReturnType<typeof setTimeout> | null = null;
    let policy = newReopenState();

    function advance(event: ReopenEvent): number {
      const decision = applyReopenEvent(policy, event);
      policy = decision.state;
      if (decision.refetch) refetch(true);
      return decision.reopenAfter;
    }

    async function open() {
      if (cancelled) return;
      try {
        await consumeRun(
          id,
          (ev) => {
            advance("frame");
            handleEvent(ev);
          },
          controller.signal,
          // Reconcile once the connection is up, never before it: a read taken
          // ahead of the subscription cannot see what the subscription exists to
          // catch.
          () => advance("attempt"),
        );
      } catch {
        // A failed attempt opened no subscription, so it has nothing to reconcile
        // against — but it still crosses the boundary the first-open credit is
        // about, so the next open that does connect owes a read.
        if (!policy.opened) advance("attempt");
      }
      if (cancelled || controller.signal.aborted) return;
      timer = setTimeout(open, advance("end"));
    }
    open();

    return () => {
      cancelled = true;
      controller.abort();
      if (timer) clearTimeout(timer);
    };
  }, [id, handleEvent, refetch]);

  function onCorrected(next: Graph, result: CorrectionResult) {
    hasGraph.current = true;
    setGraph(next);
    if (result.redispatched > 0) markStale(result.columns);
    refetch(true);
  }

  const settled = graph !== undefined || verifications !== null;
  if (!settled && graphError === null && listError === null) {
    return (
      <Panel className="mt-4 flex items-center justify-center px-6 py-16 text-sm text-muted">
        <Spinner className="mr-2" /> Loading the causal model…
      </Panel>
    );
  }

  return (
    <div className="flex flex-col gap-8 pt-4">
      {graphError && <Callout tone="error">{graphError}</Callout>}
      {listError && <Callout tone="error">{listError}</Callout>}

      {graph ? (
        <CausalGraph
          goalID={id}
          graph={graph}
          staleColumns={staleColumns}
          onCorrected={onCorrected}
          onConflict={() => refetch(true)}
        />
      ) : (
        // Only claim the dataset has no model once a read has said so — null, not the
        // undefined of a read that never answered. An empty state is a statement
        // about the dataset, and a failed read is no evidence for one.
        graph === null && (
          <Callout tone="info">
            No causal graph has been discovered for this dataset yet. The first
            verification runs discovery inline — verify a finding, and the model it
            reasoned over appears here.
          </Callout>
        )
      )}

      {verifications && <CausalVerifications verifications={verifications} />}
    </div>
  );
}
