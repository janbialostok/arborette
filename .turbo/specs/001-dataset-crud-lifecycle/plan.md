# Implementation Plan: Dataset CRUD & Hierarchy Management

**Branch**: `001-dataset-crud-lifecycle` | **Date**: 2026-08-13 | **Spec**: [spec.md](spec.md)

**Input**: Feature specification from spec.md (full dataset lifecycle in the web console, objectives as children of datasets, heuristics as children of objectives, `Datasets` header link).

## Summary

Promote the dataset from a side-effect of goal submission to a first-class, user-managed
resource with the full create/read/update/delete lifecycle in the web UI (modeled on the
AWS S3 bucket console), and make the hierarchy explicit and enforceable: every objective is
a child of exactly one dataset, every heuristic a child of exactly one objective. Concretely:
a new `datasets` Postgres table (+ `goal_registry.dataset_id`) with a `datasets` CRUD API and
web console reachable from a `Datasets` header link; objective registration requires a parent
dataset; objectives and heuristics gain guarded delete flows (with grant, graph, embeddings,
and object-store cleanup); legacy records are reconciled so no orphans remain. See
`research.md` for the decisions, `data-model.md` for entities, `contracts/rest-api.md` for
the API, and `quickstart.md` for validation.

## Technical Context

**Language/Version**: Go 1.24 (pinned `GOTOOLCHAIN`); web frontend is TypeScript on
Next.js ≥15 (App Router), Node 22.

**Primary Dependencies**: `pgx/v5` (must stay ≤ v5.7.x — v5.8+ needs Go 1.25); neo4j-go-driver/v5;
aws-sdk-go-v2 `service/s3` (keep pinned current for `BaseEndpoint`/`UsePathStyle` MinIO
selection); `golang-migrate/migrate/v4`; Next.js ≥15 + `web/` typed client.

**Storage**: Postgres (existing migrations `0001–0014`; this feature adds `0015` — new
`datasets` table, `goal_registry.dataset_id`, DELETE grants). Neo4j graph for objectives'
causal knowledge + `MetaHeuristic` nodes. S3/MinIO object store for the data source bytes
(named `datasources/<uuid>/<filename>`). pgvector `meta_heuristic_embeddings` for heuristics.

**Testing**: Go: `go test ./...` (integration gated on `ARBORETTE_INTEGRATION=1`), `make
test` with `-p 1` + localhost overrides, `go test -race ./internal/orchestrator` for the
delete/promotion paths, `go vet`. Web: `npm test` (vitest) from `web/` for the typed client
and view logic. Never assert table-global counts — per-row effects only (AGENTS.md).

**Target Platform**: Local docker compose stack (orchestrator :8080, web :3000 dev /
:8083 standalone) — an internal analyst-facing web app.

**Project Type**: Web application (Go orchestrator backend + Next.js frontend + supporting
microservices), with a graph data store and an audit trail.

**Performance Goals**: Spec SC-003 — inventory search returns within 2 s up to 1,000
datasets; lists/detail load client-side at worst O(n) for a corpus of a few hundred goals.

**Constraints**: Data source binding immutable after registration (clarification A);
non-empty dataset undeletable (clarification B); every destructive action confirmed and
audited (SC-004); audit detail keys MUST use `optimization_function_id` / `data_source_ref`
vocabulary (AGENTS.md); integration suite shares one Postgres DB (`-p 1`); `make test`
truncates `meta_heuristic_embeddings`; objects live under the `datasources/` prefix only.

**Scale/Scope**: Single analyst persona + operator; roughly a few hundred datasets/goals;
web + orchestrator + store/graph/object-store changes, no changes to the sandbox or verifier
service contracts beyond goals/deletes they read.

## Constitution Check

*GATE: Must pass before Phase 0 research. Re-check after Phase 1 design.*

The project `.specify/memory/constitution.md` is the unfilled repository template (no custom
principles ratified). The governing conventions come from `AGENTS.md`; all gates pass:

- **Toolchain/dependency discipline** — Go 1.24 pin kept; `pgx/v5` stays ≤ v5.7.x;
  `service/s3` stays current; no speculative new dependencies (object-store delete reuses the
  pinned SDK; graph delete reuses the existing driver).
- **Migrate-then-grant discipline** — new migration `0015` carries its grants; DELETE grants
  are added only where a real delete exists; `audit_log` stays append-only INSERT.
