# Tasks: Datasets as the Index Page

**Input**: Design documents from `specs/002-datasets-index-page/`

**Prerequisites**: plan.md (required), spec.md (required for user stories), research.md, data-model.md, contracts/rest-api.md, quickstart.md

**Tests**: Integration tests are included per repo convention (AGENTS.md — the declared gate is `make test` with `ARBORETTE_INTEGRATION=1` and `-p 1`; per-row assertions, never table-global counts; guarded doubles return `ctx.Err()`; deterministic `ORDER BY` tests `time.Sleep` between creates). Write/run them within each story phase. Web unit suites live in `web/lib/` (vitest, `npm test`).

**Organization**: Tasks are grouped by user story to enable independent implementation and testing of each story.

**Repo paths** (single repo, two deployables): Go module at root (`internal/`, `cmd/`, `internal/store/migrations/`); web deployable at `web/`.

## Format: `[ID] [P?] [Story] Description`

- **[P]**: Can run in parallel (different files, no dependencies)
- **[Story]**: Which user story this task belongs to (e.g., US1, US2, US3)
- Include exact file paths in descriptions

---

## Phase 1: Setup (Shared Infrastructure)

**Purpose**: Establish a clean baseline before any change.

- [X] T001 Establish green baseline: `go build ./...`, `go vet ./...`, `go test ./...` (integration skipped), and `npm test` from `web/` all pass before changes

---

## Phase 2: User Story 1 - Land on the dataset inventory (Priority: P1) 🎯 MVP

**Goal**: The root URL serves the dataset inventory (list, search, create, detail) instead of the submit-goal landing hero. Both `/` and `/datasets` render the same component, so `git` bookmarks and intra-app `router.push("/datasets")` keep resolving.

**Independent Test**: Opening `/` renders the dataset inventory — list rows, search box, and "New dataset" entry — with no submit-goal hero; filter-by-name and detail drill-down behave exactly as on `/datasets`.

### Implementation for User Story 1

- [X] T002 [US1] Replace the submit-goal hero in `web/app/page.tsx` so the root route renders `<DatasetsList />` (drop the `GoalForm`/`SectionLabel` hero imports and the register-a-goal marketing copy; keep the route itself). Do not touch `web/app/datasets/page.tsx` — it stays an alias of the same inventory.

**Checkpoint**: US1 fully functional and independently testable — root URL is the inventory; baseline still green.

---

## Phase 3: User Story 2 - See most-recently-accessed datasets first (Priority: P1)

**Goal**: A new, server-recorded per-dataset last-access time orders the inventory (most recently accessed first; never-accessed falls back to newest-created-first; deterministic on reload). `GET /datasets/{id}` is the access event and best-effort stamps the time; every inventory read returns it.

**Independent Test**: Open dataset B's detail then `GET /datasets` → B is now above A (never touched); reload returns identical order; a fresh (never-accessed) dataset sits below accessed ones (SC-002/SC-003).

### Implementation for User Story 2

- [X] T003 [US2] Create migration `internal/store/migrations/0016_datasets_last_accessed.up.sql` (and `.down.sql`): `ALTER TABLE datasets ADD COLUMN last_accessed_at timestamptz` / `ALTER TABLE datasets DROP COLUMN last_accessed_at`. No grant change — the orchestrator role already has `SELECT`/`UPDATE` on `datasets` (`//go:embed *.sql` picks the files up automatically)
- [X] T004 [US2] Extend `internal/store/datasets.go`: add `LastAccessedAt *time.Time` to `store.Dataset`, add the column to `datasetColumns`, scan it in every reader (`Get`, `GetByRef`, `List` both `q` branches), reorder `List` to `ORDER BY last_accessed_at DESC NULLS LAST, created_at DESC, id` in **both** the filter and unfiltered branches, and add `Touch(ctx, id)` running `UPDATE datasets SET last_accessed_at = now() WHERE id = $1` (depends on T003)
- [X] T005 [P] [US2] Add `Touch(ctx, id) error` to the `datasetStore` interface in `internal/orchestrator/server.go` and implement it on the `fakeDatasetStore` in `internal/orchestrator/server_test.go` (the fake MUST return `ctx.Err()` when its context is done — AGENTS.md)
- [X] T006 [P] [US2] Update `internal/orchestrator/datasets.go`: add `LastAccessedAt *time.Time json:"last_accessed_at"` to `datasetDTO`, copy it in `toDatasetDTO`, and have `handleGetDataset` best-effort `s.datasets.Touch(r.Context(), id)` after the lookup — log a failure and still serve the detail 200 (an access mark must never turn an open into a 500) (depends on T004, T005)
- [X] T007 [P] [US2] Add `last_accessed_at: string | null` to `DatasetSummary` in `web/lib/orchestrator.ts` and add the field to the `ds()` fixture in `web/lib/datasets.test.ts` (type churn only — ordering stays server-side, no client re-sort)
- [X] T008 [US2] Add tests: in `internal/store/datasets_integration_test.go`, `Touch` then `List` positions the touched row first and a never-accessed row ranks below it (create the rows under test with `time.Sleep` between inserts; assert on those rows only); in `internal/orchestrator/datasets_test.go`, `GET /datasets/{id}` records the touch on the fake and a failed `Touch` still returns the detail (depends on T004–T006)

