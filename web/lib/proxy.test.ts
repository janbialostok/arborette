import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { forward, forwardStream, forwardStreamSubmit, forwardSubmit } from "./proxy";

const CHAT_URL = "http://web.local/api/orchestrator/goals/g1/chat";

beforeEach(() => {
  process.env.ORCHESTRATOR_URL = "http://orchestrator:8080";
});

afterEach(() => {
  delete process.env.ORCHESTRATOR_URL;
  vi.unstubAllGlobals();
  vi.restoreAllMocks();
});

function chatRequest(): Request {
  return new Request(CHAT_URL, {
    method: "POST",
    headers: { "content-type": "application/json" },
    body: JSON.stringify({ messages: [{ role: "user", content: "hi" }] }),
  });
}

describe("forwardStreamSubmit", () => {
  it("streams the request body upstream and dresses the reply as an event stream", async () => {
    const fetchMock = vi.fn().mockResolvedValue(
      new Response('data: {"type":"chat_done"}\n\n', {
        status: 200,
        headers: { "content-type": "text/event-stream" },
      }),
    );
    vi.stubGlobal("fetch", fetchMock);

    const res = await forwardStreamSubmit(chatRequest(), "/goals/g1/chat");

    expect(res.status).toBe(200);
    expect(res.headers.get("content-type")).toBe(
      "text/event-stream; charset=utf-8",
    );
    expect(res.headers.get("cache-control")).toBe("no-cache, no-transform");
    expect(res.headers.get("x-accel-buffering")).toBe("no");
    await expect(res.text()).resolves.toBe('data: {"type":"chat_done"}\n\n');

    const [url, init] = fetchMock.mock.calls[0];
    expect(url).toBe("http://orchestrator:8080/goals/g1/chat");
    expect(init.method).toBe("POST");
    expect(init.duplex).toBe("half");
    expect(init.dispatcher).toBeDefined();
    expect(init.headers.get("content-type")).toBe("application/json");
    await expect(new Response(init.body).text()).resolves.toBe(
      '{"messages":[{"role":"user","content":"hi"}]}',
    );
  });

  it("relays a pre-stream JSON error with its status and content type", async () => {
    vi.stubGlobal(
      "fetch",
      vi.fn().mockResolvedValue(
        new Response(JSON.stringify({ error: "agent preview is not configured" }), {
          status: 503,
          headers: { "content-type": "application/json" },
        }),
      ),
    );

    const res = await forwardStreamSubmit(chatRequest(), "/goals/g1/chat");

    expect(res.status).toBe(503);
    expect(res.headers.get("content-type")).toBe("application/json");
    expect(res.headers.get("x-accel-buffering")).toBeNull();
    await expect(res.json()).resolves.toEqual({
      error: "agent preview is not configured",
    });
  });

  it("masks a transport failure behind the generic unavailable message", async () => {
    vi.spyOn(console, "error").mockImplementation(() => {});
    vi.stubGlobal("fetch", vi.fn().mockRejectedValue(new Error("ECONNREFUSED")));

    const res = await forwardStreamSubmit(chatRequest(), "/goals/g1/chat");

    expect(res.status).toBe(502);
    await expect(res.json()).resolves.toEqual({
      error: "orchestrator is unavailable",
    });
  });

  // A quiet stream is the normal case here — the agent can think for a while
  // before its first frame — so the inactivity timers must be off.
  it("disables undici's inactivity timers on the streaming dispatcher", async () => {
    const fetchMock = vi.fn().mockResolvedValue(new Response("", { status: 200 }));
    vi.stubGlobal("fetch", fetchMock);

    await forwardStreamSubmit(chatRequest(), "/goals/g1/chat");

    const dispatcher = fetchMock.mock.calls[0][1].dispatcher;
    // undici keeps an Agent's options behind a private symbol, so this reads an
    // internal to pin the setting that matters. If an undici upgrade renames it,
    // this fails as "undefined" — re-locate the field rather than reading it as
    // a regression in the helper.
    const options = Object.getOwnPropertySymbols(dispatcher).find(
      (s) => s.description === "options",
    );
    expect(options).toBeDefined();
    expect(dispatcher[options!]).toMatchObject({
      bodyTimeout: 0,
      headersTimeout: 0,
    });
  });
});

describe("forwardStream", () => {
  it("relays a pre-stream JSON error rather than dressing it as an event stream", async () => {
    vi.stubGlobal(
      "fetch",
      vi.fn().mockResolvedValue(
        new Response(JSON.stringify({ error: "goal not found" }), {
          status: 404,
          headers: { "content-type": "application/json" },
        }),
      ),
    );

    const req = new Request("http://web.local/api/orchestrator/goals/g1/stream");
    const res = await forwardStream(req, "/goals/g1/stream");

    expect(res.status).toBe(404);
    expect(res.headers.get("content-type")).toBe("application/json");
    await expect(res.json()).resolves.toEqual({ error: "goal not found" });
  });
});

describe("forward", () => {
  it("passes the query string through and follows the client away", async () => {
    const fetchMock = vi.fn().mockResolvedValue(
      new Response("[]", { status: 200, headers: { "content-type": "application/json" } }),
    );
    vi.stubGlobal("fetch", fetchMock);

    const req = new Request(
      "http://web.local/api/orchestrator/goals/g1/verifications?status=pending",
    );
    await forward(req, "/goals/g1/verifications");

    const [url, init] = fetchMock.mock.calls[0];
    expect(url).toBe("http://orchestrator:8080/goals/g1/verifications?status=pending");
    expect(init.signal).toBe(req.signal);
  });

  it("arms the submit timeout in place of the client signal", async () => {
    const fetchMock = vi.fn().mockResolvedValue(new Response("{}", { status: 200 }));
    vi.stubGlobal("fetch", fetchMock);

    const req = new Request("http://web.local/api/orchestrator/goals/g1/verifications/o1", {
      method: "POST",
      headers: { "content-type": "application/json" },
      body: '{"action":"confirm"}',
    });
    await forwardSubmit(req, "/goals/g1/verifications/o1");

    const [, init] = fetchMock.mock.calls[0];
    expect(init.signal).not.toBe(req.signal);
    expect(init.signal.aborted).toBe(false);
    expect(init.duplex).toBe("half");
  });
});
