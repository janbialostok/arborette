import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { getRunRecord, markCompleted, markTriggered } from "./runState";

// Minimal in-memory localStorage stubbed onto a fake `window`, so the module's
// `typeof window === "undefined"` guard sees a browser and reads/writes it.
function makeStorage() {
  const map = new Map<string, string>();
  return {
    getItem: (k: string) => (map.has(k) ? map.get(k)! : null),
    setItem: (k: string, v: string) => void map.set(k, v),
    removeItem: (k: string) => void map.delete(k),
    clear: () => map.clear(),
  };
}

let storage: ReturnType<typeof makeStorage>;

beforeEach(() => {
  storage = makeStorage();
  vi.stubGlobal("window", { localStorage: storage });
});

afterEach(() => {
  vi.unstubAllGlobals();
});

describe("runState", () => {
  it("returns the un-triggered default for an unknown id", () => {
    expect(getRunRecord("nope")).toEqual({ triggered: false, completed: false });
  });

  it("markTriggered records triggered and defaults completed to false", () => {
    markTriggered("g1");
    expect(getRunRecord("g1")).toEqual({ triggered: true, completed: false });
  });

  it("markTriggered can seed completed and clears it on re-trigger", () => {
    markTriggered("g1", true);
    expect(getRunRecord("g1")).toEqual({ triggered: true, completed: true });
    markTriggered("g1");
    expect(getRunRecord("g1")).toEqual({ triggered: true, completed: false });
  });

  it("markCompleted preserves the triggered flag", () => {
    markTriggered("g1");
    markCompleted("g1");
    expect(getRunRecord("g1")).toEqual({ triggered: true, completed: true });
  });

  it("falls back to the default on malformed stored JSON", () => {
    storage.setItem("arborette:run:g1", "{not json");
    expect(getRunRecord("g1")).toEqual({ triggered: false, completed: false });
  });

  it("coerces partial/garbage stored shapes with Boolean", () => {
    storage.setItem("arborette:run:g1", JSON.stringify({ triggered: 1 }));
    expect(getRunRecord("g1")).toEqual({ triggered: true, completed: false });
  });

  it("returns the SSR-safe default when window is absent", () => {
    vi.unstubAllGlobals();
    expect(getRunRecord("g1")).toEqual({ triggered: false, completed: false });
  });
});
