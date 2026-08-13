"use client";

import { useState } from "react";
import { errorMessage, verifyFinding } from "@/lib/orchestrator";
import { Button, Callout } from "@/components/ui";

// VerifyFindingButton asks for one finding to be verified causally. The dispatch
// answers immediately and the verification then runs for minutes, so the button
// commits to a waiting state and points at where the verdict will appear rather
// than pretending to hold a result. `pending` lets a caller that already knows a
// verification is in flight open in that state, so a reload does not re-offer a
// button whose work is already running.
export function VerifyFindingButton({
  goalID,
  interventionID,
  pending = false,
}: {
  goalID: string;
  interventionID: string;
  pending?: boolean;
}) {
  const [dispatching, setDispatching] = useState(false);
  const [dispatched, setDispatched] = useState(false);
  const [error, setError] = useState<string | null>(null);

  async function verify() {
    setError(null);
    setDispatching(true);
    try {
      await verifyFinding(goalID, interventionID);
      setDispatched(true);
    } catch (err) {
      setError(errorMessage(err, "Could not start verification. Please retry."));
    } finally {
      setDispatching(false);
    }
  }

  const waiting = dispatched || pending;
  return (
    <div className="flex flex-col items-start gap-2">
      <Button
        variant="ghost"
        onClick={verify}
        loading={dispatching}
        disabled={waiting}
      >
        {waiting ? "Verifying…" : "Verify causally"}
      </Button>
      {waiting && (
        <span className="text-[11px] leading-relaxed text-faint">
          Verifying — the verdict appears on the goal&rsquo;s Causal tab.
        </span>
      )}
      {error && <Callout tone="error">{error}</Callout>}
    </div>
  );
}
