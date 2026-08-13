# Data Model — Datasets as the Index Page

Extends the existing stores. Persistence is Postgres (`internal/store`); Neon/Neo4j, the
object store, and pgvector embeddings are **untouched** by this feature.

## Modified entity: Dataset

**`datasets`** gains one column (new migration `0016_datasets_last_accessed`):

| Column | Type | Constraints | Meaning |
|--------|------|-------------|---------|
| `last_accessed_at` | `timestamptz` | NULL, default `NULL` | When the dataset's detail view was last opened; `NULL` means the dataset has never been accessed |

No grant change: the orchestrator role already holds `SELECT`/`UPDATE` on `datasets`
(the metadata edit path). `//go:embed *.sql` auto-includes the two new files.

**Store shape** (`store.Dataset`): `LastAccessedAt *time.Time` — a pointer, because a
`NULL` column cannot scan into a bare `time.Time`. The DTO mirrors it as a nullable
`*time.Time` so "never accessed" stays distinct from an old timestamp.

### Inventory ordering (FR-002/FR-003, R2)

`DatasetStore.List` orders:

```
ORDER BY last_accessed_at DESC NULLS LAST, created_at DESC, id
```

- Most recently accessed dataset first (`DESC`).
- Never-accessed datasets (`NULL`) are strictly below every accessed one (`NULLS LAST`).
- Equal/absent last-access times resolve by `created_at DESC` (newest created first).
- Equal `created_at` resolves by `id` — the final, deterministic tie-break that makes
  the list identical across reloads (SC-002).

### Access event (FR-004, R3)

- `DatasetStore.Touch(ctx, id)` executes `UPDATE datasets SET last_accessed_at = now()
  WHERE id = $1`.
- `GET /datasets/{id}` (the detail view) calls it **best-effort**: a failed stamp is
  logged, never a 500 — opening a dataset must not depend on the write succeeding.
- The inventory list (`GET /datasets`), goal binding (`POST /goals`), and form inventory
  loads never touch; only the detail open is the access event.
- Not audited: access is not an accountability action (no new event types).

### Validation rules (unchanged)

The create / update / delete / uniqueness rules from 001 remain exactly as-is; the column
is write-only-from-the-handler and read-by-the-list.

## Modified entity: Objective (Goal)

No schema change — `goal_registry.dataset_id` already exists (0015).

### Detail objectives ordering (FR-009, R5)

The dataset detail returns its objectives partitioned so **active-run objectives come
first**, then the rest, with **most recently created first within each group**:

1. Objectives whose latest run status is `running` (the only in-flight status;
   `completed`/`failed` are terminal, `no run` is synthesized) — first group.
2. All others — second group.
3. Within each group, `created_at DESC` (the order `ListByDataset` already returns), with
   `optimization_function_id` as the final tie-break.

This is a derived, view-level ordering — a stable in-memory partition inside
`objectivesWithStatus`, not a stored column or a new query. It feeds both the detail view
and the non-empty-delete 409's blocking list.

## Ordered/derived concepts (not stored)

- **Last-access recency** — derived ordering key for the inventory; source of truth is the
  recorded `last_accessed_at` column, updated only by the detail-open stamp.
- **Active-run group** — ordering key for a dataset's objectives; latest run status only,
  never stored on the objective.

## Side effects of this feature

- `GET /datasets/{id}` mutates `datasets.last_accessed_at` (documented in
  contracts/rest-api.md). Idempotent in effect (stamps `now()`); repeated opens just
  move the row to the front of the inventory.
- Registration traffic shifts from the root page to `POST /goals` with a pre-set
  `dataset_id`; the request shape itself is unchanged.