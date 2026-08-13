# Tasks: Dataset CRUD & Hierarchy Management

**Input**: Design documents from `specs/001-dataset-crud-lifecycle/`

**Prerequisites**: plan.md (required), spec.md (required for user stories), research.md, data-model.md, contracts/rest-api.md, quickstart.md

**Tests**: Integration tests are included per repo convention (AGENTS.md — the declared gate is `make test` with `ARBORETTE_INTEGRATION=1` and `-p 1`; per-row assertions, never table-global counts; guarded doubles return `ctx.Err()`). Write/run them within each story phase. Web unit suites live in `web/lib/` (vitest, `npm test`).

**Organization**: Tasks are grouped by user story to enable independent implementation and testing of each story.

**Repo paths** (single repo, two deployables): Go module at root (`internal/`, `cmd/`, `internal/store/migrations/`); web deployable at `web/`.

## Format: `[ID] [P?] [Story] Description`

- **[P]**: Can run in parallel (different files, no dependencies)
- **[Story]**: Which user story this task belongs to (e.g., US1, US2, US3)
- Include exact file paths in descriptions

---

## Phase 1: Setup (Shared Infrastructure)

**Purpose**: Establish a clean baseline before any change.

- [X] T001 Establish green baseline: `go build ./...`, `go vet ./...`, `go test ./...` (integration skipped), and `npm test` from `web/` all pass before changes Done: verified at start and again after the feature (web: 173 tests / 11 files)

---

## Phase 2: Foundational (Blocking Prerequisites)

**Purpose**: The dataset table, store, and shared seams that EVERY user story builds on.

**⚠️ CRITICAL**: No user story work can begin until this phase is complete.

- [X] T002 Create migration `internal/store/migrations/0015_datasets.up.sql` (and `.down.sql`): `datasets` table (`id uuid pk default gen_random_uuid()`, `name text not null`, `description text not null default ''`, `status text not null default 'active' check in ('active','archived')`, `datasource_ref text not null`, `created_at`, `updated_at`), unique index on `lower(name)`, `goal_registry.dataset_id uuid` column with FK to `datasets(id)`, backfill one dataset per distinct legacy `datasource_ref` and assign `dataset_id` (named from the ref base filename), then `SET NOT NULL`; grants: orchestrator `SELECT, INSERT, UPDATE, DELETE` on `datasets` and `DELETE` on `goal_registry`, `runs`, `verification_queue`, `causal_verifications`, `data_source_registry` Done: migration applied by the compose migrate init step (`make up`); schema validated by the live integration suite
- [X] T003 [P] Implement `DatasetStore` in `internal/store/datasets.go`: `Create`, `Get`, `List` (with objective counts via LEFT JOIN + `?q` name filter), `Update` (name/description/status, `lower(name)` uniqueness check), `CountObjectives`, `Delete` (refcount-aware: delete the `data_source_registry` row only when no dataset or goal references the ref)
- [X] T004 [P] Add `Delete(ctx, key)` to `internal/objectstore/client.go` (wrap s3 `DeleteObject`) and extend the orchestrator `objectStore` interface in `internal/orchestrator/server.go` plus the test fakes (fakes must return `ctx.Err()` on a cancelled context)
- [X] T005 [P] Extend orchestrator consumer interfaces in `internal/orchestrator/server.go`: add delete methods to `goalStore`/`graphRepo`, `datasetStore`/`embeddingsStore` interfaces, new audit event names; wire `datasetStore` into `Routes()` (`GET/POST /datasets`, `GET/PATCH/DELETE /datasets/{id}`, `DELETE /goals/{id}`, `DELETE /heuristics/{id}`)

**Checkpoint**: Foundation ready — `datasets` table live, store and interfaces in place, baseline still green.

---

## Phase 3: User Story 1 - Browse the dataset inventory (Priority: P1) 🎯 MVP

**Goal**: A `Datasets` header link on every page opens the inventory console: every dataset with name, created date, status, derived usage, objective count; searchable; detail view shows metadata plus its objectives.

**Independent Test**: Click **Datasets** in the header from any page → the console lists every dataset (name, created, status, count) and the search filters rows; opening a dataset shows its metadata and objectives each linking to the objective view.

### Implementation for User Story 1

