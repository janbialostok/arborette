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

const SSE_HEADERS: HeadersInit = {
  "content-type": "text/event-stream; charset=utf-8",
  "cache-control": "no-cache, no-transform",
  connection: "keep-alive",
  // Defeat reverse-proxy response buffering so frames flush immediately.
  "x-accel-buffering": "no",
};

// passThrough relays the upstream response verbatim, carrying its status,
// content-type, and Set-Cookie so a session issued upstream (login/register
// signing the browser in) reaches the browser, and the orchestrator's {error}
// body reaches it parseable.
function passThrough(upstream: Response): Response {
  const headers = new Headers();
  copyResponseHeaders(headers, upstream, ["content-type", "set-cookie"]);
  return new Response(upstream.body, { status: upstream.status, headers });
}

// copyResponseHeaders copies the named headers from upstream into dst, using
// getAll-preserving semantics so a multi-value Set-Cookie survives (an
// underscore-prefixed cookie clearing alongside a fresh session value must not
// be collapsed).
function copyResponseHeaders(
  dst: Headers,
  upstream: Response,
  names: string[],
): void {
  for (const name of names) {
    if (name === "set-cookie") {
      for (const value of upstream.headers.getSetCookie()) {
        dst.append("set-cookie", value);
      }
    } else {
      const value = upstream.headers.get(name);
      if (value) dst.set(name, value);
    }
  }
}

// eventStream dresses a streaming reply as SSE. Both stream endpoints resolve
// the goal before setting any SSE header, so a fault answers JSON and only a 2xx
// may be relabelled -- relabelling an {error} body would hand the browser
// something it can parse as neither. A Set-Cookie from the upstream is still
// relayed (headers.setCookie is preserved on top of the SSE framing).
function eventStream(upstream: Response): Response {
  if (!upstream.ok) return passThrough(upstream);
  const headers = new Headers(SSE_HEADERS);
  for (const value of upstream.headers.getSetCookie()) {
    headers.append("set-cookie", value);
  }
  return new Response(upstream.body, {
    status: upstream.status,
    headers,
  });
}

// sessionCookie returns the browser's session cookie, if any. It is the one
// credential the BFF forwards: the orchestrator owns the session, the browser
// only ever holds it against the web origin.
function sessionCookie(req: Request): string | null {
  return req.headers.get("cookie");
}

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
  const cookie = sessionCookie(req);
  if (cookie) headers.set("cookie", cookie);

  const init: NodeRequestInit = { method: req.method, headers };
  const reads = req.method === "GET" || req.method === "HEAD";
  // A read follows the client away: nothing upstream is left half-done by
  // abandoning it, and the excerpt endpoint re-parses a whole document per call,
  // so a browser that navigates off should not leave the sandbox working. A
  // write never does, whatever its timeout — the handler behind it audits and
  // records on the request's own context, so cancelling mid-flight can land the
  // effect while losing its audit record.
  if (reads) init.signal = req.signal;
  if (!reads && req.body != null) {
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
  return passThrough(upstream);
}

export function forwardSubmit(req: Request, path: string): Promise<Response> {
  return forward(req, path, { timeoutMs: SUBMIT_TIMEOUT_MS });
}

export async function forwardStream(req: Request, path: string): Promise<Response> {
  const headers = new Headers({ accept: "text/event-stream" });
  const cookie = sessionCookie(req);
  if (cookie) headers.set("cookie", cookie);
  let upstream: Response;
  try {
    upstream = await fetch(upstreamURL(req, path), {
      method: "GET",
      headers,
      signal: req.signal,
      dispatcher: streamDispatcher(),
    } as NodeRequestInit);
  } catch (err) {
    return transportError(err);
  }
  return eventStream(upstream);
}

// forwardStreamSubmit is forwardStream for an endpoint whose stream is asked for
// with a POST body, which neither of the other two helpers covers: forwardStream
// sends no body, and forward leaves undici's inactivity timers armed, which would
// abort a turn that goes quiet while the agent thinks. The chat endpoint commits
// its 200 lazily, which eventStream already accounts for.
export async function forwardStreamSubmit(
  req: Request,
  path: string,
): Promise<Response> {
  const headers = new Headers({ accept: "text/event-stream" });
  const contentType = req.headers.get("content-type");
  if (contentType) headers.set("content-type", contentType);
  const cookie = sessionCookie(req);
  if (cookie) headers.set("cookie", cookie);

  const init: NodeRequestInit = {
    method: "POST",
    headers,
    signal: req.signal,
    dispatcher: streamDispatcher(),
  };
  if (req.body != null) {
    init.body = req.body;
    init.duplex = "half";
  }

  let upstream: Response;
  try {
    upstream = await fetch(upstreamURL(req, path), init);
  } catch (err) {
    return transportError(err);
  }
  return eventStream(upstream);
}
