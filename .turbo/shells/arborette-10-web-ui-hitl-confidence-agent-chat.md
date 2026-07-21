---
spec: .turbo/specs/arborette.md
depends_on: [arborette-02-orchestrator-api-service, arborette-06-web-ui, arborette-08-hitl-verification-backend, arborette-09-agent-chat-backend]
---

# Plan: Web UI — HITL, Confidence & Agent Chat

## Context

This shell covers the analyst-facing surface for all three new features at once, since they share the same React/Next.js conventions established by Shell 06 and are worth surveying together rather than in three separate sessions: a HITL verification interface (with source-excerpt rendering), a live confidence-bin histogram, and an agent preview tab built on arborette's default iMessage-style chat styling. The chat component itself is a reusable piece — the same default styling every arborette-produced agent uses, not something rebuilt per analyst.

## Produces

- HITL verification interface: renders a queued (or on-demand-selected) outcome's extracted value against its provenance excerpt (or a full-page fallback when no locator exists), with confirm/correct/reject actions
- Confidence-bin histogram component: live-updating view of the run's confidence distribution, consuming the SSE stream from Shell 08
- Default agent chat UI: a shared, reusable iMessage-style chat component (bubbled messages, minimal chrome) — arborette's standard styling for every produced agent, not duplicated per analyst
- "Preview agent" tab: launches the default chat UI in a preview tab, streaming against Shell 09's chat endpoint, scoped to the analyst's current goal

## Consumes

- From `arborette-02-orchestrator-api-service`: existing SSE progress-streaming pattern to extend for confidence-bin and HITL-queue updates
- From `arborette-06-web-ui`: Next.js/React app scaffolding, existing API client patterns
- From `arborette-08-hitl-verification-backend`: HITL queue REST endpoints, the excerpt/page-retrieval endpoint (returns the resolved document text the interface renders), confidence-bin SSE stream
- From `arborette-09-agent-chat-backend`: Orchestrator chat endpoint (streamed request/response contract)

## Covers Spec Requirements

- R18 (partial: UI — verification interface)
- R20 (partial: UI — histogram rendering)
- R22
- R23 (partial: UI — preview tab and launch action)

## Implementation Steps (High-Level)

1. **HITL verification interface**
   - Queue list/browse view; detail view rendering extracted value + source excerpt (or full-page fallback), fetching the excerpt/page content via Shell 08's excerpt/page-retrieval endpoint (the locator alone is coordinates, not renderable content); confirm/correct/reject actions calling Shell 08's resolution endpoint.
2. **Confidence-bin histogram**
   - Live histogram component subscribing to Shell 08's SSE confidence-bin stream, rendered alongside the existing hypothesis-tree progress view (Shell 06).
3. **Default agent chat component**
   - Shared, reusable iMessage-style chat UI (bubbled messages, minimal chrome) — documented as arborette's standard agent styling.
4. **Preview-agent tab**
   - "Preview agent" action launching the chat component in a new tab, streaming against Shell 09's chat endpoint, scoped to the current goal.

## Open Questions

- None.

## Expansion Deferred

The following are filled in when `/expand-shell` runs:

- Pattern survey against the codebase state at implementation time
- Concrete `file_path` references with named functions or symbols for each Implementation Step
- Verification section with specific test commands and smoke checks
- Context Files section with the files to read in full before editing
