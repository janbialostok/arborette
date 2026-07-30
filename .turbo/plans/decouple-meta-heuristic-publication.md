---
status: done
---

# Plan: Decouple Meta-Heuristic Publication from the Lattice's Materially-Better Gate

## Context

A consuming agent's only entry point into Arborette's knowledge is `get_optimized_heuristics` (pgvector similarity over Meta-Heuristic embeddings); `trace_causal_chain` needs an id that tool must supply. Zero Meta-Heuristics therefore means zero reachable knowledge, no matter how much Phase 1 measured. Live on goal `f0365f93` (Spaceship Titanic, 8693 rows): 72 grounded findings including a 98.9%-transported segment backed by 800+ rows, and the Sleep Cycle published 0 heuristics — observed on four goals, so not a tuning problem.

Root cause: one gate answers two questions. `materiallyBetter` (`internal/sleepcycle/worker.go:316`) measures the *search's* marginal value (did the lattice beat S\* by `MinLift` relative), but it also gates abstraction. The gate is unsatisfiable when Phase 1 is strong (`MinLift` 0.05 relative against a rate driven to 0.989 needs >1.038), so the system publishes *less* the better Phase 1 performs. Two early returns in `Run` compound it: `no conjunction formable` and `materiallyBetter empty` both skip abstraction entirely.

The change: keep `materiallyBetter` gating derived macro-segment **write-back** only, and add a **publication selection** stage over the union of Phase-1 findings and derived winners — MinSupport floor, `objective.Improves` vs the global baseline (no lift threshold, so the gate can never be unsatisfiable), support-shrunk directional score, dedupe by canonical filter set and ancestor containment, cap at top-N (default 20) — feeding the existing, unchanged abstraction stage. Decisions confirmed with the user: shrunk score; derived winners compete in the union with no guaranteed slots; cap default 20; Improves-only gate (no MinLift vs baseline); shrinkage constant k = MinSupport (no new shrinkage knob); containment dedupe uses a relative margin (near-ties keep both the broad segment and its refinement).

## Pattern Survey

### Analogous Features
- `internal/sleepcycle/worker.go:316` — `materiallyBetter` is the existing "selection stage over search winners": iterates `outcome.winners(MinSupport)`, applies `objective.Improves`, then a relative-lift gate against `bestSingle`. The publication stage duplicates this shape but ranks against the global baseline. **Unchanged by this work** — it keeps gating write-back.
- `internal/sleepcycle/beam.go:142` — `beamPolicy.frontier` is the sort-then-truncate idiom to mirror: filter by support floor, sort by directional `delta` vs baseline, tie-break on `node.canonical`, truncate to width.
- `internal/sleepcycle/worker.go:294` — `bestSingleSegment` is the existing fold over Phase-1 findings applying the support floor and `objective.Improves` — the model for the Phase-1 side of the union.
- `internal/sleepcycle/search.go:99` — `searchOutcome.winners(minSupport)` — the derived-winners side of the union (order ≥ 2, not failed, support ≥ floor).
- `internal/sleepcycle/beam.go:63` — `beamPolicy.delta` computes directional movement vs baseline (negated under Minimize) so a plain descending sort ranks both directions; the shrunk score extends this normalization.
- `internal/sleepcycle/abstract.go:25` — `abstractAll` → `abstractOne` is the terminal stage the selection feeds: per-item failure isolation, `sleepcycle_abstraction_failure` audits, resume-aware via `GetMetaHeuristic`/`EmbeddingPending`. The Claude + embedding path is untouched.
- `internal/sleepcycle/writeback.go:44` — `writeWinners`/`writeSegment` stays wired to `materiallyBetter`; publication is inserted between write-back and abstraction.

