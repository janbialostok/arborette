---
spec: .turbo/specs/arborette.md
depends_on: [arborette-01-shared-infrastructure-data-layer, arborette-02-orchestrator-api-service, arborette-07-multi-modal-extraction]
---

# Plan: Sleep-Cycle Worker

## Context

The Sleep Cycle is what turns a pile of individually-true causal triplets into reusable expert knowledge: it clusters an analyst's accumulated triplets by structural similarity and has an LLM abstract each cluster into a generalized `Meta-Heuristic`. It's a manually-triggered, ephemeral batch job (not a continuous service), and — because it's the longest-running, most failure-prone stage in the system — it needs its own crash-safe progress tracking: a cluster only counts as done once its `Meta-Heuristic` node's embedding has actually landed in `pgvector`, not merely once the Neo4j node exists.

Since the hypothesis loop can now produce `extract`-type outcomes with unverified, model-self-reported confidence (see the multi-modal extraction shell), clustering must only draw on **verified** evidence: `query`-type triplets (always verified) and `extract`-type triplets an analyst has HITL-confirmed. This gate is `Outcome.verification_status`, not the confidence weight — a model self-reporting high confidence must never be sufficient on its own to make an outcome cluster-eligible.

## Produces

- Manually-triggered batch job entry point (ephemeral container locally, AWS Batch/Fargate job in production), invoked via the same job/container identifier that the Orchestrator's Phase 2 trigger endpoint submits against — an infra-level contract (job name/image), not a code-level call from the Orchestrator
- Louvain clustering over an analyst's accumulated causal triplets, filtered to only `Outcome.verification_status` of `verified`, `confirmed`, or `corrected` — unverified and rejected `extract` outcomes are excluded from the clustering input entirely
- Per-cluster synchronous Claude abstraction calls (not the Message Batches API — see spec Design § Tech Stack for why this MVP uses synchronous calls)
- `Meta-Heuristic` node + `ABSTRACTED_FROM` edge writes, with `embedding_pending` set atomically at node creation and cleared only after the `pgvector` embedding write succeeds
- Batch/cluster progress persistence, keyed off `embedding_pending: false` (not node existence) so a crash mid-write is retried rather than silently skipped
- Degenerate-clustering handling (records "no heuristic abstracted" rather than forcing an abstraction)
- Per-cluster failure handling (one cluster's error doesn't abort the run)
- Heuristic-injection audit event reporting

## Consumes

- From `arborette-01-shared-infrastructure-data-layer`: Neo4j repository interface, `EmbeddingProvider`, `pgvector` schema
- From `arborette-02-orchestrator-api-service`: internal audit-write API endpoint
- From `arborette-07-multi-modal-extraction`: `Outcome.verification_status` field on the Neo4j repository interface

## Covers Spec Requirements

- R9
- R10
- R11
- R13 (partial: reports heuristic-injection events through the Orchestrator's audit-write API)

## Implementation Steps (High-Level)

1. **Trigger handling + verified-triplet Louvain clustering**
   - Runs graph clustering on manual trigger, querying only triplets whose `Outcome.verification_status` is `verified`, `confirmed`, or `corrected` — never gating on the confidence weight alone, since that's a self-reported, unverified signal.
2. **Degenerate-clustering detection**
   - Detects a single giant cluster or all-singletons outcome and records that no `Meta-Heuristic` could be abstracted, stopping the run rather than forcing a bad abstraction.
3. **Per-cluster abstraction + failure handling**
   - Synchronous Claude call per cluster; a single cluster's failure is recorded and the run continues with remaining clusters rather than aborting.
4. **`Meta-Heuristic` write with atomic `embedding_pending`**
   - Writes the node, its `ABSTRACTED_FROM` edges, and `embedding_pending: true` together in one Neo4j write.
5. **Embedding generation + `pgvector` write**
   - Generates the embedding via the shared `EmbeddingProvider`, writes it to `pgvector`, then clears `embedding_pending` to `false`.
6. **Crash-safe progress persistence**
   - Tracks per-cluster completion keyed off `embedding_pending: false`; a restart retries any cluster whose node is still flagged pending rather than treating it as done or reprocessing already-completed clusters.
7. **Heuristic-injection audit reporting**
   - Reports each successful abstraction to the Orchestrator's audit-write API.

## Open Questions

- Known gap (see spec Open Questions): no invalidation/versioning path exists yet for a `Meta-Heuristic` already abstracted from a triplet whose `Outcome` is later corrected or rejected via HITL — post-MVP work.

## Expansion Deferred

The following are filled in when `/expand-shell` runs:

- Pattern survey against the codebase state at implementation time
- Concrete `file_path` references with named functions or symbols for each Implementation Step
- Verification section with specific test commands and smoke checks
- Context Files section with the files to read in full before editing
