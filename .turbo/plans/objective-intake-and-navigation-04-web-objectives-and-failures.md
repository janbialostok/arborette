---
status: done
spec: .turbo/specs/objective-intake-and-navigation.md
---

# Plan: Web Objectives List & Failure Rendering

## Context

The analyst-facing surface is where all of this iteration's UX debt is felt: there is no way to return to a past or running objective without its ID, and failures show up as opaque states with no explanation. This shell closes the loop in the web UI: an objectives list view (reachable from the app nav) that lets an analyst navigate back to any objective by name and see its latest run status, and clear rendering of failures at both the moments an analyst hits them — the registration message when a fitted objective can't be validated, and the real run-time reason in the live run view. Because live triplets are ephemeral, reopening a settled objective surfaces its persisted status and failure reason rather than an empty view.

## Pattern Survey

### Analogous Features

- `web/app/api/orchestrator/heuristics/search/route.ts:6` — GET BFF route that proxies via `forward(req, "/heuristics/search")`; the direct template for the new `GET /api/orchestrator/goals` handler (no path params; query passthrough is automatic since `forward` appends `new URL(req.url).search`).
- `web/app/api/orchestrator/goals/route.ts:6` — the same route file the new `GET` co-locates into; currently exports only `POST` (via `forwardSubmit`). Adding a `GET` export here follows Next's method-per-export convention.
- `web/lib/orchestrator.ts:119` — `searchHeuristics`/`traceHeuristic` are the template for a new `listGoals()` client fn: build the path off `API_BASE`, delegate to `requestJSON<T>`, define a DTO interface above it. `HeuristicMatch` (line 36) is the DTO-interface analog.
- `internal/orchestrator/submit.go:125` — backend `goalListItemDTO` is the wire shape the new TS DTO must mirror: `optimization_function_id`, `goal_text`, `created_at` (RFC3339), `status`, optional `failure_reason` (omitted when empty). Status strings come from `internal/store/runs.go:13` (`"running"`, `"completed"`, `"failed"`) plus the synthetic `"no run"` the list handler assigns (`submit.go:162`).
- `web/components/HeuristicBrowser.tsx` (`ResultList`, `VerificationBadge`) — closest list-page analog: `T[] | null` state with distinct empty/loading/results renders, rows as bordered cards, and a `Record<string,string>` tone map keyed by a status string with a `?? default` fallback rendered as a rounded-full pill. Rows there are `<button onClick>`; the new page uses `next/link` `<Link href="/goals/[id]">` rows instead.
- `web/components/LiveRun.tsx:335` (`StatusPill`) — a second status-badge precedent keyed by an exhaustive `Record<Phase, …>` with a dot+label and `pulse-live` for active states; the place a new `failed` phase is added for step 5.

### Reusable Utilities

- `web/lib/orchestrator.ts:92` — `requestJSON<T>` — the JSON fetch wrapper `listGoals` calls; already maps non-2xx → `OrchestratorError` with the verbatim `{error}` body.
- `web/lib/orchestrator.ts:86` — `errorMessage(err, fallback)` — surface-vs-mask policy for UI call sites; the list page's catch block uses it as `HeuristicBrowser` does.
- `web/lib/proxy.ts:53` — `forward(req, path)` — the status/body-preserving proxy the new GET route uses unchanged; base-URL resolution and transport-error masking (→502 `{error:"orchestrator is unavailable"}`) are built in.
- `web/components/ui.tsx` — `Panel` (103), `SectionLabel` (8), `Callout` (75), `Button` (32), `Spinner` (61), `cn` (3) — shared primitives for cards, headers, error notices, and empty states.
- `web/lib/runState.ts:21` — `getRunRecord`/`markTriggered`/`markCompleted` — the localStorage run-lifecycle record `LiveRun` reads to tell "finished" from "never started." For step 5 the persisted `GET /goals` `status`/`failure_reason` is the **server-authoritative** counterpart; the two must be reconciled so a terminal server status deterministically promotes the phase without the localStorage guess.
- `web/components/LiveRun.tsx:415` — the existing `branch_failure` feed-item renderer (`item.payload.error` in a warn box). Step 4 is **already modeled and rendered** end-to-end: `BranchFailurePayload` (orchestrator.ts:28), the `branch_failure` union member (orchestrator.ts:16), `handleEvent`'s case (LiveRun.tsx:91), and this Feed render — verify, do not rebuild.

### Convention Anchors

