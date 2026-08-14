import { describe, expect, it } from "vitest";
import {
  SESSION_COOKIE,
  isAdmin,
  isSignedIn,
  loginURL,
  safeNextPath,
  type SessionUser,
} from "./session";

const admin: SessionUser = {
  id: "u1",
  username: "boss",
  role: "admin",
  active: true,
};

const member: SessionUser = {
  id: "u2",
  username: "analyst",
  role: "member",
  active: true,
};

const inactive = { ...member, active: false };

describe("session cookie contract", () => {
  it("tracks the orchestrator ARBORETTE_SESSION_COOKIE default", () => {
    // Must match internal/orchestrator/session.go's sessionCookieDefaultName and
    // the config default; the web middleware reads this exact name.
    expect(SESSION_COOKIE).toBe("arborette_session");
  });
});

describe("isSignedIn", () => {
  it("accepts an active user", () => {
    expect(isSignedIn(admin)).toBe(true);
    expect(isSignedIn(member)).toBe(true);
  });

  it("rejects null and deactivated users (R3)", () => {
    expect(isSignedIn(null)).toBe(false);
    expect(isSignedIn(inactive)).toBe(false);
  });
});

describe("isAdmin", () => {
  it("is true only for an active admin", () => {
    expect(isAdmin(admin)).toBe(true);
    expect(isAdmin(member)).toBe(false);
    expect(isAdmin(null)).toBe(false);
  });

  it("hides the roster from a deactivated admin", () => {
    expect(isAdmin({ ...admin, active: false })).toBe(false);
  });
});

describe("safeNextPath", () => {
  it("accepts a same-origin relative path", () => {
    expect(safeNextPath("/datasets")).toBe("/datasets");
    expect(safeNextPath("/datasets/abc123")).toBe("/datasets/abc123");
  });

  it("rejects an absolute URL", () => {
    expect(safeNextPath("https://evil.example")).toBeNull();
    expect(safeNextPath("http://evil.example/path")).toBeNull();
  });

  it("rejects a protocol-relative path", () => {
    expect(safeNextPath("//evil.example")).toBeNull();
    expect(safeNextPath("//evil.example/datasets")).toBeNull();
  });

  it("rejects a scheme-full value and empty input", () => {
    expect(safeNextPath("javascript:alert(1)")).toBeNull();
    expect(safeNextPath("")).toBeNull();
    expect(safeNextPath(null)).toBeNull();
    expect(safeNextPath(undefined)).toBeNull();
  });
});

describe("loginURL", () => {
  it("builds /login when there is no destination", () => {
    expect(loginURL()).toBe("/login");
  });

  it("carries a safe relative destination as percent-encoded ?next=", () => {
    expect(loginURL("/datasets/abc123")).toBe(
      "/login?next=%2Fdatasets%2Fabc123",
    );
    expect(loginURL("/datasets?q=alpha")).toBe(
      "/login?next=%2Fdatasets%3Fq%3Dalpha",
    );
  });

  it("round-trips through safeNextPath", () => {
    const url = loginURL("/datasets/abc123");
    const next = safeNextPath(new URL(url, "http://localhost").searchParams.get("next"));
    expect(next).toBe("/datasets/abc123");
  });

  it("ignores an unsafe destination and falls back to /login", () => {
    expect(loginURL("https://evil.example")).toBe("/login");
    expect(loginURL("//evil.example")).toBe("/login");
  });
});
