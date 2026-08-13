"use client";

import { useState } from "react";
import { useRouter } from "next/navigation";
import { chatUrl, deleteGoal, errorMessage } from "@/lib/orchestrator";
import { AgentChat } from "@/components/AgentChat";
import { CausalTab } from "@/components/CausalTab";
import { LiveRun } from "@/components/LiveRun";
import { VerificationQueue } from "@/components/VerificationQueue";
import { Callout, cn, SectionLabel } from "@/components/ui";

const TABS = [
  { id: "run", label: "Run" },
  { id: "causal", label: "Causal" },
  { id: "verify", label: "Verify" },
  { id: "agent", label: "Preview agent" },
] as const;

type TabID = (typeof TABS)[number]["id"];

export function GoalTabs({
  id,
  initialTab,
}: {
  id: string;
  initialTab?: string;
}) {
  const [tab, setTab] = useState<TabID>(() => toTab(initialTab));
  const [deleting, setDeleting] = useState(false);
  const [deleteError, setDeleteError] = useState<string | null>(null);
  const router = useRouter();
  const [visited, setVisited] = useState<Set<TabID>>(
    () => new Set<TabID>([toTab(initialTab)]),
  );

  // The run is always mounted, however the page was opened: it owns the live
  // subscription, and a stream joined late misses everything the replay buffer
  // has already dropped. The other tabs mount on first visit and stay mounted,
  // so switching away never costs their loaded state.

  // deleteObjective retires the goal and everything under it. A running loop or
  // an in-flight verification is refused with the 409's named reason, surfaced
  // verbatim; on success the analyst returns to the objectives list.
  async function deleteObjective() {
    if (!window.confirm("Delete this objective, its runs, and its graph?")) return;
    setDeleting(true);
    setDeleteError(null);
    try {
      await deleteGoal(id);
      router.push("/goals");
    } catch (err) {
      setDeleteError(
        errorMessage(err, "Could not delete the objective. Please retry."),
      );
      setDeleting(false);
    }
  }

  // The URL mirrors the tab so a view is linkable, but the state — not the URL —
  // decides what renders: a router navigation would re-render the route and put
  // the run's subscription and accumulated triplets at risk.
  function select(next: TabID) {
    setTab(next);
    setVisited((seen) => (seen.has(next) ? seen : new Set(seen).add(next)));
    const url = new URL(window.location.href);
    url.searchParams.set("tab", next);
    window.history.replaceState(null, "", url);
  }

  return (
    <div className="flex flex-col">
      <div className="flex items-center justify-between border-b border-line pb-3">
        <div className="flex items-center gap-2">
          <SectionLabel className="font-mono normal-case tracking-normal">
            {id}
          </SectionLabel>
          <span className="font-mono text-[11px] text-faint">optimization function</span>
        </div>
        <button
          type="button"
          disabled={deleting}
          onClick={() => void deleteObjective()}
          className="border border-neg/30 px-2 py-1 text-[11px] uppercase tracking-wider text-neg transition-colors hover:border-neg/60 disabled:opacity-45"
        >
          {deleting ? "Deleting…" : "Delete"}
        </button>
      </div>
      {deleteError && <Callout tone="error">{deleteError}</Callout>}

      {/* Buttons with aria-pressed, not links with aria-current: no navigation
          happens here, and the app's other in-page segmented controls express
          the same "one of N selected" state the same way. */}
      <div className="flex items-center gap-1 border-b border-line">
        {TABS.map((t) => (
          <button
            key={t.id}
            type="button"
            onClick={() => select(t.id)}
            aria-pressed={tab === t.id}
            className={cn(
              "-mb-px border-b-2 px-3 py-2.5 text-sm transition-colors",
              tab === t.id
                ? "border-signal text-signal"
                : "border-transparent text-muted hover:text-fg",
            )}
          >
            {t.label}
          </button>
        ))}
      </div>

      <div className={cn(tab !== "run" && "hidden")}>
        <LiveRun id={id} />
      </div>
      {/* Kept mounted once visited because it owns a stream subscription: unmounting
          it would drop the verdicts a post-run verify publishes. */}
      {visited.has("causal") && (
        <div className={cn(tab !== "causal" && "hidden")}>
          <CausalTab id={id} />
        </div>
      )}
      {visited.has("verify") && (
        <div className={cn(tab !== "verify" && "hidden")}>
          <VerificationQueue id={id} />
        </div>
      )}
      {visited.has("agent") && (
        <div className={cn(tab !== "agent" && "hidden")}>
          <AgentChat endpoint={chatUrl(id)} />
        </div>
      )}
    </div>
  );
}

function toTab(value: string | undefined): TabID {
  return TABS.some((t) => t.id === value) ? (value as TabID) : "run";
}
