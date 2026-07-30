import { describe, expect, it } from "vitest";
import { consumeStream } from "./sse";
import type { ChatFrame, OrchestratorEvent } from "./orchestrator";

// Build a ReadableStream that emits each string as its own Uint8Array chunk, so
// tests control exactly where chunk boundaries fall relative to frame boundaries.
function streamOf(chunks: string[]): ReadableStream<Uint8Array> {
  const encoder = new TextEncoder();
  let i = 0;
  return new ReadableStream({
    pull(controller) {
      if (i < chunks.length) {
        controller.enqueue(encoder.encode(chunks[i++]));
      } else {
        controller.close();
      }
    },
  });
}

async function collect(chunks: string[]): Promise<OrchestratorEvent[]> {
  const events: OrchestratorEvent[] = [];
  await consumeStream(streamOf(chunks), (ev) => events.push(ev));
  return events;
}

describe("consumeStream", () => {
  it("parses multiple data frames in a single chunk", async () => {
    const events = await collect([
      'data: {"type":"triplet","payload":{"baseline":1}}\n\n' +
        'data: {"type":"loop_complete"}\n\n',
    ]);
    expect(events.map((e) => e.type)).toEqual(["triplet", "loop_complete"]);
  });

  it("reassembles a frame split across chunk boundaries", async () => {
    const events = await collect([
      'data: {"type":"tri',
      'plet","payload":{"value":2}}\n',
      "\n",
    ]);
    expect(events).toEqual([{ type: "triplet", payload: { value: 2 } }]);
  });

  it("ignores frames without a data: line and empty data payloads", async () => {
    const events = await collect([
      ": comment\n\n",
      "data:\n\n",
      'data: {"type":"loop_complete"}\n\n',
    ]);
    expect(events.map((e) => e.type)).toEqual(["loop_complete"]);
  });

  it("swallows a malformed-JSON frame without tearing down the stream", async () => {
    const events = await collect([
      "data: {not valid json}\n\n",
      'data: {"type":"branch_failure","payload":{"error":"x"}}\n\n',
    ]);
    expect(events.map((e) => e.type)).toEqual(["branch_failure"]);
  });

  it("resolves when the stream ends", async () => {
    await expect(collect([])).resolves.toEqual([]);
  });

  it("rejects when the underlying reader errors", async () => {
    const failing = new ReadableStream<Uint8Array>({
      pull() {
        throw new Error("read failed");
      },
    });
    await expect(consumeStream(failing, () => {})).rejects.toThrow(
      "read failed",
    );
  });

  it("stops early when the abort signal is already aborted", async () => {
    const events: OrchestratorEvent[] = [];
    await consumeStream(
      streamOf(['data: {"type":"loop_complete"}\n\n']),
      (ev) => events.push(ev),
      AbortSignal.abort(),
    );
    expect(events).toEqual([]);
  });
});

// The chat endpoint streams a different union over the same framing, so the
// parser is exercised against it directly rather than assumed to carry over.
async function collectChat(chunks: string[]): Promise<ChatFrame[]> {
  const frames: ChatFrame[] = [];
  await consumeStream<ChatFrame>(streamOf(chunks), (ev) => frames.push(ev));
  return frames;
}

describe("consumeStream · chat frames", () => {
  it("parses a tool-using turn split across chunk boundaries", async () => {
    const frames = await collectChat([
      'data: {"type":"chat_tool_use","tool":"get_optimized_heuristics"}\n\n',
      'data: {"type":"chat_tool_result"}\n\ndata: {"type":"chat_te',
      'xt","text":"Two heuristics "}\n\n',
      'data: {"type":"chat_text","text":"cover that segment."}\n\n',
      'data: {"type":"chat_done"}\n\n',
    ]);
    expect(frames).toEqual([
      { type: "chat_tool_use", tool: "get_optimized_heuristics" },
      { type: "chat_tool_result" },
      { type: "chat_text", text: "Two heuristics " },
      { type: "chat_text", text: "cover that segment." },
      { type: "chat_done" },
    ]);
  });

  it("carries a failed tool result and an in-band error message", async () => {
    const frames = await collectChat([
      'data: {"type":"chat_tool_use","tool":"trace_causal_chain"}\n\n',
      'data: {"type":"chat_tool_result","is_error":true}\n\n',
      'data: {"type":"chat_error","message":"the agent is unavailable"}\n\n',
    ]);
    expect(frames).toEqual([
      { type: "chat_tool_use", tool: "trace_causal_chain" },
      { type: "chat_tool_result", is_error: true },
      { type: "chat_error", message: "the agent is unavailable" },
    ]);
  });

  it("ends without a terminal frame when the turn is cut off", async () => {
    const frames = await collectChat([
      'data: {"type":"chat_text","text":"Looking at "}\n\n',
    ]);
    expect(frames.some((f) => f.type === "chat_done")).toBe(false);
  });
});
