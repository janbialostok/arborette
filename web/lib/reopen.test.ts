import { describe, expect, it } from "vitest";
import {
  REOPEN_BASE_MS,
  REOPEN_CAP_MS,
  applyReopenEvent,
  newReopenState,
  type ReopenEvent,
  type ReopenState,
} from "./reopen";

// Threads a sequence of events through the policy so a test can assert on a whole
// episode rather than one transition.
function run(events: ReopenEvent[], from: ReopenState = newReopenState()) {
  let state = from;
  return events.map((event) => {
    const decision = applyReopenEvent(state, event);
    state = decision.state;
    return decision;
  });
}

describe("applyReopenEvent", () => {
  describe("first-open credit", () => {
    // The initial subscription rides the mount read; adding a second read for it
    // would double every page's opening request count.
    it("does not reconcile on the first attempt", () => {
      const [first] = run(["attempt"]);
      expect(first.refetch).toBe(false);
      expect(first.state.opened).toBe(true);
    });

    it("reconciles on every attempt after the first", () => {
      const decisions = run(["attempt", "end", "attempt", "end", "attempt"]);
      expect(decisions.map((d) => d.refetch)).toEqual([
        false,
        false,
        true,
        false,
        true,
      ]);
    });

    // The gap a reconcile covers opens when the previous subscription ends, not when
    // the next one connects — so an attempt that never connects still spends the
    // credit, leaving the following attempt to read.
    it("spends the credit on an attempt that never connects", () => {
      const decisions = run(["attempt", "end", "attempt"]);
      expect(decisions[2].refetch).toBe(true);
    });
  });

  describe("backoff", () => {
    it("waits the base delay before the first reopen", () => {
      const [, ended] = run(["attempt", "end"]);
      expect(ended.reopenAfter).toBe(REOPEN_BASE_MS);
    });

    it("doubles the delay for each stream that ends without delivering", () => {
      const decisions = run(["attempt", "end", "attempt", "end", "attempt", "end"]);
      expect(decisions.filter((_, i) => i % 2 === 1).map((d) => d.reopenAfter)).toEqual(
        [REOPEN_BASE_MS, REOPEN_BASE_MS * 2, REOPEN_BASE_MS * 4],
      );
    });

    it("stops doubling at the cap", () => {
      let state = newReopenState();
      for (let i = 0; i < 20; i++) state = applyReopenEvent(state, "end").state;
      expect(state.delay).toBe(REOPEN_CAP_MS);
      expect(applyReopenEvent(state, "end").reopenAfter).toBe(REOPEN_CAP_MS);
    });

    // A delivered frame is the only evidence the stream works. Resetting on a
    // connection instead would turn a server that accepts and immediately drops into
    // a permanent base-delay loop, each cycle paying for a reconciling read.
    it("resets the delay only when a frame actually arrives", () => {
      const backedOff = run(["attempt", "end", "attempt", "end"]).at(-1)!.state;
      expect(backedOff.delay).toBeGreaterThan(REOPEN_BASE_MS);

      // An attempt alone earns nothing.
      expect(applyReopenEvent(backedOff, "attempt").state.delay).toBe(backedOff.delay);

      // A frame does.
      const healthy = applyReopenEvent(backedOff, "frame").state;
      expect(healthy.delay).toBe(REOPEN_BASE_MS);
      expect(applyReopenEvent(healthy, "end").reopenAfter).toBe(REOPEN_BASE_MS);
    });

    it("keeps a healthy stream at the base delay however many frames arrive", () => {
      const decisions = run(["attempt", "frame", "frame", "frame", "end"]);
      expect(decisions.at(-1)!.reopenAfter).toBe(REOPEN_BASE_MS);
    });
  });
});
