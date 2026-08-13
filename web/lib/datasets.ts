// Pure dataset-inventory logic shared by the inventory, detail, and goal-form
// views. Kept out of the components so the distinctions it encodes — an in-use
// dataset is refusing deletion, a search narrows a list — are testable without
// infrastructure.

import type { DatasetSummary } from "@/lib/orchestrator";

// isInUse reports whether a dataset refuses deletion because objectives live
// under it. The backend derives `usage` at read time; this is the spelling both
// the delete confirmation and the inventory copy rely on.
export function isInUse(d: Pick<DatasetSummary, "usage" | "objective_count">): boolean {
  return d.usage === "in_use" || d.objective_count > 0;
}

// filterDatasets narrows a list by a case-insensitive name substring, used by
// the inventory search box so a keystroke never costs a round trip. An empty
// query returns the list unchanged.
export function filterDatasets(
  datasets: DatasetSummary[],
  query: string,
): DatasetSummary[] {
  const q = query.trim().toLowerCase();
  if (!q) return datasets;
  return datasets.filter((d) => d.name.toLowerCase().includes(q));
}