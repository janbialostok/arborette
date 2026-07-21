---
spec: .turbo/specs/arborette.md
depends_on: [arborette-01-shared-infrastructure-data-layer, arborette-02-orchestrator-api-service, arborette-07-multi-modal-extraction]
---

# Plan: HITL Verification Backend

## Context

Since extraction results have no labeled ground truth, the system routes low-confidence results to a human verification queue — but the loop must never block on individual verifications, and the trust gate for Sleep-Cycle clustering must be an explicit human-confirmed status, not a self-reported confidence number the model could report as 1.0 without ever being reviewed. This shell builds the Orchestrator-side machinery: the verification queue, its REST endpoints, the `verification_status` write-through on resolution, and the live confidence-bin distribution that streams alongside hypothesis-loop progress. The web UI that consumes these endpoints is a separate shell (10), since frontend work across all three new features shares conventions worth surveying once.

## Produces

- HITL verification queue (PostgreSQL table): pending/resolved requests, each referencing an `Outcome` by Neo4j node ID, its extracted value, provenance locator, confidence weight at queue time, and resolution
- REST endpoints: list pending queue entries for a goal, browse/pull up any at-or-above-threshold `extract` outcome on demand (not just queued ones), and submit a resolution (confirm/correct/reject)
- Excerpt/page-retrieval REST endpoint: given a queued outcome's provenance locator (page + character span) — or the full-page fallback when the locator is null — reads the source document from the object store and returns the resolved text excerpt (or full page) for the HITL UI to render, since a locator is only coordinates and cannot itself display document content; reuses Shell 07's PDF-text-extraction routine so the excerpt matches what produced the locator
- Resolution handling: sets `Outcome.verification_status` to `confirmed`/`corrected`/`rejected`, updates the `PRODUCED`-edge confidence to 1.0 on confirm/correct, and on correction updates the extracted value and recomputes the provenance locator against it (or nulls it, same fallback as initial extraction)
- Per-goal confidence threshold (default provided, analyst-overridable) and epoch-blocking mode flag (`speculative` default, or `blocking`) on the goal registry
- Epoch-gated, non-blocking loop behavior: by default the loop proceeds to the next tree depth immediately after a level's candidates finish, using unverified confidence scores for pruning, while that level's verifications resolve in the background; configurable per goal to block depth advancement on pending verifications instead
- Confidence-bin distribution: a histogram of `PRODUCED`-edge confidence weights across a run's triplets so far, computed incrementally as triplets are written and re-pushed over SSE when a resolution changes an existing triplet's confidence
- HITL-verification-resolution audit events, written through the existing audit-write API

## Consumes

- From `arborette-01-shared-infrastructure-data-layer`: Go workspace, PostgreSQL schema/migrations tooling, goal registry table, object-storage client (to read source documents for excerpt/page retrieval)
- From `arborette-02-orchestrator-api-service`: REST API scaffolding, SSE progress-streaming infrastructure, audit-write API, hypothesis-loop tree/depth-level orchestration to hook epoch-gating into
- From `arborette-07-multi-modal-extraction`: `Outcome.verification_status` and provenance-locator fields, `extract`-type confidence weight semantics, and the deterministic PDF-text-extraction/first-match provenance-locator computation routine — reused both for on-correction locator recompute and for the excerpt/page-retrieval endpoint, so the excerpt matches the initial-extraction locator logic rather than diverging

## Covers Spec Requirements

- R13 (partial: writes HITL-verification-resolution audit events through the Orchestrator's audit-write API)
- R18 (partial: backend — queue, endpoints, `verification_status` write-through, confidence-bin computation)
- R19
- R20 (partial: backend — confidence-bin computation and SSE inclusion)

## Implementation Steps (High-Level)

1. **HITL verification queue schema**
   - PostgreSQL migration for the queue table; per-goal confidence-threshold and epoch-mode columns on the goal registry.
2. **Queue-routing on `extract` outcomes**
   - Below-threshold outcomes queue automatically; the queue-list endpoint also exposes on-demand access to any at-or-above-threshold outcome.
3. **Resolution endpoint**
   - Confirm/correct/reject handling: `verification_status` and confidence updates, extracted-value update and locator recomputation on correction (via Shell 07's routine), audit-write API call.
4. **Excerpt/page-retrieval endpoint**
   - Given a queued outcome's locator (or the null-locator full-page fallback), reads the source document from the object store and returns the resolved text excerpt (or full page) via Shell 07's PDF-text-extraction routine, so the HITL UI can render actual document content rather than bare coordinates.
5. **Non-blocking epoch gating**
   - Hooks into the existing hypothesis-loop tree/depth-level orchestration (Shell 02) to implement `speculative` (default, non-blocking) vs `blocking` per-goal modes.
6. **Confidence-bin distribution**
   - Incremental histogram computation as triplets are written; SSE inclusion alongside existing tree/triplet progress; re-push on out-of-band resolution updates.

## Open Questions

- Exact default confidence threshold — a starting default of 0.8 is reasonable but should be tuned at implementation time against real extraction-accuracy data. The `verification_status` gate (not this threshold) is what protects Sleep-Cycle clustering from a miscalibrated confidence signal, so this tuning question is about queue ergonomics, not correctness.
- Known gap (see spec Open Questions): no invalidation/versioning path exists yet for a `Meta-Heuristic` already abstracted from a triplet later corrected or rejected here — post-MVP work.

## Expansion Deferred

The following are filled in when `/expand-shell` runs:

- Pattern survey against the codebase state at implementation time
- Concrete `file_path` references with named functions or symbols for each Implementation Step
- Verification section with specific test commands and smoke checks
- Context Files section with the files to read in full before editing
