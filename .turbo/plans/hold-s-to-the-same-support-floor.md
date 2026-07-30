---
status: done
---

# Plan: Hold S* to the Same Support Floor as the Candidates It Gates

## Context

The Sleep-Cycle worker gates its beam-search winners against S* — the best absolute objective value any eligible Phase-1 finding achieved (`bestSingleSegment` in `internal/sleepcycle/worker.go`). Candidates are held to a `MinSupport` floor (default 30 rows) in `outcome.winners()`, but S* itself has no support filter, so the bar can be set by evidence that would be rejected as a candidate. Measured live on the Spaceship Titanic goal (2026-07-27): Phase 1 overfit to a segment matching exactly 1 row whose passenger was transported, so S* = 1.0 against a 0.5036 baseline — and since the objective is a rate bounded in [0,1], the gate became mathematically unsatisfiable for the whole run (zero winners).

The fix is blocked on a prerequisite: Phase 1 never sets the sandbox's `IncludeRowCount` flag, so support is never recorded for Phase-1 outcomes. The sequence is: have Phase 1 set the flag (the sandbox seam exists and is tested in sleepcycle), persist the count on Outcome nodes, then filter S* by the same floor.

**Resolved decisions:**

- **Legacy outcomes (no recorded support): exclude.** Absent support reads as 0 and falls below the floor, same as sub-floor findings — matching the `epistemic_source` absent-value doctrine. On goals with only legacy outcomes, S* is undefined and `materiallyBetter` takes its existing degenerate-run path (zero winners) until new Phase-1 measurements accumulate support. Honest and self-healing; admitting legacy outcomes would preserve the bug indefinitely.
- **Sleepcycle writeback also sets the new field** on its sleep_derived outcomes (support is already in hand), removing the Outcome-vs-Intervention location asymmetry going forward. The existing `support` property on the Intervention stays for compatibility.
- **Missing row count in a Phase-1 response is logged but non-fatal** — the hypothesis loop's job doesn't depend on support, so the outcome persists with `Support: 0` (self-excluding from S*) rather than failing the candidate. This deliberately differs from sleepcycle's fault-on-missing (`search.go` `measure`), where support drives pruning.

## Pattern Survey

### Analogous Features

- `internal/sleepcycle/search.go:208-243` — the exact seam to mirror, already built and tested. `Worker.measure` calls `objective.ExecuteRequestFor(...)`, sets `req.IncludeRowCount = true`, reads support via `sandboxclient.RowCount(resp)`, and returns `Measurement{Value, Support}`. The Phase-1 fix is the same three moves in `hypothesis.go`.
- `internal/sleepcycle/search.go:99-107` — `searchOutcome.winners(minSupport int64)` is the floor S* must be held to: `Support >= minSupport`. `bestSingleSegment` (worker.go:290-302) currently reads only `f.Outcome.Value`.
- `internal/sleepcycle/beam.go` — beam policy also prunes on `Support >= minSupport` from the same `cfg.MinSupport` knob.
- `internal/orchestrator/hypothesis.go` — three Phase-1 `Execute` call sites: root baseline (~line 147), per-candidate in `processCandidate` (~line 174), dry-run (~line 611). Only the candidate measurement persists an Outcome (via `writeTriplet`, ~line 343); baseline and dry-run stay flagless.
- `internal/sleepcycle/writeback.go:91-138` — sleep-derived writeback persists `"support"` on the **Intervention** `Properties` map and in the audit payload, not on the Outcome.

### Reusable Utilities

- `internal/sandboxclient/client.go` — `RowCountKey = "row_count"` and `RowCount(resp ExecuteResponse) (int64, bool)` (tolerates float64/json.Number). Reuse verbatim. `ExecuteRequest.IncludeRowCount` reaches the orchestrator through the type alias in `internal/orchestrator/sandboxclient.go` — no DTO redefinition needed.
- `internal/objective/objective.go` — `ExecuteRequestFor` does **not** set `IncludeRowCount`, and `objective_test.go:72` pins it false. Set the flag on the returned struct at the call site (the sleepcycle precedent); do not change the builder.
- `internal/graph/neo4j.go` — `sleep_derived` (native scalar, comma-ok read, absent → zero value) and `marshalProvenance`/`unmarshalProvenance` (optional Outcome property, omitted when nil) are the optional-property precedents. An int64 stores natively in Neo4j — no JSON marshaling.

