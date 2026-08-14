import { describe, expect, it } from "vitest";
import { SESSION_COOKIE, isAdmin, isSignedIn, type SessionUser } from "./session";

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
