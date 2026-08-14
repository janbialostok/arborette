---

description: "Task list for Clean Test Data feature implementation"
---

# Tasks: Clean Test Data

**Input**: Design documents from `.turbo/specs/004-clean-test-data/`

**Prerequisites**: plan.md (required), spec.md (required for user stories), research.md, data-model.md, contracts/

**Tests**: Integration tests are included below — they are the repository's established convention (every store/operator surface ships a live-stack integration test gated on `ARBORETTE_INTEGRATION`), and each story's Independent Test in spec.md is exactly what they prove.

**Organization**: Tasks are grouped by user story to enable independent implementation and testing of each story.

## Format: `[ID] [P?] [Story] Description`

- **[P]**: Can run in parallel (different files, no dependencies)
- **[Story]**: Which user story this task belongs to (e.g., US1, US2, US3)
- Include exact file paths in descriptions

## Path Conventions

- Go repository: production code in `cmd/` and `internal/<pkg>/`; tests live **next to their package** (`internal/store/store_test.go`), per repo convention — there is no separate `tests/` tree
- Web module (`web/`) is untouched: the interface lists read straight from the record store, so a store wipe empties the UI

## Phase 1: Setup (Shared Infrastructure)

**Purpose**: Verify the implementation environment is healthy; no new project scaffolding is needed (this is an existing Go repo).

- [x] T001 Verify baseline: `go build ./...` and `go vet ./...` pass; confirm toolchain pins (Go 1.24 via `GOTOOLCHAIN`, pgx ≤ v5.7) and `go test ./...` stays green without infrastructure

---

## Phase 2: Foundational (Blocking Prerequisites)

**Purpose**: The wipe primitives and the test-hygiene harness every user story depends on.

**⚠️ CRITICAL**: No user story work can begin until this phase is complete

### Wipe & verification primitives

- [x] T002 Implement `store.ResetAll(ctx, pool)` in `internal/store/reset.go`: one owner-role transaction deleting in FK-safe order (`meta_heuristic_embeddings` → `runs` → `verification_queue` → `causal_verifications` → `goal_registry` → `datasets` → `data_source_registry` → `sessions` → `users` → `audit_log`) per `data-model.md`; idempotent on empty tables; report rows removed per table
- [x] T003 Implement `store.EmptyCounts(ctx, pool)` in `internal/store/reset.go`: return a per-table row count map (the ten tables above) for the gate
- [x] T004 Implement `graph.Neo4jRepository.Wipe(ctx)` in `internal/graph/neo4j.go`: `MATCH (n) DETACH DELETE n` (whole-graph wipe, per research D3)
- [x] T005 Implement `graph.Neo4jRepository.DeleteNode(ctx, id)` in `internal/graph/neo4j.go`: `MATCH (n {id:$id}) DETACH DELETE n`, no error when absent (test-fixture cleanup, per research D3)
- [x] T006 [P] Implement `objectstore.Client.ListKeys(ctx)` and `objectstore.Client.Wipe(ctx)` in `internal/objectstore/client.go`: ListKeysV2 paging plus batch `DeleteObjects`; Wipe idempotent on an empty bucket (per research D4)

### Test-hygiene harness

- [x] T007 Implement per-row cleanup helpers in `internal/testutil/testutil.go`, all owner-DSN / node-id based, all idempotent, all registered via `t.Cleanup` so they run on pass, fail, and panic (contract `test-hygiene.md`):
  - `RegisterDatasetCleanup(ctx, cfg, datasetID)` (owner DELETE, and its `data_source_registry` row when no longer referenced)
  - `RegisterGoalCleanup(ctx, cfg, goalID)` (owner DELETE of `runs`/`verification_queue`/`causal_verifications` then the goal row)
  - `RegisterEmbeddingCleanup(ctx, cfg, nodeID)` (owner DELETE by `node_id`)
  - `RegisterUserCleanup(ctx, cfg, userID)` (owner DELETE; sessions CASCADE)
  - `RegisterGraphNodeCleanup(ctx, cfg, nodeID)` (`graph.DeleteNode` via a cfg-wired repository)
  - `RegisterObjectCleanup(ctx, cfg, key)` (objectstore DELETE via a cfg-wired client)