- [X] T006 [P] [US1] Add `{ href: "/datasets", label: "Datasets" }` to `LINKS` in `web/components/AppNav.tsx` Implemented: Datasets link between Objectives and Heuristics
- [X] T007 [P] [US1] Implement `GET /datasets` handler in `internal/orchestrator/datasets.go`: `DatasetSummary[]` ordered by `created_at DESC`, derived `usage` (`empty`/`in_use`), `objective_count` via join, `?q=` substring filter Implemented: list handler returns DatasetSummary[] with derived usage/objective_count and ?q filter
- [X] T008 [P] [US1] Implement `GET /datasets/{id}` handler in `internal/orchestrator/datasets.go`: `DatasetDetail` (summary + `objectives[]` linked by `dataset_id`), `404` on unknown id Implemented: detail handler returns summary + objectives[]
- [X] T009 [US1] Add `DatasetSummary`/`DatasetDetail`/`ObjectiveSummary` types and `listDatasets`/`getDataset` to `web/lib/orchestrator.ts` (depends on T007, T008) Implemented: plus listGoals(datasetId?), createDataset, updateDataset, deleteDataset, deleteGoal, deleteHeuristic
- [X] T010 [P] [US1] Add BFF GET routes `web/app/api/orchestrator/datasets/route.ts` and `web/app/api/orchestrator/datasets/[id]/route.ts` (both `export const runtime = "nodejs"` + `dynamic = "force-dynamic"`) Implemented: datasets/route.ts (GET/POST), datasets/[id]/route.ts (GET/PATCH/DELETE)
- [X] T011 [P] [US1] Build `web/app/datasets/page.tsx` + `web/components/DatasetsList.tsx` (inventory rows, search box, link to detail; follow the `ObjectivesList.tsx` row pattern) Implemented: inventory + client-side ?q filter + DatasetCreateCard
- [X] T012 [US1] Build `web/app/datasets/[id]/page.tsx` (async page, `params: Promise<{id}>`) + `web/components/DatasetDetail.tsx` (metadata panel, objectives list linking to `/goals/[id]`) Implemented: DetailPage with MetadataPanel, ObjectivesPanel, DangerZone
- [X] T013 [US1] Add pure list/detail logic (usage derivation, `?q` filtering) to `web/lib/datasets.ts` with vitest coverage in `web/lib/datasets.test.ts` Implemented: isInUse + filterDatasets with 5 vitest cases (suites pass 173/11)
- [X] T014 [US1] Add integration tests for `GET /datasets` and `GET /datasets/{id}` (per-row assertions, gated on `ARBORETTE_INTEGRATION=1`) Implemented: TestDatasetInventory covers list/list-filter/detail + objective counts

**Checkpoint**: US1 fully functional — header link, inventory, search, detail; testable independently.

---

## Phase 4: User Story 2 - Create a dataset (Priority: P1)

**Goal**: Create a named dataset from a data source (upload or import path) and see it appear in the inventory as `empty`.

**Independent Test**: Create a dataset with a unique name + description + data source → it immediately appears in the inventory with `usage: "empty"`; a duplicate name is rejected with a conflict message.

### Implementation for User Story 2

- [X] T015 [US2] Implement `POST /datasets` multipart handler in `internal/orchestrator/datasets.go`: ingest via the `submit.go` ingest path (file or `import_path`), `Put` to object store under `datasources/<uuid>/<filename>`, `RegisterDataSourceRef`, insert `datasets` row; `409` name conflict, `400` missing name/source, clear `502` on ingest failure (depends on T003, T004) Implemented as multipart create mirroring POST /goals ingest (file/import_path, object-store Put under datasources/<uuid>/<filename>, RegisterDataSourceRef, datasets insert, 409/400/502 mapping)
- [X] T016 [P] [US2] Add `createDataset(form)` to `web/lib/orchestrator.ts` and the POST branch of `web/app/api/orchestrator/datasets/route.ts` (multipart via `forwardSubmit`) Implemented: createDataset multipart + forwardSubmit POST
- [X] T017 [US2] Add create-dataset form to `web/components/DatasetsList.tsx` (name, description, exactly-one of file/import-path — mirror `GoalForm.tsx` validation at lines 28–47) Implemented: DatasetCreateCard (file|import_path toggle, name, description)
- [X] T018 [US2] Emit `dataset_create` audit in `internal/orchestrator/datasets.go` via `recordAudit` keyed `dataset_id`, `dataset_name`, `data_source_ref` Implemented in createDataset handler
- [X] T019 [US2] Add integration test for `POST /datasets` (201 empty, 409 conflict, 400 missing source, unreadable source → clear failure, dataset entry not created) Implemented: handler tests (201/400/409/unreadable-502) + TestDatasetLifecycle 409 conflict

