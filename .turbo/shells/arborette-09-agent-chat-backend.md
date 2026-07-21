---
spec: .turbo/specs/arborette.md
depends_on: [arborette-02-orchestrator-api-service, arborette-05-mcp-server]
---

# Plan: Agent Chat Backend

## Context

The pipeline's output doesn't have to stop at a queryable graph — an analyst can preview a working, conversational agent built on it. That agent is a standard Claude tool-use agent scoped to the two read-only MCP tools, connected via Claude's native MCP connector rather than a hand-written tool-use loop. The connector is a server-side Anthropic API feature: Anthropic's own infrastructure opens the connection to the MCP Server URL, so the URL must be publicly reachable (not in-network) and the Anthropic API key must be held by a backend, never the browser. This shell builds that backend piece — the Orchestrator's chat endpoint and the MCP Server's public exposure — which the preview/chat UI (Shell 10) streams against.

## Produces

- Orchestrator chat endpoint: holds the Anthropic API key server-side, and per browser message calls the Claude Messages API (`claude-sonnet-5` default, configurable) with the native MCP connector (`mcp_servers` pointed at the MCP Server's public endpoint, `mcp_toolset` scoped to `get_optimized_heuristics`/`trace_causal_chain`), streaming the response back to the browser
- MCP Server exposed on a public, `authorization_token`-protected endpoint (already required by R12 for external downstream agents; reused here rather than a separate address)
- Chat-session binding to the analyst's active dataset/goal context. In the single-analyst/single-dataset MVP this scoping is inherent — there is exactly one dataset's worth of `Meta-Heuristic` knowledge to retrieve, and the read-only MCP tools (Shell 05) match on an operational-state string with no per-goal filter — so the binding is session/context-level, not a query-level filter the tools enforce. Finer per-goal scoping of heuristic retrieval is a post-MVP concern tied to the multi-dataset/multi-department vision, not something the MVP's MCP tools carry.

## Consumes

- From `arborette-02-orchestrator-api-service`: REST API scaffolding, streamed-response infrastructure (the same mechanism as Phase 1 progress streaming)
- From `arborette-05-mcp-server`: `get_optimized_heuristics` and `trace_causal_chain` tool implementations, to expose on a public endpoint

## Covers Spec Requirements

- R21
- R23 (partial: backend — chat endpoint and connector call)
- R24 (partial: builds the shared native-MCP-connector architecture a future export reuses; the self-contained bundle, vendored MCP server copy, and goal-scoped graph/embedding snapshot are deferred per MVP scope)

## Implementation Steps (High-Level)

1. **MCP Server public exposure**
   - Configure the MCP Server's deployment (local docker-compose tunnel for dev, public AWS endpoint in production) with `authorization_token` protection.
2. **Orchestrator chat endpoint**
   - Holds the Anthropic API key; on each browser message, calls the Messages API with the native MCP connector (beta header `mcp-client-2025-11-20`) scoped to the two read-only tools; streams the response back.
3. **Dataset/goal session context**
   - Binds a chat session to the analyst's active dataset/goal context. In the single-dataset MVP this is session-level context, not a query-time filter — the MCP tools retrieve from the one dataset's heuristics; per-goal retrieval scoping is deferred to the multi-dataset vision.

## Open Questions

- Exact packaging format for production agent export (R24) — container image, downloadable zip, or something else. R24 itself is deferred past the MVP; this shell's job is only to build the connector-based architecture so export is additive packaging work later, not to build the export bundler.

## Expansion Deferred

The following are filled in when `/expand-shell` runs:

- Pattern survey against the codebase state at implementation time
- Concrete `file_path` references with named functions or symbols for each Implementation Step
- Verification section with specific test commands and smoke checks
- Context Files section with the files to read in full before editing
