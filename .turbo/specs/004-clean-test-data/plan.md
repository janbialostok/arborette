# Implementation Plan: Clean Test Data

**Branch**: `004-clean-test-data` | **Date**: 2026-08-14 | **Spec**: [spec.md](./spec.md)

**Input**: Feature specification from `.turbo/specs/004-clean-test-data/spec.md`

## Summary

Testing has left the shared dev stack cluttered: datasets, objectives, runs,
verifications, audit rows, graph nodes, and stored objects accumulate with every
`make test` and are never removed. This feature (1) gives the operator a
repeatable, idempotent wipe that empties every store, and (2) makes every
integration test remove exactly what it creates — backed by a post-suite
cleanliness gate that fails `make test` and names any residue.

Technical approach (from research.md): a one-shot `cmd/cleanup` binary (the
`cmd/migrate` precedent) with `clean` and `check` modes, using the owner Postgres
role so no runtime-role grants need widening; a `store.ResetAll` transactional
FK-ordered delete; a whole-graph `graph.Wipe`; an `objectstore.Wipe`; a set of
`testutil` `t.Cleanup` helpers that replace the environment-destructive
`TruncateEmbeddings`; and `make clean-data` + a `make test` that ends with
`cleanup check`.

## Technical Context

**Language/Version**: Go 1.24 (pinned via `GOTOOLCHAIN`); no toolchain bump, no
new module dependencies. Existing deps reused (pgx ≤ v5.7, neo4j-go-driver/v5,
aws-sdk-go-v2 S3).

**Primary Dependencies**: internal `store`, `graph`, `objectstore`, `config`,
`testutil` packages; `github.com/jackc/pgx/v5` (owner pool), the Neo4j driver,
and the S3 client for the three wipe surfaces.

**Storage**: PostgreSQL 16 + pgvector (record store: `datasets`, `goal_registry`,
`data_source_registry`, `runs`, `verification_queue`, `causal_verifications`,
`meta_heuristic_embeddings`, `audit_log`, `users`, `sessions`); Neo4j (graph);
MinIO/S3 (object store).

**Testing**: `go test ./...` (unit, gated cleanly off without infra),
`make test` (integration: `ARBORETTE_INTEGRATION=1 go test -p 1 ./...` with
localhost overrides), new `cmd/cleanup check` gate appended to `make test`.

**Target Platform**: Linux containers (compose services) + macOS host dev;
one-shot binaries built with `go build ./...`.

**Project Type**: Multi-service Go backend (HTTP services + one-shot CLI jobs)
with a separate Next.js web module. This feature changes backend/internal
tooling and tests only.

**Performance Goals**: `cleanup clean` and `cleanup check` complete in under a
few seconds at dev scale (full wipe of a repository with thousands of rows);
test-hygiene adds ~zero overhead because cleanup is `t.Cleanup` deletes of rows a
test already touches.

**Constraints**: Go 1.24 pinned (pgx must stay ≤ v5.7; no new heavy deps);
least privilege — destructive paths run as owner, no new DELETE grants, and the
"DELETE only, no TRUNCATE anywhere" grant discipline is preserved; `-p 1` for
the integration suite; tests never assert table-global counts (per-row only); no
commit until instructed.

**Scale/Scope**: ~10 Postgres tables, 1 Neo4j graph, 1 object-store bucket, and
every integration-gated test that writes to a persistent store across
`internal/{store,graph,objectstore,heuristics,orchestrator,mcpserver,sandbox}`.

## Constitution Check

*GATE: Must pass before Phase 0 research. Re-checked after Phase 1 design.*

Derived from AGENTS.md (the governing constitution):

- **G1 — Integration gating**: all new behavior stays behind
  `ARBORETTE_INTEGRATION`/Make target so `go build`, `go vet`, `go test ./...`
  remain green without infrastructure. The `cmd/cleanup` integration tests and
  the `make test` gate both follow this. **PASS**.
- **G2 — Shared-DB discipline**: the suite shares one Postgres, so it keeps
  `-p 1`, per-row assertions, and across-`now()` ordering sleeps; the gate is a
  post-suite command, the one legitimate whole-table check. **PASS**.
- **G3 — Least privilege / grant discipline**: cleanup runs as the owner role;
  runtime roles get no new grants; `TruncateEmbeddings` (which bypassed the
  discipline by truncating a shared table) is replaced by per-row owner deletes.
  No TRUNCATE anywhere new. **PASS**.
- **G4 — No environment-destructive side effects**: removing the embeddings
  truncation repairs the AGENTS.md-noted divergence (stale embeddings vs
  embedding-less nodes) rather than worsening it. **PASS**.