**Checkpoint**: US2 complete — creation flow ends with the dataset visible in the inventory.

---

## Phase 5: User Story 3 - Register an objective under a dataset (Priority: P1)

**Goal**: Every objective is a child of exactly one dataset. `POST /goals` accepts `dataset_id` (binds to the dataset's source, no re-upload); API clients omitting it get an implicit dataset so no orphan ever exists; the web form always picks or creates a parent.

**Independent Test**: Registering an objective without a dataset is impossible through the UI; with one it succeeds and appears under that dataset. Legacy goals reconcile at migration (audit `dataset_reconcile`).

### Implementation for User Story 3

- [X] T020 [US3] Add `SetDatasetID` + `ListByDataset` in `internal/store/goalregistry.go` and `internal/store/datasets.go` (parent-link store support, used by the join) Implemented: GoalRegistry.SetDatasetID / ListByDataset + DatasetStore.GetByRef
- [X] T021 [US3] Update `POST /goals` in `internal/orchestrator/submit.go` to accept optional `dataset_id`: bind the goal to that dataset's `datasource_ref` (no upload expected); when absent, run the legacy ingest and create an implicit dataset around the new ref; `409` on unknown/archived/unreadable dataset (depends on T020) Implemented: bound vs legacy-implicit branches; 409 dataset not found/archived/unreadable; handler tests in submitdataset_test.go
- [X] T022 [P] [US3] Add `dataset_id` filter to `GET /goals` in `internal/orchestrator/submit.go` and `listGoals(datasetId)` in `web/lib/orchestrator.ts` Implemented + handler test (dataset-scoped vs full list)
- [X] T023 [US3] Add dataset selector to `web/components/GoalForm.tsx` (choose existing dataset, or "create new dataset" inline: name + upload; form always sends `dataset_id`) Implemented: active-dataset `<select>`; bound goals send only dataset_id, unbound keep the file/path tabs
- [X] T024 [US3] Emit `dataset_reconcile` audit from the orchestrator boot path (datasets created / goals assigned by the migration backfill) so legacy reconciliation is reported for review Implemented: ReconcileDatasets at boot (orphan re-parent + implicit dataset mint, audit `dataset_reconcile`) + handler test
- [X] T025 [US3] Add integration tests for `POST /goals` with `dataset_id` (parent enforced, orphan refused, implicit-dataset fallback for API clients, goal appears under dataset detail) Implemented: TestGoalDatasetBinding (bound insert, ListByDataset scope, re-parent by SetDatasetID, unknown-goal error, running + pending delete guards) + handler bound/implicit tests

**Checkpoint**: US3 complete — hierarchy invariant holds for all new objectives; objectives visible under their dataset.

---

## Phase 6: User Story 4 - Edit a dataset (Priority: P2)

**Goal**: Rename, update description, or archive/unarchive a dataset from its details view. Metadata only — the data source is immutable. Archived datasets refuse new objective registration.

**Independent Test**: Rename (uniqueness-checked), edit description, archive → each change is persisted and reflected after reload; archiving blocks new objective registration; renaming onto an existing name is rejected.

### Implementation for User Story 4

- [X] T026 [US4] Implement `PATCH /datasets/{id}` in `internal/orchestrator/datasets.go`: accepts `name`/`description`/`status` only, rejects `datasource_ref`, touches `updated_at`, `409` on `lower(name)` conflict, `404` unknown (depends on T003)
- [X] T027 [P] [US4] Add `updateDataset` to `web/lib/orchestrator.ts` and the PATCH branch of `web/app/api/orchestrator/datasets/[id]/route.ts` Implemented: JSON PATCH (name/description/status), returns DatasetSummary
- [X] T028 [US4] Add edit UI (rename / describe / archive-unarchive) to `web/components/DatasetDetail.tsx`; add the archived-guard to `POST /goals` (submit against an archived dataset → `409` with a clear message) Implemented: MetadataPanel edit + status toggle; archived bound-goal 409 in submitdataset_test.go
- [X] T029 [US4] Emit `dataset_update` audit (keyed `dataset_id`, `dataset_name`, changed fields) and add integration tests for `PATCH` (200 persist, 409 conflict, `datasource_ref` rejected, archive blocks registration) Implemented: dataset_update audit; handler PATCH tests; TestDatasetLifecycle covers persist/conflict, archived block covered by handler test

**Checkpoint**: US4 complete — datasets stay accurately labeled; edits are safe and recorded.

---

## Phase 7: User Story 5 - Browse and remove heuristics under an objective (Priority: P2)

**Goal**: An objective's heuristics list shows exactly its children; an analyst can remove one (confirmed), after which it no longer appears in search or trace.

**Independent Test**: Open an objective → see exactly its heuristics (not another objective's); remove one with confirmation → gone from the list and from `GET /heuristics/search` / trace; an empty objective shows a "none yet" state.

### Implementation for User Story 5

- [X] T030 [P] [US5] Implement `DeleteMetaHeuristic(id)` in `internal/graph/neo4j.go` (`MATCH (m:MetaHeuristic {id:$id}) DETACH DELETE m` via the write harness) and `DeleteByGoal(goalID)` in `internal/store/embeddingstore.go` Implemented: DeleteMetaHeuristic in neo4j.go + DeleteByGoal in embeddingstore.go
- [X] T031 [US5] Implement `DELETE /heuristics/{id}` in `internal/orchestrator/heuristics.go`: graph delete + embeddings `Delete(nodeID)`, `404` unknown, `heuristic_remove` audit keyed `heuristic_id` + `optimization_function_id` (depends on T030) Implemented: DELETE /heuristics/{id} handler + handler tests
- [X] T032 [P] [US5] Add `deleteHeuristic` to `web/lib/orchestrator.ts` and BFF `web/app/api/orchestrator/heuristics/[id]/route.ts` (DELETE) Implemented: DELETE branch + typed deleteHeuristic
- [X] T033 [US5] Add remove-heuristic affordance to the objective detail / `web/components/HeuristicBrowser.tsx` (confirmation dialog; row disappears; search/trace reflect removal) Implemented: TracePanel Remove button with confirm; removed match dropped from results/selection; empty state intact
- [X] T034 [US5] Add integration test for `DELETE /heuristics/{id}` (scoped children shown, removal reflected in `search`/`trace`, per-row assertions) Done: TestDeleteMetaHeuristicLive (internal/graph/delete_integration_test.go) seeds a heuristic + a derived one, deletes it, and asserts the node/trace/batch-hydration/list all drop it, the derived node survives with its ABSTRACTED_FROM edge gone, and absent-id delete is idempotent — green against the live Neo4j

**Checkpoint**: US5 complete — heuristics are managed children of their objective.

---

## Phase 8: User Story 6 - Delete an objective and retire a dataset (Priority: P3)

**Goal**: Delete an objective (with its heuristics/runs/verifications/graph knowledge) after confirmation; delete a dataset once empty. Deletion is blocked — with the offending children named — while a run is `running`, a verification is pending, or a dataset still has objectives.

**Independent Test**: Non-empty dataset delete → `409` listing objectives, nothing lost; objective delete (confirmed) removes it and its heuristics and updates the dataset's count; empty dataset delete succeeds and removes it from the inventory; audit rows record every action.

### Implementation for User Story 6

- [X] T035 [US6] Add transactional `Delete` in `internal/store/goalregistry.go` (delete `runs`, `verification_queue`, `causal_verifications` rows then the goal row; guard: refuse while a `runs` row is `running` or a verification is pending/in-flight) + `DeleteByGoal` in `internal/store/runs.go`, `verificationqueue.go`, `causalverifications.go` (depends on T003) DEPARTURE: done as one transactional GoalRegistry.Delete (guard + child deletes + goal row). The separate runs/verificationqueue/causalverifications DeleteByGoal methods were folded into the tx guard/delete, because guard+delete must be atomic; no per-store DeleteByGoal methods were added.
- [X] T036 [P] [US6] Implement `DeleteGoalGraph(goalID)` in `internal/graph/causal.go`: `DETACH DELETE` `State`/`Intervention`/`Outcome`/`MetaHeuristic`/`CausalGraphMeta`/`DataColumn` matched by `goal_id` or `origin_goal_id` (via the write harness) Implemented: DeleteGoalGraph matching goal_id or origin_goal_id via the write harness
- [X] T037 [US6] Implement `DELETE /goals/{id}` in `internal/orchestrator/goals.go`: running/pending guard → `409`, then store tx + `DeleteGoalGraph` + `DeleteByGoal` embeddings, `objective_delete` audit keyed `optimization_function_id`/`data_source_ref`/`dataset_id` (depends on T035, T036) Implemented: DELETE /goals/{id} handler + handler tests
- [X] T038 [US6] Implement `DELETE /datasets/{id}` in `internal/orchestrator/datasets.go`: `409` non-empty (list objectives), else delete row + refcount-aware `data_source_registry`/object-store cleanup (`Delete` from T004), `dataset_delete` audit (depends on T037) Implemented: DELETE /datasets/{id} handler with non-empty 409 (objectives list), refcount-aware ref/object retirement, dataset_delete audit + handler tests
- [X] T039 [P] [US6] Add `deleteGoal`/`deleteDataset` to `web/lib/orchestrator.ts` and BFF `web/app/api/orchestrator/goals/[id]/route.ts` + `datasets/[id]/route.ts` (DELETE) Implemented: DELETE branches on both routes + typed deleteGoal/deleteDataset
- [X] T040 [US6] Add delete UI with confirmation to `web/components/DatasetDetail.tsx` and the objective detail view; blocked non-empty path renders the offending objectives; destructive actions require confirmation (SC-004) Implemented: DatasetDetail DangerZone (confirm-before-delete token; in-use gating surfaces offending objectives) + GoalTabs objective Delete (confirm + 409 reason)
- [X] T041 [US6] Add integration tests for the delete flows + guards (409 running, 409 non-empty with list, invariant holds afterward — no orphaned children, per-row assertions) Implemented: TestGoalDatasetBinding (running + pending refusal, then success, ListByDataset empty), TestDatasetLifecycle (goal-bound FK refusal then success), TestDeleteGoalRetiresEmbeddings (goal + embeddings retire, dataset freed, CountObjectives 0)

**Checkpoint**: US6 complete — the full lifecycle closes; the hierarchy invariant survives every delete.

---

## Phase 9: Polish & Cross-Cutting Concerns

**Purpose**: Full-suite validation, race coverage, and artifact hygiene.

- [X] T042 [P] Run `go test -race ./internal/orchestrator` covering the delete flows and the auto-promotion goroutine paths (AGENTS.md race requirement) Done: green (1.9s) after the submit-dataset and reconcile handler tests landed
- [X] T043 Run the full `quickstart.md` validation end-to-end against the live stack (`make up`, scoped `go test`, `make web-dev`, curl steps 1–8) and fix any drift Done: stack up + migrate applied, full integration gate green (`ARBORETTE_INTEGRATION=1 go test -p 1 ./...`, 20/20), curl steps 1–8 smoke-verified (create/list/q/bind/legacy-implicit-parent/edit+409+immutable-ref-reject/non-empty-409/delete). DRIFT FIXED: PATCH /datasets/{id} silently ignored a datasource_ref in the body — the handler now rejects it with 400 "datasource_ref is immutable and cannot be changed" (+ handler test)
- [X] T044 [P] Re-check audit-detail vocabulary (`optimization_function_id`, `data_source_ref`, `dataset_id`) across every new event; verify web build passes (`npm run build` from `web/`) Done: reconcile/create/update/delete audits keyed with the vocabulary; `npm run build` green with all new routes
- [X] T045 [P] Update spec/plan artifacts (`spec.md` Clarifications, `plan.md`, `data-model.md`) to match any implementation departures; add backlog entries to `.turbo/improvements.md` only for deliberately skipped work Done: departures recorded in spec.md Clarifications (implicit-dataset mint, boot reconcile, synthesized status); T034's skipped integration test logged in improvements.md; data-model.md already matched

---

## Dependencies & Execution Order

### Phase Dependencies

- **Setup (Phase 1)**: No dependencies — can start immediately.
- **Foundational (Phase 2)**: Depends on Setup; BLOCKS all user stories.
- **User Stories (Phase 3+)**: All depend on Foundational completion.
  - Sequential in priority order (P1 → P2 → P3), or in parallel once Foundational is done.
- **Polish (Final Phase)**: Depends on the user stories shipped.

### User Story Dependencies

- **US1 (P1)**: After Foundational. Independent.
- **US2 (P1)**: After Foundational. Independent of US1 (own handler, form, BFF POST).
- **US3 (P1)**: After Foundational. Uses the `datasets` table + detail join from US1 but is independently testable via `POST /goals`.
- **US4 (P2)**: After Foundational + US2 (shares `datasets.go` handler file — schedule after US2 to avoid same-file conflicts).
- **US5 (P2)**: After Foundational. Independent of US1–US4 (graph + embeddings + `heuristics` route).
- **US6 (P3)**: After Foundational + US4 (shares `datasets.go`), uses graph/embedding seams from US5. Guarded delete is the capstone.

### Within Each User Story

- Store/interface additions before handlers
- Handlers before typed client / BFF / web
- Implementation before its integration tests
- Story complete before moving to the next priority

### Parallel Opportunities

- T002, T003, T004, T005 (Foundational) touch distinct files — T003/T004/T005 can run together once T002's schema intent is fixed; T005's interface additions depend on T003.
- US1: T006, T007, T008, T010, T011 parallel (distinct files).
- US2: T015 (backend) vs T016/T017 (web) parallel; T018/T019 after T015.
- US3: T020, T022, T023, T024 parallel after T021's contract is set; T025 last.
- US4: T026 vs T027/T028 parallel; T029 after T026.
- US5: T030, T032 parallel; T031 after T030; T033/T034 after T031.
- US6: T035, T036, T039 parallel; T037 after T035+T036; T038 after T037; T040/T041 after T037.

---

## Parallel Example: User Story 1

```bash
# Backend endpoints (distinct files):
Task: "Implement GET /datasets list handler in internal/orchestrator/datasets.go"
Task: "Implement GET /datasets/{id} detail handler in internal/orchestrator/datasets.go"

# Web (distinct files):
Task: "Add Datasets link to web/components/AppNav.tsx"
Task: "Add BFF GET routes web/app/api/orchestrator/datasets/route.ts and datasets/[id]/route.ts"
Task: "Build web/app/datasets/page.tsx + web/components/DatasetsList.tsx"

# After handlers exist:
Task: "Add DatasetSummary/DatasetDetail types + listDatasets/getDataset to web/lib/orchestrator.ts"
```

---

## Implementation Strategy

### MVP First (User Story 1 Only)

1. Complete Phase 1: Setup (baseline green).
2. Complete Phase 2: Foundational (CRITICAL — blocks all stories).
3. Complete Phase 3: User Story 1 (header link, inventory list, search, detail).
4. **STOP and VALIDATE**: US1 independently testable (header link on every page; list + search + detail correct).
5. Deploy/demo if ready.

### Incremental Delivery

1. Setup + Foundational → foundation ready (datasets table, store, seams).
2. Add US1 (browse inventory) → test independently → MVP.
3. Add US2 (create) → test → demo.
4. Add US3 (register objective under dataset) → test → the hierarchy is live.
5. Add US4 (edit) → test → demo.
6. Add US5 (remove heuristics) → test → demo.
7. Add US6 (delete objective/dataset) → test → full lifecycle closes.
8. Polish: quickstart run, race suite, artifact hygiene.

### Parallel Team Strategy

With multiple developers: Setup + Foundational together; then US1/US2/US3 (P1) in parallel on distinct files; US4/US5 (P2) next; US6 (P3) last (shares `datasets.go`).

---

## Notes

- [P] tasks = different files, no dependencies.
- [Story] label maps the task to its user story for traceability.
- Each user story is independently completable and testable.
- Do NOT write/run new migrations outside `internal/store/migrations/`; every write migration ships its grants.
- The repo's gate is `make test` (`ARBORETTE_INTEGRATION=1`, `-p 1`) — assertions must be per-row, never table-global; guarded doubles return `ctx.Err()` on cancelled contexts.
- `make test` truncates `meta_heuristic_embeddings`: never compare embedding observations taken before a gate run with ones taken after.
- ⚠️ **Do not commit** — the user explicitly requested no git commits until instructed. Complete tasks, validate, and leave the working tree uncommitted.
