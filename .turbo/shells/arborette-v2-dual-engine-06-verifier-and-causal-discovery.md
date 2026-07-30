---
spec: .turbo/specs/arborette-v2-dual-engine.md
depends_on: [arborette-v2-dual-engine-01-service-seams-and-internal-auth, arborette-v2-dual-engine-02-sandbox-substrate, arborette-v2-dual-engine-03-launchers-and-serve-modes, arborette-v2-dual-engine-04-retrieval-integrity-and-injection]
---

# Plan: Verifier Service & Autonomous Causal Discovery

## Context

This shell births Engine B's home and its first stage: the `cmd/verifier` service and the autonomous causal-discovery pipeline. Discovery is the reframe at V2's center — the system infers the causal graph from the source data itself (CI tests own edge existence; Claude orients only what statistics cannot; analysts can correct later but never gate). It also ships the bundled ground-truth dataset, because discovery accuracy is only measurable against a known planted graph — the dataset is this shell's regression fixture and every later shell's demo substrate.

## Produces

- `cmd/verifier` + `internal/verifier`: serve mode (`POST /verifications` accepted but discovery-only handling here; full verification lands in Shell 7) and one-shot job mode via the launcher seam
- Sandbox `/analyze` endpoint with `contingency` and `moments` request kinds (reusing existing compilers, quoting, binding; hardened like `/execute`)
- PC-style discovery: CI-test sweep (bounded conditioning sets), collider/Meek orientation, batched LLM edge-orientation call (fenced, flat schema); fail-closed handling of failed tests; column cap with deterministic selection
- Persisted causal graph: `DataColumn` nodes + `CAUSES` edges with provenance/confidence/version; cross-process single-flight via Postgres advisory lock; orchestrator read endpoint
- `OrientCausalEdges` method on `llm.Client` following the generate(+repair) pattern
- Bundled synthetic dataset (~1–5k rows) with planted ground-truth graph (a confounder, a true effect, an interaction effect) + discovery precision/recall regression tests

## Consumes

- Staging cache + hardened boundary (analysis endpoints inherit both) — from Shell 2
- Serve-mode + launcher pattern and internal auth — from Shell 3
- Prompt-fencing helper (R29) for the `OrientCausalEdges` call — from Shell 4
- Consolidated `sandboxclient`/audit client — from Shell 1
- `internal/graph` node-writer patterns (open properties map), `internal/llm` completer seam — from existing codebase

## Covers Spec Requirements

- R4
- R5
- R6
- R24 (partial: Verifier serve mode + launcher-seam invocation)
- R30 (partial: ground-truth dataset creation, discovery-accuracy fixtures, and Engine A exercisability at defaults)

## Implementation Steps (High-Level)

1. **Create the bundled dataset** with documented ground-truth causal structure; register/exercise it through Engine A at defaults.
2. **Add `/analyze` with `contingency` and `moments` kinds** to the sandbox behind the Shell 2 hardening.
3. **Scaffold `cmd/verifier`** with serve + job modes on the Shell 3 pattern, consuming the consolidated clients.
4. **Implement the CI-test sweep and skeleton construction** (chi-square/G-test, partial correlation, binned hybrid; fail-closed on errors; column cap with deterministic selection).
5. **Deterministic orientation (colliders + Meek), then the batched LLM orientation call** — orient-or-abstain only, never add/delete edges.
6. **Persist the graph** (nodes/edges/version, provenance) behind the advisory-lock single-flight; expose the orchestrator read endpoint.
7. **Regression-test discovery** against the planted graph (precision/recall).

## Open Questions

- CI-test tuning: significance threshold + multiple-testing correction, conditioning-set bound, quantile bin count, column-cap default (spec Open Questions).
- Whether `/analyze` gets its own rate/concurrency class under the Shell 2 caps (spec Open Question).

## Expansion Deferred

The following are filled in when `/expand-shell` runs:

- Pattern survey against the codebase state at implementation time
- Concrete `file_path` references with named functions or symbols for each Implementation Step
- Verification section with specific test commands and smoke checks
- Context Files section with the files to read in full before editing