**Checkpoint**: Foundation ready — every store can be wiped, every gate question can be answered, and every test has a fixture-cleanup primitive.

---

## Phase 3: User Story 1 - Start from a clean slate (Priority: P1) 🎯 MVP

**Goal**: `make clean-data` wipes every dataset, objective, and derived record/artifact, leaving the interface empty; the wipe is repeatable and idempotent.

**Independent Test**: Run `make clean-data` against a cluttered stack (spec Scenario 1): it exits 0, rerunning is a no-op-exit-0, `cleanup check` reports zero, and the Datasets/Objectives lists in the web UI render empty.

### Tests for User Story 1

- [x] T008 [US1] Write `store.ResetAll`/`EmptyCounts` integration test in `internal/store/reset_integration_test.go` (gated on `ARBORETTE_INTEGRATION`): seed datasets/goals/runs/verifications/audit/embeddings rows, run `ResetAll`, assert every table is empty and `EmptyCounts` reports zero; assert a second `ResetAll` on the empty database succeeds (idempotence, FR-003)
- [x] T009 [P] [US1] Write `graph.Wipe`/`DeleteNode` integration test in `internal/graph/wipe_integration_test.go` (gated): seed goal-scoped and goal-less nodes, `DeleteNode` removes only the target, `Wipe` empties the graph; absent-id `DeleteNode` is a no-op
- [x] T010 [P] [US1] Extend `internal/objectstore/objectstore_test.go` (gated): `ListKeys` sees a put key, `Wipe` empties the bucket and is idempotent, absent-key delete is a no-op

### Implementation for User Story 1

- [x] T011 [US1] Implement `cmd/cleanup/main.go` (the `cmd/migrate`/`cmd/dbbootstrap` precedent): `config.Load()`, owner pool + Neo4j + objectstore wiring, and the two modes — `clean` runs `store.ResetAll` + `graph.Wipe` + `objectstore.Wipe` and logs the per-store summary (FR-001/002/004); `check` runs `store.EmptyCounts` + graph node count + `objectstore.ListKeys` and prints a clean confirmation or names each offender table with a sample of ids (FR-008/010). Exit codes per `contracts/cleanup-cli.md`: 0 clean, 1 wipe failed or check found residue, 2 usage error. No args prints usage
- [x] T012 [US1] Add the `clean-data` target to `Makefile` (per `contracts/makefile.md`): source `.env`, apply the `HOST_ENV` localhost overrides (`POSTGRES_HOST=localhost`, `S3_ENDPOINT=http://localhost:9000`, ...), run `go run ./cmd/cleanup clean`
- [x] T013 [US1] Run spec quickstart Scenario 1 end to end against a live stack; verify the web UI Datasets/Objectives lists are empty and `cleanup check` passes

**Checkpoint**: The clutter is gone and can be re-made gone at will; the wipe is independently demonstrated.

---

## Phase 4: User Story 2 - Tests leave no trace (Priority: P1)

**Goal**: Every integration-gated test removes exactly what it created — on success, failure, and panic — in the record store, the graph, and the object store; consecutive suite runs leave identical state.

**Independent Test**: Run `make test` then `cleanup check` (or the US3 gate once wired) — all counts zero after the run; a second `make test` produces the identical clean end-state (spec Scenario 2).

### Implementation for User Story 2

**Store package (`internal/store`)**

- [x] T014 [US2] Add fixture cleanup to `internal/store/store_test.go`: register `RegisterEmbeddingCleanup` for every node in the embedding round-trip/ranking/scope/floor/null-goal/repair tests; `RegisterGoalCleanup`+`RegisterDatasetCleanup` (dataset registered first so its goal deletes first) for the registry/grants/runs tests; `RegisterUserCleanup` where users are created; delete the appended audit row in `TestAuditBoundaryOrchestrator` (owner DELETE; the only role allowed to) and the registered ref in `TestDataSourceRegistry`
- [x] T015 [US2] Remove `TruncateEmbeddings` from `internal/testutil/testutil.go` and prune its call sites in `internal/store/store_test.go`, replacing them with per-row `RegisterEmbeddingCleanup` (repairs the AGENTS.md divergence where `make test` destroyed real Meta-Heuristic embeddings); keep the tests' own freshly-seeded rows
- [x] T016 [US2] Add fixture cleanup to `internal/store/datasets_integration_test.go`: register dataset/goal cleanup in `TestDatasetInventory`, `TestDatasetAccessRanking`, `TestDatasetLifecycle` (including the ref-registry rows it registers), `TestGoalDatasetBinding` (dataset + goal + runs + causal verification), `TestDeleteGoalRetiresEmbeddings`
- [x] T017 [US2] Add fixture cleanup to `internal/store/usersessions_integration_test.go`: register `RegisterUserCleanup` in `TestUserAccountLifecycle`, `TestSessionLifecycle` (user; sessions cascade), `TestUsersGrants`