- BFF route files are thin: `export const runtime="nodejs"; export const dynamic="force-dynamic";` plus a one-line delegation to a `proxy.ts` helper. No logic in route files.
- Browser code never fetches upstream directly — it calls `API_BASE = "/api/orchestrator"` fns in `orchestrator.ts`; DTOs are `export interface` blocks co-located above their fetch fn; `ORCHESTRATOR_URL` stays server-only.
- Orchestrator `{error}` bodies are analyst-safe and surfaced verbatim; transport/parse failures are masked behind a caller fallback. `GoalForm.tsx:57-63` **already surfaces the POST /goals 422 message verbatim** via `errorMessage` into a `Callout` — step 3 is confirm-only.
- Page/component split: `app/**/page.tsx` are thin server components rendering a `"use client"` component from `web/components/` (e.g. `heuristics/page.tsx` → `HeuristicBrowser`). The new list page follows this: `app/goals/page.tsx` → a new client component. `/goals` resolves to `app/goals/page.tsx` and `/goals/{id}` to `app/goals/[id]/page.tsx` — no route collision.
- AppNav links: `LINKS` array at `AppNav.tsx:7` with an `active` predicate; the existing `"/"` entry claims `pathname.startsWith("/goals")` as active (line 26), so adding a `/goals` list link requires reworking that predicate (see step 2).
- Tests: `lib/*.test.ts` with Vitest, `vi.stubGlobal("fetch", …)` returning canned `Response` objects; error-path assertions use `rejects.toMatchObject({name,message,status})`. A `listGoals` test mirrors `orchestrator.test.ts`'s `searchHeuristics` cases. No component/rendering tests exist — testing is lib-layer only.
- No date-formatting utility exists; `created_at` rendering is new (parse RFC3339 → `toLocaleString`/`Intl`), done inline in the list component.

### Proposed Alignment

Follow the existing templates closely. The new GET route is a near-copy of `heuristics/search/route.ts` using `forward`; `listGoals` + its DTO mirror `searchHeuristics`/`HeuristicMatch`; the list page mirrors the `page.tsx`→client-component split and `HeuristicBrowser`'s list/empty/error structure, swapping `<button>` rows for `<Link>` rows and reusing the `VerificationBadge` tone-map shape for a status badge. Steps 3 and 4 are pre-built — verify rather than rebuild. The genuine design work is step 5: reconcile the server-authoritative `status`/`failure_reason` from `GET /goals` with `LiveRun`'s localStorage heuristic so a reopened settled run shows persisted status/reason instead of the neutral/perpetual live state — add a `failed` phase to `StatusPill` and let a fetched terminal status seed the phase deterministically.

## Implementation Steps

1. **Add the objectives-list BFF route**
   - In `web/app/api/orchestrator/goals/route.ts`, add a `GET(req: Request)` export delegating to `forward(req, "/goals")`, alongside the existing `POST`. Keep the `runtime`/`dynamic` consts. No new file — co-locate in the existing route module.

2. **Add the typed client fn + DTO**
   - In `web/lib/orchestrator.ts`, add an `export interface GoalListItem` mirroring `goalListItemDTO` (`optimization_function_id: string`, `goal_text: string`, `created_at: string`, `status: string`, `failure_reason?: string`). Add `export function listGoals(): Promise<GoalListItem[]>` calling `requestJSON<GoalListItem[]>(\`${API_BASE}/goals\`)`, following the `searchHeuristics` shape.
   - Add a Vitest case in `web/lib/orchestrator.test.ts` mirroring the `searchHeuristics` success + error-passthrough cases (canned `Response`, `rejects.toMatchObject` for a non-2xx `{error}` body).

3. **Build the objectives list page**
   - Add `web/app/goals/page.tsx` (thin server component) rendering a new `"use client"` component `web/components/ObjectivesList.tsx`.
   - `ObjectivesList` fetches via `listGoals()` on mount (`useEffect`), holding `GoalListItem[] | null` state with distinct loading (`Spinner`), error (`Callout tone="error"` via `errorMessage`), empty ("no objectives yet — link to `/`"), and results renders, modeled on `HeuristicBrowser`'s `ResultList`.
   - Each row is a `next/link` `<Link href={\`/goals/${item.optimization_function_id}\`}>` bordered card showing `goal_text`, a formatted `created_at` (inline RFC3339 → `new Date(...).toLocaleString()`), and a **status badge**. The badge reuses the `VerificationBadge` tone-map pattern with a `Record<string,string>` keyed on `running`/`completed`/`failed`/`no run` and a `?? default` fallback.
   - For a `failed` row, also surface its `failure_reason` inline (a warn-toned line under the goal text), so the list itself explains the failure per R8's acceptance — not only the reopened run view.

