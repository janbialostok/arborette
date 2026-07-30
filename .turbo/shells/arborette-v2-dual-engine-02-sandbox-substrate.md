---
spec: .turbo/specs/arborette-v2-dual-engine.md
depends_on: [arborette-v2-dual-engine-01-service-seams-and-internal-auth]
---

# Plan: Sandbox Substrate — Staging Cache & Boundary Hardening

## Context

Engine B turns the sandbox from an occasionally-hit query executor into the hot substrate for CI-test sweeps, adjustment queries, refutation batteries, and MCTS measurement volume — every one of which re-pays a full object-store download and DuckDB re-parse today. Meanwhile the service boundary has no auth, no ref scoping, no concurrency cap, and validates allowlists only after staging a full download. The spec requires the cache (R22) and the hardening (R28) to be designed together, because a cache changes the disk/eviction story the hardening must bound.

## Produces

- Content-addressed staging cache keyed by `data_source_ref` with bounded size and eviction, amortizing downloads/parses across a run's measurements
- Hardened sandbox boundary: shared-secret auth on `/introspect` and `/execute` (and inherited by later analysis endpoints), `data_source_ref` validation against the registry, per-request concurrency cap, allowlist validation before staging, `http.MaxBytesReader` body caps
- Config surface for cache sizing, eviction, concurrency, and body caps

## Consumes

- Shared-secret internal-auth middleware pattern — from Shell 1
- `internal/sandbox` (`filesource.go` stage, `server.go` handlers, `compile.go` allowlists) — from existing codebase

## Covers Spec Requirements

- R22
- R28

## Implementation Steps (High-Level)

1. **Design the cache + eviction together with disk caps**: staged datasets cached by immutable content-addressed ref; bounded total size; eviction policy that respects in-flight uses.
2. **Move allowlist/static validation before staging** so statically-invalid requests never trigger a download.
3. **Add auth, ref scoping, concurrency cap, and body caps** to every sandbox handler via the Shell 1 middleware pattern.
4. **Thread cache + caps through config** and update the orchestrator/sleep-cycle clients to send the shared secret.

## Open Questions

- Whether the future `/analyze` request kinds warrant a distinct rate/concurrency class from `/execute` (spec Open Question; the cap design here should leave room).

## Expansion Deferred

The following are filled in when `/expand-shell` runs:

- Pattern survey against the codebase state at implementation time
- Concrete `file_path` references with named functions or symbols for each Implementation Step
- Verification section with specific test commands and smoke checks
- Context Files section with the files to read in full before editing