**Graph package (`internal/graph`)**

- [x] T018 [US2] Register node cleanup inside the shared seed helpers so every caller inherits it: `seedTriplet` (`internal/graph/graph_test.go`), `seedGoalTriplet` (`internal/graph/sleepcycle_test.go`), `seedExtractionTriplet` (`internal/graph/extraction_test.go`), `seedCausalTriplet` (`internal/graph/causalverification_integration_test.go`), `seedGraph` (`internal/graph/correction_integration_test.go`) — register `RegisterGraphNodeCleanup` per created node id (fresh `testutil.NewID` ids need no rename; stable *definition* strings like "abstraction" ride the same fresh ids)
- [x] T019 [US2] Register cleanup for any seed not routed through the shared helpers in `internal/graph/metaheuristic_ontology_test.go` and `internal/graph/delete_integration_test.go` (per-node delete; must remain a no-op on nodes the test already deleted via `DeleteMetaHeuristic`/`DeleteGoalGraph`)

**Service integration suites**

- [x] T020 [US2] Add cleanup to `internal/heuristics/heuristics_test.go`: `RegisterGraphNodeCleanup` for the seeded triplet + meta-heuristic and `RegisterEmbeddingCleanup` for the upsert
- [x] T021 [P] [US2] Add cleanup to `internal/mcpserver/tools_integration_test.go`: same pattern — nodes + embedding rows the suite seeds
- [x] T022 [P] [US2] Add cleanup to `internal/objectstore/objectstore_test.go`: `RegisterObjectCleanup` for the key `TestPutGetRoundTrip` writes
- [x] T023 [P] [US2] Add cleanup to `internal/sandbox/filesource_test.go` inside `putObject` itself (single change point; `RegisterObjectCleanup` per ref so every fixture object goes with its test)

**Scope note**: `internal/embedding/ollama_test.go` (LLM HTTP only, no persistence) and `internal/orchestrator` (fake-collaborator tests, no DB writes) need no hygiene changes.

**Checkpoint**: Run `make test` then `cleanup check` — zero residue; run it twice and the end-state is identical (FR-005/007/009).

---

## Phase 5: User Story 3 - The gate catches regressions (Priority: P2)

**Goal**: `make test` ends with a cleanliness check that fails the run and names the offenders whenever a test leaves residue.

**Independent Test**: spec quickstart Scenario 3 — temporarily leave one dataset behind, run `make test`, the gate fails naming the table; remove the defect, `make test` passes again.

### Implementation for User Story 3

- [x] T024 [US3] Wire the gate into the `test` target of `Makefile` (per `contracts/makefile.md`): after `ARBORETTE_INTEGRATION=1 go test -p 1 ./...`, run `go run ./cmd/cleanup check` with the same env, so the target fails if tests or gate fail; leave bare `go test ./...` untouched (no infra)
- [x] T025 [US3] Validate gate correctness with a manual leak probe against the live stack (quickstart Scenario 3): seed one dataset, confirm `make test`/`cleanup check` fails and names the `datasets` table, then wipe and confirm green; no committed test ships with the leak
- [x] T026 [US3] Confirm the gate treats a freshly `clean`-ed environment as passing (FR-010) by running `make clean-data` then `make test` on a clean stack

---

## Phase 6: Polish & Cross-Cutting Concerns

**Purpose**: Consistency, documentation, and the no-infra gate stays green.

