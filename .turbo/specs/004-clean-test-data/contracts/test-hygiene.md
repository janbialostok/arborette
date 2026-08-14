# Contract: test-hygiene helpers and invariants

The shared machinery (spec FR-005..007, FR-009) that makes every integration
test remove exactly what it created. Lives in `internal/testutil`, the package
integration tests already use for gating, setup, and id generation.

## Helpers (all registered via `t.Cleanup`, so they run on pass, fail, and panic)

| Helper | Removes | Mechanism |
|--------|---------|-----------|
| `RegisterDatasetCleanup(ctx, cfg, id)` | one `datasets` row | owner DELETE by `id` |
| `RegisterGoalCleanup(ctx, cfg, id)` | one `goal_registry` row | owner DELETE by id |
| `RegisterEmbeddingCleanup(ctx, cfg, nodeID)` | one embedding row | owner DELETE by `node_id` |
| `RegisterUserCleanup(ctx, cfg, id)` | one `users` row (sessions CASCADE) | owner DELETE by id |
| `RegisterGraphNodeCleanup(ctx, cfg, id)` | one graph node + its edges | `graph.DeleteNode` by `id` |
| `RegisterObjectCleanup(ctx, cfg, key)` | one object-store key | `objectstore` DELETE by key |

- Each helper is idempotent: if the fixture is already gone (e.g. a test
  exercised a delete path), the cleanup succeeds quietly.
- Callers register the helper immediately after creating the fixture, before any
  assertion, so a mid-test failure still triggers it.

## Invariants

1. **A test removes only its own fixtures.** No table-global truncation, no
   sweeping another test's rows. This replaces `testutil.TruncateEmbeddings`
   (which today destroys every real Meta-Heuristic embedding on the shared
   stack); embedding tests now delete the rows they inserted.
2. **Fixtures are uniquely attributable.** Dataset names/refs and goal texts keep
   their `testutil.NewID(t)` suffix and gain a `fixture-` prefix where
   user-visible. Graph fixtures — including the stable meta-heuristic ids graph
   tests reference across files — are all registered for node cleanup by id, so
   a run ends with a zero-node graph.
3. **Every integration-gated test that writes to a persistent store uses the
   helpers.** A test that creates a dataset, objective, graph node, embedding,
   account, session, object, run, verification, or audit row must clean it. The
   gate (see `cleanup-cli.md`) is the enforcement backstop, so an omission fails
   the whole `make test` with the offending table and ids named.
4. **Deletion order inside the helpers mirrors production.** A dataset is never
   removed while its objectives still reference it; the goal cleanup runs first.
   Per-test cleanup is idempotent against the store's existing delete guards
   because it runs as owner.
5. **No new runtime grants.** Cleanup helpers run over the owner DSN (the only
   role with DELETE everywhere), so least privilege is preserved and the
   `make test` stack needs no config change beyond what `HOST_ENV` already sets.

## Attribute-ability (FR-006)

With the gate's zero-tolerance on residue, `cleanup check` names the offending
table and a sample of ids when it fails; the `fixture-` prefix plus the fresh
UUID suffix tells an operator which run and which test left the row.