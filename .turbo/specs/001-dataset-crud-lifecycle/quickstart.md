# Quickstart — Validate Dataset CRUD & Hierarchy

Runnable end-to-end validation for the feature. Implementation bodies live in the plan/tasks,
not here. Requires the live stack (`make up`) per the repo's integration conventions.

## Prerequisites

- `make up` (compose: postgres, minio, neo4j, orchestrator :8080, verifier, web :8083).
- Sourced `.env`; an integration run uses `ARBORETTE_INTEGRATION=1` and localhost overrides
  (notably `S3_ENDPOINT=http://localhost:9000` — without it the S3 client targets real AWS).
- Note: `make test` truncates `meta_heuristic_embeddings` on gate runs; re-publish or
  re-embed before comparing embedding-derived observations across runs.

## Backend validation (Go, against the live stack)

Smoke-verify the new endpoints like the sleep-cycle worker pattern: `go build ./cmd/orchestrator`,
run the binary with `.env` plus localhost overrides and a free `ORCHESTRATOR_PORT`, then curl.

1. **Create a dataset**
   `curl -F name=Sales -F description="retail data" -F file=@data.csv localhost:8080/datasets`
   → `201`, `usage: "empty"`, `objective_count: 0`.
2. **List the inventory**
   `curl localhost:8080/datasets` → the dataset appears with `usage: "empty"`;
   `?q=sale` also returns it.
3. **Register an objective as its child**
   `curl -F goal="maximize avg(revenue)" -F dataset_id=<id> localhost:8080/goals`
   → `201` with `dataset_id`; `GET /datasets/<id>` now lists it (usage `in_use`,
   `objective_count: 1`); `GET /goals` shows the objective under the dataset.
4. **Refuse an orphan**
   A goal without `dataset_id` is still auto-parented (an implicit dataset is created) —
   verify no goal ever reports a missing parent; the invariant holds (SC-002).
5. **Edit (metadata only)**
   `curl -X PATCH localhost:8080/datasets/<id> -H 'Content-Type: application/json' -d '{"name":"SalesQ3","description":"Q3 retail"}'` → `200`; sending `datasource_ref` in the body is rejected; renaming to an existing name → `409`.
6. **Refuse non-empty delete**
   `curl -X DELETE localhost:8080/datasets/<id>` while objectives exist → `409` listing them.
7. **Remove a heuristic child**
   `curl -X DELETE localhost:8080/heuristics/<id>` → `204`; gone from
   `GET /heuristics/search` for its objective; objective's heuristics list reflects it.
8. **Delete objective then dataset**
   `curl -X DELETE localhost:8080/goals/<id>` → `204` (refused → `409` while a run is
   `running`); the dataset is now `empty`; `DELETE /datasets/<id>` → `204`; `GET /datasets`
   no longer returns it.

## Integration tests

- `make test` (sets `ARBORETTE_INTEGRATION=1`, `-p 1`, localhost overrides, truncates
  embeddings). New coverage observes **per-row effects, never table-global counts**
  (AGENTS.md): e.g. assert the created dataset's row, the objective join, the delete result —
  never a sweep's affected-row total.
- Guarded doubles that stand in for the object store / graph must return `ctx.Err()` on a
  cancelled context (AGENTS.md), since the delete and audit paths detach contexts.
- Run `go test -race ./internal/orchestrator` when touching the delete paths / promotion
  goroutines; `go vet ./...` and `go build ./...` must stay green without infra.

## Frontend validation (web/)

- `make web-dev` (dev server on :3000 against :8080), or the standalone :8083 after rebuild.
- **Header**: `Datasets` appears at the top of the header on every page (Submit goal,
  Objectives, Heuristics, dataset detail).
- **List/create**: open `/datasets` → inventory rows (name, created, status, usage,
  objective count); Create fills name/description + upload → row appears.
- **Detail/edit**: open a dataset → its objectives listed (children); rename / describe /
  archive reflected after reload; archive blocks new objective registration on it.
- **Delete**: non-empty → blocked with the offending objectives; empty → confirmed delete
  removes it; every destructive action required a confirmation.
- **Heuristics**: an objective's heuristics list shows exactly its children; removing one
  confirms and it disappears from search.
- **Unit suites**: `npm test` from `web/` (vitest) covers the new typed client functions,
  the dataset list/detail pure logic, and the dataset selector — no infrastructure needed.

## Expected outcomes (mapping to success criteria)

- Full lifecycle in < 5 min (SC-001): steps 1–8 above without docs.
- SC-002/SC-005: enforced in step 4 and 6 (no orphans, no non-empty delete);
  legacy goals reconciled at migration (audit `dataset_reconcile`).
- SC-003: `GET /datasets` with `?q=` returns in well under 2 s at 1,000 datasets.
- SC-004: every destructive action above is gated (409/confirmation) and lands an
  `audit_log` row (`dataset_delete`, `objective_delete`, `heuristic_remove`).