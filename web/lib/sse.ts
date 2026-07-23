import type { OrchestratorEvent } from "./orchestrator";

// consumeStream reads an SSE response body, parsing `data: <json>\n\n` frames
// (the orchestrator emits no event:/id: lines) and invoking onEvent per frame.
// It resolves when the stream ends — the server closes it on loop_complete, or
// a drop cuts it — and rejects on a read error. Using a manual reader (not
// EventSource) lets the caller own termination and avoid auto-reconnect storms.
export async function consumeStream(
  body: ReadableStream<Uint8Array>,
  onEvent: (ev: OrchestratorEvent) => void,
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
          onEvent(JSON.parse(json) as OrchestratorEvent);
        } catch {
          // Skip a malformed frame rather than tearing down the whole stream.
        }
      }
    }
  } finally {
    reader.releaseLock();
  }
}