### Convention Anchors

- **Optional node property, presence-tolerant read:** `epistemic_source` (`domain.go:42-55`, `neo4j.go:219-228`) — absent value gets a defined default at the read boundary; consumers must not assume presence. `sleep_derived` (`neo4j.go:96,480`) is the same for a native scalar.
- **Property key constants:** `domain.go:71-77` (`PropNewFilters` etc.) — a property spelled in both writer and reader gets a shared const to prevent drift.
- **Config knob:** `MinSupport` default 30 at `internal/config/config.go:238` (`SLEEPCYCLE_SEARCH_MIN_SUPPORT`) → `sleepcycle.Config.MinSupport`; `materiallyBetter` already passes `int64(w.cfg.MinSupport)` to `winners` (worker.go:314).
- **Test harnesses:** orchestrator `fakeSandbox`/`fakeRepo` (`server_test.go:196-281`, assertion template at `hypothesis_test.go:130-135`); sleepcycle fake sandbox with `defaultSupport`/`omitRowCount` (`fakes_test.go:194-256`) and `MinSupport`-driven tests (`winner_test.go`, `support_pruning_test.go`); graph integration tests gated by `ARBORETTE_INTEGRATION` with `TestSleepDerivedRoundTrips` as the round-trip template (`internal/graph/sleepcycle_test.go`).

### Proposed Alignment

This is "make Phase-1 do what sleepcycle already does": mirror the `search.go` measurement seam in `processCandidate`, persist support as an optional native Outcome property following the `sleep_derived`/`epistemic_source` precedents, and hold `bestSingleSegment` to the same `Support >= minSupport` comparison `winners` uses. `ListEligibleFindings` returns whole Outcome nodes, so the new field flows to `bestSingleSegment` through `graph.CausalTriplet.Outcome` with no Cypher change; the filter lives in Go so the same query can keep feeding abstraction/clustering unrestricted.

## Implementation Steps

1. **Add the domain field and property constant**
   - In `internal/domain/domain.go`, add `Support int64` to `Outcome` with a doc comment stating the doctrine: the matched-row count of the measurement that produced this outcome; 0 means unrecorded (legacy or extraction outcomes), and consumers must treat absent/zero as below any positive support floor.
   - Add `PropSupport = "support"` beside the existing property-key constants (`PropNewFilters` block).

2. **Persist and hydrate the property in the graph layer**
   - In `internal/graph/neo4j.go` `CreateOutcome`, write `support` as a top-level native int64 property only when `o.Support > 0` (mirroring the nil-provenance omission), using `domain.PropSupport`.
   - In `outcomeFromNode`, hydrate with a presence-tolerant comma-ok read (`node.Props[domain.PropSupport].(int64)`), absent → 0 — same shape as `sleep_derived`.

3. **Record support in Phase 1**
   - In `internal/orchestrator/hypothesis.go` `processCandidate`: assign the `ExecuteRequestFor` result to a variable, set `req.IncludeRowCount = true`, then `Execute` — mirroring `search.go:208-215`. Leave the root-baseline and dry-run call sites unchanged.
   - After the existing `NumericValue` extraction, read `support, ok := sandboxclient.RowCount(resp)`; when `!ok`, log (version-skew signal — the sandbox seam is tested, so absence is unexpected) and continue with 0.
   - Thread `support` through `writeTriplet` (new parameter) into `domain.Outcome{..., Support: support}`, and add `"support"` to the `hypothesis_outcome` audit payload for parity with the writeback audit.

4. **Set support on sleep-derived outcomes in the writeback**
   - In `internal/sleepcycle/writeback.go`, set `Support: seg.measurement.Support` on the `domain.Outcome` it builds (~line 104-115). Keep the existing Intervention `Properties["support"]` entry, switching its key to `domain.PropSupport`.

5. **Filter S\* by the floor**
   - In `internal/sleepcycle/worker.go`, give `bestSingleSegment` a `minSupport int64` parameter and skip findings with `f.Outcome.Support < minSupport` before the value comparison.
   - Pass `int64(w.cfg.MinSupport)` at the call site, exactly as `materiallyBetter` does for `winners`.
   - Update the doc comment: S* is also undefined when no eligible finding meets the support floor — feeding `materiallyBetter`'s existing degenerate-run (nil → zero winners) disposition, which is the deliberate legacy-outcome behavior.

