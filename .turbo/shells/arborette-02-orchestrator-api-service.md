---
spec: .turbo/specs/arborette.md
depends_on: [arborette-01-shared-infrastructure-data-layer, arborette-03-sandbox-execution-service]
---

# Plan: Orchestrator/API Service

## Context

The Orchestrator is the system's center of gravity: it's the only service the web UI talks to, the only service holding audit-table credentials, and the owner of both the goal registry and the Active Hypothesis Loop (Phase 1). It registers analyst goals, translates them into a structured Evaluation Matrix via Claude, and — on a separate, explicit trigger — runs the interactive hypothesis-tree loop against the Sandbox Execution service, writing empirically-grounded causal triplets and streaming live progress to the analyst's session. It's also the sole audit-table writer, exposing an internal API so other services (the Sleep-Cycle Worker) can log without direct database access.

## Produces

- `submit_analyst_goal` REST endpoint (registers a goal, generates the Evaluation Matrix, writes to the goal registry — does **not** start Phase 1)
- On-disk file ingestion at goal intake: for a data source that is "a file already present on disk" (a path on a volume mounted into the Orchestrator's environment, e.g. a configured local-import directory), reads it from the mount and copies it into the object store — converging on the same object-store-backed representation as an uploaded file before Sandbox Execution ever reads it
- REST endpoint to trigger the Active Hypothesis Loop for a given `optimization_function_id`
- REST endpoint to manually trigger the Sleep Cycle — submits a named job invocation (an ephemeral container start locally, an AWS Batch `SubmitJob` call in production) rather than calling the Worker's code directly, so this shell only needs to agree on the job/container identifier contract with the Sleep-Cycle Worker shell, not depend on its implementation existing yet
- Heuristic-browsing REST endpoints (search + trace) for the web UI, built on the shared heuristics-query package
- SSE endpoint streaming Phase 1 progress (tree nodes, causal triplets) to the web UI, decoupled from the loop's own execution
- Internal audit-write API (used by this service and proxied to by the Sleep-Cycle Worker), attaching the configured stub analyst identity to every record
- Phase 1 hypothesis-loop orchestration: LLM intervention-tree proposal via structured outputs, per-candidate calls to the Sandbox Execution service, causal-triplet writes to Neo4j, per-branch failure handling, append-not-replace semantics on re-trigger

## Consumes

- From `arborette-01-shared-infrastructure-data-layer`: Go workspace, Neo4j repository interface, goal-registry/audit PostgreSQL schema, object-storage client, shared heuristics-query package
- From `arborette-03-sandbox-execution-service`: schema-introspection API endpoint, intervention-execution API endpoint

## Covers Spec Requirements

- R1
- R4 (partial: at goal intake, writes an uploaded file — or reads and copies an on-disk/mounted-volume file — into object storage and records the reference)
- R5
- R6 (partial: LLM proposes structured intervention parameters via structured outputs)
- R7
- R8
- R13 (partial: implements the audit-write API and writes its own intervention/outcome/failure events through it)
- R14

## Implementation Steps (High-Level)

1. **`submit_analyst_goal` endpoint**
   - Accepts the NL goal + data-source reference; for an uploaded file writes it to object storage, and for an on-disk/mounted-volume file reads it from the mount and copies it into object storage (both paths converge on the same object-store representation); calls Claude (structured outputs) for the Evaluation Matrix, writes the registration to the goal registry, returns `optimization_function_id`.
2. **Goal-intake schema introspection**
   - Calls the Sandbox Execution service's introspection endpoint during intake and folds the result into the Evaluation Matrix / variable mapping.
3. **Phase 1 trigger endpoint + hypothesis-tree proposal**
   - Looks up the goal registry by `optimization_function_id`; LLM proposes a directed tree of candidate interventions (system-default breadth/depth/stopping condition) via structured outputs.
4. **Per-candidate execution + causal-triplet writes**
   - Calls the Sandbox Execution service's intervention-execution endpoint per candidate, measures the result against the Evaluation Matrix, writes the `(State) -> [Intervention] -> (Outcome)` triplet to Neo4j.
5. **SSE progress streaming + failure handling**
   - Streams tree/triplet progress to the web UI over the same session (independent of the loop's own execution — a dropped connection doesn't abort the loop); aborts and records a failing branch via the audit-write API while continuing siblings; re-triggering the loop for a goal with existing triplets appends rather than replaces.
6. **Phase 2 trigger endpoint**
   - Manually kicks off the Sleep-Cycle Worker for the analyst's dataset by submitting a job (ephemeral container locally, AWS Batch job in production) identified by a shared job/container name — an infra-level invocation, not a direct code call, so it doesn't require the Worker's own shell to be implemented first.
7. **Internal audit-write API**
   - The single write path to the audit table; attaches the stub analyst identity to every record; callable by this service and the Sleep-Cycle Worker.
8. **Heuristic-browsing REST endpoints**
   - Search (embedding similarity via the shared heuristics-query package) and trace (graph traversal) endpoints backing the web UI's heuristic browser.

## Open Questions

- Exact hypothesis-tree default parameters (breadth, depth, stopping condition) — left to implementation-time tuning against real sample data.
- Full authentication, SSO, and per-department access-control design for the post-MVP multi-analyst system — out of scope for this shell's stub-identity approach, but this is the service that would eventually own it.

## Expansion Deferred

The following are filled in when `/expand-shell` runs:

- Pattern survey against the codebase state at implementation time
- Concrete `file_path` references with named functions or symbols for each Implementation Step
- Verification section with specific test commands and smoke checks
- Context Files section with the files to read in full before editing
