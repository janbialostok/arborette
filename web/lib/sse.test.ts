import { afterEach, describe, expect, it, vi } from "vitest";
import { consumeRun, consumeStream } from "./sse";
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

describe("consumeRun", () => {
  afterEach(() => {
    vi.unstubAllGlobals();
  });

  it("subscribes to the goal's stream and consumes its frames", async () => {
    const fetchMock = vi.fn().mockResolvedValue(
      new Response(streamOf(['data: {"type":"loop_complete"}\n\n']), { status: 200 }),
    );
    vi.stubGlobal("fetch", fetchMock);

    const events: OrchestratorEvent[] = [];
    await consumeRun("g 1", (ev) => events.push(ev), new AbortController().signal);

    expect(events).toEqual([{ type: "loop_complete" }]);
    const [url, init] = fetchMock.mock.calls[0];
    expect(url).toBe("/api/orchestrator/goals/g%201/stream");
    expect(init.headers).toEqual({ accept: "text/event-stream" });
  });

  // The throw is what turns an error response into the caller's reconnect decision.
  // Resolving instead would read as a stream that ended, and a 404 would look like a
  // finished run — silently, forever.
  it("throws when the stream cannot be established", async () => {
    vi.stubGlobal(
      "fetch",
      vi.fn().mockResolvedValue(new Response("nope", { status: 502 })),
    );

    await expect(
      consumeRun("g1", () => {}, new AbortController().signal),
    ).rejects.toThrow("stream status 502");
  });

  it("throws when a 200 carries no body to read", async () => {
    vi.stubGlobal(
      "fetch",
      vi.fn().mockResolvedValue({ ok: true, status: 200, body: null }),
    );

    await expect(
      consumeRun("g1", () => {}, new AbortController().signal),
    ).rejects.toThrow("stream status 200");
  });

  // Everything the causal surface does to reconcile a gap hangs off this ordering:
  // onOpen must fire after the response is known good and before any frame is
  // delivered, so a read it triggers is taken against an established subscription.
  it("fires onOpen once, after the status check and before the first frame", async () => {
    vi.stubGlobal(
      "fetch",
      vi.fn().mockResolvedValue(
        new Response(
          streamOf([
            'data: {"type":"triplet","payload":{}}\n\n',
            'data: {"type":"loop_complete"}\n\n',
          ]),
          { status: 200 },
        ),
      ),
    );

    const order: string[] = [];
    await consumeRun(
      "g1",
      (ev) => order.push(`frame:${ev.type}`),
      new AbortController().signal,
      () => order.push("open"),
    );

    expect(order).toEqual(["open", "frame:triplet", "frame:loop_complete"]);
  });

  // Aborting is how a caller stops receiving frames when its view goes away, and
  // that only works if the signal reaches the body reader — forgetting to pass it on
  // still aborts the request but leaves already-buffered frames arriving at a
  // component that has unmounted.
  it("stops delivering frames once the signal is aborted", async () => {
    const controller = new AbortController();
    vi.stubGlobal(
      "fetch",
      vi.fn().mockResolvedValue(
        new Response(
          streamOf([
            'data: {"type":"triplet","payload":{}}\n\n',
            'data: {"type":"loop_complete"}\n\n',
          ]),
          { status: 200 },
        ),
      ),
    );

    const seen: OrchestratorEvent[] = [];
    await consumeRun(
      "g1",
      (ev) => {
        seen.push(ev);
        controller.abort();
      },
      controller.signal,
    );

    expect(seen).toEqual([{ type: "triplet", payload: {} }]);
  });

  it("does not fire onOpen for a response it rejects", async () => {
    vi.stubGlobal(
      "fetch",
      vi.fn().mockResolvedValue(new Response("nope", { status: 404 })),
    );
    const onOpen = vi.fn();

    await expect(
      consumeRun("g1", () => {}, new AbortController().signal, onOpen),
    ).rejects.toThrow();
    expect(onOpen).not.toHaveBeenCalled();
  });
});
