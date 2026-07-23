// Server-only BFF proxy to the orchestrator. The browser calls same-origin
// /api/orchestrator/*; these helpers forward to ${ORCHESTRATOR_URL}/* so the
// orchestrator's missing CORS is never hit and its URL never reaches the client
// bundle. The discipline mirrors the Go OrchestratorClient: surface the
// orchestrator's own {error} body verbatim (analyst-safe), mask transport
// failures into a generic message.

import { Agent, type Dispatcher } from "undici";

// A goal submission blocks on an inline Claude call; give the upstream fetch a
// generous overall cap so it covers that latency without hanging forever.
const SUBMIT_TIMEOUT_MS = 5 * 60 * 1000;

// undici defaults bodyTimeout to 300s — a gap-between-chunks timer that would
// abort a quiet SSE stream, since the orchestrator's 10-minute loop emits no
// heartbeat between triplets. This dispatcher disables both inactivity timers
// for the stream fetch only. Shared (not per-request) so sockets pool instead
// of leaking.
let streamAgent: Agent | null = null;
function streamDispatcher(): Dispatcher {
  if (!streamAgent) {
    streamAgent = new Agent({ bodyTimeout: 0, headersTimeout: 0 });
  }
  return streamAgent;
}

function base(): string {
  const url = process.env.ORCHESTRATOR_URL;
  if (!url) throw new Error("ORCHESTRATOR_URL is not set");
  return url.replace(/\/+$/, "");
}

function upstreamURL(req: Request, path: string): string {
  return `${base()}${path}${new URL(req.url).search}`;
}

function transportError(err: unknown): Response {
  console.error("orchestrator proxy: transport failure:", err);
  return new Response(JSON.stringify({ error: "orchestrator is unavailable" }), {
    status: 502,
    headers: { "content-type": "application/json" },
  });
}

// undici's RequestInit accepts a `dispatcher`, but the DOM fetch types don't
// declare it; this widens the init type without an `any` cast.
type NodeRequestInit = RequestInit & { duplex?: "half"; dispatcher?: Dispatcher };

// forward proxies a JSON or multipart request, preserving status and body. The
// request body is streamed through (never buffered/parsed) so the multipart
// 512 MiB upload path and its Content-Type boundary survive intact; streaming a
// body requires duplex: 'half' on Node/undici.
export async function forward(
  req: Request,
  path: string,
  opts: { timeoutMs?: number } = {},
): Promise<Response> {
  const headers = new Headers();
  const contentType = req.headers.get("content-type");
  if (contentType) headers.set("content-type", contentType);

  const init: NodeRequestInit = { method: req.method, headers };
  const hasBody = req.method !== "GET" && req.method !== "HEAD" && req.body != null;
  if (hasBody) {
    init.body = req.body;
    init.duplex = "half";
  }
  if (opts.timeoutMs) init.signal = AbortSignal.timeout(opts.timeoutMs);

  let upstream: Response;
  try {
    upstream = await fetch(upstreamURL(req, path), init);
  } catch (err) {
    return transportError(err);
  }

  const outHeaders = new Headers();
  const upstreamType = upstream.headers.get("content-type");
  if (upstreamType) outHeaders.set("content-type", upstreamType);
  return new Response(upstream.body, {
    status: upstream.status,
    headers: outHeaders,
  });
}

export function forwardSubmit(req: Request, path: string): Promise<Response> {
  return forward(req, path, { timeoutMs: SUBMIT_TIMEOUT_MS });
}

export async function forwardStream(req: Request, path: string): Promise<Response> {
  let upstream: Response;
  try {
    upstream = await fetch(upstreamURL(req, path), {
      method: "GET",
      headers: { accept: "text/event-stream" },
      signal: req.signal,
      dispatcher: streamDispatcher(),
    } as NodeRequestInit);
  } catch (err) {
    return transportError(err);
  }

  return new Response(upstream.body, {
    status: upstream.status,
    headers: {
      "content-type": "text/event-stream; charset=utf-8",
      "cache-control": "no-cache, no-transform",
      connection: "keep-alive",
      // Defeat reverse-proxy response buffering so frames flush immediately.
      "x-accel-buffering": "no",
    },
  });
}
