// One streamed assistant turn, folded from chat frames and reduced to what the
// view should do with it. This lives outside the component because the commit
// rule is a contract, not a presentation detail: the orchestrator rejects a
// transcript whose messages are not all non-empty and whose last turn is not the
// analyst's, and the browser holds the only copy of that transcript. Committing
// an empty or half-streamed assistant turn would make every later message fail
// with no way to prune the bad turn short of reloading the page.

import type { ChatFrame } from "./orchestrator";

export const CHAT_FALLBACK = "The agent is unavailable right now. Please retry.";
export const CHAT_TRUNCATED =
  "The reply ended before it finished. Send the message again to retry.";
export const CHAT_SILENT = "The agent ended its turn without saying anything.";

export type ToolState = "running" | "done" | "failed";

export interface ToolActivity {
  tool: string;
  toolID: string;
  state: ToolState;
}

// ChatTurn is the turn in flight, accumulated outside the transcript so an
// incomplete reply is never posted back as history.
export interface ChatTurn {
  text: string;
  done: boolean;
  failure: string | null;
  activity: ToolActivity[];
}

export function newChatTurn(): ChatTurn {
  return { text: "", done: false, failure: null, activity: [] };
}

// applyChatFrame folds one frame into the turn.
export function applyChatFrame(turn: ChatTurn, frame: ChatFrame): ChatTurn {
  switch (frame.type) {
    case "chat_text":
      return { ...turn, text: turn.text + (frame.text ?? "") };
    case "chat_tool_use":
      return {
        ...turn,
        activity: [
          ...turn.activity,
          {
            tool: frame.tool || "tool",
            toolID: frame.tool_id ?? "",
            state: "running",
          },
        ],
      };
    case "chat_tool_result":
      return {
        ...turn,
        activity: settle(turn.activity, frame.tool_id, frame.is_error),
      };
    case "chat_done":
      return { ...turn, done: true };
    case "chat_error":
      return { ...turn, failure: frame.message || CHAT_FALLBACK };
    default:
      return turn;
  }
}

// TurnOutcome is what the view does once the stream ends: commit the reply to
// the transcript, or drop it and say why.
export type TurnOutcome =
  | { kind: "commit"; content: string }
  | { kind: "notice"; tone: "error" | "info"; message: string };

// turnOutcome decides a finished turn's fate: a failed or half-streamed turn
// reports instead of vanishing, so the analyst knows the reply was dropped.
export function turnOutcome(turn: ChatTurn): TurnOutcome {
  if (turn.failure) return { kind: "notice", tone: "error", message: turn.failure };
  // A stream that stops without its terminal frame is a dropped reply, not a
  // finished one.
  if (!turn.done) {
    return { kind: "notice", tone: "error", message: CHAT_TRUNCATED };
  }
  if (!turn.text.trim()) {
    return { kind: "notice", tone: "info", message: CHAT_SILENT };
  }
  return { kind: "commit", content: turn.text };
}

// settle closes out the tool call a result answers, matched by the id the API
// puts on both frames. A turn may open several tools before any of them report,
// so position alone cannot identify the call.
//
// An id that matches no open call settles nothing: it names a call this view
// never opened or already closed, and guessing a row for it would mark an
// unrelated tool — one still running — as finished or failed. Only a result
// carrying no id at all falls back to position, which would otherwise leave a
// row spinning for the rest of the turn.
function settle(
  activity: ToolActivity[],
  toolID: string | undefined,
  failed?: boolean,
): ToolActivity[] {
  const idx = toolID
    ? activity.findIndex((a) => a.toolID === toolID && a.state === "running")
    : activity.findLastIndex((a) => a.state === "running");
  if (idx === -1) return activity;
  const next = [...activity];
  next[idx] = { ...next[idx], state: failed ? "failed" : "done" };
  return next;
}
