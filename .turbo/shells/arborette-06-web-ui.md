---
spec: .turbo/specs/arborette.md
depends_on: [arborette-02-orchestrator-api-service]
---

# Plan: Web UI

## Context

This is the analyst-facing surface, and it's the one place in the system where polish is explicitly a requirement, not a nice-to-have — demos of arborette shouldn't get derailed by rough UX. The web UI is a separate React/Next.js deployable that talks only to the Orchestrator's REST API (including its heuristic-browsing endpoints), covering the full analyst workflow: registering a goal, watching the hypothesis loop run live, and browsing the heuristics it eventually produces.

## Produces

- Goal-submission form with a data-source picker (file upload or on-disk file selection)
- Live, SSE-driven view of the hypothesis tree and resulting causal triplets as Phase 1 runs
- Heuristic browser listing `Meta-Heuristic` nodes with drill-down into their supporting evidence (backed by the Orchestrator's trace endpoint)

## Consumes

- From `arborette-02-orchestrator-api-service`: `submit_analyst_goal`, Phase 1/Phase 2 trigger endpoints, the SSE progress stream, and the heuristic-browsing (search + trace) REST endpoints

## Covers Spec Requirements

- R15

## Implementation Steps (High-Level)

1. **Next.js/React app scaffolding**
   - Project setup, API client for the Orchestrator's REST surface.
2. **Goal-submission form**
   - NL goal input plus a data-source picker (upload or on-disk file selection); calls `submit_analyst_goal` and surfaces the returned `optimization_function_id`.
3. **Live hypothesis-loop view**
   - Subscribes to the Orchestrator's SSE stream and renders the hypothesis tree and causal triplets as Phase 1 progresses; handles a dropped connection gracefully (the loop itself keeps running server-side).
4. **Heuristic browser**
   - Lists `Meta-Heuristic` nodes (via the search endpoint) with a drill-down detail view showing supporting evidence (via the trace endpoint).

## Open Questions

- None.

## Expansion Deferred

The following are filled in when `/expand-shell` runs:

- Pattern survey against the codebase state at implementation time
- Concrete `file_path` references with named functions or symbols for each Implementation Step
- Verification section with specific test commands and smoke checks
- Context Files section with the files to read in full before editing
