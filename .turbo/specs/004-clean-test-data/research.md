# Research: Clean Test Data

Phase 0 output for `.turbo/specs/004-clean-test-data/spec.md`. Resolves every
open question from the technical context with a decision, the rationale, and the
alternatives rejected.

## D1: Cleanup mechanism

- **Decision**: A new one-shot CLI `cmd/cleanup` with two modes — `clean` (wipe)
  and `check` (verify cleanliness). Driven by new Makefile targets: `make
  clean-data` for the operator wipe; `make test` appends `cleanup check` as the
  post-suite gate.
- **Rationale**: Matches the established one-shot binary pattern (`cmd/migrate`,
  `cmd/dbbootstrap`: `config.Load()` → `context.Background()` → run → exit). A
  standalone binary can connect as the **owner** role, which is the only role
  with DELETE on every table (the runtime roles hold no DELETE on `audit_log`
  and the service role holds no DELETE on `users`/`sessions`). It is repeatable
  and idempotent, operator-invoked, and testable with the same integration gate.
- **Alternatives considered**:
  - *HTTP admin endpoint on the orchestrator* — the orchestrator's runtime role
    cannot delete audit rows, so the full-reset semantics would need a grant
    widening (violates least privilege) or a back-channel owner connection; and
    it puts a data-wipe behind a network-visible surface. Rejected.
  - *Raw SQL / psql script* — untestable, no graph or object-store coverage, and
    a script pointed at the wrong DSN is a bigger footgun than a config-loaded
    binary. Rejected; the binary loads the same `.env` the rest of the stack
    uses.

## D2: Postgres wipe

- **Decision**: A new `store.ResetAll(ctx, pool)` over an owner pool that deletes
  rows in FK-safe order inside one transaction: `meta_heuristic_embeddings` →
  `runs` → `verification_queue` → `causal_verifications` → `goal_registry` →
  `datasets` → `data_source_registry` → `sessions` → `users` → `audit_log`.
  Deleting already-empty tables is a no-op, so a second run on a clean database
  succeeds and changes nothing (FR-003).
- **Rationale**: A single transactional reset is atomic, idempotent, and
  dependency-ordered (children before parents, per the NO ACTION FKs). Reusing
  `store.Pool` keeps connection semantics identical to every other store path and
  makes the wipe integration-testable. The owner role needs no new grants; the
  least-privilege surface of the runtime roles stays untouched (FR-004).
- **Alternatives considered**:
  - *Loop the existing `GoalRegistry.Delete` / `DatasetStore.Delete`* — those are
    guarded by `ErrGoalRunning`/`ErrVerificationPending`; a reset must remove
    rows in any lifecycle state, and per-entity loops are non-atomic. Rejected.
  - *One `TRUNCATE ... CASCADE` statement as owner* — works, but it bypasses the
    codebase's explicit DELETE-grant design and gives no ordering control; the
    embedded DELETE statements are reviewed the same way migrations are.
    Rejected.

## D3: Graph wipe

- **Decision**: Add `graph.Neo4jRepository.Wipe(ctx)` = `MATCH (n) DETACH DELETE
  n` (the entire graph), plus `DeleteNode(ctx, id)` =
  `MATCH (n {id:$id}) DETACH DELETE n` for per-test fixture cleanup.
- **Rationale**: Today's residue is not fully goal-scoped — several suites seed
  goal-less triplets (`graph_test.go`'s `seedTriplet`, the heuristic/embedding
  fixtures with an empty goal) that `DeleteGoalGraph` can never reach. A full
  wipe matches the spec's assumption that the shared graph holds only test
  artifacts. `DeleteNode` is the minimal tool for individual tests to remove
  exactly what they created (FR-005/009).
- **Alternatives considered**:
  - *Goal-scoped deletes only* — orphans the large legacy goal-less fixture
    population, so the graph would never actually be clean. Rejected.
  - *Drop constraints and recreate* — no benefit over DETACH DELETE and risks the
    per-label uniqueness constraints. Rejected.

## D4: Object-store wipe

- **Decision**: Add `objectstore.Client.Wipe(ctx)` that lists every key in the
  bucket (`ListObjectsV2` in pages) and deletes them in `DeleteObjects` batches,
  idempotent on an empty bucket. Also expose a `ListKeys(ctx)` helper for the
  check mode and for per-test attribution.
- **Rationale**: The object store holds test objects (see `objectstore_test.go`
  and sandbox test fixtures), and the spec requires stored data artifacts to go
  with their datasets (FR-002/009). Listing-and-deleting is idempotent and
  never touches the bucket itself, so concurrently bootstrapping services are
  unaffected.
