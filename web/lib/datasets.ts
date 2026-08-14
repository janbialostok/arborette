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

// The ownership vocabulary. `access` is the acting user's relationship to the
// dataset; every row the inventory carries is owner or shared (the backend
// never emits the rows a user cannot reach), so the UI copy keys off these two
// predicates rather than re-deriving the predicate itself.
export function isOwner(d: Pick<DatasetSummary, "access" | "owner_id">, actingUserId: string): boolean {
  return d.access === "owner" && d.owner_id === actingUserId;
}

export function isSharedToMe(d: Pick<DatasetSummary, "access">): boolean {
  return d.access === "shared";
}

// canAdminister gates the owner-only mutating actions (edit, archive, delete,
// share management): a collaborator can read and run, but only the owner
// changes the dataset. Ownerless legacy rows are off-limits to everyone until
// the backend attributes them.
export function canAdminister(
  d: Pick<DatasetSummary, "access" | "owner_id">,
  actingUserId: string,
): boolean {
  return d.access === "owner" && d.owner_id === actingUserId && d.owner_id !== null;
}

// canRegisterGoal gates the inventory/toolbar "run a goal against this"
// affordance. Explicitly shared datasets are open to the collaborator; datasets
// whose access is "none" (or an un-attributed ownerless row) are not.
export function canRegisterGoal(d: Pick<DatasetSummary, "access" | "owner_id">, actingUserId: string): boolean {
  return isOwner(d, actingUserId) || isSharedToMe(d);
}