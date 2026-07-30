import type { OrchestratorEvent } from "./orchestrator";

// consumeStream reads an SSE response body, parsing `data: <json>\n\n` frames
// (the orchestrator emits no event:/id: lines) and invoking onEvent per frame.
// It resolves when the stream ends — the server closes it on loop_complete, or
// a drop cuts it — and rejects on a read error. Using a manual reader (not
// EventSource) lets the caller own termination and avoid auto-reconnect storms.
//
// The frame type is a parameter because the service streams two unrelated
// unions over the same wire format — a run's progress and a chat turn — and
// they never mix on one connection.
export async function consumeStream<T = OrchestratorEvent>(
  body: ReadableStream<Uint8Array>,
  onEvent: (ev: T) => void,
  signal?: AbortSignal,
): Promise<void> {
  const reader = body.getReader();
  const decoder = new TextDecoder();
  let buffer = "";
  try {
    for (;;) {
      if (signal?.aborted) return;
      const { done, value } = await reader.read();
      if (done) return;
      buffer += decoder.decode(value, { stream: true });
      let sep: number;
      while ((sep = buffer.indexOf("\n\n")) !== -1) {
        const frame = buffer.slice(0, sep);
        buffer = buffer.slice(sep + 2);
        const dataLine = frame
          .split("\n")
          .find((line) => line.startsWith("data:"));
        if (!dataLine) continue;
        const json = dataLine.slice(5).trim();
        if (!json) continue;
        try {
          onEvent(JSON.parse(json) as T);
        } catch {
          // Skip a malformed frame rather than tearing down the whole stream.
        }
      }
    }
  } finally {
    reader.releaseLock();
  }
}
