# Implementation Plan: Dataset Ownership & Sharing

**Branch**: `006-dataset-ownership-sharing` | **Date**: 2026-08-14 | **Spec**: [spec.md](./spec.md)

**Input**: Feature specification from `/specs/006-dataset-ownership-sharing/spec.md`

## Summary

Today the console is a single shared workspace: every signed-in user sees every dataset and everything beneath it (goals, runs, verifications, heuristics, embeddings, graph views) regardless of who created it. Nothing user-facing records ownership and no query filters by the acting user.

This feature makes the **dataset the ownership root**: a dataset, and every child object beneath it, is owned by the signed-in user who created it, and is invisible to every other user unless the owner explicitly shares the dataset with that account. Children inherit dataset accessibility, so access is decided once at the dataset level and every surface — lists, detail views, goal-keyed routes, streams, heuristics search, similarity retrieval — enforces it server-side. The acting-session identity (already resolved by the session guard into `AnalystsFromContext`) becomes the ownership stamp: new datasets record an `owner_id`, new goals a `created_by`, and a new `dataset_shares` table records explicit per-account grants.

Three decisions from the spec drive the design: pre-existing and system-created objects are owned by an **admin caretaker** (backfilled to the earliest active admin, self-healed when the first admin registers); **administrators follow the same ownership/sharing rules as any other user** (no data-visibility exception); sharing conveys **working access without dataset administration** (view + contribute + manage children the recipient created; only the owner edits/deletes the dataset and manages sharing).

The technical approach: a migration (`0018_dataset_ownership`) adds `datasets.owner_id`, `goal_registry.created_by`, and a `dataset_shares` table with grants; the store layer gains user-scoped reads (`ListAccessible`, `GetAccessible`, `CanAccess`, share CRUD) and a scoped similarity search; every orchestrator handler enforces an access predicate before serving data, with lists filtered and inaccessible single objects answered uniformly (404), non-owner writes answered 403; SSE streams re-check access on each forwarded frame so revocation takes effect mid-stream; and the web client surfaces a "shared with you" marker, hides owner-only controls from collaborators, and adds a share-management panel for owners.

## Technical Context

**Language/Version**: Go 1.24 (toolchain pinned by `GOTOOLCHAIN`) for the backend; TypeScript on Next.js ≥15 App Router, Node 22, for `web/`. A new migration ships in `internal/store/migrations`.

**Primary Dependencies**: No new dependencies. Reuses existing seams: `github.com/jackc/pgx/v5` (≤ v5.7.x per the pinned toolchain) for the store layer, the orchestrator's narrow consumer interfaces (`datasetStore`, `goalStore`, `heuristicsService`) for unit-testable handlers, and the existing session/identity plumbing. The Anthropic/DeepInfra/Ollama SDK surface and the MCP server are untouched.

**Storage**: PostgreSQL (existing schema) — `datasets` gains `owner_id REFERENCES users(id)`; `goal_registry` gains `created_by REFERENCES users(id)`; new `dataset_shares (dataset_id, user_id, created_at, created_by)` with `PRIMARY KEY (dataset_id, user_id)` and `ON DELETE CASCADE` both ways (a removed account loses its shares automatically — FR-012). `meta_heuristic_embeddings` similarity search gains an access join through goal → dataset. Neo4j graph nodes (outcomes, causal graph, heuristics/triplets) remain keyed by `goal_id`; access is enforced at the orchestrator before any graph read, never in the graph itself. Object store (S3/MinIO) refs are reached only through dataset access at the handler.

**Testing**: Go unit tests with fakes (extend `internal/orchestrator` fakes with the new methods; `-race` on promotion/run-lifecycle surfaces per AGENTS.md); integration suite gated on `ARBORETRIET_INTEGRATION` and run via `make up` + `make test` (`-p 1`, shared single Postgres DB, post-suite cleanliness gate); web unit tests under `web/lib/` via `npm test` (vitest, no infrastructure). Integration tests register per-row teardown through `internal/testutil` (`RegisterDatasetCleanup` cascades share rows via the dataset FK).

**Target Platform**: Linux containers via compose (orchestrator, sandbox, sleepcycle-serve, verifier-serve, Postgres, Neo4j, MinIO, `web` standalone image); host-side local runs for smoke tests.

**Project Type**: Web-service (REST/server-sent-events Go orchestrator + Next.js frontend) with background worker services (Sleep-Cycle, Verifier).

**Performance Goals**: No new performance budget. Access predicates are primary-key/existence joins; list surfaces remain single queries. The heuristic similarity search keeps its hnsw + iterative-scan post-filter behavior, extended with an access join over goal → dataset; expected corpus scale (≤ tens of thousands of rows) keeps it exhaustive within `max_scan_tuples`.