- **Alternatives considered**:
  - *Delete and recreate the bucket* — races `EnsureBucket` bootstrapping on
    other services and would make the command non-idempotent under a live
    stack. Rejected.

## D5: Cleanliness gate

- **Decision**: `cleanup check` is the gate. It reports the row count per PG
  table plus the graph node count and object-store key count; any non-zero entry
  prints the offending table and a sample of ids and exits non-zero. `make test`
  runs `go test -p 1 ./...` and then `cleanup check` as the last step (always
  under `ARBORETTE_INTEGRATION=1`, against the live stack).
- **Rationale**: A post-suite command guarantees **ordering** — a Go test package
  named `internal/cleanliness` would sort near the front of `go test ./...` and
  run before most suites, so it cannot serve as the gate. The check shares the
  same binary and owner DSN as the wipe, needs no new grants, and names leftover
  offenders (FR-008). An empty environment — the state after `make clean-data` —
  passes, which is the spec's FR-010 requirement.
- **Alternatives considered**:
  - *A dedicated integration test package* — cannot be last in `go test ./...`;
    also one binary with two modes is simpler than two entry points. Rejected.
  - *Makefile `psql -c` counting* — fragile quoting of the owner DSN, and no
    graph or object-store coverage. Rejected.

## D6: Test-hygiene harness

- **Decision**: Extend `internal/testutil` with per-row cleanup helpers, each
  registered via `t.Cleanup` and idempotent, all owner-/node-id based:
  `RegisterDatasetCleanup`, `RegisterGoalCleanup`, `RegisterEmbeddingCleanup`,
  `RegisterUserCleanup`, `RegisterGraphNodeCleanup`, `RegisterObjectCleanup`.
  Remove `TruncateEmbeddings` and its call sites; the embedding tests delete only
  the rows they create.
- **Rationale**: Meets FR-005 (a test removes exactly what it created, whether it
  passes, fails, or panics — `t.Cleanup` runs on all three) and FR-007 (the end
  state is identical across consecutive runs). Relationships between helpers and
  their registration order mean datasets are deleted after their goals, so the
  existing store integrity redirects funnel through the same helpers. Dropping
  `TruncateEmbeddings` also repairs the AGENTS.md-noted side effect where
  `make test` destroyed every real Meta-Heuristic embedding on the shared stack.
- **Alternatives considered**:
  - *Suite-level truncation between packages* — still destroys shared state,
    violates "the test cleans up after itself", and needs an ordering seam that
    does not exist today. Rejected.

## D7: Fixture attributability

- **Decision**: Keep the existing `testutil.NewID(t)`-suffixed fixture names and
  refs (already collision-free on the shared DB), standardize a `fixture-`
  prefix on user-visible dataset names and goal texts so residue is instantly
  recognizable, and register **all** graph fixtures — including the load-bearing
  stable ids like the meta-heuristic ontology fixtures — for per-run node
  deletion rather than renaming them.
- **Rationale**: FR-006 requires attribution. With the gate's zero-tolerance on
  residue, attribution is a debugging aid (which test most recently touched a
  table) rather than a sharding key; renaming stable fixture ids that graph
  tests reference across files would churn assertions for no behavioral gain.
- **Alternatives considered**:
  - *Rename every stable fixture id* — unnecessary churn; the stable ids are
    only memorable because tests reference them, and cleanup removes them anyway.
    Rejected.

## D8: Reproducibility constraints

- **Decision**: Preserve the existing discipline that gate the suite's
  reproducibility rather than replacing it: `-p 1` (the shared Postgres has no
  per-test isolation), per-row assertions instead of table-global counts, and
  `time.Sleep` between `now()`-ordered inserts. The gate is executed by a
  separate program after the suite, so it is the one place a whole-table count is
  legitimate.
- **Rationale**: AGENTS.md calls these out as the invariants that keep the shared
  database usable; the feature removes accumulation but must not pretend the
  underlying shared-DB semantics changed. Embedding-observation caveats
  (truncate-then-compare) disappear once no test truncates.
- **Alternatives considered**: per-test databases or schemas — a much larger
  change (advisory locks, cross-package sharing, migrations per schema) that the
  feature does not need. Rejected for this feature; noted as future work.

## Consolidated assumptions carried into design

- Postgres wipe, graph wipe, and object-store wipe are only ever run against the
  shared dev/test stack; no production guard is required beyond the config-loaded
  DSN the binary already uses.
- `audit_log` is wiped with the rest: the environment is a dev/test sandbox, the
  wipe log is the operator's record, and stale audit rows referencing removed
  goals/datasets would otherwise clutter ops queries.
- New runtime grants are unnecessary; every destructive path in this feature runs
  as owner.