### Reusable Utilities
- `internal/sleepcycle/canonical.go:71` — `CanonicalFilters(filters)` — stable set-semantic key; the dedupe key and the component encoding for subset tests (newline-joined sorted per-constraint JSON).
- `internal/sleepcycle/canonical.go:118` — `decodeConstraints(v)` — mandatory decode of graph-returned filter properties (`[]any` of `map[string]any` → `[]domain.Constraint`).
- `internal/sleepcycle/canonical.go:100` — `derivedID(ns, roleMetaHeuristic, canonical)` — the Meta-Heuristic id mint; a Phase-1 candidate's id derives from its effective-filter canonical exactly as a derived winner's does.
- `internal/objective/objective.go:78,96` — `NumericValue(value, label)` and `Improves(baseline, value, direction)` — value extraction and the direction-aware gate.
- `internal/sleepcycle/worker.go:40` — `liftEpsilon` — not needed by the shrunk score (no division by baseline), but the direction-normalization convention next to it is.
- `internal/sleepcycle/search.go:145` — `appendMissing(dst, ids...)` — provenance id-set dedupe for `abstractedFrom`.
- `internal/sleepcycle/beam.go:195` — `beamPolicy.prunedBySubset` — prior art for subset/containment reasoning over canonical keys.
- `internal/sleepcycle/worker.go:57` — `Config.validate` — where `MaxPublications < 1` is rejected, matching existing guards.

### Convention Anchors
- Config quartet: `sleepcycle.Config` (`worker.go:49`) ↔ `config.SleepCycleConfig` (`internal/config/config.go:119`) ↔ `SLEEPCYCLE_SEARCH_*` env defaults (`config.go:232-240`) ↔ translation in `cmd/sleepcycle/main.go`. New knobs are added in all four places plus `config_test.go` fallback tests.
- Consumer-defined narrow interfaces: any new graph read belongs on `searchRepo` (`worker.go:78`). This change needs none — `ListEligibleFindings` already returns full triplets with `Intervention.Properties`.
- Per-item failure isolation + typed audits; "produced nothing" is a **successful** degenerate run via `noAbstraction` → `sleepcycle_no_abstraction` (`worker.go:361`), never an error.
- Eligible-finding semantics: `ListEligibleFindings` (`internal/graph/neo4j.go:335`) excludes `sleep_derived` interventions and filters to `domain.SearchEligibleStatuses`; legacy `Outcome.Support` is 0 and self-excludes under any positive floor (`internal/domain/domain.go:118`).
- Test patterns: `internal/sleepcycle/fakes_test.go` (`newHarness`, `fakeRepo` recording `interventions`/`heuristics`/`abstractedFrom`, `fakeSandbox` scripted by `segmentKey`, `finding`/`findingWithSupport` builders, `h.audits.find`); `winner_test.go` is the model for floor/gate selection tests.
- `.gitignore` gotcha: a bare `orchestrator` line shadows `cmd/orchestrator`; use `git add -f` when staging files under it (not expected here, but the config quartet touches `cmd/sleepcycle`, which is unaffected).

### Proposed Alignment
Blend, heavily reusing what exists: a new `publish.go` paralleling `materiallyBetter`'s shape but ranking the union with the `frontier` sort-then-truncate idiom, `CanonicalFilters` keys for dedupe, `Improves`/`NumericValue` for the gate and score, and the existing config-quartet threading for the one new knob. The one genuinely new piece is the unified candidate representation feeding `abstractAll`.

## Implementation Steps

