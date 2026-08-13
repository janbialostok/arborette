# Research Notes — Dataset CRUD & Hierarchy Management

Resolves the unknowns in `plan.md` Technical Context. Decisions below are grounded in
the existing codebase (migrations `0001–0014`, orchestrator REST surface, store layer,
Neo4j graph, object store, and the `web/` App-Router BFF).

## R1 — First-class dataset storage

**Decision**: New `datasets` Postgres table (next migration `0015`) as the managed
container, and a new nullable-then-backfilled `goal_registry.dataset_id` column for the
objective → dataset parent link. `data_source_registry` remains the raw ref ledger the
sandbox validator reads.

**Rationale**: `data_source_registry` is a plain set of refs (`ref` PK + `created_at`,
migration `0010`) with no lifecycle state; the sandbox gates every request through
`validateRef` → `DataSourceRefExists`. A managed dataset needs name, description, status,
and update/delete semantics that do not belong in that ledger. Goals already carry a
`datasource_ref` text column with no FK anywhere; adding a real `dataset_id` FK gives the
hierarchy an enforceable home.

**Alternatives considered**:
- *Extend `data_source_registry` with name/status/description* — rejected: it conflates the
  raw ingestion ledger (whose contract the sandbox validator depends on) with user-managed
  metadata, and refs can be shared by multiple goals/datasets.
- *Derive datasets virtually from distinct refs* — rejected: no storage for lifecycle state
  (archived status, description), and delete accounting needs a durable row.

## R2 — Dataset ↔ data-source cardinality

**Decision**: A dataset holds exactly one `datasource_ref`; the binding is fixed at
creation and never editable (spec clarification: metadata-only edits). Multiple datasets may
reference the same ref; the UI surfaces the duplication but permits it (spec Edge Case).

**Rationale**: Matches the confirmed clarification and the existing immutable
`datasource_ref` contract (V2 spec R6: "a goal's data source is immutable after
registration").

## R3 — Objective delete propagation

**Decision**: App-level transactional delete in the store that removes the child rows
explicitly, plus app-level cleanup of the graph, embeddings, and object store around it.
Add the missing DELETE grants in a new migration. Refuse deletion while a hypothesis run is
`running` or a verification is pending/in-flight.

**Rationale**: Three tables reference `optimization_function_id` with **NO ACTION** FKs —
`runs` (0006), `verification_queue` (0009), `causal_verifications` (0013) — so a plain goal
`DELETE` would fail. The store currently exposes no delete; grants give the orchestrator no
DELETE on any of these tables (0005:12, 0006:18, 0009:30, only `meta_heuristic_embeddings`
DELETE exists via 0008). Because the side effects span Postgres, Neo4j, pgvector, and the
object store, a single SQL `ON DELETE CASCADE` migration would hide a multi-store operation;
explicit ordering keeps each store's failure mode visible and auditable.

**Alternatives considered**:
- *`ON DELETE CASCADE` migration on the three FKs* — rejected: covers only Postgres rows;
  the graph (`State`/`Intervention`/`Outcome`/`MetaHeuristic`/`CausalGraphMeta`/`DataColumn`
  nodes keyed by `goal_id`), embeddings (`meta_heuristic_embeddings.goal_id`), and the object
  store object still need app-level handling, and cascade silently drops rows that the audit
  vocabulary wants surfaced.
- *Soft-delete (status = deleted)* — rejected: the spec's lifecycle is full CRUD with real
  removal (US6), and the system already has no tombstone pattern for goals.

## R4 — Heuristic removal

**Decision**: New graph method `DeleteMetaHeuristic(id)` using `DETACH DELETE`
(`MATCH (m:MetaHeuristic {id:$id}) DETACH DELETE m` — the node carries inbound/outbound
`ABSTRACTED_FROM` edges), reusing the existing embeddings `Delete(nodeID)` store method
(DELETE grant already exists, 0008). Wired through a new `DELETE /heuristics/{id}` route.

**Rationale**: The driver (neo4j-go-driver/v5 v5.28.4) already runs `DETACH DELETE` today
(`causal.go:560–569`); the writeOp harness is reusable. No graph delete-by-goal exists
today; MetaHeuristic delete is the smallest correct addition, and embeddings orphan
cleanup already runs (`internal/heuristics/service.go retireOrphans`).

## R5 — Object store deletion

**Decision**: Add `Delete(ctx, key)` to `internal/objectstore/client.go` (wraps the
aws-sdk-go-v2 s3 `DeleteObject`) and to the orchestrator's `objectStore` consumer interface
(`server.go:144`), plus the test fakes. Delete the `datasources/<uuid>/<filename>` object
only when the dataset (and any other dataset/goal referencing the same ref) is gone.

**Rationale**: The client has `NewKey`/`Put`/`Get`/`IsNotFound`/`EnsureBucket` but no
delete (research §5). Refcount-aware deletion is required because refs may be shared and
`RegisterDataSourceRef` is idempotent (`ON CONFLICT DO NOTHING`).

## R6 — Objective registration under a dataset

**Decision**: `POST /goals` gains an optional `dataset_id`. When provided, the goal binds to
the dataset's `datasource_ref` (no re-upload). When absent (legacy web submit, MCP
`submit_analyst_goal`), the current ingest path runs and an implicit dataset is created
around the new ref, preserving the invariant "no objective without a dataset". The web form
offers "choose existing dataset" or "create new dataset" (name + upload), so the UI always
has a parent.