- [x] T027 [P] Update `AGENTS.md` testing guidance to match the new reality: `make test` no longer truncates `meta_heuristic_embeddings` (per-row cleanup instead), fixture residue no longer accumulates (per-test `t.Cleanup` + the gate), scoped host runs should end with `cleanup check`, and `make clean-data` exists for the one-time wipe
- [x] T028 [P] Add a backlog entry to `.turbo/improvements.md` for per-test Postgres isolation (separate DB/schema) as the future alternative to `-p 1` sharing, per research D8's rejected alternatives
- [x] T029 [P] Run validation: `go build ./...`, `go vet ./...`, `go test ./...` (no infra) all green; `go test -race ./internal/orchestrator` green per the AGENTS.md race rule
- [x] T030 Run the full quickstart validation (Scenarios 1–4) against a live stack and note the results in this feature's plan

---

## Dependencies & Execution Order

### Phase Dependencies

- **Setup (Phase 1)**: No dependencies — start immediately
- **Foundational (Phase 2)**: Depends on Setup; **BLOCKS all user stories** (wipes + harness must exist first)
- **User Stories (Phases 3–5)**: Depend on Foundational; story phases proceed P1 → P1 → P2
- **Polish (Phase 6)**: Depends on all stories complete

### User Story Dependencies

- **User Story 1 (P1)**: Starts after Foundational; needs `store.ResetAll`/`graph.Wipe`/`objectstore.Wipe` (T002/T004/T006). No dependency on US2/US3; independently demoable at its checkpoint
- **User Story 2 (P1)**: Starts after Foundational; needs the testutil helpers (T007). Independent of US1 and US3; its verification uses `cmd/cleanup check`, which lands in the US1 binary task (T011), so US2 can validate itself before US3 wires the Make target
- **User Story 3 (P2)**: Starts after Foundational; its `check` mode exists from T011. Depends only on the binary being present, so it may proceed in parallel with US1/US2

### Within Each User Story

- Implementation primitives before their integration tests (T008–T010 against T002–T006; then the binary T011)
- US2 goes bottom-up: helpers exist (T007), then store → graph → service suites, then the rank-and-file regressions
- Story checkpoint before moving to the next phase

### Parallel Opportunities

- Foundational: T006 (objectstore) is [P] alongside the store/graph primitives
- US1 tests: T008, T009, T010 are mutually [P]
- US2: T020, T021, T022, T023 are [P] (different packages); T014–T017 (store) and T018–T019 (graph) could be staffed in parallel by different engineers since they touch different files — but `internal/graph` and `internal/store` package files both edit shared seed helpers, so keep per-package owners
- US1, US2, US3 can be staffed in parallel once T011 (or at least T002–T007) lands: the wipe story, the hygiene sweep, and the gate wiring touch disjoint files

### Parallel Example: User Story 2 package sweep

```bash
# Launch the disjoint-package hygiene tasks together:
Task: "T020 heuristics_test.go cleanup"
Task: "T021 [P] mcpserver tools_integration_test.go cleanup"
Task: "T022 [P] objectstore_test.go cleanup"
Task: "T023 [P] sandbox filesource_test.go cleanup (putObject change point)"
```

---

## Implementation Strategy

### MVP First (User Story 1 Only)

1. Complete Phase 1: Setup
2. Complete Phase 2: Foundational (CRITICAL — blocks all stories)
3. Complete Phase 3: User Story 1 — `make clean-data` empties the stack, idempotent
4. **STOP and VALIDATE**: `cleanup check` zero, UI lists empty, wipe is rerunnable
5. This alone delivers spec SC-001/SC-002 with no test changes

### Incremental Delivery

1. Setup + Foundational → wipe primitives and harness exist
2. US1 → operator wipe (MVP) → validate
3. US2 → test suite stops accumulating; verified each run ends zero
4. US3 → the gate makes "clean end-state" enforced, not aspirational
5. Polish → AGENTS.md/backlog accurate, full quickstart validated

### Parallel Team Strategy

With multiple developers:

1. Everyone completes Setup + Foundational together
2. Once Foundational is done:
   - Developer A: US1 (wipe binary + `make clean-data`)
   - Developer B: US2 hygiene sweep (store first, then graph, then service suites)
   - Developer C: US3 gate wiring (works once T011's `check` mode exists)
3. Stories integrate without file conflicts: cmd/, internal test files, and the Makefile are disjoint

---

## Notes

- [P] tasks = different files, no dependencies
- [Story] label maps task to the spec's user stories for traceability
- Each story is independently completable and testable at its checkpoint
- No commit happens until the user instructs it