- **G5 — Planning artifacts**: the plan/spec stay in `.turbo/specs/`; this
  commit-family will carry tasks completed against `.turbo/plans/` when
  instructed to commit. **PASS**.
- **G6 — Race discipline**: orchestration fixtures touched by the feature keep
  their mutex-guarded doubles; `go test -race ./internal/orchestrator` remains
  the relevant check where promotion/run-lifecycle doubles are involved.
  **PASS** — no new shared-goroutine state is introduced.

No violations; Complexity Tracking is not required.

## Project Structure

### Documentation (this feature)

```text
.turbo/specs/004-clean-test-data/
├── plan.md              # This file (/speckit.plan command output)
├── research.md          # Phase 0 output
├── data-model.md        # Phase 1 output
├── quickstart.md        # Phase 1 output
├── contracts/           # Phase 1 output (cleanup-cli.md, makefile.md, test-hygiene.md)
├── checklists/          # Phase 0 output (requirements.md)
└── tasks.md             # Phase 2 output (/speckit.tasks command - NOT created by /speckit.plan)
```

### Source Code (repository root)

```text
cmd/cleanup/                  # NEW: operator wipe + gate binary
└── main.go                   # args: clean|check; wires config, store, graph, objectstore

internal/
├── store/
│   ├── reset.go              # NEW: ResetAll (owner tx, FK-safe order) + EmptyCounts reads
│   └── *_integration_test.go # hygiene: t.Cleanup for every seeded row
├── graph/
│   ├── neo4j.go              # += Wipe(ctx) (DETACH DELETE all); DeleteNode(ctx, id)
│   └── *_test.go             # hygiene: register node cleanup for every seed helper
├── objectstore/
│   ├── client.go             # += Wipe(ctx) (list+batch delete); ListKeys(ctx)
│   └── *_test.go             # hygiene: delete put keys
├── testutil/
│   └── testutil.go           # += Register*Cleanup helpers; drop TruncateEmbeddings
└── {heuristics,mcpserver,sandbox,orchestrator}/
    └── *_integration_test.go # hygiene: fixtures cleaned via testutil helpers

Makefile                      # += clean-data target; test target appends the gate
```

**Structure Decision**: follows the repository's existing layout — one-shot
operator binaries live in `cmd/` (the `migrate`/`dbbootstrap` precedent), store
surfaces live in the owning `internal/` packages, and shared test machinery
lives in `internal/testutil`. No new top-level directories; the web module is
untouched because the interface lists read straight from the record store (a
store cleanup empties the UI).

## Complexity Tracking

> Not required — the Constitution Check passed with no violations that need
> justification.

## Validation (T030, 2026-08-14)

All quickstart scenarios run against the live compose stack and pass:

- **Scenario 1 (SC-001/SC-002, tidy wipe)**: `make clean-data` on a stack
  holding a probe dataset through `go run ./cmd/cleanup clean` removed the 1
  row and reported it per store; `cleanup check` then reported
  `clean (all tables, graph, and object store empty)` and exited 0; a second
  run of the scheme (wipe + check) is a no-op. Idempotence and the accidental-
  residue wipe path are both demonstrated.
- **Scenario 2 (SC-003/SC-005, reproducible end state)**: `make test` was run
  three times from a clean state; every run ended with the `check` gate green
  and the identical end-state (all counts zero). The pre-existing
  graph/embedding residue hypothesis is retired: the gate leaves the stores as
  clean as `make clean-data` does.
- **Scenario 3 (SC-004/FR-008, gate catches a regression)**: seeded one
  `datasets` row behind the suite's back; `cleanup check` named it
  (`residue found: datasets: 1 rows`) and exited 1; `make clean-data` removed
  it and the gate went green again.
- **Scenario 4 (FR-009, scoped host run)**: `go test -p 1` over the store,
  graph, heuristics, mcpserver, sandbox and objectstore packages with the
  `HOST_ENV` localhost overrides passed, and a manual `cleanup check` after
  the scoped run confirmed `clean`.
- **T029 validation**: `go build ./...`, `go vet ./...`, bare `go test ./...`
  (no infra), and `go test -race ./internal/orchestrator` are all green.

Implementation notes that corrected the plan along the way: `RegisterDatasetCleanup`
clears a dataset's bound goals (runs/verifications first, then the goal rows) before
deleting the dataset, because `t.Cleanup` is LIFO and a goal's cleanup is not
guaranteed to have run first; `RegisterGoalGraphCleanup` was added for
goal-scoped graph fixtures (the causal `DataColumn`/`CausalGraphMeta` corpus) via
the repo's existing `DeleteGoalGraph`; and the audit-boundary test reads its
appended row's id through an owner connection, since the orchestrator role has
no SELECT on `audit_log` either.