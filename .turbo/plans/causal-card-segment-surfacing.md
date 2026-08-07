---
status: done
---

# Plan: Surface the finding's segment on causal-verification cards

## Context

On the goal page's **Causal** tab, each causal-verification card shows a truncated finding id
(`finding · 7726c2d4`) and a pair of effect numbers (`unadjusted` / `adjusted`), but never the
**segment those numbers describe** — no filter predicates, no objective label. An analyst reading
`adjusted 0.3136` has no way to tell what it is *of*; the `/goals/{id}/causal-verifications` DTO
carries only `intervention_id` + effects, and there is no other UX affordance (no chips, no link,
no route) that resolves the id back to its human-readable form.

The human-readable form already exists on the graph: every verified finding's `Intervention` node
carries `objective_label` and `effective_filters` in its `properties` JSON, keyed by the same
`intervention_id` the DTO already returns. This change hydrates the DTO with the finding's segment
(objective label + the full cumulative filter set) and renders it as chips on the card, so the
effect is self-describing. It also rewords two awkward copy strings in the same component.

Two decisions were settled during planning: (1) **no Run-tab deep-link** — Run-tab findings are
ephemeral (in-memory SSE only, gone after reload/eviction), the cards have no anchor, and `GoalTabs`
reads `?tab=` only at mount, so a durable link would dead-end; the chips are the human-readable form
and fully answer the question without it. (2) **Show the full segment** (`effective_filters`), not
the marginal `new_filters`, because the causal card has no parent-branch context that would make a
marginal predicate meaningful.

## Pattern Survey

### Analogous Features

- `internal/orchestrator/causalgraph.go:50` — `handleCausalGraph`/`toCausalGraphDTO`: the closest
  DTO-hydration precedent. Resolves the goal, reads a domain value from the graph seam, maps a
  tag-less domain type into a snake_case DTO. Same shape the enriched `toCausalVerificationDTO`
  takes.
- `internal/orchestrator/hypothesis.go:575` — `writeTriplet`: the producer of the Phase-1
  `Intervention.properties` JSON (`PropObjectiveAggregation`, `PropObjectiveLabel`, `PropNewFilters`,
  `PropEffectiveFilters`). Confirmed key set at the source.
- `internal/orchestrator/hypothesis.go:629` — the `"triplet"` SSE payload (the wire shape to
  mirror): `objective_label: obj.Label`, `filters: renderConstraints(effective)`,
  `new_filters: renderConstraints(cand.Filters)` — string-chip arrays. The causal DTO mirrors the
  `objective_label` + `filters` field names for one shared vocabulary across Run and Causal tabs.
- `internal/orchestrator/promote.go:189` — `rankFindings`: the decode precedent for reading a
  finding's segment back off stored properties —
  `domain.DecodeConstraints(f.Intervention.Properties[domain.PropEffectiveFilters])` — and it
  *skips* undecodable filters rather than erroring (the graceful-degrade convention to copy).
- `web/components/EffectReadout.tsx:58-69` — the Run-tab filter-chip rendering. Its chip
  `<span>` class is byte-identical to the `adjustment_set` chip already in
  `CausalVerifications.tsx:114-119`, so that chip style is the de-facto shared primitive.

### Reusable Utilities

- `internal/graph/neo4j.go:150` — `GetIntervention(ctx, id) (domain.Intervention, error)` — the
  "intervention by id → properties" read **already exists** on the interface the Server holds
  (`s.repo`); `handleVerifyFinding` (`causalverify.go:68`) already calls it. No new graph method.
- `internal/domain/decode.go:17` — `domain.DecodeConstraints(v any) ([]Constraint, error)` —
  decodes a stored property (`[]any` of `map[string]any`) back to `[]domain.Constraint`.
- `internal/orchestrator/hypothesis.go:735` — `renderConstraints(filters []domain.Constraint)
  []string` — renders a constraint slice to predicate chips (non-nil empty → `[]`); wraps
  `domain.RenderConstraint` (`domain.go:364`). Package-local, directly reusable.
- `internal/domain/domain.go:82-86` — `Prop*` key constants (`PropEffectiveFilters`,
  `PropObjectiveLabel`, …). Read the property map through these, never string literals.
- `web/lib/format.ts` — `shortId` (already at `CausalVerifications.tsx:65`), `formatNumber`.
- `web/components/ui.tsx` — `SectionLabel`, `Badge`, `Panel`, `cn`. No extracted chip component; the
  chip is an inline `<span>` duplicated across EffectReadout and CausalVerifications.

### Convention Anchors

- DTO hydration lives in the handler file via a `toXxxDTO` function; DTO structs carry explicit
  `json:"snake_case"` tags because domain/store types have none (`causalverify.go:113`).
- Null-safety: effects stay `*float64` → `null`; `AdjustmentSet` nil → `[]` (`causalverify.go:156-159`).
  A finding whose intervention 404s or whose filters fail to decode must degrade to empty chips /
  omitted label — never fail the list.
