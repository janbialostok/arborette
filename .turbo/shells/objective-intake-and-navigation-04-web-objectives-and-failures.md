---
spec: .turbo/specs/objective-intake-and-navigation.md
depends_on: [objective-intake-and-navigation-02-schema-aware-intake, objective-intake-and-navigation-03-run-status-and-list-backend]
---

# Plan: Web Objectives List & Failure Rendering

## Context

The analyst-facing surface is where all of this iteration's UX debt is felt: there is no way to return to a past or running objective without its ID, and failures show up as opaque states with no explanation. This shell closes the loop in the web UI: an objectives list view (reachable from the app nav) that lets an analyst navigate back to any objective by name and see its latest run status, and clear rendering of failures at both the moments an analyst hits them — the registration message when a fitted objective can't be validated, and the real run-time reason in the live run view. Because live triplets are ephemeral, reopening a settled objective surfaces its persisted status and failure reason rather than an empty view.

## Produces

- A **BFF route** `GET /api/orchestrator/goals` proxying the Orchestrator's objectives-list endpoint.
- An **objectives list page** — goal text, created-at, and a latest-run **status badge** — reachable from `AppNav`, each row linking into the existing run view.
- **Intake-failure rendering** — the registration surface shows the Orchestrator's validation/introspection message verbatim when a fitted objective can't be registered.
- **Run-time failure rendering** — the live run view shows the real branch-failure reason (now meaningful) instead of an opaque state.
- **Settled-run reopen** — opening an objective whose run has finished/failed shows its persisted status and failure reason (from the runs backend) rather than an empty live view.

## Consumes

- From `objective-intake-and-navigation-02-schema-aware-intake`: the `POST /goals` validation/introspection failure messages surfaced on registration.
- From `objective-intake-and-navigation-03-run-status-and-list-backend`: the `GET /goals` list endpoint, per-objective latest run status, and the persisted failure reason; the real run-time failure reasons carried on the SSE stream.
- From existing codebase: the Next.js web UI (`web/` — the app shell/nav, the goal-submission form, the live run view, the BFF proxy pattern, and the typed orchestrator client).

## Covers Spec Requirements

- R8
- R11

## Implementation Steps (High-Level)

1. **Add the objectives-list BFF route**
   - A same-origin `GET /api/orchestrator/goals` route handler proxying the Orchestrator, with the typed client + DTOs.
2. **Build the objectives list page**
   - A list route rendering goal text, created-at, and a status badge per objective, wired into `AppNav`, linking each row to `/goals/[id]`.
3. **Render intake failures on registration**
   - Surface the Orchestrator's validation/introspection message verbatim on the goal-submission surface.
4. **Render run-time failures in the live view**
   - Show the real branch-failure reason from the SSE stream in the run view.
5. **Show settled-run status + reason on reopen**
   - When an opened objective's run is finished/failed, display its persisted status and failure reason instead of a perpetual/empty live state.

## Open Questions

- None.

## Expansion Deferred

The following are filled in when `/expand-shell` runs:

- Pattern survey against the codebase state at implementation time
- Concrete `file_path` references with named functions or symbols for each Implementation Step
- Verification section with specific test commands and smoke checks
- Context Files section with the files to read in full before editing
