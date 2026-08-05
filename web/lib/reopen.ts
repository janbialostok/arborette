// The retry policy behind a subscription that has no terminal state to stop at.
// A run view can stop reconnecting when its run finishes; a surface that watches
// for verdicts arriving at any time cannot, so these rules are the only thing
// bounding it.

// The delay before reopening, and its ceiling. Every reopen also costs a
// reconciling read, so a stream that keeps ending must cost progressively less.
export const REOPEN_BASE_MS = 1500;
export const REOPEN_CAP_MS = 30_000;

// ReopenState is what the policy remembers between attempts: how long to wait
// before the next one, and whether a subscription has already been opened at all.
export interface ReopenState {
  delay: number;
  opened: boolean;
}

// What just happened to the stream.
//
// - attempt: a connection is being opened.
// - frame:   a data frame arrived, which is the ONLY evidence the stream is
//            healthy. A connection accepted and dropped before delivering
//            anything has earned nothing.
// - end:     the stream finished or faulted. Both reopen; only the delay differs,
//            and the delay is carried here rather than by which path ended it.
export type ReopenEvent = "attempt" | "frame" | "end";

export interface ReopenDecision {
  state: ReopenState;
  // Reconcile by reading, because events published while nothing was subscribed
  // were dropped. False on the very first attempt, which rides the initial read.
  refetch: boolean;
  // How long to wait before reopening, for the caller that is about to. Always the
  // delay this state carries — an event that does not end the connection simply
  // leaves it unspent.
  reopenAfter: number;
}

export function newReopenState(): ReopenState {
  return { delay: REOPEN_BASE_MS, opened: false };
}

// applyReopenEvent advances the policy. Opening credits the first attempt against
// the initial read; a delivered frame resets the delay; an ended stream hands back
// the delay to wait and doubles it toward the cap.
//
// The first-attempt credit is spent on the attempt, not on a successful connection,
// so an opening that never connects still leaves the next one reconciling — which
// is what it must do, since the gap it has to cover opened the moment the previous
// subscription ended.
export function applyReopenEvent(
  state: ReopenState,
  event: ReopenEvent,
): ReopenDecision {
  switch (event) {
    case "attempt":
      return {
        state: { ...state, opened: true },
        refetch: state.opened,
        reopenAfter: state.delay,
      };
    case "frame":
      return {
        state: { ...state, delay: REOPEN_BASE_MS },
        refetch: false,
        reopenAfter: REOPEN_BASE_MS,
      };
    case "end":
      return {
        state: { ...state, delay: Math.min(state.delay * 2, REOPEN_CAP_MS) },
        refetch: false,
        reopenAfter: state.delay,
      };
  }
}
