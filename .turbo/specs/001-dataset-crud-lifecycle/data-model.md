# Data Model — Dataset CRUD & Hierarchy Management

Extends the existing stores. Persistence is Postgres (via `internal/store`), the Neo4j
graph (`internal/graph`), the S3/MinIO object store (`internal/objectstore`), and the
pgvector embeddings table (`meta_heuristic_embeddings`).

## New entity: Dataset

**Table `datasets`** (new migration `0015`):

| Column | Type | Constraints | Meaning |
|--------|------|-------------|---------|
| `id` | `uuid` | PK, default `gen_random_uuid()` | Stable identity; the web client keys on it |
| `name` | `text` | NOT NULL | User-visible name, unique case-insensitively |
| `description` | `text` | NOT NULL, default `''` | Free-form annotation |
| `status` | `text` | NOT NULL, default `'active'`, CHECK in (`active`,`archived`) | User-set lifecycle state |
| `datasource_ref` | `text` | NOT NULL | Object-store key of the bound data source (fixed at creation) |
| `created_at` | `timestamptz` | NOT NULL, default `now()` | Armor of record for the inventory list |
| `updated_at` | `timestamptz` | NOT NULL, default `now()` | Touched on every edit |

Derived (never stored): **usage** = `empty` when the dataset has zero objectives, else
`in_use`; **objective_count** = count of goals whose `dataset_id` points at the dataset.

**Uniqueness rule (FR-004)**: name unique under `lower(name)` — "Sales" and "sales"
conflict. Implement as a unique index on `lower(name)`.

### State transitions

```
                create ────────────────► active ──► archived (user edit)
                (with data source)         │ ▲         │
                                           │ │         │
                                           │ └─────────┘ (user edit)
                                           │
                                           └──► deleted, only when objective_count = 0
```

- `archive` / `unarchive` set `status` only.
- Delete is permitted for any `status` as long as the dataset is empty of objectives.

### Validation rules

- Create requires a name and a data source (file or import path); duplicate name → 409.
- Edit (PATCH) touches name/description/status only; the data source is immutable
  (clarification A). Rename into a conflicting name → 409, original preserved.
- A dataset whose ref cannot be read (`IsNotFound`) stays manageable; objective
  registration against it is refused.

## Modified entity: Objective (Goal)

**`goal_registry`** gains one column (migration `0015`):

| Column | Type | Constraints | Meaning |
|--------|------|-------------|---------|
| `dataset_id` | `uuid` | FK → `datasets(id)`, backfilled then `NOT NULL` | The objective's parent dataset |

- Legacy backfill (R7): one dataset per distinct `datasource_ref` among existing goals;
  each goal's `dataset_id` set to its ref's dataset; then `NOT NULL` is applied.
- No change to `datasource_ref`: it remains the per-entity binding and equals the parent
  dataset's `datasource_ref` for dataset-created goals.
- Objective **delete** (new) is refused while either a `runs` row is `running` or
  `verification_queue` has a `pending`/in-flight row for the goal (R10). The delete
  removes, in one store transaction: `runs`, `verification_queue`, `causal_verifications`
  rows, the `goal_registry` row; then app-level cleanup of the graph (R3), the embeddings
  rows (`meta_heuristic_embeddings.goal_id`), and — refcount-aware — the registry/object-store
  cleanup that belongs to the now-deleted dataset's ref. Audit record `objective_delete`
  keyed by `optimization_function_id` and `data_source_ref`.
- **Supplier enumeration**: `runs`, `verification_queue`, `causal_verifications` each have a
  NO ACTION FK to `optimization_function_id` and must be deleted before the goal row.

## Modified entity: Heuristic (MetaHeuristic)

Unchanged schema — the objective parent already exists as the node property `goal_id`
(plus `origin_goal_id` / `origin_datasource_ref` provenance). Two behaviors are added:

- **Remove** (FR-013): `DETACH DELETE` the `MetaHeuristic` node (its `ABSTRACTED_FROM`
  edges included) in the graph + `DELETE FROM meta_heuristic_embeddings WHERE node_id = $1`
  (existing `Delete`, 0008 grant). Audit `heuristic_remove` keyed by node id + goal id.
- **Scoping in reads** (FR-012): heuristic search/trace already filter by `goal_id`
  (`SearchScope`); the UI surfaces this as "children of the objective".

## Audit records (write-only, `audit_log` via `Append`)

New event types with the codebase vocabulary:

| Event | Detail keys |
|-------|-------------|
| `dataset_create` | `dataset_id`, `dataset_name`, `data_source_ref` |
| `dataset_update` | `dataset_id`, `dataset_name`, changed fields |
| `dataset_delete` | `dataset_id`, `dataset_name`, `data_source_ref` |
| `objective_delete` | `optimization_function_id`, `data_source_ref`, `dataset_id` |
| `heuristic_remove` | `heuristic_id`, `optimization_function_id` |
| `dataset_reconcile` | datasets created, goals assigned (counts, from backfill) |

`audit_log` is append-only; no delete or read endpoint is added. Deletion of objectives is
recorded with `optimization_function_id` (never the wire DTO's `goal_id` spelling), per
AGENTS.md.