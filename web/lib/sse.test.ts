import { describe, expect, it } from "vitest";
import { consumeStream } from "./sse";
import type { OrchestratorEvent } from "./orchestrator";

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