- Test fakes: `fakeRepo.GetIntervention` (`server_test.go:519`) reads
  `interventionsByID map[string]domain.Intervention`, returns `graph.ErrNotFound` on miss;
  `repoWithFinding` (`causalverify_test.go:24`) seeds it. `TestListCausalVerifications`
  (`causalverify_test.go:99`) decodes into `[]causalVerificationDTO` and asserts snake_case fields.
- Copy strings are inline JSX literals in `CausalVerifications.tsx` (not in `web/lib/causal.ts`'s
  `OUTCOME_COPY`), edited in place at `:99` and `:108-110`.

### Proposed Alignment

Backend: follow the `toCausalGraphDTO` precedent — enrich each record in
`handleListCausalVerifications` via the already-present `s.repo.GetIntervention`, decode
`PropEffectiveFilters` with `domain.DecodeConstraints`, render with `renderConstraints`, read
`PropObjectiveLabel`. Add `objective_label string` + `filters []string` to `causalVerificationDTO`,
mirroring the `"triplet"` SSE payload's names. Loop the single read (bounded by verification count);
no batch method. Degrade a 404/undecodable/`nil`-filter intervention to empty chips + empty label.
Frontend: reuse the inline chip `<span>` verbatim for the segment chips and `SectionLabel` for the
objective label; extend the `CausalVerification` TS type; reword the two copy strings in place. No
Run-tab link, no `GoalTabs`/`LiveRun` changes.

## Implementation Steps

1. **Add segment fields to `causalVerificationDTO`** — `internal/orchestrator/causalverify.go:117`
   - Add `ObjectiveLabel string \`json:"objective_label"\`` and
     `Filters []string \`json:"filters"\`` to the struct (mirroring the `"triplet"` SSE payload
     field names at `hypothesis.go:636-639`).
   - `Filters` follows the non-nil-empty convention: never `null`, always at least `[]`.

2. **Hydrate the segment in the list handler** — `internal/orchestrator/causalverify.go:137`
   (`handleListCausalVerifications`) and `:155` (`toCausalVerificationDTO`)
   - After `s.causalVerifications.ListForGoal(...)` returns `records`, for each record resolve its
     segment: `iv, err := s.repo.GetIntervention(r.Context(), rec.InterventionID)`.
   - On success, decode `iv.Properties[domain.PropEffectiveFilters]` with
     `domain.DecodeConstraints`, render with `renderConstraints`, and read
     `iv.Properties[domain.PropObjectiveLabel]` as a string (guard the `map[string]any` type
     assertion — absent/non-string → `""`).
   - On `GetIntervention` error (incl. `graph.ErrNotFound`) **or** a decode error **or** a `nil`
     `PropEffectiveFilters` (the baseline-style node shape at `hypothesis.go:581`,
     `claim.go:200`, `writeback.go:86`): degrade — `objectiveLabel = ""`, `filters = []string{}`.
     Log at most a debug line; **never** fail the list (mirrors `rankFindings` skipping undecodable
     filters at `promote.go:189`).
   - Change `toCausalVerificationDTO` to accept the resolved segment (add params
     `objectiveLabel string, filters []string`, or a small `segment` struct) and populate the two
     new fields; the handler builds the segment per record before calling it. Keep the existing
     field mapping (`causalverify.go:160-173`) unchanged.
   - Disposition of the per-record read: a plain loop (verification counts are per-goal and small);
     do **not** add a batch `GetInterventions` — that is a deviation to justify only on measured
     N+1 cost.

3. **Extend the `CausalVerification` wire type** — `web/lib/orchestrator.ts` (the
   `CausalVerification` interface, ~`:262`)
   - Add `objective_label: string` and `filters: string[]` to match the enriched DTO. No fetch
     change is needed — the existing causal-verifications fetch passes the JSON through.
   - These are **required** fields, so update the two existing fixtures that build full
     `CausalVerification` literals or they fail typecheck (`include: **/*.ts`, no
     `ignoreBuildErrors`): add `objective_label` + `filters` defaults to the `verification` factory
     in `web/lib/causal.test.ts` (~`:38-53`), and add the two fields to the `const records:
     CausalVerification[]` literal in `web/lib/orchestrator.test.ts` (~`:521-536`). `npm run test`
     (vitest, no typecheck) would still pass, so this only surfaces under `tsc`/`next build`.

4. **Render the segment on `VerificationCard`** — `web/components/CausalVerifications.tsx:60`
   - Directly under the header row / headline block (after `:85`, before the effects grid at `:87`),
     add a segment block: the objective label via `<SectionLabel>{record.objective_label}</SectionLabel>`
     (render the block only when `objective_label` is non-empty), followed by the effective-filter
     chips reusing the inline chip `<span>` verbatim from `EffectReadout.tsx:61-66` /
     `CausalVerifications.tsx:114-119`
     (`className="rounded border border-line bg-surface-2 px-1.5 py-0.5 font-mono text-[10px] text-muted"`),
     mapped over `record.filters`.
   - When `record.filters` is empty (a degraded/legacy record), render neither the chips nor the
     label — the card falls back to today's id-plus-effects layout, so the change is purely additive
     and never shows an empty segment block.