**Checkpoint**: US2 complete — inventory ordering is recency-driven and identical across reloads.

---

## Phase 4: User Story 3 - Clean header navigation (Priority: P2)

**Goal**: The **Submit goal** link is gone from every page header, and no in-app affordance points at the removed landing page; Objective/Datasets/Heuristics + brand mark remain.

**Independent Test**: On any page the header shows only Objectives, Datasets, Heuristics, and the brand mark; following the Objectives empty-state prompt lands on the dataset inventory (open a dataset to register), never a dead route.

### Implementation for User Story 3

- [X] T009 [P] [US3] Remove `{ href: "/", label: "Submit goal" }` from `LINKS` in `web/components/AppNav.tsx` and drop the now-dead `link.href === "/" ? pathname === "/" : ...` active-state special case (keep the brand `Link href="/"` and the uniform `pathname.startsWith(href)` check for the remaining entries)
- [X] T010 [P] [US3] Repoint the Objectives empty-state prompt in `web/components/ObjectivesList.tsx` (currently links `/` with "Submit a goal"): change the copy to guide toward registering from a dataset's detail and point the link at `/datasets`
- [X] T011 [US3] Grep the whole `web/` tree and confirm no other link targets the removed landing page — the only remaining `/` references are the brand mark and the navigation census; log any stragglers (depends on T009, T010)

**Checkpoint**: US3 complete — header advertises only live pages; every view stays reachable.

---

## Phase 5: User Story 4 - Register an objective from the dataset's detail view (Priority: P2)

**Goal**: Objective registration moves onto the dataset detail: an active dataset hosts an inline registration form bound to it (goal text only — the data source is already there); an archived dataset hides the form and gives the reason. `POST /goals` binding and the archived 409 gate already exist — no backend change.

**Independent Test**: On an active dataset's detail, register an objective → it is created bound to that dataset, appears under it and in Objectives, and opens on its live view; on an archived dataset's detail there is no registration form and the reason is shown.

### Implementation for User Story 4