4. **Wire the list into AppNav**
   - In `web/components/AppNav.tsx`, add `{ href: "/goals", label: "Objectives" }` to `LINKS`. Rework the `active` predicate so `"/"` matches only `pathname === "/"` (drop the `|| pathname.startsWith("/goals")`), letting the generic `startsWith` branch light "Objectives" for both `/goals` and `/goals/{id}`. Confirm "Submit goal" (`/`) no longer highlights on run pages.

5. **Confirm intake-failure rendering on registration (verify-only)**
   - Verify `GoalForm.tsx:57-63` surfaces the orchestrator's `POST /goals` 422 `{error}` body verbatim into a `Callout tone="error"` via `errorMessage`. Confirm the 422 message from `submit.go:92` ("could not fit the goal to the data source: …") reaches the Callout. No code change expected; if a gap is found, close it minimally.

6. **Confirm run-time failure rendering in the live view (verify-only)**
   - Verify the `branch_failure` SSE path renders end-to-end: `orchestrator.ts:16/28` union + payload, `LiveRun.tsx:91` `handleEvent` case, and the `LiveRun.tsx:415` Feed render of `item.payload.error`. Confirm the real reason (`hypothesis.go:285` `Payload{"error": cause.Error()}`) is shown. No code change expected; if a gap is found, close it minimally.