6. **Tests**
   - `internal/sleepcycle` — **migrate the shared fixtures first**: the `finding()` helper (`fakes_test.go:46`) and `findingsFor()` (`beam_test.go:16`) build eligible findings whose `Outcome` has zero-value `Support`. Once Step 5 lands, every existing Run-based test that sets `MinSupport` to 30 or 50 (`winner_test.go:44`, `abstract_test.go`, `abstracted_from_test.go`, `writeback_test.go`, `beam_test.go:204`, `failure_paths_test.go`, `support_pruning_test.go`) would see its findings self-exclude from S*, S* go nil, and winners drop to zero. Give `finding()` a default `Outcome.Support` of 100 (clears both the 30 and 50 floors, matching the fake sandbox's `defaultSupport: 100`), flowing through `findingsFor` unchanged; existing tests must stay green without touching their `MinSupport` values.
   - `internal/sleepcycle`: a `bestSingleSegment` test beside `winner_test.go` — findings above/below/at the floor and with zero (legacy) support, built via a support-taking fixture variant (e.g., `findingWithSupport`, wrapping `finding()` with an explicit `Outcome.Support`); assert the sub-floor best value does not set S* and an all-legacy input yields nil. A writeback test asserting the created Outcome carries `Support`.
   - `internal/orchestrator`: extend `fakeSandbox` to capture requests and emit `sandboxclient.RowCountKey` (`float64(n)`, mirroring the sleepcycle fake); assert `processCandidate` sends `IncludeRowCount: true` and the persisted `repo.outcomes[0].Support` matches; assert a response without the key still persists the outcome with `Support: 0`.
   - `internal/objective`: confirm the existing `objective_test.go:72` pin (builder leaves the flag false) still passes unmodified.
   - `internal/graph` (integration): a `support` round-trip test modeled on `TestSleepDerivedRoundTrips` — create an Outcome with `Support: 45`, read it back via `ListEligibleFindings`; and a legacy-absence case — an Outcome created with `Support: 0` has no `support` property in the store and hydrates as 0.

## Verification

- `go test ./internal/domain/... ./internal/graph/... ./internal/objective/... ./internal/orchestrator/... ./internal/sleepcycle/...` — all unit tests pass, including the new ones from Step 6.
- `ARBORETTE_INTEGRATION=1 go test ./internal/graph/...` against the local stack (`make up`) — the new round-trip and legacy-absence tests pass.
- Live end-to-end spot-check (optional but decisive): on the local stack, register the Spaceship Titanic goal, run the hypothesis loop, then trigger the Sleep-Cycle worker. Before this change the singleton finding (support 1, value 1.0) set S* = 1.0 and produced zero winners; after, that finding is excluded from S*, so S* comes from a supported finding (< 1.0) and the run can produce winners. Inspect the persisted Phase-1 Outcome nodes for a `support` property matching plausible row counts.
- Edge cases to spot-check: a goal whose Phase-1 outcomes are all legacy (no `support` property) yields S* = nil and the degenerate-run path (zero winners, no crash); extraction-path outcomes persist without a `support` property.

## Context Files

- `internal/sleepcycle/worker.go` — `bestSingleSegment`, `materiallyBetter`, `Config.MinSupport`; the gate being fixed.
- `internal/sleepcycle/search.go` — `Worker.measure` (the `IncludeRowCount` seam to mirror) and `winners` (the floor being matched).
- `internal/orchestrator/hypothesis.go` — `processCandidate` and `writeTriplet`; where support starts flowing and gets persisted.
- `internal/graph/neo4j.go` — `CreateOutcome`, `outcomeFromNode`, `ListEligibleFindings`; the persistence and hydration boundary, with `sleep_derived`/`epistemic_source` as in-file precedents.
- `internal/sandboxclient/client.go` — `IncludeRowCount`, `RowCountKey`, `RowCount`; the shared request/response contract.
- `internal/sleepcycle/fakes_test.go` — the fake sandbox/repo harness new sleepcycle tests build on.
