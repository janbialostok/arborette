# Tasks: Dataset Ownership & Sharing

**Input**: Design documents from `/specs/006-dataset-ownership-sharing/`

**Prerequisites**: plan.md (required), spec.md (required for user stories), research.md, data-model.md, contracts/

**Tests**: Integration tests are included. This repo's governing constitution is `AGENTS.md`, which treats the gated integration suite (`ARBORETTE_INTEGRATION`, `make up` + `make test`, per-row teardown, post-suite cleanliness gate) as mandatory for every backend change — the pre-existing suite must stay green (e.g. `dataset_shares` must join `ResetTables`). Web unit rules live under `web/lib/` as vitest suites per AGENTS.md.

**Organization**: Tasks are grouped by user story to enable independent implementation and testing of each story.

## Format: `[ID] [P?] [Story] Description`

- **[P]**: Can run in parallel (different files, no dependencies)
- **[Story]**: Which user story this task belongs to (e.g., US1, US2, US3)
- Include exact file paths in descriptions

## Path Conventions

- Go module at repository root; `web/` is the separate Next.js deployable
- Backend: `internal/store/`, `internal/orchestrator/`, `internal/heuristics/`, `internal/sleepcycle/`
- Frontend: `web/lib/`, `web/components/`
- Migrations: `internal/store/migrations/`

---

## Phase 1: Setup (Shared Infrastructure)

**Purpose**: Baseline verification — this is a mature repo, no project initialization needed.

- [ ] T001 Establish baseline: `go build ./...`, `go vet ./...`, and `go test ./...` (unit, no infra) plus `npm test` from `web/` are all green before the first edit.

---

## Phase 2: Foundational (Blocking Prerequisites)

**Purpose**: Schema, ownership columns, the access predicate, and the store heuristics seams that EVERY user story depends on.

**⚠️ CRITICAL**: No user story work can begin until this phase is complete.

