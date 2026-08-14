# Data Model: Dataset Ownership & Sharing

Phase 1 output. The access model is dataset-rooted; every entity below either records ownership or participates in the access predicate.

## Access predicate (the core invariant)

For a signed-in user `u` and a dataset `d`:

```
accessible(d, u) := d.owner_id = u.id
                 OR EXISTS (SELECT 1 FROM dataset_shares sh
                            WHERE sh.dataset_id = d.id AND sh.user_id = u.id)
```

- A user **owns** a dataset when `d.owner_id = u.id`.
- A user **collaborates** on a dataset when the share branch holds.
- Every parent→child read resolves to `accessible` on the child's dataset; there is no independent child-level visibility.
- This predicate is expressed as SQL and reused by every store read; the orchestrator is the only enforcer, the web client only renders its result.

## Entities

### Dataset (`datasets`)

| Field | Type | Notes |
|---|---|---|
| `id` | uuid PK | existing |
| `name` | text NOT NULL | existing, case-insensitive unique |
| `description` / `status` | text | existing |
| `datasource_ref` | text NOT NULL | existing, immutable |
| `created_at` / `updated_at` / `last_accessed_at` | timestamptz | existing |
| **`owner_id`** | uuid NULL REFERENCES users(id) | **NEW.** Creator when created by a signed-in user; NULL for pre-existing/system-created rows until attributed to the admin caretaker (migration backfill → first active admin, ordered by `created_at, id`; runtime self-heal on first-admin registration and at boot reconciliation). NULL-owner rows are visible to nobody until attributed. |

**Validation / lifecycle rules**:
- Every dataset has exactly one owner after attribution; the owner is never changed by this feature (no UI transfer; a removed owning account triggers the reassignment edge below).
- Owner-only: `PATCH /datasets/{id}`, `DELETE /datasets/{id}`, and share management.
- Deleting a dataset removes its share rows (`ON DELETE CASCADE`) and, per existing behavior, is refused while objectives exist (409).
- **Owner-account removal edge**: deleting a user who owns datasets must not orphan them invisible — ownership is reassigned to the caretaker admin (by the same earliest-admin rule), so the data stays reachable to a real caretaker.

### Share (`dataset_shares`) — NEW table

| Field | Type | Notes |
|---|---|---|
| `dataset_id` | uuid NOT NULL REFERENCES datasets(id) **ON DELETE CASCADE** | the granted dataset |
| `user_id` | uuid NOT NULL REFERENCES users(id) **ON DELETE CASCADE** | the recipient account |
| `created_at` | timestamptz NOT NULL DEFAULT now() | grant time |
| `created_by` | uuid NULL REFERENCES users(id) | the acting owner who granted (audit support) |

- `PRIMARY KEY (dataset_id, user_id)` — one grant per dataset/account.
- Row present ⇒ the recipient has working access to the dataset and all children (FR-009); row removed ⇒ access ends immediately (FR-010).
- **Removed account** edge: the CASCADE deletes the grant (FR-012). **Deactivated account** edge: the session guard's `active` re-check stops access at the next request; the grant row may remain (harmless) until the dataset is removed.
- Grants are created/revoked only by the dataset owner (FR-011); never created for the owner themselves.

### Goal (child object, `goal_registry`)

| Field | Type | Notes |
|---|---|---|
| all existing columns | — | `optimization_function_id` PK, `goal_text`, `dataset_id NOT NULL` (parent link), `datasource_ref`, matrix/target fields, track/claim, etc. |
| **`created_by`** | uuid NULL REFERENCES users(id) | **NEW.** The signed-in user who registered the goal; NULL for pre-existing rows (legacy/system). |

- **Visibility**: inherited from `dataset_id` — a goal is reachable iff `accessible(dataset_id, u)`.
- **Child-management (FR-014)**: a goal may be deleted by its `created_by` or by the dataset's owner; a collaborator who did not create a given goal is refused (403). Legacy goals (`created_by` NULL) may be deleted by the dataset owner.
- A goal registered into a shared dataset by a collaborator belongs to the same access set as the dataset; the collaborator's `created_by` is what lets them later delete their own goal.
- **Implicit minting**: an unbound goal submission that mints an implicit dataset (object-store ref without a dataset, `resolveDatasetForRef`) makes the acting user the new dataset's owner, and the goal's `created_by` the same user.

### Child subtree (runs, verifications, outcomes, causal, heuristics/embeddings)

| Surface | Parent key | Access rule |
|---|---|---|
| `runs` | `optimization_function_id` | reached only via an accessible goal |
| `verification_queue` | `optimization_function_id` | via accessible goal |
| `causal_verifications` | `goal_id` | via accessible goal |
| `meta_heuristic_embeddings` (pgvector) | `goal_id` (NULL = legacy corpus) | via accessible goal; legacy NULL-goal rows only for the admin caretaker |
| Neo4j graph nodes (outcomes, causal graph, Meta-Heuristic ↔ triplet chains, heuristic nodes) | `goal_id` property + `datasource_ref` | never queried without a prior goal-access gate in the orchestrator |

No new columns or tables for this subtree: enforcement is purely at the orchestrator boundary over the goal's dataset.

### User (account, `users`)

Unchanged columns. This feature adds two relationships onto `users`: being `datasets.owner_id` and being a `dataset_shares.user_id`. The admin/member role is **not** a visibility factor (Q2: admins follow the same rules).

### Admin caretaker (derived)

Not a stored entity. Defined as the earliest active admin (`ORDER BY created_at, id LIMIT 1`), it is the target of the ownership backfill and self-heal, and the only user who can see legacy NULL-goal rows and ownerless (yet-unattributed) rows.

## State transitions

- **Dataset** created by a person: `owner_id` set at insert; visible to `{owner}`.
- **Dataset** created by a system/unattributed: `owner_id = NULL`; visible to `{}` until the caretaker attribution runs (boot reconcile / first-admin registration), then `{caretaker}`.
- **Share** grant: row inserted ⇒ recipient's access set includes the dataset's whole child tree; revocation deletes the row ⇒ recipient's access set drops it immediately (next data action).
- **Child created** by owner or collaborator: `created_by` stamped; access set unchanged (already inherits the dataset).
- **Owner account deleted**: the account's owned datasets are reassigned to the caretaker admin; its share grants CASCADE away.
- **Recipient account deleted**: the share rows CASCADE away.
- **Recipient deactivated**: no row change; the guard stops the next request.