7. **Show settled-run status + reason on reopen**

   The run view (`LiveRun.tsx`) currently infers "finished vs never-started" purely from the `localStorage` `runState` record, which is blind on a fresh browser. This step reconciles it with the server-authoritative persisted status/reason from `GET /goals` (there is no per-goal status route — the backend exposes only the list endpoint; fetching the list and finding this `id` is accepted overhead). Because the reconciliation interacts with an already-armed idle timer, an ephemeral stream that replays nothing before the first triplet, and a persisted status that can be *stale*, it must be built to a stated invariant rather than case-by-case — earlier point patches here regressed one hazard into a worse one.

   **Reopen-resolution invariant.** The persisted status seeds only the *initial* resolution; the live SSE stream is always authoritative the moment it delivers an event (any triplet/failure promotes the view to live). And **every resolved, non-live state must offer a forward action**: the view must never end in a disabled spinner, a perpetual reconnect loop, or present an in-progress/running objective as a fresh never-started goal whose "Run Phase 1" button launches a hazardous duplicate concurrent run.

   Worked failure cases the design must prevent (each is exercised in Verification):
   - **(a) neutral-flash duplicate-run hazard** — reopening a genuinely running objective (fresh browser, pre-first-triplet startup window) must not resolve to `neutral` with a "Run Phase 1" button.
   - **(b) crashed/abandoned-run trap** — an orchestrator that dies mid-run leaves `runs.status` durably `running`. Reconciliation is boot-time only (`FailOrphaned`, `runs.go:108`, flips every still-`running` row to `failed` once at Orchestrator boot), so a crash leaves the row durably `running` until the next restart, after which it surfaces as `failed`. Both reopen outcomes are handled: a still-durable `running` takes the degrade-to-recoverable-`neutral` path below; a post-restart `failed` takes the `failed` phase + Callout path. Reopening a still-`running` crashed run must **not** trap the view in a disabled-button + perpetual-reconnect UI; it must degrade to the recoverable state today's `neutral` provides (an enabled re-run).
   - **(c) latency race** — a `listGoals()` round-trip slower than the idle timer must not flash `neutral` before the persisted seed arrives.
   - **(d) stale failure Callout** — re-running a failed objective must clear the prior `failure_reason` before the fresh run streams.

   Mechanism:
   - **Deterministic seeding via refs, not a bare `setPhase`.** The mount fetch resolves *after* the subscribe effect has synchronously armed its idle timer (`idleMs` from `triggeredRef.current`, `LiveRun.tsx:136`) and connected the stream, so a bare `setPhase(...)` from the fetch would be clobbered when the already-armed timer later fires `resolveSilence` (`LiveRun.tsx:72`). Store the fetched status in a ref (e.g. `persistedStatusRef`) that `resolveSilence`/`onStreamEnd` (`72`, `114`) consult, and re-resolve once it lands.
   - **Order the fetch against the idle timer (closes (a) and (c)).** Connect the stream immediately at mount (do not delay subscription), but gate the *silence resolution* on the persisted-status fetch settling: `resolveSilence` must not commit to `neutral` while the fetch is still pending — it re-defers (re-arms a short timer) until the fetch resolves or a **bounded max wait of `IDLE_TRIGGERED_MS` (8000 ms) from mount** elapses, after which it falls back to the existing `localStorage`/idle resolution as if the fetch had failed (so a hung `listGoals()` cannot defer resolution forever). So the 2500 ms `IDLE_UNTRIGGERED_MS` timer can no longer flash `neutral` ahead of the seed.
   - **Terminal statuses seed a surviving resolution.** `completed` → `complete` (handle like `completedRef` so the idle timer can't override it); `failed` → the new `failed` phase (below), likewise ref-backed so a later `resolveSilence` won't revert it.
   - **`running` must NOT force the triggered/reconnect path (avoids trap (b)).** Do **not** set `triggeredRef` from a persisted `running` status — that would route `onStreamEnd` into the perpetual `reconnecting → scheduleReconnect → re-subscribe` loop (`114-123`) and hold the primary button disabled (`starting ∈ isRunning`, `221`). Instead, **arm the wait budget at a concrete site**: when the fetch resolves with `running` and no event has arrived yet (`receivedRef.current` is false), the fetch-resolve handler sets phase to the waiting/`starting` state (so the startup window shows "waiting for the live stream," not a "Run Phase 1" button) **and arms its own fresh `IDLE_TRIGGERED_MS` (8000 ms) timer** whose callback degrades `running` → `neutral` *only if* still no events (`receivedRef.current` false). This timer is distinct from the mount idle timer and the pending-fetch re-defer timer, and — like the existing idle timer — is cleared by `handleEvent`'s `clearIdle` path (reuse `idleTimerRef` so an arriving event cancels it) so a live run is never degraded. This deliberately does **not** reuse the mount `idleMs` at line 136 (fixed once from `triggeredRef`, which stays false here by design); the 8000 ms budget is armed only on the `running` fetch-resolve. If any event arrives → live (the stream wins, at any time). If the stream stays silent past the 8000 ms budget, or `onStreamEnd` fires with `triggeredRef` false, resolve to the **recoverable `neutral`** state (enabled "Run Phase 1"), *not* a disabled spinner or reconnect loop — so a genuinely live run shows live while a crashed/abandoned `running` degrades to recoverable.
   - **`listGoals()` rejection is non-fatal.** The fetch can reject when the orchestrator is down (502 via the proxy) or on a transport/parse failure; on any error, swallow it and fall back to the existing `localStorage`/idle resolution (the run view must still work when the list endpoint is unavailable). A `no run`/absent record (id not yet listed, e.g. a mid-registration race) takes the same fallback path. In both cases the pending-fetch gate above must release so silence still resolves.
   - **Reset persisted state on re-run (closes (d)).** `runPhase1` (`LiveRun.tsx:177-198`) already resets `items`/`phase`/`sleep`/`triggeredRef`/`completedRef`/`receivedRef`/`seqRef`; it must **also** clear the new `persistedStatusRef`/`failure_reason` state, so re-running a failed objective drops the stale `failure_reason` Callout before the fresh run's triplets stream.

   - **Add a `failed` phase** to the `Phase` union and to `StatusPill`'s `Record<Phase, …>` (a `bg-warn`/`text-warn` non-live entry; `tsc` enforces exhaustiveness).
   - **Add `failed` to the settled terminal set** (`settled` at `LiveRun.tsx:224`, currently `complete || neutral`). A failed run is conceptually settled: this gives it the re-run affordance — primary button label "Re-run Phase 1", the sleep-cycle button, and the "Re-running starts a brand-new run…" hint — consistent with a completed run. `isRunning` (`LiveRun.tsx:221`) is unchanged (stays `connecting || starting || live`), so the "Run Phase 1" button stays enabled on a failed run.
   - **Extend `Feed`'s empty-state to render a settled `failed` state** (`LiveRun.tsx:368-400`). That switch special-cases only `neutral` and `complete`; without a `failed` branch, `phase === "failed"` with zero streamed items falls through to the pending branch's "Waiting for the loop / Introspecting…" spinner, contradicting the failure. Add a `phase === "failed"` branch mirroring the `neutral`/`complete` `EmptyState`s that presents the run as failed.
   - When the persisted status is `failed`, render `failure_reason` (from the list record) in a `Callout tone="error"` in the settled view, so a reopened failed run shows *why* rather than an in-progress spinner or the neutral "No active run" empty state.

## Verification

- `cd web && npx tsc --noEmit` — the new DTO, `listGoals`, `ObjectivesList`, the `failed` phase, and AppNav edits typecheck; the `Record<Phase,…>` in `StatusPill` stays exhaustive after adding `failed`, and `Feed`'s empty-state switch handles `failed`.
- `cd web && npx vitest run lib/orchestrator.test.ts` — the new `listGoals` success + error-passthrough cases pass alongside the existing suite.
- `cd web && npx next lint` (or the project's configured lint) — no new lint errors in the added files.
- Manual smoke (dev server + running orchestrator/stack): register a valid goal → it appears in `/goals` with a status badge and formatted timestamp; each row links to `/goals/{id}`. AppNav highlights "Objectives" on both `/goals` and `/goals/{id}`, and "Submit goal" only on `/`.
- Intake failure: submit a goal that can't be fit to the data source → the registration surface shows the orchestrator's 422 message verbatim (not a generic error).
- Run-time failure: trigger a run that produces a `branch_failure` → the live feed shows the real reason text.
- Settled reopen (the key new behavior): open `/goals/{id}` for a run that **completed or failed in a different browser / with localStorage cleared** → a completed run shows "Complete"; a failed run shows a `failed` badge, its persisted `failure_reason` in an error Callout, and the settled failed empty-state (**no** "Waiting for the loop" spinner), plus the "Re-run Phase 1" affordance. A never-run goal still resolves to neutral.
- Running reopen (duplicate-run guard, case (a)): open `/goals/{id}` in a **fresh browser** for a run whose persisted status is `running` and that is still in its pre-first-triplet startup window → the view resolves to the waiting/`starting` state and stays subscribed, **not** the neutral "No active run" state with its "Run Phase 1" button. Confirm no way to launch a second concurrent run from the reopened running view while it is streaming.
- Crashed/abandoned running (case (b)): with an objective whose persisted status is durably `running` but no live stream exists (simulate a stranded `running` row / no events ever arrive), reopen it → after the `IDLE_TRIGGERED_MS` window the view resolves to the **recoverable** neutral state (enabled "Run Phase 1"), **not** a perpetual spinner or a reconnect loop with a disabled button.
- Latency race (case (c)): with `GET /goals` artificially slower than `IDLE_UNTRIGGERED_MS` (2500 ms) for a `running` objective, reopen it → the view does **not** flash the neutral "Run Phase 1" state before the persisted seed lands; silence resolution waits for the fetch.
- Re-run clears stale failure (case (d)): on a reopened `failed` run showing its `failure_reason` Callout, click "Re-run Phase 1" → the stale Callout clears before the fresh run streams; it does not render beside new triplets.
- Degraded list endpoint: with the orchestrator's `GET /goals` returning 502 (or unreachable), open `/goals/{id}` → the run view still resolves via the existing localStorage/idle path and renders (no unhandled rejection, no transport error surfaced); the objectives list page shows its error Callout.
- Edge cases to spot-check: `no run` / absent persisted status still resolves to neutral; a persisted `running` status does not clobber a live SSE stream (real triplets still render); an objective whose `id` is absent from the list (e.g. mid-registration race) degrades to the existing localStorage/idle behavior rather than erroring.

## Context Files

- `web/components/LiveRun.tsx` — the run view; step 7's reconciliation touches `Phase`, `StatusPill`, `resolveSilence`/`onStreamEnd`, and the settled-state renders. Read in full before editing.
- `web/lib/runState.ts` — the localStorage heuristic step 7 reconciles against; explains what "settled vs never-started" means client-side and why server truth is needed.
- `web/lib/orchestrator.ts` — where `GoalListItem` + `listGoals` are added; shows `requestJSON`/`errorMessage`/`OrchestratorError` conventions and the DTO-above-fn layout.
- `web/lib/proxy.ts` — `forward` semantics the new GET route relies on (query passthrough, status/body preservation, transport masking).
- `web/components/HeuristicBrowser.tsx` — the list-page + status-badge template `ObjectivesList` mirrors (state shape, empty/error/results renders, tone-map badge).
- `web/components/AppNav.tsx` — the nav + active-predicate the new link edits.
- `web/components/GoalForm.tsx` — confirms step 5's 422 verbatim path already exists.
- `internal/orchestrator/submit.go` (`goalListItemDTO`, `handleListGoals`) and `internal/store/runs.go` (`RunStatus` strings) — the authoritative wire shape and status vocabulary the TS DTO and badge map must match.