5. **Reword the two copy strings** — `web/components/CausalVerifications.tsx:99` and `:108-110`
   - `:99` `caption="after blocking the back-door paths the model names"` →
     `caption="after blocking the back-door paths in the discovered model"`.
   - `:108-110` `"no adjustment needed — the model names no confounder of this effect"` →
     `"no adjustment needed — the discovered model has no confounder to block for this effect"`.
   - (Exact wording is open to taste; the fix is removing the "the model names" verb-as-noun
     awkwardness while keeping the "given the discovered model" caveat intact.)

6. **Backend test: segment enrichment + degrade** — `internal/orchestrator/causalverify_test.go:99`
   (`TestListCausalVerifications`) and the `testServer`/`repoWithFinding` fakes
   - Seed `interventionsByID` (via `repoWithFinding`/`fakeRepo`) so the verified record's
     `InterventionID` resolves to a `domain.Intervention` whose `Properties` carry
     `PropEffectiveFilters` (a real constraint slice, e.g. `prior_gpa < 3`) and
     `PropObjectiveLabel` (`"avg(gpa_change)"`); wire that `repo` into the `testServer`.
   - Assert the decoded body's `filters` equals the rendered predicate strings and `objective_label`
     equals `"avg(gpa_change)"`.
   - Add a degrade case: a record whose `InterventionID` is absent from `interventionsByID` (so
     `GetIntervention` returns `graph.ErrNotFound`) → response still `200`, that record's
     `filters == []` and `objective_label == ""`.

## Verification

- **Backend unit** — `go test ./internal/orchestrator/ -run TestListCausalVerifications`: the
  enrichment assertions and the `ErrNotFound` degrade case pass; confirm the test seeds
  `PropEffectiveFilters`/`PropObjectiveLabel` and asserts the real rendered values (not a fake
  pass-through). Run `go build ./...` for the DTO/signature change.
- **Frontend typecheck** — `cd web && npm run build` (or `tsc --noEmit`): the extended
  `CausalVerification` type compiles and `VerificationCard` consumes `filters`/`objective_label`.
  This is green only after the two fixture literals in `web/lib/causal.test.ts` and
  `web/lib/orchestrator.test.ts` gain the new required fields (Step 3); `npm run test` alone will
  not catch the gap because vitest does not typecheck.
- **Manual smoke** — with the local stack up, open
  `http://localhost:8083/goals/0dfd44b3-cf74-4039-90ae-86358c1b9d2f?tab=causal`. Expect each card
  to show `avg(gpa_change)` and the segment chips — e.g. finding `7726c2d4` renders
  `prior_gpa < 3`, `bedtime_variability < 60`, `nights_tracked_fraction >= 0.8` (matches the
  Neo4j `properties` for that intervention). Confirm the reworded captions read cleanly and the
  `not_identifiable` card (`3b0a13bb`) still renders (null effects) with its own chips.
- **Degrade spot-check** — the feature is additive: a card whose intervention no longer resolves (or
  a legacy `nil`-filter record) shows the pre-change id-plus-effects layout with no empty segment
  block, and the list still returns `200`.

## Context Files

- `internal/orchestrator/causalverify.go` — the DTO, `toCausalVerificationDTO`, and
  `handleListCausalVerifications` being enriched; also shows `handleVerifyFinding` already calling
  `s.repo.GetIntervention`.
- `internal/orchestrator/hypothesis.go` — `writeTriplet` (the property producer, `:575`), the
  `"triplet"` SSE payload field names to mirror (`:629`), and `renderConstraints` (`:735`).
- `internal/orchestrator/promote.go` — `rankFindings` (`:189`): the decode-and-skip-on-error
  precedent for reading `PropEffectiveFilters` off a stored intervention.
- `internal/domain/decode.go` and `internal/domain/domain.go` (`Prop*` constants `:82-86`,
  `RenderConstraint` `:364`, `Intervention.Properties` `:110`) — the decode/render seam.
- `internal/orchestrator/causalverify_test.go` and `internal/orchestrator/server_test.go`
  (`fakeRepo.GetIntervention` `:519`, `repoWithFinding` `:24`) — the fake-wiring and assertion
  patterns to extend.
- `web/components/CausalVerifications.tsx` — the `VerificationCard`, the chip pattern already used
  for `adjustment_set` (`:114-119`), and the two copy strings (`:99`, `:108-110`).
- `web/components/EffectReadout.tsx` — the Run-tab chip `<span>` (`:58-69`) to reuse verbatim.
- `web/lib/orchestrator.ts` — the `CausalVerification` interface to extend and the causal fetch that
  passes the JSON through.
