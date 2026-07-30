---
spec: .turbo/specs/arborette-v2-dual-engine.md
depends_on: []
---

# Plan: Service Seams & Internal Auth

## Context

V2 adds a fourth service (the Verifier) that would otherwise become the third copy-paste consumer of duplicated HTTP plumbing, and a second internal audit writer against an unauthenticated endpoint. This shell consolidates the seams first — the spec explicitly requires R31 to land before the Verifier consumes them — and establishes the service-to-service shared-secret contract every later internal surface (audit writes, worker serve endpoints, sandbox) reuses. It also lands the batched Meta-Heuristic fetch so the read path the Verifier's heuristic function later hammers is no longer N+1, and anchors the bare `orchestrator` line in `.gitignore` that breaks search and staging in exactly the directories this project touches most.

## Produces

- Shared HTTP JSON-writer/error helpers in `internal/service`, adopted by the sandbox and orchestrator servers
- One unified orchestrator HTTP client (merging `auditclient` and the MCP server's client) with a single typed error and timeout config
- Direct `sandboxclient.NewClient` usage everywhere (alias var removed); `sandboxclient` package tests of its own
- Narrow consumer interfaces for Sleep-Cycle-only graph methods (off the shared `graph.Repository`)
- Batched `GetMetaHeuristics(ctx, ids)` graph call used by `heuristics.Query`
- Config-driven shared-secret header middleware for internal endpoints; `POST /internal/audit` authenticated with it
- Anchored `/orchestrator` `.gitignore` entry

## Consumes

- `internal/service`, `internal/auditclient`, `internal/sandboxclient`, `internal/mcpserver` client, `internal/graph`, `internal/heuristics` — from existing codebase

## Covers Spec Requirements

- R23
- R27
- R31

## Implementation Steps (High-Level)

1. **Extract shared JSON writer/error helpers** into `internal/service` and adopt in sandbox + orchestrator.
2. **Unify the orchestrator HTTP clients** (auditclient + mcpserver's) behind one implementation.
3. **Remove the `NewSandboxClient` alias var**; give `internal/sandboxclient` its own test coverage (moving what currently lives in orchestrator tests).
4. **Narrow the graph interface**: move `ListEligibleFindings`/`MarkStaleMetaHeuristics` consumers onto narrow interfaces so non-Sleep-Cycle implementers stop paying for them.
5. **Add `GetMetaHeuristics` batch method** and use it in `heuristics.Query`.
6. **Introduce the shared-secret internal-auth middleware** (config-driven header) and require it on `POST /internal/audit`; update the worker's audit client to send it.
7. **Anchor the `.gitignore` entry** to `/orchestrator`.

## Open Questions

None

## Expansion Deferred

The following are filled in when `/expand-shell` runs:

- Pattern survey against the codebase state at implementation time
- Concrete `file_path` references with named functions or symbols for each Implementation Step
- Verification section with specific test commands and smoke checks
- Context Files section with the files to read in full before editing