- [X] T012 [P] [US4] Add an `initialDatasetID?: string` prop to `web/components/GoalForm.tsx`: when set, hide the picker and the upload/path ingest and submit `dataset_id` fixed — *departure:* the `listDatasets` fetch stays (it resolves the bound dataset's display name in the read-only source bar); success unchanged: `router.push("/goals/{id}")`; when unset, today's picker/upload mode renders exactly as now
- [X] T013 [US4] Add the registration entry to `web/components/DatasetDetail.tsx`: for an active dataset render a collapsible bound `<GoalForm initialDatasetID={id} />` above/next to the Objectives panel; for an archived dataset render the reason ("An archived dataset cannot accept new objectives") and no form (depends on T012)
- [X] T014 [US4] Update the dataset-detail empty-objectives copy in `web/components/DatasetDetail.tsx` (currently "Submit one with it selected as the data source") to point at the inline registration entry

**Checkpoint**: US4 complete — registration is reachable from a dataset's detail with no capability lost.

---

## Phase 6: User Story 5 - Browse a dataset's objectives in a defined order (Priority: P2/P3)

**Goal**: A dataset's objectives render active-run-first (latest run status `running`), then the rest, newest-created-first within each group — identical across reloads. Same order for the non-empty-delete 409's blocking list.

**Independent Test**: A dataset whose objective has a running run lists it above settled objectives; within a group the newest-created objective is first; reload returns the same order (SC-006).

### Implementation for User Story 5

- [X] T015 [US5] Add the active-first partition to `objectivesWithStatus` in `internal/orchestrator/datasets.go`: after synthesizing each objective's latest-run status, stable-partition the slice so objectives whose status is `running` come first — `ListByDataset` already feeds `created_at DESC`, so a stable partition keeps newest-created-first within each group with `optimization_function_id` as the final tie-break; apply the same partition to the delete-409's blocking list (depends on prior US2 work in the same file — schedule after US2)
- [X] T016 [US5] Add handler tests asserting the partition on a fixture dataset's own rows: a `running` objective sorts above a `completed` and a `no run` objective, newest-created first within each group, stable across the detail read and the delete 409 (`TestDatasetObjectiveOrdering` in `internal/orchestrator/datasets_test.go` — *departure:* placed there, with the shared `datasetTestServer`/`fakeRuns`, rather than in `submitdataset_test.go`)

**Checkpoint**: US5 complete — objectives lists have a deliberate, deterministic order.

---

## Phase 7: Polish & Cross-Cutting Concerns

**Purpose**: Full-suite validation, race coverage, and artifact hygiene.

- [X] T017 [P] Run `go test -race ./internal/orchestrator` covering the touched detail/registration handlers and their fakes (AGENTS.md race requirement)
- [X] T018 Run the full `quickstart.md` validation end-to-end against the live stack (`make up`, scoped `go test`, curl steps 1–6, `make web-dev` checks, `npm run build` from `web/`) and fix any drift — *departure:* validated via the repo gate instead — `make up`, `make test` (full integration suite against the live stack, including `TestDatasetAccessRanking` against real Postgres), `npm run build` + `npm test` + `tsc --noEmit` from `web/` — rather than the step-by-step curl walkthrough; the homebrew-PG port shadow (improvements.md) was resolved operationally to get the gate green (depends on all stories)
- [X] T019 [P] Re-check that no new audit events were introduced and the audit-detail vocabulary is untouched; verify `go vet ./...`, `go build ./...`, and `web` build stay green
- [X] T020 [P] Update spec/plan artifacts (`spec.md` Clarifications, `plan.md`, `data-model.md`) to match any implementation departures; add entries to `.turbo/improvements.md` only for deliberately skipped work

---

## Dependencies & Execution Order

### Phase Dependencies

- **Setup (Phase 1)**: No dependencies — can start immediately.
- **US1 (Phase 2)**: After Setup. Frontend-only; the MVP.
- **US2 (Phase 3)**: After Setup (+ US1 for the home-page context). Owns the backend ordering/access seam.
- **US3 (Phase 4)**: After US1 (removing the header link must not strand the landing page before the root swap).
- **US4 (Phase 5)**: After Setup. Independent of US2/US3/US5 (touches `GoalForm.tsx`/`DatasetDetail.tsx` only).
- **US5 (Phase 6)**: After US2 — both edit `internal/orchestrator/datasets.go`; schedule US5 after US2 to avoid same-file conflicts.
- **Polish (Final Phase)**: Depends on the stories shipped.

### User Story Dependencies

- **US1 (P1)**: Independent; the root-URL swap.
- **US2 (P1)**: Independent file-wise of US1 but is the other half of the home-page experience.
- **US3 (P2)**: Depends on US1 only.
- **US4 (P2)**: Independent — no backend change, bound submit reuses the existing `POST /goals` + archived 409.
- **US5 (P2/P3)**: Depends on US2 (same `datasets.go`).

### Within Each User Story

- Store/interface additions before handlers (US2: T003→T004→T005/T006)
- Backend before web typing (US2: T006 before T007)
- Implementation before its integration tests (T008, T016)
- Story complete before moving to the next priority

### Parallel Opportunities

- US2: T005, T006, T007 run together once T004's store change is set (distinct files: `server.go`/fakes, `datasets.go`, `web/lib`).
- US3: T009, T010 parallel; T011 after.
- US4: T012 vs T013/T014 parallel (distinct files).
- US3 can run in parallel with US2 (disjoint files); US4 can run in parallel with US2/US3/US5.

---

## Parallel Example: User Story 2

```bash
# Backend seams (distinct files, after T004):
Task: "Add Touch to datasetStore interface + fake (internal/orchestrator/server.go, server_test.go)"
Task: "Add LastAccessedAt to datasetDTO + best-effort Touch in handleGetDataset (internal/orchestrator/datasets.go)"
Task: "Add last_accessed_at to DatasetSummary + ds() fixture (web/lib/orchestrator.ts, web/lib/datasets.test.ts)"
```

---

## Implementation Strategy

### MVP First (User Story 1 Only)

1. Phase 1: Setup (baseline green).
2. Phase 2: US1 — root URL serves the inventory (one-file change).
3. **STOP and VALIDATE**: `/` renders the inventory exactly as `/datasets` with no hero.
4. Deploy/demo if ready.

### Incremental Delivery

1. Setup + US1 → root is the inventory (MVP).
2. Add US2 → last-access ordering + access stamp → test independently → demo.
3. Add US3 → header/link cleanup → test → demo (no dead links).
4. Add US4 → registration from the dataset detail → test → demo (no capability lost).
5. Add US5 → active-first objectives ordering → test → demo.
6. Polish: quickstart run, race suite, artifact hygiene.

### Parallel Team Strategy

With multiple developers: Studio + US1 first; then US2, US3, US4 in parallel on disjoint files (orchestrator/store/web-lib vs AppNav/ObjectivesList vs GoalForm/DatasetDetail); US5 after US2 (same `datasets.go`).

---

## Notes

- [P] tasks = different files, no dependencies.
- [Story] label maps the task to its user story for traceability.
- Each user story is independently completable and testable.
- Do NOT write/run new migrations outside `internal/store/migrations/`; migration `0016` ships no grant changes (the orchestrator already reads/writes `datasets`).
- The repo's gate is `make test` (`ARBORETTE_INTEGRATION=1`, `-p 1`) — assertions must be per-row, never table-global; guarded doubles return `ctx.Err()` on cancelled contexts; deterministic `ORDER BY` tests `time.Sleep` between creates.
- `GET /datasets/{id}` gains a documented best-effort side effect (`last_accessed_at = now()`); the shared integration DB persists those stamps run-to-run, so ordering tests assert on rows the test itself owns/creates.
- `make test` truncates `meta_heuristic_embeddings`: this feature touches no embedding rows, but retain the AGENTS.md discipline about before/after observations.
- ⚠️ **Do not commit** — the user explicitly requested no git commits until instructed. Complete tasks, validate, and leave the working tree uncommitted.