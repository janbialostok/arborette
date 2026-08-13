# Contracts — Orchestrator REST API

The orchestrator (`internal/orchestrator`) is the sole API the web UI talks to. All new
endpoints are JSON except `POST /datasets` (multipart, mirroring `POST /goals`). Every new
route gets a matching BFF proxy route under `web/app/api/orchestrator/...` and a typed
function in `web/lib/orchestrator.ts`.

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
  data_source_ref: string
}

DatasetDetail extends DatasetSummary {
  objectives: ObjectiveSummary[]   // children, ascending by created_at
}

ObjectiveSummary {
  optimization_function_id: string
  goal_text: string
  dataset_id: string               // parent dataset
  status: string                   // run status incl. synthetic "no run"
  created_at: string
}
```

## Endpoints

### `POST /datasets` — create a dataset

Multipart fields: `name` (required), `description` (optional), and exactly one of `file` /
`import_path` (mirrors `POST /goals` ingest). Ingests the source, puts it in the object
store under `datasources/<uuid>/<filename>`, registers the ref, and inserts the `datasets`
row.

- **201** → `DatasetSummary`; the new dataset is empty (`usage: "empty"`).
- **409** → name conflict; **400** → missing name/source; **502** → ingest/infrastructure
  failure with a clear reason.

### `GET /datasets` — list the inventory

Query: `?q=` (name substring, case-insensitive). Returns `DatasetSummary[]` ordered by
`created_at DESC`, each with derived `usage` and `objective_count` (LEFT JOIN over
`goal_registry.dataset_id`).

- **200** → `DatasetSummary[]` (possibly `[]`).

### `GET /datasets/{id}` — dataset detail

- **200** → `DatasetDetail` with its `objectives`.
- **404** → unknown id.

### `PATCH /datasets/{id}` — edit metadata

Body: `{ name?, description?, status? }`. Metadata only — `datasource_ref` is immutable and
not accepted.

- **200** → updated `DatasetSummary`.
- **409** → renamed into a conflicting name; **404** → unknown id.

### `DELETE /datasets/{id}` — delete an empty dataset

- **204** → deleted (also removes the ref if nothing else references it, and the object).
- **409** → still has objectives: body lists them, no data removed.
- **404** → unknown id.

### `GET /goals?dataset_id={id}` — objectives of a dataset

Additive filter on the existing list endpoint; options: nothing else changes.

### `POST /goals` — register an objective (additive change)

Optional field `dataset_id`. When present, the goal binds to that dataset's
`datasource_ref` and no file/import upload is expected. When absent, the legacy ingest runs
and an implicit dataset is created around the new ref, so every goal still ends up with a
parent (FR-007 invariant). The web form always sends a `dataset_id`.

- **201** → existing success DTO (`optimization_function_id`, …), plus `dataset_id`.
- **409** → `dataset_id` unknown or its source unreadable.

### `DELETE /goals/{id}` — delete an objective

- **204** → deleted, together with its runs/verifications/embeddings/graph nodes.
- **409** → refused: a run is `running` or a verification is pending/in-flight.
- **404** → unknown id.

### `DELETE /heuristics/{id}` — remove a heuristic

- **204** → graph node detached, embeddings row removed.
- **404** → unknown id.

## Audit detail keys

All new events key the `detail` map with the codebase vocabulary:
`dataset_id`, `dataset_name`, `optimization_function_id`, `data_source_ref` (see
`data-model.md`). The wire DTOs may spell the goal id `optimization_function_id`; the audit
detail MUST use the same key (AGENTS.md).

## Error shape

Errors are JSON `{ "error": "<reason>" }`; refusal cases (409) include the offending
children/resource names so the UI can render actionable guidance.