# Data Model: Clean Test Data

Phase 1 output for `.turbo/specs/004-clean-test-data/spec.md`. Documents every
store the feature must wipe, every relationship that constrains wipe order, and
the entities the test-hygiene contract must keep residue-free.

## Stores

The feature touches three stores, matching the spec's "every storage layer":

| Store | Technology | Holds | Wipe surface |
|-------|------------|-------|--------------|
| Record store | Postgres (+ pgvector) | datasets, objectives, runs, verifications, audit, accounts | `store.ResetAll` (owner) |
| Graph | Neo4j | State/Intervention/Outcome/MetaHeuristic/DataColumn/CausalGraphMeta nodes + edges | `graph.Wipe` |
| Object store | MinIO / S3 | data-source objects | `objectstore.Wipe` |

## Record store

### Tables and relationships

| Table | Key fields | References (FK) | Referenced by |
|-------|-----------|------------------|---------------|
| `datasets` | `id`, `name` (unique lower), `status` (active/archived), `datasource_ref` | — (ref is a text ledger key, no FK) | `goal_registry.dataset_id` |
| `goal_registry` | `optimization_function_id`, `goal_text`, `evaluation_matrix`/`target_fields`, `datasource_ref`, `dataset_id` | `dataset_id → datasets.id` (NO ACTION) | `runs`, `verification_queue`, `causal_verifications` |
| `data_source_registry` | `ref` (PK) | — | `datasets.datasource_ref`, `goal_registry.datasource_ref` (logical, no FK) |
| `runs` | `run_id`, `optimization_function_id`, `status`, `failure_reason` | `optimization_function_id → goal_registry` (NO ACTION) | — |
| `verification_queue` | `queue_id`, `optimization_function_id`, `outcome_id`, `status` | `optimization_function_id → goal_registry` (NO ACTION) | — |
| `causal_verifications` | `id`, `goal_id`, `status` | `goal_id → goal_registry` (NO ACTION) | — |
| `meta_heuristic_embeddings` | `node_id`, `goal_id` (nullable), `embedding` | — (node id is an app-assigned UUID, no FK) | — |
| `audit_log` | `id`, `action`, `event_type`, `detail` (jsonb) | — | — (detail carries `optimization_function_id`, `data_source_ref`, `dataset_id` per the shared vocabulary) |
| `sessions` | `id`, `user_id`, `token_hash` | `user_id → users.id` (CASCADE) | — |
| `users` | `id`, `username` (unique lower), `role`, `active` | — | `sessions` |

### Wipe order (children before parents)

1. `meta_heuristic_embeddings` — no FK; removed first so goal-scoped rows go
   with their goals later.
2. `runs`, `verification_queue`, `causal_verifications` — NO ACTION refs to
   `goal_registry`; must go before the goal row.
3. `goal_registry` — NO ACTION ref from `datasets` via `dataset_id`; must go
   before the dataset row.
4. `datasets` — before `data_source_registry` (logical pairing, no FK).
5. `data_source_registry`.
6. `sessions` (CASCADE to users would remove it anyway; explicit is clearer).
7. `users`.
8. `audit_log` — last; wiped like everything else in the dev/test sandbox.

All in one transaction (`store.ResetAll`), so a mid-wipe failure rolls back to a
fully-populated prior state rather than a half-wiped one.

### Integrity guarantees that already exist and are reused

- `GoalRegistry.Delete` guards on `runs.status='running'` and pending
  verifications — irrelevant to the reset (owner DELETE ignores the guard) but
  still the contract for the per-goal HTTP delete.
- `datasets.name` and `users.username` unique-case-insensitive indexes make
  fixed fixture names unsafe on the shared DB; this is why fixtures must keep
  `testutil.NewID`-suffixed names.

## Graph store

- Labels: `State`, `Intervention`, `Outcome`, `MetaHeuristic`, `DataColumn`,
  `CausalGraphMeta`; each has a uniqueness constraint on `id`.
- Every goal-owned node carries `goal_id` (MetaHeuristic additionally
  `origin_goal_id`). Goal-less nodes exist in legacy fixtures, so the wipe is a
  whole-graph `MATCH (n) DETACH DELETE n`.
- Per-test cleanup uses `DeleteNode(ctx, id)` so a test removes only its own
  nodes.

## Object store

- One bucket (`S3_BUCKET`); keys are path-like (`datasources/<ref>/<file>` for
  real objects, `test/...` for fixtures).
- Wipe lists all keys and deletes them in batches; idempotent on an empty
  bucket.

## State transitions relevant to cleanup

- Dataset: `active` ↔ `archived` (user-managed); wipe removes both.
- Run: `running` → `completed|failed`; the reset removes rows in any state —
  the guarded per-goal delete is not reused for the wipe.
- No new state is introduced by this feature; the wipe is not a lifecycle
  transition, it is a teardown.