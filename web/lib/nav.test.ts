import { existsSync } from "node:fs";
import { join } from "node:path";
import { describe, expect, it } from "vitest";
import { NAV_LINKS } from "./nav";

// US5: the primary header is exactly brand + Datasets + the corner chip. These
// tests hold the links list to that contract without component tests: exactly
// one entry, no Objectives/Heuristics, and every href maps to a real route.
describe("NAV_LINKS", () => {
  it("has exactly one entry: Datasets", () => {
    expect(NAV_LINKS).toHaveLength(1);
    expect(NAV_LINKS[0]).toEqual({ href: "/datasets", label: "Datasets" });
  });

  it("never references the removed header entries", () => {
    const labels = NAV_LINKS.map((l) => l.label);
    expect(labels).not.toContain("Objectives");
    expect(labels).not.toContain("Heuristics");
    const hrefs = NAV_LINKS.map((l) => l.href);
    expect(hrefs.some((h) => h.startsWith("/goals"))).toBe(false);
    expect(hrefs.some((h) => h.startsWith("/heuristics"))).toBe(false);
  });

  it("maps every href to a real route page", () => {
    // lib/ sits next to app/, so a href like /datasets must have app/datasets/.
    for (const link of NAV_LINKS) {
      const pageModule = join(__dirname, "..", "app", link.href, "page.tsx");
      expect(existsSync(pageModule), `${link.href} must resolve to ${pageModule}`).toBe(true);
    }
  });
});