1. **Introduce the publication candidate type and selection stage** (`internal/sleepcycle/publish.go`, new file)
   - Define `candidate`: `canonical string`, `filters []domain.Constraint`, `value float64`, `support int64`, `score float64`, `abstractedFrom []string`.
   - Both constructors are methods on `*Worker` — their bodies need `w.report` (audits), the goal id, and `w.cfg.MinSupport` (floor and shrinkage k), none of which a free function carries.
   - `(w *Worker) candidatesFromFindings(ctx context.Context, goalID string, findings []graph.CausalTriplet, obj objective.Objective, baseline float64) []candidate`: for each finding, decode `Intervention.Properties[domain.PropEffectiveFilters]` via `decodeConstraints` (the *effective* set — the cumulative branch segment `Outcome.Value` was measured under, not `PropNewFilters`); skip on decode error with a per-item `sleepcycle_publication_failure` audit (action name defined here; emitted via `w.report` with `optimization_function_id`, the finding's `Intervention.ID`, and the error — mirroring `sleepcycle_measurement_failure`'s shape); skip when `Outcome.Support <= 0` unconditionally (legacy/extract findings; also removes the `0/(0+0)` NaN weight when `MinSupport == 0`, since production `measure` already treats zero-support as failed), when `Outcome.Support < int64(w.cfg.MinSupport)`, when `NumericValue(f.Outcome.Value, obj.Label)` fails, or when `!objective.Improves(baseline, v, obj.Direction)`. `abstractedFrom` = `appendMissing(nil, f.State.ID, f.Intervention.ID, f.Outcome.ID)`. Canonical via `CanonicalFilters(effectiveFilters)`; skip empty filter sets (a finding with no effective filters *is* the baseline — nothing to publish).
   - `(w *Worker) candidatesFromWinners(written []winner, atoms []atom, obj objective.Objective, baseline float64) []candidate`: skip any winner failing `objective.Improves(baseline, won.value, obj.Direction)` — `materiallyBetter` gates against S\*, not the baseline, and S\* can sit *below* baseline (existing tests pin findings at 5.0 vs baseline 10.0), so a winner beating S\* while still below baseline must be written back but **not** published; without this skip it would enter selection with a negative score, violating the positive-score assumption the containment margin relies on. Then map each surviving `winner` using `won.node.canonical`, `won.node.filters`, `won.value`; support comes from the winner's measurement — extend `winner` to carry `support int64` (populated in `writeSegment` from `seg.measurement.Support`, which is already in scope there). `abstractedFrom` = existing `abstractedFromIDs(won, sources)` logic (atom sourceIDs + interventionID + outcomeID) — move or reuse it here.
   - Scoring: `score = directionalDelta(value, baseline, direction) · support/(support + k)` with `k = float64(w.cfg.MinSupport)`, computed inside both constructor methods; `directionalDelta` mirrors `beamPolicy.delta` (delta = value − baseline, negated under Minimize). Note the algebraic identity: this equals shrinking the value toward baseline by the empirical-Bayes weight and then taking the delta. With `MinSupport == 0` shrinkage vanishes — acceptable, the operator disabled the floor explicitly — and the denominator is always positive because support-0 candidates were excluded above.
   - `selectPublications(cands []candidate, maxN int) []candidate`:
     - Exact dedupe by `canonical`, keeping the higher score (a derived winner and a Phase-1 finding with the same segment mint the same Meta-Heuristic id anyway).
     - Containment dedupe with a margin rule: split each canonical key on `"\n"` into a component set; if A's components ⊂ B's components (strict subset), the pair restates one relationship at two resolutions — drop the lower-scoring one **only when the winner's score beats it by a relative margin** (`containmentMargin`, package const `0.10`: drop the loser iff `winnerScore > loserScore · (1 + containmentMargin)`, comparing absolute values consistently since scores of gate-cleared candidates are positive by construction). Near-ties keep both — a 500-row broad segment is not evicted by a 120-row refinement that outscores it by 2%, while a depth-4 branch restating one relationship at four sliding resolutions still collapses to its strongest. O(n²) over the post-exact-dedupe set is fine at these sizes.
     - Sort descending by score, tie-break ascending on `canonical` (determinism, mirroring `frontier`).
     - Truncate to `maxN`.

