import { describe, expect, it } from "vitest";
import {
  applyChatFrame,
  newChatTurn,
  turnOutcome,
  CHAT_FALLBACK,
  CHAT_SILENT,
  CHAT_TRUNCATED,
  type ChatTurn,
} from "./chatTurn";
import type { ChatFrame } from "./orchestrator";

function fold(frames: ChatFrame[]): ChatTurn {
  return frames.reduce(applyChatFrame, newChatTurn());
}

describe("applyChatFrame", () => {
  it("accumulates streamed text across frames", () => {
    const turn = fold([
      { type: "chat_text", text: "Two heuristics " },
      { type: "chat_text", text: "cover that segment." },
    ]);
    expect(turn.text).toBe("Two heuristics cover that segment.");
  });

  it("tolerates a text frame that carries no text", () => {
    expect(fold([{ type: "chat_text" }]).text).toBe("");
  });

  it("opens a row per tool call and settles it on its result", () => {
    const turn = fold([
      { type: "chat_tool_use", tool: "get_optimized_heuristics", tool_id: "t1" },
      { type: "chat_tool_result", tool_id: "t1" },
      { type: "chat_tool_use", tool: "trace_causal_chain", tool_id: "t2" },
      { type: "chat_tool_result", tool_id: "t2", is_error: true },
    ]);
    expect(turn.activity).toEqual([
      { tool: "get_optimized_heuristics", toolID: "t1", state: "done" },
      { tool: "trace_causal_chain", toolID: "t2", state: "failed" },
    ]);
  });

  it("settles the call a result names, not the most recent one", () => {
    const turn = fold([
      { type: "chat_tool_use", tool: "get_optimized_heuristics", tool_id: "t1" },
      { type: "chat_tool_use", tool: "trace_causal_chain", tool_id: "t2" },
      { type: "chat_tool_result", tool_id: "t1", is_error: true },
      { type: "chat_tool_result", tool_id: "t2" },
    ]);
    expect(turn.activity).toEqual([
      { tool: "get_optimized_heuristics", toolID: "t1", state: "failed" },
      { tool: "trace_causal_chain", toolID: "t2", state: "done" },
    ]);
  });

  it("falls back to the most recent open call when a result carries no id", () => {
    const turn = fold([
      { type: "chat_tool_use", tool: "a", tool_id: "t1" },
      { type: "chat_tool_use", tool: "b", tool_id: "t2" },
      { type: "chat_tool_result" },
    ]);
    expect(turn.activity.map((a) => a.state)).toEqual(["running", "done"]);
  });

  it("names an unnamed tool rather than rendering a blank row", () => {
    expect(fold([{ type: "chat_tool_use" }]).activity).toEqual([
      { tool: "tool", toolID: "", state: "running" },
    ]);
  });

  it("ignores a result that pairs with no open call", () => {
    expect(fold([{ type: "chat_tool_result", tool_id: "t9" }]).activity).toEqual([]);
  });

  // Falling back to position would brand a still-running tool as failed.
  it("leaves running calls alone when a result names an unknown id", () => {
    const turn = fold([
      { type: "chat_tool_use", tool: "a", tool_id: "t1" },
      { type: "chat_tool_use", tool: "b", tool_id: "t2" },
      { type: "chat_tool_result", tool_id: "t9", is_error: true },
    ]);
    expect(turn.activity.map((a) => a.state)).toEqual(["running", "running"]);
  });

  it("ignores a duplicate result for a call already settled", () => {
    const turn = fold([
      { type: "chat_tool_use", tool: "a", tool_id: "t1" },
      { type: "chat_tool_use", tool: "b", tool_id: "t2" },
      { type: "chat_tool_result", tool_id: "t1" },
      { type: "chat_tool_result", tool_id: "t1", is_error: true },
    ]);
    expect(turn.activity).toEqual([
      { tool: "a", toolID: "t1", state: "done" },
      { tool: "b", toolID: "t2", state: "running" },
    ]);
  });

  it("records the terminal and error frames", () => {
    expect(fold([{ type: "chat_done" }]).done).toBe(true);
    expect(fold([{ type: "chat_error", message: "upstream is down" }]).failure).toBe(
      "upstream is down",
    );
  });

  it("substitutes the generic message for an error frame with none", () => {
    expect(fold([{ type: "chat_error" }]).failure).toBe(CHAT_FALLBACK);
  });

  it("leaves the turn untouched by a frame type it does not know", () => {
    const turn = newChatTurn();
    expect(applyChatFrame(turn, { type: "not_a_frame" } as unknown as ChatFrame)).toBe(
      turn,
    );
  });
});

describe("turnOutcome", () => {
  it("commits a turn that finished with text", () => {
    const turn = fold([
      { type: "chat_text", text: "Nothing relevant has accumulated yet." },
      { type: "chat_done" },
    ]);
    expect(turnOutcome(turn)).toEqual({
      kind: "commit",
      content: "Nothing relevant has accumulated yet.",
    });
  });

  it("drops a turn that failed in-band, surfacing the backend's message", () => {
    const turn = fold([
      { type: "chat_text", text: "partial" },
      { type: "chat_error", message: "the agent is unavailable" },
    ]);
    expect(turnOutcome(turn)).toEqual({
      kind: "notice",
      tone: "error",
      message: "the agent is unavailable",
    });
  });

  it("drops a turn whose stream stopped without its terminal frame", () => {
    const turn = fold([{ type: "chat_text", text: "half a rep" }]);
    expect(turnOutcome(turn)).toEqual({
      kind: "notice",
      tone: "error",
      message: CHAT_TRUNCATED,
    });
  });

  it("drops a finished turn that said nothing, rather than committing an empty message", () => {
    const turn = fold([
      { type: "chat_tool_use", tool: "get_optimized_heuristics" },
      { type: "chat_tool_result" },
      { type: "chat_text", text: "   \n " },
      { type: "chat_done" },
    ]);
    expect(turnOutcome(turn)).toEqual({
      kind: "notice",
      tone: "info",
      message: CHAT_SILENT,
    });
  });

  it("prefers the in-band failure over the missing-terminal complaint", () => {
    const turn = fold([{ type: "chat_error", message: "boom" }]);
    expect(turnOutcome(turn)).toMatchObject({ message: "boom" });
  });
});
