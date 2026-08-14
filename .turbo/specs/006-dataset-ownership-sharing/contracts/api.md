# API Contract Deltas: Dataset Ownership & Sharing

Phase 1 output. The orchestrator is the only external API surface (the web BFF proxies it); the object store, Sandbox, and Neo4j are internal. Existing shapes are unchanged except where noted. The security boundary is server-side: every delta below is enforced by the orchestrator, never by the client.

## Datasets

### `GET /datasets?q=` (list)

- **Semantics change**: returns only datasets the acting user owns or has shared with them. Filtering is server-side; clients must not expect an absent dataset to surface as a placeholder, count, or error.
- **DTO change**: each item gains two fields:
  - `access: "owner" | "shared"` — derived from the access predicate (owner branch vs share branch).
  - `owner_id: string` — the owning account.
- Existing fields unchanged (`id`, `name`, `description`, `status`, `usage`, `objective_count`, `data_source_ref`, `created_at`, `updated_at`, `last_accessed_at`).

### `GET /datasets/{id}` (detail)

- **Semantics change**: requires access. An inaccessible id answers **404** `{"error":"dataset not found"}` (no existence oracle); owner/collaborator both succeed.
- **DTO change**: inherits `access` and `owner_id` from the summary shape, plus:
  - `owner_username: string` (nullable until the owner row is resolvable; always present for attributed data).
  - `shares: ShareGrant[]` — **present only when `access === "owner"`**; omitted (or empty) otherwise so a collaborator cannot enumerate recipients (FR-011).
    - `ShareGrant { user_id: string; username: string; created_at: string }`

### `POST /datasets`

- Creates the dataset owned by the acting user (`owner_id` = session identity). Response DTO gains `access: "owner"` and `owner_id`. No other change.

### `PATCH /datasets/{id}`

- **New 403**: a collaborator (access but not owner) is refused `403 {"error":"dataset ownership required"}`. A user with no access at all gets the 404 (never reaches this check). Owner path unchanged.

### `DELETE /datasets/{id}`

- Same access/ownership rule as PATCH: collaborator → 403; no access → 404. Empty-check/409-objectives and ref retirement unchanged.

## Sharing (owner-only)

### `GET /datasets/{id}/shares`

- Returns `ShareGrant[]` for the **owner**; 403 for a collaborator; 404 for no-access/unknown dataset.

### `PUT /datasets/{id}/shares/{username}`

- Grants the named account working access to the dataset and its children. Body empty.
- Owner-only (403/404 as above). `username` is matched case-insensitively against the account roster (mirroring the sign-in lookup).
- **409** `dataset already shared with this user` when the grant already exists; **404** `user not found` for an unknown account; **400** when the username is the owner's own (an owner already has access; a self-share is a no-op error).
- 200/201 with the created `ShareGrant`.

### `DELETE /datasets/{id}/shares/{username}`

- Revokes the grant. Idempotent for an already-absent grant (204). Owner-only; collaborator → 403; no access → 404.

## Goals and children

### `POST /goals` (bound to `dataset_id`)

- **New gate**: the acting user must be able to access the target dataset (owner or collaborator). No access → **404** `dataset not found`; archived-dataset 409 and all ingest behavior unchanged.
- Unbound submissions (legacy ingest) mint/own the implicit dataset as the acting user (see data-model, implicit-minting).
- Response unchanged: `{optimization_function_id, dataset_id}`.

### `GET /goals?dataset_id=`

- Returns only goals whose dataset the user can access (all goals, or a dataset's goals). Filtering is server-side; the response shape is unchanged.

### Goal-keyed routes (`POST /goals/{id}/hypothesis-loop`, `GET /goals/{id}/stream`, `POST /goals/{id}/chat`, `POST /goals/{id}/sleep-cycle`, `GET /goals/{id}/verifications`, `POST /goals/{id}/verifications/{outcomeID}`, `GET /goals/{id}/outcomes`, `GET /goals/{id}/outcomes/{outcomeID}/excerpt`, `GET /goals/{id}/causal-graph`, `POST /goals/{id}/causal-graph/corrections`, `POST /goals/{id}/findings/{interventionID}/verify`, `GET /goals/{id}/causal-verifications`, `DELETE /goals/{id}`)

- **New gate**: goal → dataset → `accessible`. No access → **404** `goal not found`. Owner/collaborator behavior unchanged, with one addition: `DELETE /goals/{id}` is refused **403** to a collaborator who did not create the goal (`created_by` ≠ acting user) and whose actor is not the dataset owner (FR-014); the goal's creator or the dataset owner succeed.

## Heuristics

### `GET /heuristics/search?q=&k=&goal_id=`

- **Semantics change**: results limited to heuristics whose goal's dataset the user can access. A `goal_id` scope additionally verifies access to that goal (404 if not). Legacy corpus rows (no goal) appear only when the acting user is the admin caretaker.
- Response shape unchanged (`{id, definition}[]`).

### `GET /heuristics/{id}/trace` and `DELETE /heuristics/{id}`

- Gate on the heuristic's owning goal's dataset; legacy (goal-less) rows gate on the caretaker. Inaccessible → **404**; delete refused the same way. Deletion additionally follows FR-014 (creator or dataset owner) for goal-scoped rows.

## Streams (SSE) — `GET /goals/{id}/stream`

- Access is validated at subscription; every forwarded frame re-checks access, and the stream is closed when it is lost (FR-008). Replay-from-hub content is subject to the same gate at subscribe time.

## Errors added

| Case | Status | Body |
|---|---|---|
| Single-object read with no access | 404 | `{"error":"dataset not found"}` / `{"error":"goal not found"}` |
| Owner-only write by a collaborator | 403 | `{"error":"dataset ownership required"}` |
| Deleting a goal a collaborator did not create | 403 | `{"error":"only the goal's creator or the dataset owner can delete it"}` |
| Duplicate share | 409 | `{"error":"dataset already shared with this user"}` |
| Self-share | 400 | `{"error":"you already own this dataset"}` |
| Unknown share recipient | 404 | `{"error":"user not found"}` |

## Audit events (vocabulary preserved)

New actions recorded with the codebase-wide detail keys:
- `dataset_share` → `{"dataset_id", "user_id", "shared_with": ...}` using `user_id` for the recipient account.
- `dataset_unshare` → `{"dataset_id", "user_id"}`.
- Existing `dataset_create` gains `owner_id`; `goal_submit` gains `created_by`; audited under the same `optimization_function_id` / `data_source_ref` / `dataset_id` keys.