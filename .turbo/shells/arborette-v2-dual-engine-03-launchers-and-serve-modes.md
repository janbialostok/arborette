---
spec: .turbo/specs/arborette-v2-dual-engine.md
depends_on: [arborette-v2-dual-engine-01-service-seams-and-internal-auth]
---

# Plan: Real Launchers & Worker Serve Modes

## Context

`StubLauncher` logs and returns 202 without starting anything, so the local stack cannot demo Phase 2 through its own interface — and V2 adds a second worker (the Verifier) that needs exactly the same invocation seam for batch auto-promotion. This shell makes the `JobLauncher` seam real: the Sleep-Cycle worker gains a serve mode, the orchestrator gains an `HTTPLauncher` selected by config, and the pattern is established for the Verifier shell to reuse. The one-shot job invocation keeps working as the AWS Batch seam.

## Produces

- Sleep-Cycle worker serve mode (`POST /runs` running one cycle per request), authenticated with the Shell 1 shared secret
- `HTTPLauncher` implementing `JobLauncher`, selected when the worker URL config is set; `StubLauncher` fallback preserved
- The serve-mode + launcher pattern the Verifier shell copies (its `POST /verifications` endpoint)
- docker-compose wiring so the web UI's "Run Sleep Cycle" button works locally

## Consumes

- Shared-secret internal-auth middleware — from Shell 1
- `internal/orchestrator/joblauncher.go` (`JobLauncher`, `StubLauncher`), `cmd/sleepcycle` wiring — from existing codebase

## Covers Spec Requirements

- R24 (partial: Sleep-Cycle worker serve mode + HTTPLauncher/StubLauncher selection)

## Implementation Steps (High-Level)

1. **Split the Sleep-Cycle worker's wiring from its run** so one process can serve many runs.
2. **Add serve mode** (`POST /runs {optimization_function_id}`) behind internal auth; keep the one-shot `-goal` invocation.
3. **Implement `HTTPLauncher`** behind the `JobLauncher` interface with config-driven selection and stub fallback.
4. **Wire docker-compose/env** so the trigger button is real locally; document the AWS Batch seam remains the production path.

## Open Questions

None

## Expansion Deferred

The following are filled in when `/expand-shell` runs:

- Pattern survey against the codebase state at implementation time
- Concrete `file_path` references with named functions or symbols for each Implementation Step
- Verification section with specific test commands and smoke checks
- Context Files section with the files to read in full before editing