2. **Rewire `Run` so publication is decoupled from the search's gates** (`internal/sleepcycle/worker.go`)
   - Keep the flow through `bestSingleSegment` and `buildAtoms` unchanged.
   - Replace the `len(atoms) < 2 || MaxOrder < 2` early **return** with a branch: when no conjunction is formable, skip search and write-back (`written` stays empty) but fall through to publication. No new audit action: record it by adding a `search_skipped` reason string (the current "no conjunction formable" text) to the run-complete audit detail, which step 2's last bullet already extends.
   - When `materiallyBetter` returns empty: skip `writeWinners` (nothing cleared the search's own gate) but fall through to publication.
   - After write-back: `cands := append(w.candidatesFromFindings(ctx, goalID, findings, obj, baseline), w.candidatesFromWinners(written, atoms, obj, baseline)...)`; `selected := selectPublications(cands, w.cfg.MaxPublications)`.
   - If `selected` is empty: `noAbstraction(ctx, goalID, "no candidate cleared publication selection", len(atoms), bestSingle)` and return nil — same degenerate-run convention, new reason string.
   - Otherwise call the reworked `abstractAll` (step 3) with `selected`. Update the run-complete audit to also report `published` (count of selected candidates) and, when set, the `search_skipped` reason, alongside `winners`/`measurements`.

3. **Adapt the abstraction stage to consume candidates** (`internal/sleepcycle/abstract.go`)
   - Change `abstractAll`/`abstractOne` to take `[]candidate` instead of `[]winner` + atoms/sources: `mhID := derivedID(target.namespace, roleMetaHeuristic, cand.canonical)`; `llm.MacroSegment{..., Filters: cand.filters, Value: cand.value}`; `CreateMetaHeuristic(..., cand.abstractedFrom)`.
   - The resume/skip logic (`GetMetaHeuristic` → `EmbeddingPending` three-way branch), `abstractSegment` repair loop, and `embed` tail are untouched. Re-selecting an already-published candidate on a later run costs zero Claude calls by construction.
   - `abstractedFromIDs` moves into candidate construction (step 1); delete the old signature.

4. **Thread the `MaxPublications` knob through the config quartet**
   - `internal/sleepcycle/worker.go`: add `MaxPublications int` to `Config`; `validate` rejects `< 1`.
   - `internal/sleepcycle/fakes_test.go`: add `MaxPublications` to `testConfig()` (e.g. `20`) — every worker test constructs through `newHarness → NewWorker → cfg.validate()`, so a zero value would fail the whole suite at harness construction.
   - `internal/config/config.go`: add `MaxPublications` to `SleepCycleConfig`; default `intEnv("SLEEPCYCLE_MAX_PUBLICATIONS", 20)` (note: publication is not a search knob, so the env name drops the `_SEARCH` segment deliberately); extend the struct doc comment.
   - `cmd/sleepcycle/main.go`: copy the field across in the `Config` translation.
   - `internal/config/config_test.go`: fallback test mirroring `TestMinLiftFallback`.

5. **Tests** (`internal/sleepcycle/publish_test.go`, plus touch-ups where signatures changed)
   - Extend the fixture builders first: `finding`/`findingWithSupport` (`fakes_test.go:48-65`) set only `new_filters`, but `candidatesFromFindings` reads `effective_filters` — without the extension every existing fixture decodes to an empty set and is skipped as baseline. Default the builders to `effective_filters == new_filters`, and add a variant (e.g. `findingWithEffective(id, value, support, newFields, effectiveFields)`) for depth-N cases where effective ⊃ new. Reuse `asProperty` for the encoding.
   - Shrunk ordering: baseline 0.5, a 54-row value-1.0 candidate vs an 800-row value-0.989 candidate → the 800-row candidate ranks first (the live pathology, pinned). **This test must override `MinSupport` to 30** (the production default): with `testConfig()`'s `MinSupport: 0`, k = 0 makes every weight 1 and the raw deltas (0.5 vs 0.489) rank the 54-row candidate first — the mechanism under test is disabled at the default config. At MinSupport 30 the scores are ≈0.321 vs ≈0.471.
   - Below-baseline winner exclusion: S\* below baseline (findings at 5.0, baseline 10.0, Maximize), a derived winner beating S\* but below baseline → written back (interventions recorded) yet not abstracted; no negative-score candidate enters selection.
   - Improves-only gate: a candidate at/below baseline (in-direction) is excluded; direction Minimize is exercised too.
   - Legacy self-exclusion: a support-0 finding produces no candidate under MinSupport 30, **and** under MinSupport 0 (the unconditional `Support <= 0` skip — pins the NaN guard).
   - Effective-vs-new filters: a depth-2 finding publishes its cumulative segment (canonical built from effective filters).
   - Containment dedupe with margin: `{A}` and `{A,B}` where the superset outscores by more than `containmentMargin` → only the superset survives; a near-tie (within the margin) keeps both; disjoint sets both survive.
   - Exact dedupe: derived winner and Phase-1 finding with the same canonical → one candidate, higher score kept.
   - Cap: more eligible candidates than `MaxPublications` → exactly `MaxPublications` abstracted (assert on `fakeRepo.heuristics` count).
   - End-to-end through the harness: strong-Phase-1 scenario where `materiallyBetter` yields nothing → write-back writes 0 interventions but heuristics are still created (the headline regression test for the improvement); and the no-conjunction-formable path (`len(atoms) < 2`) still publishes Phase-1 candidates.
   - **Migrate the existing degenerate-path tests deliberately — the "publication decoupled from search gates" change is not mechanical for them.** Once the builders default `effective_filters == new_filters`, any test whose findings improve over its baseline and clear the floors will now publish where it previously asserted `sleepcycle_no_abstraction`. Known affected assertions: `winner_test.go:142` (`TestMaxOrderOneIsDegenerate` — 5 improving findings over baseline 0, MaxOrder 1), `termination_test.go:110` (`TestSandboxFailureAfterBaselineExitsCleanly` — 2 improving findings, baseline succeeds), plus the `no_abstraction` assertions at `degenerate_test.go:71`, `support_pruning_test.go:162`, and `failure_paths_test.go:341`, each to be reassessed case by case. Fix direction: where the test's subject is search/degeneracy mechanics (order cap, empty frontier), keep the degenerate assertion by making the findings non-publishable (baseline at/above the finding values, or support below the floor); where the test's subject is the run's output contract, invert the assertion to expect publication. **Never "fix" these reds by suppressing Phase-1 publication on degenerate paths — that reintroduces the exact regression this plan removes.**
   - `materiallyBetter` unchanged: existing write-back gating assertions (written interventions/outcomes counts) still hold; only abstraction-side expectations shift per the bullet above.
   - Empty selection → `sleepcycle_no_abstraction` audit with the new reason via `h.audits.find`.

## Verification

- `go test ./internal/sleepcycle/... ./internal/config/...` — new publish tests green, and the migrated degenerate-path tests (step 5's enumeration) green for the *stated* reason: degenerate assertions hold because their fixtures are non-publishable, not because publication was suppressed.
- `go build ./...` and `go vet ./...` clean (signature changes in `abstractAll` ripple through worker only).
- Live smoke (optional but decisive, per `reference_arborette_local_e2e_ops`): `make up`, re-run the Sleep Cycle against goal `f0365f93` (Spaceship Titanic) — expect the run-complete audit to report `published > 0` and `get_optimized_heuristics` to return a non-empty list; the 800+-row 98.9% segment should rank above the 54-row 100% segment in what got abstracted.
- Degenerate cases spot-checked in tests rather than live: all-legacy findings (support 0) still yield `sleepcycle_no_abstraction` with the new reason string.

## Context Files

- `internal/sleepcycle/worker.go` — `Run` flow, `Config`, `materiallyBetter`, `bestSingleSegment`, `noAbstraction`; the file the rewire happens in.
- `internal/sleepcycle/abstract.go` — `abstractAll`/`abstractOne`/`abstractedFromIDs`/`embed`; the consumer whose input type changes.
- `internal/sleepcycle/writeback.go` — `winner` struct (gains `support`), `writeSegment`; unchanged gating.
- `internal/sleepcycle/canonical.go` — `CanonicalFilters` component encoding (subset tests depend on the newline-joined form), `derivedID`, `decodeConstraints`.
- `internal/sleepcycle/search.go` — `atom`/`Node`/`measuredNode`/`winners`, `buildAtoms`, `appendMissing`.
- `internal/sleepcycle/beam.go` — `delta` direction normalization and `frontier` sort idiom to mirror; `prunedBySubset` containment prior art.
- `internal/sleepcycle/fakes_test.go` and `internal/sleepcycle/winner_test.go` — harness, builders, and the selection-test style to extend.
- `internal/config/config.go` and `cmd/sleepcycle/main.go` — the config quartet the new knob threads through.
