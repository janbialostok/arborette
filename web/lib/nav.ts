// The primary header navigation. US5 reduced it to a single entry: the header
// shows the brand mark, Datasets, and the corner chip -- the dataset flow is
// the analyst's path to objectives and heuristics, so no separate links
// duplicate it. Each href must resolve to a real route (nav.test.ts asserts
// this so a dead entry fails the suite).
export interface NavLink {
  href: string;
  label: string;
}

export const NAV_LINKS: NavLink[] = [{ href: "/datasets", label: "Datasets" }];