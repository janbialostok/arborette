# Contracts — Orchestrator REST API

The orchestrator (`internal/orchestrator`) is the sole API the web UI talks to. This
feature changes two existing endpoints' output shape and one endpoint's ordering, and
**adds no endpoints**. Every route keeps its BFF proxy under `web/app/api/orchestrator/...`
(those proxies are pass-through and do not change).

## Data transfer types

```
DatasetSummary {
  id: string            // uuid
  name: string
  description: string
  status: "active" | "archived"
  usage: "empty" | "in_use"        // derived from objective count
  objective_count: number
  created_at: string               // RFC3339
  updated_at: string
  last_accessed_at: string | null  // NEW: detail-open stamp; null = never accessed
  data_source_ref: string
}

DatasetDetail extends DatasetSummary {
  objectives: ObjectiveSummary[]   // running first, then newest-created first per group
}

ObjectiveSummary {
  optimization_function_id: string
  goal_text: string
  dataset_id: string               // parent dataset
  status: string                   // latest run status incl. synthetic "no run"
  created_at: string
}
```

`last_accessed_at` is added to the summary union: both `GET /datasets` rows and
`GET /datasets/{id}` details carry it.

## Endpoints

### `GET /datasets` — list the inventory

Query: `?q=` (name substring, case-insensitive; unchanged). **Ordering changes** to:

```
ORDER BY last_accessed_at DESC NULLS LAST, created_at DESC, id
```

- Most recently accessed dataset first; never-accessed datasets (`NULL`) below all
  accessed ones; equal/absent recency resolves by newest-created-first; `id` is the
  final deterministic tie-break.

- **200** → `DatasetSummary[]` (possibly `[]`).

### `GET /datasets/{id}` — dataset detail

**Documented side effect**: this read stamps `last_accessed_at = now()` for the requested
dataset (the "access event"). The stamp is best-effort — a failed write is logged and the
detail is still served with a 200; the response never 500s because the stamp failed.

- **200** → `DatasetDetail`. `objectives` are ordered **active (running) run first, then
  the rest, newest-created first within each group** (`optimization_function_id` tie-break).
- **404** → unknown id.

### `PATCH /datasets/{id}` / `DELETE /datasets/{id}` — unchanged

Metadata edit (no `datasource_ref` accepted) and empty-only delete (409 with the blocking
objectives) are as documented in 001's contract. The 409 objectives list uses the same
active-first ordering for consistency.

### `POST /goals` — register an objective (unchanged shape, new call site)

- **Request**: multipart with `goal` + exactly `dataset_id` (bound — sent by the dataset
  detail's registration entry), or the legacy file/import-path ingest. No new fields.
- **201** → existing success DTO; the goal is bound to `dataset_id`.
- **409** → `dataset_id` unknown; **409** → `dataset is archived and cannot accept new
  objectives` (the gate the detail view avoids showing by hiding the form on archived
  datasets).

## Error shape

Unchanged: JSON `{ "error": "<reason>" }`; refusal cases (409) include the offending
children/resource names.

## Audit detail keys

No new events. Access is not an accountability action; the existing
`dataset_create`/`dataset_update`/`dataset_delete` events keep their established keys
(`dataset_id`, `dataset_name`, `data_source_ref`, `optimization_function_id`).