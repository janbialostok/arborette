# Quickstart: Clean Test Data

Validation guide for `.turbo/specs/004-clean-test-data/spec.md`. Each scenario
proves one success criterion end to end against a live stack. See
`contracts/` and `data-model.md` for the contracts and entities referenced here.

## Prerequisites

- A running stack: `make up` (Postgres, Neo4j, MinIO, orchestrator, web).
- `make test` reaches localhost services via the `HOST_ENV` overrides baked into
  the target; a scoped `go test ./internal/<pkg>/` from the host additionally
  needs them (especially `S3_ENDPOINT=http://localhost:9000`).

## Scenario 1 — Clean out the existing clutter (SC-001, SC-002)

```text
make up
make clean-data
make clean-data    # idempotence: second run must succeed and change nothing
```

**Expected** (SC-002): both invocations exit 0. After the runs:

- `cleanup check` reports all counts zero.
- The Datasets and Objectives lists in the web UI render empty (SC-001) — the
  interface reads those lists straight from the record store.
- Neo4j has zero nodes; the object store has zero keys.

## Scenario 2 — Tests leave no trace, reproducible end state (SC-003, SC-005)

```text
make clean-data
make test
cleanup check      # after run 1 -> clean, and check exits 0
make test
cleanup check      # after run 2 -> identical clean state
```

**Expected** (SC-003): the post-run end state is identical across the two runs —
`cleanup check` passes both times, with no accumulation in any table, in the
graph, or in the object store. This also verifies SC-005 indirectly: any test
without cleanup would leave the rows the gate names.

## Scenario 3 — The gate catches a regression (SC-004, FR-008)

1. Temporarily add a leaking test that creates a dataset (or a graph node) and
   never cleans it up.
2. `make test`.
3. **Expected**: the suite runs, then `cleanup check` fails with a non-zero exit
   naming the offending table and the sample ids.
4. Remove the defect; `make test` passes again.

## Scenario 4 — Scoped package run stays green (FR-009, AGENTS.md host rules)

```text
set -a; . ./.env; set +a
S3_ENDPOINT=http://localhost:9000 ARBORETTE_INTEGRATION=1 \
  go test -p 1 ./internal/store ./internal/graph
cleanup check
```

**Expected**: the packages pass with `-p 1`, and the manual gate confirms they
left the stores clean. (Scoped runs do **not** run the Make target's automatic
gate; call `cleanup check` yourself.)

## What is not covered here

- Implementation details and per-file tasks: `tasks.md` (created by
  `/speckit.tasks`).
- Runs against an unclean stack: `cleanup check` failing after `make test`
  before the first `make clean-data` is expected — the gate asserts the clean
  end-state the spec defines, and Story 1 is the prerequisite.