- [X] T002 Write `internal/store/migrations/0018_dataset_ownership.up.sql`: add `datasets.owner_id uuid REFERENCES users(id)`; add `goal_registry.created_by uuid REFERENCES users(id)`; create `dataset_shares (dataset_id uuid REFERENCES datasets(id) ON DELETE CASCADE, user_id uuid REFERENCES users(id) ON DELETE CASCADE, created_at timestamptz NOT NULL DEFAULT now(), created_by uuid REFERENCES users(id), PRIMARY KEY (dataset_id, user_id))`; backfill `datasets.owner_id` to the earliest active admin (`ORDER BY created_at, id LIMIT 1`); `GRANT SELECT, INSERT, DELETE ON dataset_shares TO arborette_orchestrator` and `GRANT UPDATE (owner_id) ON datasets`, `GRANT UPDATE (created_by) ON goal_registry` to the orchestrator role. (models: spec FR-001/FR-002, data-model Dataset/Share/Goal)
- [X] T003 Write `internal/store/migrations/0018_dataset_ownership.down.sql` mirroring the up-migration (drop `dataset_shares`, drop the two columns, revoke the grants).
- [X] T004 Add `dataset_shares` to `ResetTables` in `internal/store/reset.go`, positioned before `datasets` (FK-safe child-before-parent order, so the operator reset and the post-suite cleanliness gate both cover it).
- [X] T005 [P] Store: add `OwnerID` to `store.Dataset`, extend `datasetColumns` and every scan list (`Get`, `GetByRef`, `List`) plus `Create` (stamps `owner_id`) in `internal/store/datasets.go`.
- [X] T006 [P] Store: add the accessible predicate and user-scoped reads — `ListAccessible(ctx, userID, query string) ([]Dataset, error)`, `GetAccessible(ctx, userID, id string) (Dataset, error)`, `CanAccess(ctx, userID, datasetID string) (bool, error)` — in `internal/store/datasets.go` (SQL: `owner_id = $1 OR EXISTS(SELECT 1 FROM dataset_shares WHERE dataset_id = d.id AND user_id = $1)`).
- [X] T007 [P] Store: add share grant CRUD — `Share(ctx, datasetID, userID, actorID string) error`, `RevokeShare(ctx, datasetID, userID string) error`, `ListShares(ctx, datasetID string) ([]ShareGrant, error)` where `ShareGrant{UserID, Username, CreatedAt}` joins `users` for the recipient name — in `internal/store/datasets.go`.
- [X] T008 [P] Store: add `CreatedBy` to `store.Goal`, extend `goalColumns` and every scan list (`Get`, `List`, `ListByDataset`, `ListObjectives`) plus `Insert` (stamps `created_by`) in `internal/store/goalregistry.go`.
- [X] T009 [P] Store: add goal user-scoped reads — `ListAccessible(ctx, userID string) ([]Goal, error)`, `ListByDatasetAccessible(ctx, userID, datasetID string) ([]Goal, error)`, `GetAccessible(ctx, userID, goalID string) (Goal, error)` — in `internal/store/goalregistry.go` (goal → `dataset_id` → accessible predicate).
- [X] T010 [P] Store: add the caretaker oracle `EarliestActiveAdmin(ctx) (store.User, error)` (`ORDER BY created_at, id LIMIT 1 WHERE role='admin' AND active`) in `internal/store/usersessions.go`, used by backfill self-heal and owner-death reassignment.
- [X] T011 [P] Store: user-scoped similarity search in `internal/store/embeddingstore.go` — the cross-goal branch filters rows whose goal's dataset the user can access (join goal → `datasets` + `dataset_shares`), and legacy NULL-goal corpus rows are returned only to the caretaker; `store.SearchScope` gains a `UserID`/caretaker marker so the existing `SimilaritySearch` signature stays the single seam.
- [X] T012 `heuristics.Service.Query` threads the access scope through to the scoped similarity search (extend `store.SearchScope` usage, no signature-drama beyond the scope field from T011) in `internal/heuristics/service.go`.
- [X] T013 Orchestrator seams in `internal/orchestrator/server.go`: extend the narrow `datasetStore` interface (`ListAccessible`, `GetAccessible`, `CanAccess`, `Share`, `RevokeShare`, `ListShares`, `Get`/`GetByRef` owner reads) and `goalStore` (`ListAccessible`, `ListByDatasetAccessible`, `GetAccessible`, `Get` with `CreatedBy`) and `heuristicsService.Query` (access scope). Add `actingUser(r)` (reads `AnalystsFromContext`, falls back to `s.identity.Current` for non-guard paths) in `internal/orchestrator/identity.go`.
- [X] T014 Extend the orchestrator fakes (`fakeDatasets`, `fakeGoals`, `fakeHeur`, and any `-race`-shared doubles with mutex-guarded accessors) in `internal/orchestrator/server_test.go` so the package compiles against the widened T013 seams; extend `internal/testutil` cleanup helpers only if a new persistent fixture needs one (share rows cascade via `RegisterDatasetCleanup`'s dataset FK — no new helper expected).

**Checkpoint**: Foundation ready — migration applied, ownership columns live, access predicate expressed in SQL, store/interface seams widened, fakes compile. User story implementation can now begin in parallel.

---

## Phase 3: User Story 1 - The inventory shows only what I can access (Priority: P1) 🎯 MVP

**Goal**: The dataset inventory (home + `/datasets`) shows only what the acting user owns or was shared, never anything else, in any form — with a "shared with you" marker distinguishing shared rows.

**Independent Test**: Sign in as two different users; user B creates a dataset; user A's inventory (home and `/datasets`) shows none of B's work with no placeholder/count/error hinting at it, while B's own inventory shows it.

### Tests for User Story 1

> **NOTE: Write these tests FIRST, ensure they FAIL before implementation**

- [X] T015 [P] [US1] Integration test (gated on `ARBORETTE_INTEGRATION`, per-row teardown via `testutil.RegisterUserCleanup`/`RegisterDatasetCleanup`): two users — B creates a dataset, A sees nothing on `/datasets`, B sees it — in a new `internal/orchestrator/access_ownership_integration_test.go`.

### Implementation for User Story 1

- [X] T016 [P] [US1] `handleListDatasets` switches to `datasets.ListAccessible(ctx, actingUser, q)` and classifies each row `access: "owner" | "shared"` in `internal/orchestrator/datasets.go`; `datasetDTO` gains `Access string \`"owner"|"shared"\`` and `OwnerID string`.
- [X] T017 [US1] Web DTO: `DatasetSummary` and `DatasetDetail` gain `access` and `owner_id` in `web/lib/orchestrator.ts`; the typed client passes them through unchanged.
- [X] T018 [P] [US1] Web rules: add `isOwner(access)`, `isSharedToMe(access)`, `canAdminister(access)`, and `canRegisterGoal(status, access)` to `web/lib/datasets.ts`, with vitest coverage in `web/lib/datasets.test.ts`.
- [X] T019 [US1] Web inventory: render the neutral "shared with you" marker (only when `isSharedToMe`) beside the status/objective badges in `web/components/DatasetsList.tsx`.

**Checkpoint**: US1 fully functional and testable independently — the primary visibility boundary exists.

---

## Phase 4: User Story 2 - Children inherit the dataset's visibility (Priority: P1)

**Goal**: Every child object — goals, runs, verifications, heuristics, outcomes, causal views — is reachable exactly when its dataset is accessible to the acting user, and refuses direct deep links with the uniform 404.

**Independent Test**: User B creates a dataset with several goals and runs; as user A, no goal, run, verification, heuristic, or outcome under B's dataset is reachable through `/goals`, the heuristics browser, a direct URL, or a search — while a dataset B shares with A reveals its children normally.

### Tests for User Story 2

- [X] T020 [P] [US2] Integration test (gated): B's dataset with goals + heuristics embeds under datasets; A reaches none via `/goals`, `GET /goals/{id}`, heuristics search/trace/delete, or `/datasets/{id}` (all 404 `not found`), and everything renders for A after B shares the dataset — extend `internal/orchestrator/access_ownership_integration_test.go`.

### Implementation for User Story 2

- [X] T021 [US2] `handleListGoals` filters through `goals.ListAccessible` (all) or `ListByDatasetAccessible` (per `?dataset_id=`) in `internal/orchestrator/submit.go`.
- [X] T022 [US2] Uniform goal-keyed gate: `lookupGoal` in `internal/orchestrator/server.go` answers `404 {"error":"goal not found"}` when the acting user cannot access the goal's dataset — this single helper gates every `/goals/{id}/*` handler (hypothesis-loop, stream, chat, sleep-cycle, verifications, outcomes/outcomes excerpt, causal-graph, corrections, findings verify, causal-verifications, delete).
- [X] T023 [P] [US2] `handleGetDataset` answers `404 {"error":"dataset not found"}` for an inaccessible dataset, and `Touch` (the access event the inventory ranks by) is gated on access in `internal/orchestrator/datasets.go`.
- [X] T024 [US2] Heuristics: `handleHeuristicSearch` scopes results to heuristics whose goal's dataset is accessible (legacy NULL-goal rows only for the caretaker); `handleHeuristicTrace`/`handleDeleteHeuristic` gate on the heuristic's owning goal; both use the access-scoped `heuristicsService.Query` and `actingUser` in `internal/orchestrator/heuristics.go`.

**Checkpoint**: US1 AND US2 both work independently — no child surface leaks.

---

## Phase 5: User Story 3 - Creating something makes me its owner (Priority: P1)

**Goal**: Anything the acting user creates — a dataset, or a goal (and its descendants) under a dataset — is immediately theirs: visible only to the creator and whoever they share with, with the creator recorded.

**Independent Test**: Create a dataset as one user and confirm it is owned by and only visible to that user immediately; create a goal under a shared dataset and confirm the goal's creator is recorded and its accessibility follows the dataset.

### Tests for User Story 3

- [X] T025 [P] [US3] Integration test (gated): a created dataset's `owner_id` is the creator and no other user sees it; a goal registered in a shared dataset has its `created_by` recorded and inherits the dataset's access set — extend `internal/orchestrator/access_ownership_integration_test.go`.

### Implementation for User Story 3

- [X] T026 [US3] `handleCreateDataset` stamps `OwnerID: actingUser` on the `store.Dataset` it inserts; audit `dataset_create` gains `owner_id` in `internal/orchestrator/datasets.go`.
- [X] T027 [US3] `handleSubmitGoal`: a bound goal checks `CanAccess(actingUser, dataset_id)` first (`404 dataset not found` otherwise, archived-409 unchanged); the goal insert stamps `CreatedBy: actingUser` in `internal/orchestrator/submit.go`. Implicit-mint ownership: `resolveDatasetForRef`/`datasetForRef` gain an owner parameter so an unbound submission's new implicit dataset is owned by the acting user (system/boot paths pass the caretaker identity); audit `goal_submit` gains `created_by`.

**Checkpoint**: Ownership exists at the moment of creation; new data is private by default; implicit mints never leak (US3.3).

---

## Phase 6: User Story 4 - Share a dataset with another user (Priority: P2)

**Goal**: A dataset owner explicitly shares a dataset with another account by username; the grant conveys working access (view + contribute + manage children the recipient created) and takes effect immediately, while sharing management stays owner-only.

**Independent Test**: Share a dataset with a second account; that account's inventory lists it and its children are reachable with working access on their next load, but the client never offers them dataset administration.

### Tests for User Story 4

- [X] T028 [P] [US4] Integration test (gated): owner shares by username → recipient's next `/datasets` load lists it and its goals/heuristics render; the collaborator is refused share-management (403); grants are per-recipient and independent — extend `internal/orchestrator/access_ownership_integration_test.go`.

### Implementation for User Story 4

- [X] T029 [P] [US4] Add share endpoints in `internal/orchestrator/datasets.go` + routes in `internal/orchestrator/server.go`: `GET /datasets/{id}/shares` (owner-only, else 403; `ShareGrant[]`), `PUT /datasets/{id}/shares/{username}` (owner-only; case-insensitive username lookup → 404 `user not found`; self-share 400 `you already own this dataset`; duplicate 409 `dataset already shared with this user`; 200 with the created grant), all wired to the T007 store methods. Audit `dataset_share` with `{"dataset_id", "user_id", "shared_with"}` keys. (DELETE route intentionally deferred to US5/T033.)
- [X] T030 [P] [US4] Web client: `listShares(id)`, `shareDataset(id, username)`, `revokeShare(id, username)` in `web/lib/orchestrator.ts`; vitest in `web/lib/orchestrator.test.ts`.
- [X] T031 [US4] Web sharing panel in `web/components/DatasetDetail.tsx` (rendered only when `canAdminister`): recipient list (username + grant time), add-by-username, revoke-with-confirm; surfaces exact `{error}` on 409/404/400. (`revokeShare`/DELETE wired because the DELETE route was pulled forward — see T033.)

**Checkpoint**: US1–US4 functional — sharing is the sanctioned way in, and it grants working access, not administration.

---

## Phase 7: User Story 5 - Revoke sharing and lose access immediately (Priority: P2)

**Goal**: Revoking a share ends access on the recipient's next data action — inventory, direct link, stream, search — with no stale continuation, including over open SSE streams.

**Independent Test**: Revoke a share; the recipient's next load of any surface (inventory, direct link, stream, or search) no longer reaches the dataset or its children.

### Tests for User Story 5

- [ ] T032 [P] [US5] Integration test (gated): before revoke the recipient reads normally; after `DELETE /datasets/{id}/shares/{username}` their next `/datasets` load and next goal-keyed read are refused; an already-open `/goals/{id}/stream` closes on the next data frame post-revoke — extend `internal/orchestrator/access_ownership_integration_test.go`.

### Implementation for User Story 5

- [X] T033 (endpoint half) [US5] Revoke handler: `DELETE /datasets/{id}/shares/{username}` (owner-only, idempotent 204 for an already-absent grant) wired to `RevokeShare`; audit `dataset_unshare` with `{"dataset_id", "user_id"}` in `internal/orchestrator/datasets.go` + route in `server.go`. Pulled forward so the T031 panel's revoke-with-confirm is end-to-end; the remaining US5 work is the T034 stream re-check.
- [ ] T034 [US5] Stream revocation (FR-008): `handleStream` re-checks `CanAccess(actingUser, goal.DatasetID)` before writing each forwarded/replayed frame (on a bounded cadence over the hub's channel) and closes the stream when access is lost — in `internal/orchestrator/stream.go`; the shared `Hub` stays access-agnostic.

**Checkpoint**: US1–US5 — revocation is immediate and streams die with the grant.

---

## Phase 8: User Story 6 - Only the owner administers the dataset (Priority: P2)

**Goal**: Only the dataset owner can edit metadata, delete the dataset, or manage sharing; a collaborator can work (view, create/run goals, manage their own children) but cannot rename, destroy, or re-share, and cannot delete a child they did not create.

**Independent Test**: Share a dataset; the collaborator is refused on rename, delete, and share-management while the owner succeeds on all three — and the collaborator can create/delete their own goal but not delete a goal they did not create.

### Tests for User Story 6

- [ ] T035 [P] [US6] Integration test (gated): collaborator `PATCH`/`DELETE /datasets/{id}` and share-management → 403 `dataset ownership required`; owner succeeds; collaborator deletes own goal (204) but deleting another collaborator's/owner's goal → 403 `only the goal's creator or the dataset owner can delete it` — extend `internal/orchestrator/access_ownership_integration_test.go`.

### Implementation for User Story 6

- [ ] T036 [US6] `handleUpdateDataset`/`handleDeleteDataset` answer `403 {"error":"dataset ownership required"}` to a collaborator (access but not owner) while a no-access caller keeps the 404 path in `internal/orchestrator/datasets.go`.
- [ ] T037 [US6] `handleDeleteGoal` (FR-014): after the US2 access gate, refuse 403 to a collaborator who is not the goal's `created_by` and not the dataset owner in `internal/orchestrator/goals.go` (legacy goal with NULL `created_by` deletable by the dataset owner).
- [ ] T038 [US6] Web `DatasetDetail.tsx`: owner sees metadata editor, objective form, delete control, sharing panel; collaborator sees name/description/status, objective list, active objective form, and a "shared with you" marker but NO metadata editor, delete, or sharing panel (absent, not disabled); deep-link to an inaccessible id renders the not-found state. All affordance decisions flow from `canAdminister`/`canRegisterGoal` in `web/lib/datasets.ts`.

**Checkpoint**: US1–US6 — the owner holds the administrative edge; collaborators work without administering.

---

## Phase 9: User Story 7 - Administrators follow the same rules (Priority: P3)

**Goal**: The admin role grants no data-visibility exception, and pre-existing/system-created data (and mentally-minted datasets) is attributed to an admin caretaker so it stays reachable only to that caretaker and whomever they share with.

**Independent Test**: Sign in as an admin; a dataset owned by another user and not shared with the admin is invisible to the admin on every surface — the same outcome as any unrelated user — and pre-existing/system data resolves to the caretaker owner.

### Tests for User Story 7

- [ ] T039 [P] [US7] Integration test (gated): an admin sees only owned/shared datasets across inventory, direct links, `/goals`, and heuristics — no route behaves differently; caretaker attribution: a pre-existing/NULL-owner dataset is visible to the earliest active admin and to nobody else; legacy NULL-goal embedding rows appear in searches only for the caretaker — extend `internal/orchestrator/access_ownership_integration_test.go`.

### Implementation for User Story 7

- [ ] T040 [US7] Caretaker self-heal: attribute every ownerless (`owner_id IS NULL`) dataset to the earliest active admin at first-admin registration and at boot reconciliation — extend `ReconcileDatasets` (or add a sibling reconcile) in `internal/orchestrator/datasets.go` and its boot call in `cmd/orchestrator/main.go`; NULL-owner rows are visible to nobody until attributed.
- [ ] T041 [US7] Owner-death/last-owner edge: deleting a user who owns datasets reassigns those datasets to the caretaker (earliest-admin rule) so the data is never orphaned invisible; legacy system-published heuristics (NULL `goal_id` embeddings) are scoped to the caretaker on every surface via the T011/T024 scope. No admin visibility exception anywhere — enforcement is uniformly the T006/T009 predicate.

**Checkpoint**: All user stories independently functional; admins and caretaker hold no back door.

---

## Phase 10: Polish & Cross-Cutting Concerns

**Purpose**: Improvements that affect multiple user stories.

- [ ] T042 [P] Sleep-Cycle grounding corpus read scoped to the goal's dataset plus system-owned rows (legacy) so the worker never retrieves across an ownership boundary — `internal/sleepcycle/grounding.go` (uses the T011 scoped similarity).
- [ ] T043 [P] Audit vocabulary sweep: confirm every new record uses the codebase-wide keys (`dataset_id`, `user_id` always for share/unshare; `optimization_function_id`/`data_source_ref` preserved on `dataset_create`/`goal_submit` additions) per `contracts/api.md` — spot-check in `internal/orchestrator/`.
- [ ] T044 [P] Full-gate verification: `make up` then `make test` (integration, `-p 1`) so the post-suite cleanliness gate is green; run `go test -race ./internal/orchestrator` when touching promotion/run-lifecycle doubles (AGENTS.md G7).
- [ ] T045 [P] Frontend gate: `npm test` from `web/` green (new vitest suites for `datasets.ts`, `orchestrator.ts` share client, predicates).
- [ ] T046 Run `quickstart.md` validation: walk the 7 end-to-end scenarios (private-by-default, inheritance, share=working-access-not-admin, immediate revocation, admins-follow-rules, caretaker backfill, implicit-mint privacy) and reconcile any plan/quickstart line that no longer holds — update the plan's status/departures in the same change set.

---

## Dependencies & Execution Order

### Phase Dependencies

- **Setup (Phase 1)**: No dependencies — baseline green first.
- **Foundational (Phase 2)**: Depends on Setup — BLOCKS all user stories.
- **User Stories (Phase 3+)**: All depend on Foundational completion; P1 stories (US1–US3) first, then P2 (US4–US6), then P3 (US7). Stories can proceed in parallel if staffed (they touch different files), or sequentially in priority order.
- **Polish (Final Phase)**: Depends on all desired user stories being complete.

### User Story Dependencies

- **US1 (P1)**: after Foundational — no other story needed; the MVP.
- **US2 (P1)**: after Foundational — needs the access predicate (T006/T009) only, but its same-predicate tests pair naturally with US1.
- **US3 (P1)**: after Foundational + US2's bound-goal access gate (T022/T027 interplay).
- **US4 (P2)**: after US1 (DTO/`CanAccess`) and the share store (T007) — the share endpoints.
- **US5 (P2)**: after US4 (revoke is a share-management sibling) — adds stream re-check.
- **US6 (P2)**: after US2/US4 — reuses the access gate and share endpoints for 403 ownership rules.
- **US7 (P3)**: after US1/US2/T006/T009/T010 — caretaker attribution and the admin-uniform enforcement finalize the boundary.

### Within Each User Story

- Tests MUST be written and FAIL before implementation.
- Store seams before handlers; interfaces before fakes; handlers before web DTO/UI.
- Story complete before moving to the next priority.

### Parallel Opportunities

- T005–T011 (store methods + migration: T002–T004) all mark **[P]** and can run together.
- All **Tests** tasks (T015, T020, T025, T028, T032, T035, T039) can each be launched with their story's implementation once the foundational T013/T014 seams exist.
- Different user stories touch disjoint files (`datasets.go` vs `submit.go` vs `goals.go` vs `heuristics.go` vs `stream.go` vs `web/*`) and can proceed in parallel after Foundational.

---

## Parallel Example: User Story 4

```bash
# Launch the US4 test and the web client seam together:
Task: "Integration test (gated): owner shares by username … extend internal/orchestrator/access_ownership_integration_test.go"
Task: "Web client listShares/shareDataset/revokeShare in web/lib/orchestrator.ts (+ vitest)"
# Then the non-parallel handlers on top of the store methods:
Task: "Add share endpoints in internal/orchestrator/datasets.go + routes in server.go"
Task: "Web sharing panel in web/components/DatasetDetail.tsx (canAdminister-gated)"
```

---

## Implementation Strategy

### MVP First (User Story 1 Only)

1. Complete Phase 1: Setup (baseline green)
2. Complete Phase 2: Foundational (CRITICAL — blocks all stories)
3. Complete US1 → STOP and VALIDATE: two-user inventory isolation test
4. Deploy/demo if ready

### Incremental Delivery

1. Setup + Foundational → Foundation ready
2. US1 → test independently → demo (MVP)
3. US2 → test independently → demo
4. US3 → test independently → demo
5. US4–US6 (P2) → share/grant, revoke-immediacy, owner-only admin → demo
6. US7 (P3) → admin-uniform + caretaker → demo
7. Polish: full gate + quickstart + docs

### Parallel Team Strategy

1. Team completes Setup + Foundational together
2. Once Foundational is done, split by story: US1 (inventory+DTO) / US3 (ownership stamping) back-to-back, then US2 (child gates), then US4/US5 (shares), then US6 (admin rules), then US7 (caretaker).
3. Stories integrate independently; each keeps the shared suite green.

---

## Notes

- [P] tasks = different files, no dependencies.
- [Story] label maps task to its user story for traceability.
- Every test returns a nil/empty default when its scripted store is exhausted — script one failure per expected refusals-before-success (AGENTS.md).
- The shared single Postgres DB (hence `make test -p 1`): integration tests assert per-row effects, never table-global counts, and register per-row teardown (`testutil.Register*Cleanup`) right after creating fixtures — never truncate `meta_heuristic_embeddings` or any shared table.
- Teardown is LIFO-safe: `RegisterDatasetCleanup` retires a dataset's goals/runs/verifications and its share rows via FK cascade, so a share-backed goal never blocks dataset cleanup regardless of registration order.
- Where an `ORDER BY` relies on `now()`-defaulted timestamps, `time.Sleep` a couple ms between inserts (AGENTS.md).
- Verify tests fail before implementing; commit after each task or logical group (do NOT commit until the user instructs — the feature is marked `DO NOT COMMIT TO GIT BEFORE INSTRUCTED`).
- Stop at any checkpoint to validate the story independently.
- Avoid: vague tasks, same-file conflicts, cross-story dependencies that break independence.