**Constraints**:
- Security lives server-side; the session guard's `AnalystsFromContext` is the acting-user source for analyst routes; `/internal/*` (service-to-service) and boot reconciliation keep the stub/fallback identity and never surface user data.
- Existing-object leak policy: inaccessible single-object GETs answer the uniform 404 (same philosophy as the session guard's generic 401 — no existence oracle); a non-owner write to an accessible-but-not-owned dataset answers 403.
- Audit details keep the codebase-wide vocabulary (`dataset_id`, `user_id`, `optimization_function_id`, `data_source_ref`); new actions `dataset_share` / `dataset_unshare`.
- The shared single test database: never truncate shared tables; per-row teardown; `-p 1`; keep `make test` and the post-suite cleanliness gate green (new `dataset_shares` table added to `ResetTables`).

**Scale/Scope**: Small-team console; datasets in the low thousands, shares bounded by the account roster. No new service, no new project, no new dependency. Out of scope: group/org-wide sharing, transfer of ownership via the UI, per-column or per-object ACLs finer than the dataset.

## Constitution Check

*GATE: Must pass before Phase 0 research. Re-checked after Phase 1 design.*

The `.specify/memory/constitution.md` is an unfilled template; the governing constitution is `AGENTS.md`.

| Gate | Verdict | Basis |
|------|---------|-------|
| G1 Go toolchain & dependency discipline | **PASS** | No new Go dependency; pgx stays ≤ v5.7.x; web deps untouched (Next ≥15 unchanged). |
| G2 Tests need no infrastructure | **PASS** | Store/handler unit tests use fakes and `t.Setenv`; integration gated on `ARBORETRIET_INTEGRATION`. |
| G3 Shared-DB test hygiene | **PASS** | New fixtures registered per-row via `testutil` right after creation (never truncate `meta_heuristic_embeddings` or other tables); `RegisterDatasetCleanup` removes share rows by FK cascade; assertions are per-row, never table-global counts. |
| G4 Audit vocabulary | **PASS** | New audit events key with codebase-wide `dataset_id` / `user_id` / `optimization_function_id` spellings. |
| G5 Post-suite cleanliness gate | **PASS** | `dataset_shares` added to `ResetTables` (before `datasets`), so the gate and operator reset cover it. |
| G6 `make test` ordering/teardown | **PASS** | Where an `ORDER BY`-defaulted timestamp matters new tests `time.Sleep` a couple ms between inserts; teardown is LIFO-safe (goal/dataset cleanups retire dependents). |
| G7 `-race` on run-lifecycle doubles | **PASS** | Any run-lifecycle double extended for this feature is mutex-guarded and read via accessors; `go test -race ./internal/orchestrator` exercised when touching promotion/run lifecycle. |
| G8 web/lib unit coverage | **PASS** | Access/client rules (shared badge logic, owner-vs-collaborator derivations) live in `web/lib/` with vitest tests; no component tests are introduced. |

No gate violations; the Complexity Tracking table is not needed.

**Post-design re-check (after Phase 1):** re-evaluated against the concrete design. The design adds one table (`dataset_shares`, placed in `reset.go` FK-safe order before `datasets`), two nullable FK columns, no new dependency, no new service, and keyed audit events under the codebase vocabulary — G1, G2, G3, G5, G6, G8 continue to hold. G4 holds: `dataset_share`/`dataset_unshare` use `dataset_id`/`user_id` keys. G7 holds: stream re-checks live in the handler write-loop, not in the shared `Hub`, so hub doubles need no identity threading; run-lifecycle doubles extended for promotion/run tests remain mutex-guarded.

## Project Structure

### Documentation (this feature)

```text
.turbo/specs/006-dataset-ownership-sharing/
├── plan.md              # This file
├── research.md          # Phase 0: decisions + codebase findings
├── data-model.md        # Phase 1: entities, fields, access predicate
├── quickstart.md        # Phase 1: end-to-end validation scenarios
├── contracts/           # Phase 1: API + UI contract deltas
└── tasks.md             # Phase 2 (created by /speckit.tasks)
```

### Source Code (repository root)

The feature lands inside the existing Go module and `web/` app — no new projects, services, or packages of note beyond the store additions.

```text
internal/store/
├── migrations/0018_dataset_ownership.up.sql   # datasets.owner_id, goal_registry.created_by, dataset_shares, grants, backfill
├── migrations/0018_dataset_ownership.down.sql
├── datasets.go                                 # owner_id on Dataset; ListAccessible/GetAccessible/CanAccess; ShareStore (Share/Revoke/ListShares)
├── goalregistry.go                             # created_by on Goal; ListAccessible/ListByDatasetAccessible/GetAccessible
├── embeddingstore.go                           # user-scoped similarity search (goal->dataset access join)
└── reset.go                                    # dataset_shares added to ResetTables

internal/orchestrator/
├── server.go                                   # datasetStore/goalStore/heuristicsService seams gain access methods
├── datasets.go                                 # owner on create; access-filtered list/detail; shares handlers; 403/404 policy
├── submit.go                                   # bound-goal access check; implicit-mint ownership; goal created_by
├── goals.go                                    # goal-keyed delete access (own child or dataset owner)
├── heuristics.go                               # user-scoped search/trace; delete access via owning goal
├── stream.go                                   # per-frame access re-check (FR-008)
├── verification.go / causalgraph.go /
│   causalverify.go / corrections.go / chat.go /
│   sleepcycle.go / promote.go                  # goal access gate before serving
├── identity.go                                 # actingUser(r) helper over AnalystsFromContext
└── *_test.go                                   # extended fakes + access tests

internal/heuristics/                            # Query gains a userID scope param
internal/sleepcycle/                            # grounding corpus read scoped to goal's dataset + system-owned (no user-data leak)
internal/testutil/                              # cleanup helpers updated for the new table if needed

web/lib/
├── orchestrator.ts                             # DatasetSummary.access, share/revoke/listShares client, owner derivations
├── datasets.ts (+ datasets.test.ts)
└── (existing vitest suites)

web/components/
├── DatasetsList.tsx                            # "shared with you" row marker
├── DatasetDetail.tsx                           # owner-only Metadata/Danger/share controls; collaborator read-only + share panel for owner
└── (no new routes; existing views re-render filtered data)
```

**Structure Decision**: The single-project layout with a Go module + `web/` deployable is retained; the feature threads through existing store/orchestrator/web seams, adding migration-worthy schema (one migration), user-scoped store methods, handler-level access gates, and web-client DTO/UI deltas. This matches the established convention (consumer-side narrow interfaces in `server.go`, DTOs separate from store structs, `reset.go` table ordering).

## Complexity Tracking

> Not filled: the Constitution Check passes with no violations to justify.