**Rationale**: FR-007 (mandatory parent) and the "no orphans" invariant must hold without
breaking the MCP client or existing submit contract. Additive optional param is the least
disruptive path; the UI enforces the choice.

## R7 — Legacy reconciliation

**Decision**: Backfill migration creates one dataset per distinct `datasource_ref` among
pre-existing goals (named from the ref's base filename), assigns each goal's `dataset_id`,
then applies `NOT NULL`. An audit event `dataset_reconcile` records how many datasets were
created and how many goals were assigned (spec FR-010 "reporting the reconciliation for
review").

**Rationale**: `data_source_registry` was backfilled the same way in migration 0010; a goal
with no `datasource_ref` cannot exist (`NOT NULL`). Naming from the ref keeps datasets
recognizable.

## R8 — Web UI seams

**Decision**: 
- `AppNav.tsx` `LINKS` array gains `{href: "/datasets", label: "Datasets"}` (header link
  confirmed in clarification).
- New pages `/datasets` (list + create) and `/datasets/[id]` (detail/edit/delete + its
  objectives), following the existing `web/app/goals/page.tsx` + `[id]/page.tsx` pattern
  (async page with `params: Promise<{id}>`).
- BFF proxy routes `web/app/api/orchestrator/datasets/route.ts` (GET+POST) and
  `datasets/[id]/route.ts` (GET+PATCH+DELETE), each `export const runtime = "nodejs"` and
  `dynamic = "force-dynamic"`; DELETE/PATCH ride the existing `forward` helper
  (`web/lib/proxy.ts`).
- Typed client `web/lib/orchestrator.ts` gains `listDatasets`, `createDataset`,
  `getDataset`, `updateDataset`, `deleteDataset`, and a `dataset_id` filter on `listGoals`.
- `GoalForm` gains a dataset selector; heuristic removal surfaces in the objective detail /
  heuristic browser.

**Rationale**: There is no reusable table primitive; `ObjectivesList`'s hand-rolled row
pattern is the model. The BFF proxy mirror is one-for-one (every backend route has a thin
route.ts), and `web/lib/` is where the vitest suites live.

## R9 — Grants & audit vocabulary

**Decision**: New migration extends grants additively: orchestrator gets
`SELECT, INSERT, UPDATE, DELETE ON datasets`, plus `DELETE ON goal_registry`, `runs`,
`verification_queue`, `causal_verifications`, `data_source_registry`. Audit events use the
codebase vocabulary: `dataset_create`, `dataset_update`, `dataset_delete`,
`objective_delete`, `heuristic_remove`, `dataset_reconcile`, keyed with `dataset_id`,
`dataset_name`, `optimization_function_id`, `data_source_ref`.

**Rationale**: AGENTS.md mandates the audit `detail` vocabulary and the migration+grants
pattern (every write migration ships its grants). Deletion surfaces are new to the system,
so new events are required; keys reuse existing names so dashboards stay consistent.

## R10 — Concurrency guards

**Decision**: Objective delete is refused while a `running` run row or a pending/in-flight
verification exists; dataset delete is refused while objectives remain (409 listing them).
Unique-name conflicts on create/update return 409.

**Rationale**: The SSE loop, verifier, and sleep-cycle worker read goals by id
(`cmd/sleepcycle/main.go:35`, `cmd/verifier/main.go:33`); serializing delete against live
runs avoids orphaning in-flight work and keeps the "no orphaned children" invariant testable.
