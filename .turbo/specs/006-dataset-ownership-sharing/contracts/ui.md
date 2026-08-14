# UI Contract Deltas: Dataset Ownership & Sharing

Phase 1 output. The console keeps its existing routes and chrome; this feature changes what a given signed-in user sees and may do. Behavior described here is the user-visible envelope — the server (see [api.md](./api.md)) remains the authority.

## Dataset inventory (home + `/datasets`)

- A row owned by the viewer renders as today.
- A row shared with the viewer gets a neutral **"shared with you"** marker (alongside the status/objective badges), so the distinction is visible at a glance (US1.3).
- Nothing else appears. A viewer with no ownership or share sees no row, no placeholder, and no count hinting at hidden datasets.

## Dataset detail (`/datasets/[id]`)

The detail view is role-aware, driven by the `access` field.

**Owner** (`access: "owner"`):
- Metadata editor, objective registration form, delete control — all as today.
- New **"Sharing"** panel: lists recipients (username + grant time), adds a recipient by username, revokes a recipient. Empty state invites the first share.

**Collaborator** (`access: "shared"`):
- Sees the dataset name/description/status, the objective list, and — if the dataset is active — the objective registration form (working access includes creating goals, FR-009).
- Does **not** see the metadata editor, the delete control, or the sharing panel; those affordances are absent, not disabled (the server 403s/404s if driven anyway).
- A small marker notes the dataset is shared with them rather than owned.

**Neither** (direct URL to an inaccessible dataset): the detail view renders the not-found state (the 404 surface), consistent with a deleted or unknown dataset — no partial dataset content.

## Goals list, goal detail, heuristics browser

- No layout change: the lists are server-filtered, so a user simply sees fewer rows. A refused deep link surfaces the not-found/error state rather than a content frame.

## Share management interactions (owner only)

- **Add**: type a username → `PUT /datasets/{id}/shares/{username}` → on success the recipient appears in the list; on `409 already shared`, `404 user not found`, or `400 self-share`, surface the exact `{error}` message.
- **Revoke**: confirm then remove → the recipient drops from the list; henceforth the recipient's inventory no longer shows the dataset (its client views are stale until their next load, and the server 404s/403s any in-flight action, FR-008).

## Client rules living in `web/lib/`

To keep the derivations unit-testable in vitest (no component tests in this repo, per AGENTS.md):
- `isOwner(dataset.access) === dataset.access === "owner"`
- `isSharedToMe(dataset.access) === dataset.access === "shared"`
- `canAdminister(dataset.access) === isOwner(...)` — gates the Metadata editor, DangerZone, and sharing panel.
- `canRegisterGoal(dataset.status, dataset.access)` — active for both owner and collaborator, archived for neither (existing rule, now access-aware).
- The shared-row marker and the owner-only panel render solely from these predicates.

## Error presentation

- Inaccessible-object reads surface the server's 404 message ("dataset not found" / "goal not found") as the not-found trust state — the same tone as a deleted object; no special "permission" framing that would hint at the object's existence.
- Owner-only actions driven anyway (stale tabs) surface the server's 403 text verbatim.