---
spec: .turbo/specs/arborette.md
depends_on: [arborette-01-shared-infrastructure-data-layer, arborette-02-orchestrator-api-service]
---

# Plan: MCP Server

## Context

The MCP Server is arborette's read-side interface for downstream AI agents: it lets any MCP-speaking consumer query the accumulated `Meta-Heuristic` knowledge and its supporting evidence without understanding the graph schema or writing Cypher. Its role in the MVP is intentionally narrow — heuristic consumption (`get_optimized_heuristics`, `trace_causal_chain`) plus goal registration (`submit_analyst_goal`, proxied through) — advancing a goal through either phase is a human-analyst, REST-only action in this MVP. The server itself never mutates state directly; it reads through the same shared query logic the Orchestrator's REST endpoints use, and proxies writes to the Orchestrator.

## Produces

- `get_optimized_heuristics` MCP tool: passes the caller's operational-state string to the shared heuristics-query package (which embeds it internally) and returns semantically-matching `Meta-Heuristic` definitions
- `trace_causal_chain` MCP tool: walks `ABSTRACTED_FROM` edges back to the underlying causal triplets for a given heuristic
- `submit_analyst_goal` MCP tool: proxies to the Orchestrator's REST endpoint rather than writing directly

## Consumes

- From `arborette-01-shared-infrastructure-data-layer`: shared heuristics-query package (embedding search + graph traversal), Neo4j repository interface (read-only)
- From `arborette-02-orchestrator-api-service`: `submit_analyst_goal` REST endpoint to proxy to

## Covers Spec Requirements

- R12

## Implementation Steps (High-Level)

1. **MCP server scaffolding**
   - Set up using `github.com/modelcontextprotocol/go-sdk`.
2. **`get_optimized_heuristics` tool**
   - Passes the raw query string to the shared heuristics-query package, which embeds it and runs the `pgvector` similarity search internally; returns the matching `Meta-Heuristic` definitions. This tool does not call `EmbeddingProvider` directly.
3. **`trace_causal_chain` tool**
   - Walks `ABSTRACTED_FROM` edges from a heuristic back to its supporting `Intervention`/`State`/`Outcome` triplets.
4. **`submit_analyst_goal` tool**
   - Proxies the call to the Orchestrator's REST endpoint; the MCP server performs no direct writes.

## Open Questions

- None.

## Expansion Deferred

The following are filled in when `/expand-shell` runs:

- Pattern survey against the codebase state at implementation time
- Concrete `file_path` references with named functions or symbols for each Implementation Step
- Verification section with specific test commands and smoke checks
- Context Files section with the files to read in full before editing