- **Audit vocabulary** — `optimization_function_id`, `data_source_ref`, `dataset_id` keying.
- **Testability** — integration suite remains infrastructuraless-green; per-row assertions;
  guarded doubles return `ctx.Err()`; `-race` on goroutine-touching paths.
- **No unwarranted complexity** — no new service, no schema-versioning, no soft-delete
  tombstone model added.

## Project Structure

### Documentation (this feature)

```text
specs/001-dataset-crud-lifecycle/
├── plan.md              # this file
├── research.md          # Phase 0 — decisions, rationale, alternatives
├── data-model.md        # Phase 1 — datasets/goal/h heuristic state + transitions
├── quickstart.md        # Phase 1 — end-to-end validation guide
├── contracts/
│   └── rest-api.md      # Phase 1 — orchestrator REST + web BFF contracts
└── tasks.md             # created by /speckit.tasks (next phase)
```

### Source Code (repository root)

```text
internal/store/
├── migrations/
│   └── 0015_datasets.up.sql / .down.sql   # datasets table, goal dataset_id + backfill,
│                                          # DELETE grants (goal_registry, runs,
│                                          # verification_queue, causal_verifications,
│                                          # data_source_registry, datasets)
├── goals.go (dataset store)               # DatasetStore: Create/Get/ListByName/Update/
│                                          # CountObjectives/Delete (tx with child rows)
├── goalregistry.go                        # + SetDatasetID (backfill), Delete (tx),
│                                          # ListByDataset
├── runs.go / verificationqueue.go /       # + DeleteByGoal (tx members)
│   causalverifications.go
├── embeddingstore.go                      # + DeleteByGoal (new)
└── objectstore/client.go                  # + Delete(ctx, key)

internal/graph/
├── metaheuristics.go or neo4j.go          # + DeleteMetaHeuristic(id) DETACH DELETE
└── causal.go                              # + DeleteGoalGraph(goalID) DETACH DELETE
                                          #   (State/Intervention/Outcome/MetaHeuristic/
                                          #   CausalGraphMeta/DataColumn by goal_id)

internal/orchestrator/
├── server.go                              # goalStore/graphRepo/objectStore/auditStore/
                                          # datasetStore interfaces + Routes() additions
├── datasets.go                            # handleCreate/List/Get/Update/DeleteDataset
├── submit.go                              # POST /goals accepts dataset_id (parent binding)
├── goals.go (delete)                      # handleDeleteGoal (+ running/pending guard, audit)
├── heuristics.go                          # handleDeleteHeuristic (+ audit)
└── audit.go / recordAudit                 # new events: dataset_*, objective_delete, ...

web/
├── components/AppNav.tsx                  # + LINKS entry { href: "/datasets", "Datasets" }
├── components/DatasetsList.tsx            # inventory rows + create (hand-rolled, per pattern)
├── components/DatasetDetail.tsx           # edit metadata, delete, objectives + heuristics
├── components/GoalForm.tsx                # dataset selector (existing / create new)
├── lib/orchestrator.ts                    # + listDatasets/createDataset/getDataset/
                                          #   updateDataset/deleteDataset; listGoals(dataset_id)
├── lib/ might add datasets.ts (pure logic) # for vitest coverage
└── app/api/orchestrator/
    ├── datasets/route.ts                  # GET + POST
    ├── datasets/[id]/route.ts             # GET + PATCH + DELETE
    ├── goals/[id]/route.ts                # DELETE
    ├── heuristics/[id]/route.ts           # DELETE
    └── goals/route.ts                     # POST passes through dataset_id
```

**Structure Decision**: The repo already has a two-deployable layout — the Go module
(`internal/`, `cmd/`) and the standalone `web/` Next.js deployable — plus a Store / Graph /
Object-store / Audit data layer. The feature extends that existing layout in place: new
store methods + one migration under `internal/store`, new graph delete methods under
`internal/graph`, new orchestrator routes under `internal/orchestrator`, and the matching BFF
routes + components under `web/`. A new cluster-unified dataset page/wiring of store and API
is the only new grouping; the object store, graph, audit, and web follow the patterns already
in the codebase (no re-architecture). No shell/project split is warranted.

## Complexity Tracking

> No `AGENTS.md` / constitution violations justified — no added projects, no repository
> pattern, no schema versioning. The new table and delete methods are additive within the
> existing layered design; the table is intentionally empty (none needed).