# Quickstart — Validate Datasets as the Index Page

Runnable end-to-end validation for the feature. Implementation bodies live in plan/tasks,
not here. Requires the live stack (`make up`) per the repo's integration conventions.

## Prerequisites

- `make up` (compose: postgres, minio, neo4j, orchestrator :8080, verifier, web :8083).
- Sourced `.env`; an integration run uses `ARBORETTE_INTEGRATION=1` and localhost
  overrides (notably `S3_ENDPOINT=http://localhost:9000`).
- The shared integration DB persists rows between runs — including `last_accessed_at`
  stamps from earlier runs. Assert the ordering on rows **you** created/touched, never a
  global first-row claim (AGENTS.md). For `ORDER BY` determinism, `time.Sleep` a few ms
  between creates.

## Backend validation (Go, against the live stack)

Smoke-verify like the sleep-cycle-worker pattern: `go build ./cmd/orchestrator`, run the
binary with `.env` + localhost overrides and a free `ORCHESTRATOR_PORT`, then curl.

1. **Last-access tie (equal/absent) falls back to newest-created-first**
   Create two datasets `A` then `B` without opening either:
   `curl -F name=A -F import_path=contracts/a.csv localhost:8080/datasets`
   (`import_path` avoids real uploads if you prefer). `GET /datasets` lists `B` before
   `A` (both `last_accessed_at: null`).
2. **Opening a dataset is the access event**
   `curl localhost:8080/datasets/<A-id>` → `200` with `last_accessed_at` set (not null).
   `GET /datasets` → now `A` is first: the just-opened dataset jumped over `B` (FR-002,
   SC-002). Repeating the open just refreshes the stamp.
3. **Never-accessed stays below accessed**
   Open only `A`; `GET /datasets` → `A` (accessed) above `B` (null), regardless of B's
   created_at.
4. **Deterministic on reload** — two consecutive `GET /datasets` return identical order.
5. **Registration from the detail binds the goal**
   `curl -F goal="grow revenue" -F dataset_id=<A-id> localhost:8080/goals`
   → `201` with `dataset_id`; `GET /datasets/<A-id>` lists it. Repeat with an **archived**
   dataset → `409 dataset is archived and cannot accept new objectives`.
6. **Active-first objective ordering**
   Register `G1`, then `G2` (newest); run G1 (Phase-1 run leaves it `running` / or seed a
   `running` run row). `GET /datasets/<A-id>` → `G1` (running) above `G2`; within a group
   newest created comes first.

## Integration tests

- `make test` (sets `ARBORETTE_INTEGRATION=1`, `-p 1`, localhost overrides). New coverage
  asserts **per-row effects**: `GET /datasets/<created-id>` sets that row's
  `last_accessed_at`; `List` places the just-touched row before a never-touched one; the
  objectives partition is asserted on a fixture dataset's own rows, never a global order.
- The detail GET's stamp is best-effort: a test drives `fakeDatasetStore.Touch` to return
  `ctx.Err()` on a done context (AGENTS.md) and asserts the detail still 200s.
- `go test -race ./internal/orchestrator` when the detail/registration handlers and their
  fakes are touched; `go vet ./...` and `go build ./...` stay green without infra.

## Frontend validation (web/)

- `make web-dev` (dev server on :3000 against :8080), or the standalone :8083 after rebuild.
- **Index page** — opening the root URL renders the dataset inventory (list, search,
  "New dataset"), not the submit-goal hero. The header shows **Objectives · Datasets ·
  Heuristics** and no **Submit goal** link; the brand mark returns to the (now inventory)
  root.
- **Ordering** — after opening a dataset's detail and returning to the list, that dataset
  sits at the top; reload keeps the order (SC-002/SC-003/SC-006).
- **Registration on the detail** — an active dataset's detail offers the registration
  entry: entering an objective submits it bound to that dataset, lands on the goal's live
  view, and the dataset's objectives list gains it. An archived dataset's detail shows the
  reason instead of the form.
- **Objectives order** — a dataset whose objective has a running run lists it above its
  settled objectives; within a group newest created is first, stable across reloads.
- **No dead links** — Objectives' empty-state prompt points to the inventory (open a
  dataset to register), never to a removed landing page; `router.push("/datasets")`
  back-links and post-delete redirects resolve as before.
- **Unit suites**: `npm test` from `web/` (vitest) — fixtures gain `last_accessed_at`;
  the typed client compiles against the new nullable field. No infrastructure needed.

## Expected outcomes (mapping to success criteria)

- SC-001: root URL inventory loads in well under 2 s at 1,000 datasets.
- SC-002/SC-003: ordering above is identical across reloads and no dataset is dropped.
- SC-004: the removed landing page has no inbound links; every header view still reaches
  the inventory, objectives, datasets, and heuristics.
- SC-005: registration from the detail works and lands on the live view; archived refusal
  is respected.
- SC-006: a dataset's objectives render in the defined order on every load.