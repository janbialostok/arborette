# Improvements

Out-of-scope improvement opportunities captured during work sessions. Review periodically and pull items into active work when appropriate.

### Batch the Meta-Heuristic graph fetch in heuristics.Query to avoid an N+1

- **Type**: plan
- **Category**: performance
- **Where**: `internal/heuristics/service.go` (Query), interface in `internal/graph/repository.go`
- **Why**: Query calls `repo.GetMetaHeuristic(id)` once per similarity hit, each opening a separate Neo4j session/read tx — k round-trips on the `get_optimized_heuristics` read hot path. Add a batched `GetMetaHeuristics(ctx, ids)` (`MATCH (m:MetaHeuristic) WHERE m.id IN $ids`) and use it in Query. Deferred: no read handlers consume this seam yet and k is small; batch when the MCP/Orchestrator handler shells land.
- **Update (2026-07-28)**: the deferral rationale has expired — both handlers now consume it (`internal/mcpserver/tools.go:148`, `internal/orchestrator/heuristics.go:69`) at k up to `maxSearchK` = 100, not "small", so this is a live cost rather than a hypothetical one. The seam also moved: it is now `heuristicRepo` in `internal/heuristics/service.go`, not `graph.Repository`. Still deferred only because the round-trips are not yet the bottleneck.
- **Noted**: 2026-07-21

### Harden the Sandbox Execution service boundary (auth, ref scoping, concurrency, early rejection)

- **Type**: plan
- **Category**: reliability
- **Where**: `internal/sandbox/{server.go,filesource.go,compile.go}`
- **Why**: The service is currently an internal, orchestrator-trusted primitive with no request-boundary hardening (only `ReadHeaderTimeout` is set). Deferred to a later security-focused shell: (1) `/introspect` and `/execute` are unauthenticated and accept any `data_source_ref`, so any object in the bucket is readable — no per-goal/tenant ref scoping; (2) no per-request concurrency cap, so parallel large-object requests can exhaust disk/memory (each stages up to `SANDBOX_MAX_OBJECT_BYTES` + `SANDBOX_MAX_TEMP_DIR_SIZE` spill); (3) aggregation/operator allowlist rejection happens only after the full object is staged, so a statically-invalid request still forces a full download (DoS amplification) — validate the allowlists before `stage()`; (4) neither handler wraps `r.Body` in `http.MaxBytesReader`, so an unauthenticated caller can POST an arbitrarily large body (e.g. a huge `filters` array) — add a per-handler body cap alongside the concurrency work.
- **Noted**: 2026-07-21

### Authenticate the Orchestrator's internal audit-write endpoint

- **Type**: plan
- **Category**: reliability
- **Where**: `internal/orchestrator/audit.go` (handleAudit), `internal/orchestrator/server.go` (Routes), `internal/config/config.go`
- **Why**: `POST /internal/audit` is on the public mux (orchestrator port 8080) with no authentication, so any network client can forge audit records stamped with the trusted stub identity — undermining the "sole audit writer" integrity guarantee the design exists to protect. Fix belongs with the deferred auth/SSO work: add a config-driven shared-secret header on the worker→orchestrator contract, or bind `/internal/` routes to an internal-only listener. Deferred because full auth is out of scope for this shell and the only consumer (the Sleep-Cycle Worker) isn't built yet.
- **Noted**: 2026-07-22

### Extract the shared HTTP JSON-writer helpers into internal/service

- **Type**: direct
- **Category**: refactor
- **Where**: `internal/service/` (new helper), `internal/sandbox/server.go`, `internal/orchestrator/server.go`
- **Why**: `writeJSON`/`writeErr`/`errorResponse` are copy-pasted between the sandbox and orchestrator services, differing only in a log prefix. Extract a `service.WriteJSON`/`WriteError` helper into `internal/service` (which already owns `RunHTTPServer`, taking a service name) and update both services so the next HTTP service reuses one seam. Deferred because it reopens already-shipped shell-03 (sandbox) code.
- **Noted**: 2026-07-22

### Cast BOOLEAN columns to 0/1 for numeric aggregation in the Sandbox

- **Type**: direct
- **Category**: feature
- **Where**: `internal/sandbox/compile.go` (aggregation SQL build), `internal/sandbox/server.go`
- **Why**: `avg`/`sum` over a BOOLEAN column returns 400 `"numeric aggregation over non-numeric column"`, which blocks the natural "maximize the rate of a boolean flag" optimization goal (e.g. avg(Transported) as a transport rate). Rate-of-a-flag is a core analytics framing; casting BOOLEAN → INTEGER (0/1) in the aggregation compile would let these goals produce triplets. Surfaced during web-UI e2e testing with the Spaceship Titanic dataset; the web UI correctly rendered the resulting root-baseline branch_failure.
- **Noted**: 2026-07-22

### Rebind (and surface) the hypothesis objective field against the introspected schema

- **Type**: plan
- **Category**: reliability
- **Where**: `internal/orchestrator/{hypothesis.go,submit.go}`, response DTO consumed by `web/`
- **Why**: The pinned objective field is matched name/case-exact and an unmatched goal target is only logged (`submit` still returns 201), so a goal whose target doesn't bind to a real column fails silently at run time — the root baseline 400s, the run emits one branch_failure and zero triplets, and the analyst gets no upfront signal. Rebind the objective to the introspected column (case-insensitive / closest match) and/or return the target-binding result from `POST /goals` so the web UI can warn before Phase 1. Found during web-UI e2e testing (targets like `Transported`/`transported` failing to bind).
- **Noted**: 2026-07-22

### Extract LiveRun's phase-resolution into a pure reducer for unit testing

- **Type**: plan
- **Category**: testing
- **Where**: `web/components/LiveRun.tsx` (resolveSilence, handleEvent, onStreamEnd)
- **Why**: The run-lifecycle state machine — the trickiest logic in the web UI (disambiguating a silent stream into complete/starting/neutral, the loop_complete→completed flip, the triggered-run reconnect path) — is inlined into `useCallback`s closed over refs, so it can only be reached through a rendered component and is currently untested. Lifting the decision logic into a pure reducer (`{triggered, completed, received}` + event → next phase, with side effects like markCompleted/scheduleReconnect kept in the component) would make every branch unit-testable with the existing vitest harness (no jsdom needed). Deferred rather than done in the polish loop: this logic is correctness-reviewed and e2e-validated (it fixed the stuck-in-waiting bug), so a testability-driven refactor of it is better done deliberately than in an automated iteration.
- **Noted**: 2026-07-23

### Migrate the web UI lint setup to ESLint flat config at the Next 16 upgrade

- **Type**: plan
- **Category**: dx
- **Where**: `web/package.json` (lint script), `web/.eslintrc.json`
- **Why**: `web/` uses `next lint` + a legacy `.eslintrc.json`, which works on the pinned Next 15.5 but only emits a deprecation warning — `next lint` is removed in Next 16. Migrating to `eslint.config.mjs` flat config now on 15.5 would need the `@eslint/eslintrc` `FlatCompat` shim (non-trivial); on Next 16 the official codemod lands the clean native `eslint-config-next/core-web-vitals` flat import directly. Defer the migration to the Next 16 upgrade and run the codemod then, rather than a two-step migration now.
- **Noted**: 2026-07-23

### Web-tier hardening if the UI is ever exposed beyond localhost (rate limit + CSP)

- **Type**: plan
- **Category**: reliability
- **Where**: `web/lib/proxy.ts`, `web/next.config.ts`, `web/app/api/orchestrator/*`
- **Why**: The BFF proxy applies no independent rate limit or request-size cap on cost-incurring endpoints (`POST /goals` blocks on an inline Claude call; hypothesis-loop/sleep-cycle are single-click triggers), relying entirely on the orchestrator's upstream caps and the internal-only trust model. Framing/nosniff/referrer headers were added, but a real Content-Security-Policy was deliberately omitted (App Router injects inline bootstrap scripts, so a strict CSP needs per-request nonce middleware + dynamic rendering). If the web service is ever published beyond localhost, add per-IP rate limiting, an early stream-byte cap on the submit path, and a nonce-based CSP.
- **Noted**: 2026-07-23

### Harden the hypothesis proposal→execute path against real schemas (zero-triplet runs)

- **Type**: investigate
- **Category**: reliability
- **Where**: `internal/orchestrator/hypothesis.go` (runLoop, pinObjective, processCandidate), `internal/llm` (ProposeInterventionTree prompt/schema)
- **Why**: Real runs frequently produced zero triplets: Claude's `ProposeInterventionTree` either returned no candidate interventions or pinned a non-column objective field, so the loop finished immediately or failed the baseline. Root cause (prompt grounding on the actual introspected schema, structured-output constraints, or objective selection) needs analysis before a fix. Observed across multiple runs during web-UI e2e testing; the web UI handled the empty/failed runs correctly, so this is purely an orchestrator/LLM robustness question.
- **Noted**: 2026-07-22

### Wide-dataset goal registration fails: evaluation-matrix tool schema exceeds Anthropic's grammar limit

- **Type**: plan
- **Category**: reliability
- **Where**: `internal/llm` (`GenerateEvaluationMatrix` structured-output/strict tool schema), invoked from `internal/orchestrator/submit.go` (`handleSubmitGoal`)
- **Why**: Registering a goal against a wide dataset (e.g. Spaceship Titanic `train.csv`, ~14 columns) fails. Claude returns `400 invalid_request_error: "The compiled grammar is too large... Simplify your tool schemas or reduce the number of strict tools."`; the orchestrator maps it to a 502 "evaluation matrix generation failed". The schema-aware intake embeds dataset columns as enum constraints AND the recursive objective-expression AST in one strict tool schema, so the compiled constrained-decoding grammar exceeds Anthropic's ceiling. Registration is broken for real-world datasets — a core intake limitation, not an edge case. Discovered during the objectives-list/failure-rendering change, which correctly surfaced the failure.
- **Update (2026-07-24)**: Commit `b9f40b9` removed the per-column enums (grounding columns via prompt + deterministic post-validation) and made both structured-output schemas width-independent + static. **Verified insufficient via live E2E** (`make up`, real Anthropic API): registering the 14-col CSV *still* 502s with the same grammar-too-large 400 — and a **narrow 4-col dataset fails identically** (schema is now byte-identical regardless of width). A live schema-depth probe pinpointed the real ceiling: the **objective-expression AST depth**, not the columns. Depth 3 (current `maxExprDepth`) and depth 2 both FAIL; depth 1, depth 0, a leaves-only value (2 kinds), and a bare-column target all PASS. So schema-aware intake never worked against the real API. Expressiveness caveat: depth 1 covers bare-column / boolean-rate / cast / arithmetic objectives, but the CASE-bucket shape (`avg(CASE WHEN age > 18 THEN 1 ELSE 0 END)`) is depth 2 and would be lost by simply lowering the depth. **Recommended fix**: stop emitting the strict AST schema for the value expression — describe the AST in the prompt and parse+validate the returned JSON deterministically via the existing sandbox dry-run (same philosophy the column fix used), preserving full expressiveness at zero grammar cost. Cheaper interim: lower `maxExprDepth` to 1 (drops CASE-bucketing / nested compound objectives).
- **Noted**: 2026-07-23

### Extract and unit-test the LiveRun reopen-reconciliation decision core

- **Type**: plan
- **Category**: testing
- **Where**: `web/components/LiveRun.tsx` — `resolveSilence` + `reconcileStatus` (persisted run status + stream flags → target phase / "defer")
- **Why**: The highest-risk new logic in the web objectives change (reconciling server-persisted run status with the live SSE stream and the localStorage heuristic on reopen) has zero automated tests — it was verified only by trace + live preview + code review. The pure decision is a pure mapping that could be factored into a `web/lib` helper and unit-tested with Vitest, following the existing `runState.ts` + `runState.test.ts` precedent, upholding four invariants (no neutral-flash duplicate-run, no crashed-running trap, no latency race, no stale failure Callout). Marked `plan` because the timer orchestration isn't cleanly extractable without also adding a fake-timer harness the project lacks, so the extraction boundary needs deciding first. Deferred deliberately: extracting immediately after the logic stabilized carried refactor risk not worth taking inline.
- **Noted**: 2026-07-23

### Render objective_label and distinguish new_filters on the web triplet card

- **Type**: direct
- **Category**: feature
- **Where**: `web/components/EffectReadout.tsx` (fields already plumbed via `web/lib/orchestrator.ts` `TripletPayload` and `internal/orchestrator/hypothesis.go` `writeTriplet`)
- **Why**: The `triplet` SSE payload already carries `objective_label` and `new_filters` end to end, but the card renders neither — it shows only the cumulative `filters` chips and the direction-colored delta. Surfacing the objective label and visually distinguishing the candidate's newly-added filter (`new_filters`) from the full effective segment (`filters`) would let a reader see which predicate this triplet's marginal effect came from, without any backend change. Deferred during the flexible-filters change as a deliberate forward-provisioning-then-render split; low priority, purely additive UI.
- **Noted**: 2026-07-24

### Extend the Sleep-Cycle abstraction leak guard to filter literal values, not just column names

- **Type**: plan
- **Category**: reliability
- **Where**: `internal/llm/abstract.go` (`LeakedConcreteTerms`, `macroSegmentPrompt`), enforced at `internal/sleepcycle/abstract.go` (`abstractSegment`)
- **Why**: The abstraction prompt renders raw dataset literals into the model context (e.g. `HomePlanet = Mars`) and the system prompt asks the model to strip "column names, category values, dataset jargon" — but the deterministic guard only checks column names. A definition retaining a raw category value passes validation and is written, embedded, and served verbatim via `get_optimized_heuristics` to a different consumer boundary than the analyst who owns the data. Sequencing matters: the word-boundary matcher fix has landed, but naively extending the same substring check to literals would be worse than the gap — `new`/`web`, boolean `True`/`False`, bare numeric thresholds, and single-character categorical values would match nearly every definition, and a false leak silently drops the macro-segment after burning a repair call. Prefer matching `Abstraction.OntologyTerms[].Concrete` (the model already reports what it mapped) over scanning arbitrary literals; if scanning, skip boolean/numeric operands and apply a minimum token length plus a stop-word screen.
- **Noted**: 2026-07-26

### Consolidate the seams left by the objective/sandboxclient/auditclient extractions

- **Type**: plan
- **Category**: refactor
- **Where**: `internal/objective/`, `internal/sandboxclient/`, `internal/auditclient/`, `internal/orchestrator/sandboxclient.go`, `internal/graph/repository.go`
- **Why**: Four non-behavioral seams the Sleep-Cycle work created, each left in place deliberately to keep that change's blast radius small. (1) `internal/sandboxclient` ships with no tests — its coverage still lives in `internal/orchestrator`, reaching it through the alias shim, so deleting the shim silently deletes the only coverage (`internal/objective` has since gained its own tests). (2) `internal/auditclient` duplicates `mcpserver.OrchestratorClient`: two hand-maintained HTTP clients for the same service off the same `ORCHESTRATOR_URL`, each with its own typed error, timeout constant, and decode branch — the exact duplication the sandbox-client extraction removed. (3) `var NewSandboxClient = sandboxclient.NewClient` leaves two spellings for building one client (`cmd/orchestrator` vs `cmd/sleepcycle`) and makes a constructor a mutable package var, unique in this codebase. (4) `ListEligibleFindings`/`MarkStaleMetaHeuristics` were added to the shared `graph.Repository` but are consumed only through the worker's own narrow `searchRepo`, so every implementer and fake pays for a Sleep-Cycle-only surface.
- **Noted**: 2026-07-26

### Reconcile Meta-Heuristic embeddings across Neo4j and pgvector, not just the pending flag

- **Type**: plan
- **Category**: reliability
- **Where**: `internal/sleepcycle/abstract.go` (`resumeEmbeddings`, `embed`), `internal/graph/neo4j.go` (`ListEmbeddingPending`), `internal/store/embeddingstore.go`
- **Why**: Resume keys solely on Neo4j's `embedding_pending`, cleared only after the pgvector upsert succeeds. That is right for a mid-write crash but makes the flag a one-way latch: if the pgvector row later disappears while the node survives, the pair diverges permanently and nothing notices — the node reports itself complete, resume skips it, and it is invisible to `/heuristics/search` and `get_optimized_heuristics`, which are served entirely by the HNSW index. `/trace` still resolves it, so it looks healthy from the graph side. Observed live 2026-07-26 (four sleep-derived heuristics, `embedding_pending = false`, zero embedding rows); the proximate cause was benign test truncation, but a Postgres PITR restore, a failover losing recent writes, or any retention job on that table reaches the same state — the two stores share no transaction. Recurred larger 2026-07-27 on goal `f0365f93`: 18 of 20 Meta-Heuristics left vector-less by a `make test` run (`internal/testutil/testutil.go:63` TRUNCATEs the table) between two sleep cycles, all still reporting `embedding_pending = false`. Now interacts with the publication stage, which counts a resumed heuristic as published — so `published` and graph reachability both look healthy while the entry point that actually serves consumers returns nothing. Fix: widen the resume pass into a reconcile pass — diff graph Meta-Heuristic ids against `meta_heuristic_embeddings.node_id` and re-embed the difference, reusing the existing embed tail.
- **Update (2026-07-28)**: Recurred a third time and was measured end to end while fixing the heuristics-browser 500. **23 of 25** real Meta-Heuristics held `embedding_pending = false` with no pgvector row, so `/heuristics/search` was serving a **2-heuristic corpus out of 25** — the user-visible symptom was "the browser returns irrelevant results", which read as a ranking problem and was actually 92% of the corpus being invisible. Note the read path now self-heals the *opposite* direction (an embedding whose node is gone is skipped and its row retired, `internal/heuristics/service.go` `retireOrphans`), which makes this entry the remaining half of the reconcile story rather than a separate concern — and the guard comment there records why resume cannot cover it. Restoring the 23 required embedding them out-of-band; there is still no in-product path.
- **Noted**: 2026-07-26

### Harden the abstraction prompt against stored prompt injection

- **Type**: plan
- **Category**: reliability (security)
- **Where**: `internal/llm/abstract.go` (`macroSegmentPrompt`, `RepairMetaHeuristic`), read path in `internal/heuristics/service.go` (`Query`), `internal/mcpserver/tools.go` (`get_optimized_heuristics`)
- **Why**: `macroSegmentPrompt` concatenates analyst-supplied goal text and dataset-derived strings into the user message with no delimiting or data-framing, and `RepairMetaHeuristic` re-injects the model's own prior definition. Goal text enters through two unauthenticated surfaces (`POST /goals` on the bare mux, and the MCP `submit_analyst_goal` tool). The injection vector itself predates the Sleep Cycle — Phase-1 prompts already interpolate goal text — but this feature escalates it from transient to **stored**: the model's output is persisted as a Meta-Heuristic, embedded, and then served by `heuristics.Query`, which takes no goal or tenant parameter, so any caller retrieves any goal's heuristics. Injected instructions therefore land verbatim in a different agent's context. `LeakedConcreteTerms` cannot catch this — injected text contains no column name, so the repair loop never fires. Fix in three parts, cheapest first: fence the untrusted spans (goal text, column names, prior definition) with an explicit "fenced content is data, never instructions" system line; add a length/shape sanity check on the returned definition; then goal-scope the read path so cross-tenant retrieval is impossible. Related: "Extend the Sleep-Cycle abstraction leak guard to filter literal values" above covers the inverse direction -- concrete values leaking out.
- **Noted**: 2026-07-27

### Reject same-column threshold conjunctions in the Sleep-Cycle beam

- **Type**: direct
- **Category**: performance
- **Where**: `internal/sleepcycle/beam.go` (`expand`, alongside `prunedBySubset`), atoms built in `internal/sleepcycle/search.go` (`buildAtoms`)
- **Why**: Phase 1's greedy descent tightens one threshold at a time and writes every intermediate as its own atom, so the atom set is dominated by same-column variants. Measured on goal 5a675382: 16 atoms spanning only 3 columns (`basket_items` ≥4/5/6/8/10/12, `discount_pct` ≤0/1/2/5/10, `tenure_months` ≥12/24/36/48/60). Same-column atoms on the same operator are totally ordered, so conjoining two is degenerate — `basket_items>=4 ∧ basket_items>=12` *is* `basket_items>=12` and measures identically, so it can never beat the tighter atom alone. That makes 29% of order-2 and **73% of order-3 candidates** (410 of 560) semantically dead. The cost is not just wasted measurements against `MaxMeasurements`: a degenerate node carries real support and a real value, so it clears the support floor, ranks on directional delta, and **occupies a beam slot**, displacing the cross-column combinations that can actually win. Fix in `expand` rather than `buildAtoms` — rejecting a candidate that conjoins two atoms sharing a `(field, op)` pair is the same shape as the existing subset rejection and needs no new state, whereas collapsing the atom set discards the `sourceIDs` provenance the abstraction stage links Meta-Heuristics back through. Verify with a test asserting no generated candidate holds two atoms of one `(field, op)`, and by re-running 5a675382 for a measurement count below 120 with higher frontier column diversity.
- **Noted**: 2026-07-27

### Support entity-relative and windowed objectives (grouped time-series segments)

- **Type**: plan
- **Category**: feature
- **Where**: `internal/sandbox/compile.go` (`aggregateSelect`, `allowedAgg`), `internal/domain/domain.go` (Expression AST), `internal/datasource/` (entity key + time column), orchestrator intake
- **Why**: The sandbox's entire emitted surface is one flat `SELECT CAST(AGG(operand) AS DOUBLE) [, count(*)] FROM read_csv(…) [WHERE p AND p]` — no `GROUP BY`, `OVER`, `JOIN`, CTE, or `ORDER BY` — and the expression AST (`column_ref|literal|cast|comparison|arithmetic|case`) has no aggregate-within-expression and no partition/order spec. Timestamps are recognized as a type only so `min`/`max` cast correctly; there is no temporal arithmetic, `lag`, or windowing. Every row is therefore independent, so any signal defined relative to an entity's own history is inexpressible: card-fraud segmentation needs amount-vs-that-card's-trailing-mean, velocity (N transactions in M minutes), geo-impossibility between consecutive transactions — all requiring `PARTITION BY entity ORDER BY ts`. Note the narrowness is currently the injection defense (every identifier quoted, every value bound, fixed allowlists), so a window grammar materially widens the attack surface and needs its own threat pass. **Cheaper interim path, and probably the right first move:** keep the engine as-is and precompute entity-relative features into the dataset as plain columns, then express the goal in the engine's native shape — `maximize avg(is_fraud = True)` under filter interventions, structurally identical to the working `avg(Transported = True)` goal. Worth pursuing: fraud is interaction-heavy (high amount ∧ foreign ∧ 3am ∧ new-merchant), the regime where the beam search beats greedy Phase 1, unlike the monotone surfaces tested so far.
- **Noted**: 2026-07-27

### Hold S* to the same support floor as the candidates it gates

- **Type**: plan
- **Category**: reliability
- **Where**: `internal/sleepcycle/worker.go` (`bestSingleSegment`), `internal/orchestrator/hypothesis.go` (measurement path), `internal/objective/objective.go` (`ExecuteRequestFor`), `internal/graph/neo4j.go` (persist support on Outcome)
- **Why**: `bestSingleSegment` takes the best absolute value across all eligible findings with **no support filter**, while `outcome.winners()` holds Sleep-Cycle candidates to `MinSupport` (30). The bar is therefore set by evidence that would itself be rejected as a candidate. Measured live on the Spaceship Titanic goal 2026-07-27: Phase 1 produced `CryoSleep=True ∧ HomePlanet=Europa ∧ Destination=55 Cancri e ∧ Age<=12` matching **exactly 1 row**, that passenger was transported, so `best_single` = 1.0 against a 0.5036 global baseline. Because the objective is a rate bounded in [0,1], no conjunction can exceed 1.0 and the gate became mathematically unsatisfiable for the whole run — zero winners despite healthy column diversity (10 columns, 28% degeneracy). Any bounded objective where Phase 1 overfits to a singleton kills the Sleep Cycle by construction, independent of dataset width or search quality. The fix is blocked on a prerequisite: `IncludeRowCount` appears nowhere in the orchestrator, so **Phase 1 never records support at all**. Sequence: have Phase 1 set the flag (the sandbox seam already exists and is tested), persist the count on its Outcome nodes, then filter S* by the same floor. Decide explicitly how legacy outcomes with no recorded support are treated — excluding them silently shrinks S* on existing goals, admitting them preserves the bug.
- **Shipped (2026-07-28)**: done in `00dccce`. `worker.go:257` passes `int64(w.cfg.MinSupport)` into `bestSingleSegment`, which now skips findings below the floor; the prerequisite landed too — `hypothesis.go:176` sets `IncludeRowCount = true` and `:191` reads the count back. The legacy-outcome question was decided in code: `Support <= 0` is excluded unconditionally, even at a floor of 0.
- **Noted**: 2026-07-27

### Amortize sandbox dataset staging across Sleep-Cycle measurements

- **Type**: plan
- **Category**: performance
- **Where**: `internal/sandbox/filesource.go` (`stage`), `internal/sandbox/server.go` (Execute path), `internal/sleepcycle/search.go` (`runSearch`/`measure`)
- **Why**: Every beam measurement re-runs `FileSource.stage` — full object-store download, fresh in-memory DuckDB, full `read_csv_auto` re-parse with schema re-sniffing — so a 200-measurement run does ~199 redundant staging cycles on an identical file. Per-measurement cost is dataset-size-linear (~1–2.5 s at 1M rows → 4–8 min runs; ~20–60 min at 10M rows). Candidate shapes: stage once per run keyed by data_source_ref, convert to Parquet at registration, or batch a beam level's candidates against one staged DB. Benefits both the current beam and any future UCT policy identically since both pay per-measurement through the same seam; interacts with the sandbox-hardening entry's concurrency/disk-cap concerns (a cache changes the eviction story).
- **Noted**: 2026-07-27

### Schema-derived atom vocabulary for the Sleep Cycle (V2 spec input)

- **Type**: plan
- **Category**: feature
- **Where**: `internal/sleepcycle/search.go` (`buildAtoms`), `internal/sandbox` introspection, LLM proposal/vetting surface
- **Why**: The beam's reachable space is capped by what Phase 1's LLM proposals happened to surface — a column the hypothesis loop never touched is invisible to the sleep cycle, and the vocabulary it does get is measurably noisy (same-column threshold ladders, singleton overfits). Augment `buildAtoms` behind a flag with schema-enumerated atoms: enum values directly, quantile/equal-frequency cuts for numerics. Invert the LLM to a critic role — one call to flag leakage/tautology columns (ID, post-outcome, objective-correlates that support/lift gates cannot catch) and optionally rank atoms to order the beam's first level. Order-1 measurement of every atom is the Apriori base level the level-wise beam already implements, so support-floor pruning bounds the added cost — but it presupposes the staging-amortization entry on wide datasets. Provenance shifts from Phase-1 `sourceIDs` to the sleep cycle's own sleep_derived triplets for enumerated atoms; the `ABSTRACTED_FROM` linkage needs that decision made explicitly. De-risk first: compare frontier column diversity and measurement efficiency on a known goal vs findings-only atoms. Core input to the V2 dual-engine spec (beam in the episodic layer, UCT in the sleep cycle).
- **Noted**: 2026-07-27

### Decouple Meta-Heuristic publication from the lattice's materially-better gate

- **Type**: plan
- **Category**: feature
- **Where**: `internal/sleepcycle/worker.go` (`materiallyBetter`, `Run` abstraction path), `internal/sleepcycle/abstract.go`, `internal/sleepcycle/writeback.go`, consumed via `internal/heuristics/service.go` + `internal/mcpserver/tools.go`
- **Why**: A consuming agent's only entry point is `get_optimized_heuristics` (pgvector similarity over Meta-Heuristic embeddings); `trace_causal_chain` needs an id that tool must supply. Zero Meta-Heuristics therefore means zero reachable knowledge, no matter how much Phase 1 measured. Live 2026-07-27 on goal `f0365f93` (Spaceship Titanic, 8693 rows): the loop produced **72 grounded findings**, including a 98.9%-transported segment backed by 800+ rows, and the Sleep Cycle wrote 0 winners and 0 heuristics — an agent asking about that dataset gets an empty list. Root cause is one gate answering two questions: `materiallyBetter` measures the *search's* marginal value (did the lattice beat S\*), but it also gates abstraction, which should instead depend on support, lift against the **global baseline**, and non-redundancy. The system therefore publishes *less* the better Phase 1 performs — observed on four goals now, so it is not a tuning problem. Note the gate is unsatisfiable here at every support floor: `MinLift` 0.05 relative against a [0,1] rate that Phase 1 drives to 0.989 needs >1.038. **Not** "abstract every finding" — 72 findings is 72 Claude calls plus embeddings, cumulative tree filters make a depth-4 branch restate one relationship at four resolutions, and the 54-row 100% segment is 0.6% of rows found over 21 atoms (the multiple-comparisons trap). Proposal: keep `materiallyBetter` gating derived write-back (it stops a macro-segment duplicating an existing finding), and add a publication selection over the union of Phase-1 findings and derived winners — reuse `MinSupport`, rank on lift vs the global baseline, dedupe by canonical filter set and ancestor containment along each branch, cap top-N. The abstraction stage needs no change: it takes filters + value + baseline, all of which a Phase-1 finding already carries. Worth evaluating a support-weighted or shrunk score instead of the raw value — raw rate ranks the 54-row 1.0 above the 800-row 0.989, shrinkage orders them correctly, and it would have prevented the original singleton-S\* pathology without a hard floor at all.
- **Shipped (2026-07-28)**: done in `932e1b9`, including the support-shrunk ranking. The proposal is now `selectPublications` (`internal/sleepcycle/publish.go:181`) → `dedupeByCanonical` (`:211`) → `dropContainedRestatements` (`:248`) → cap, wired at `worker.go:274-288`; `materiallyBetter`'s doc comment records the separation.
- **Noted**: 2026-07-27

### Label the id lookups in `CreateMetaHeuristic` so evidence re-linking stops scanning all nodes

- **Type**: direct
- **Category**: performance
- **Where**: `internal/graph/neo4j.go` (`CreateMetaHeuristic`), `internal/graph/schema.go` (`InitSchema`), consumed by `internal/sleepcycle/abstract.go` (`abstractOne` resume path)
- **Why**: Both id lookups are label-free — `OPTIONAL MATCH (n {id: wantId})` in the validation pass and `MATCH (t {id: targetId})` in the write — while `InitSchema` creates only per-label uniqueness constraints, so neither can use an index and each id costs an all-nodes scan. That was once per newly-minted Meta-Heuristic; the publication stage now re-issues the call for every resumed candidate, so it is paid every run for up to `MaxPublications` (20) candidates × 3–5 ids × 2 scans, inside write transactions, against a graph that grows with every Phase-1 finding. Correctness is unaffected and the 30-minute budget absorbs it today. Fix by labelling both patterns (the id already implies the label set), adding an unlabeled index, or skipping the re-link when the candidate adds no new evidence.
- **Noted**: 2026-07-28

### Harden the heuristics read-path self-heal (four risks accepted knowingly at ship time)

- **Type**: plan
- **Category**: reliability
- **Where**: `internal/heuristics/service.go` (`Query`, `retireOrphans`), `internal/store/embeddingstore.go` (`SimilaritySearch`, `Delete`), `internal/store/migrations/0008_embedding_delete_grant.up.sql`, handler at `internal/orchestrator/heuristics.go`
- **Why**: The read path now skips similarity hits whose graph node is gone and retires their pgvector rows, gated on one live sibling in the same batch. Each risk below was surfaced in review and **deliberately accepted** — these are not oversights, and the two disputed windows are recorded so they are not re-litigated. (1) **The guard is per-batch, not per-goal.** `cmd/sleepcycle/main.go:30` requires `-goal`, so repair granularity is per-goal: re-run goal A but not goal B after a graph reset and a live hit from A corroborates permanently deleting B's embeddings. Narrowing it needs a goal id on the embedding row, which the table does not carry. *Disputed, do not re-raise:* there is no `NEO4J_DATABASE` in the codebase, and a wrong `NEO4J_URI` gives a wholly foreign graph where `live == 0` and the guard correctly holds; replica lag is unreachable on the single-instance `neo4j:5`, and bookmarks would not help anyway since write and read live in different processes. (2) **TOCTOU on the delete.** The miss is observed at the top of the loop and the delete runs after the batch, without re-observing the miss, with nothing serializing sleepcycle against read traffic; deterministic `derivedID` means a concurrent repair's fresh embedding can be deleted, leaving `embedding_pending = false` with no vector. Fix is *not* a Go-side timestamp (Go and Postgres clocks skew) — return the observed `(node_id, updated_at)` from `SimilaritySearch` and delete `WHERE node_id = $1 AND updated_at = $2`. (3) **No back-fill.** `LIMIT` is applied before the graph filter, so orphans consume slots; sub-`k` returns are unconditional and an all-orphan top-`k` deadlocks (`live == 0` blocks the deletion that would clear it). Fix is over-fetch-and-filter, or push liveness into SQL. (4) **The GET is destructive on an unauthenticated surface** (bare mux, no middleware, `k` up to `maxSearchK` = 100), and 0008 grants DELETE to `arborette_orchestrator`, widening 0005's SELECT-only stance on that role — so prefetch, crawlers, and retries can trigger deletion. Accepted for the localhost-only trust model; revisit with the web-tier-hardening and orchestrator-auth entries above. The audit gap is **not** closable inside `heuristics.Service` — 0005 gives the service role no privilege on `audit_log` and `cmd/mcpserver` wires the Service as that role, so auditing belongs at the orchestrator handler (return retired ids from `Query`, let the Server record them).
- **Noted**: 2026-07-28

### Give the heuristic similarity search a relevance floor (or drop free-text search)

- **Type**: plan
- **Category**: feature
- **Where**: `internal/store/embeddingstore.go` (`SimilaritySearch`), `internal/heuristics/service.go` (`Query`), `internal/orchestrator/heuristics.go` (`defaultSearchK`), `web/app/heuristics/page.tsx`
- **Why**: `SimilaritySearch` is unbounded k-NN (`ORDER BY embedding <=> $1 LIMIT $2`), so it always returns `min(k, corpus)` rows however distant — an unrelated query gets the whole corpus back, ranked, with nothing marking it as unrelated. Measured 2026-07-28 on a clean 25-heuristic corpus: gibberish ("zzz quantum banana unrelated nonsense") best distance **0.4986** vs on-topic queries **0.4236–0.4634**, so the signal is real but the margin is ~0.05 and a cutoff near 0.48–0.50 is calibratable today. The narrowness is structural, not a tuning accident: the Sleep Cycle's abstraction stage *enforces* de-lexicalization (`LeakedConcreteTerms` plus a repair loop reject any definition naming real columns, yielding `[Origin Category]`/`[System Output]` prose), which strips exactly the domain vocabulary a free-text query matches on — the engine deliberately destroys the signal the search depends on. So decide the affordance before tuning a constant: a distance floor is the cheap fix, but filtering by objective/goal, or embedding goal context alongside the definition, may fit the data model better. Related: goal-scoping the read path is also the third part of the stored-prompt-injection entry above. Watch for the confound seen live — a corpus derived from a single dataset can only return that dataset's heuristics, which looks like bad ranking but is coverage.
- **Noted**: 2026-07-28

### Stop integration tests accumulating fixture nodes in the shared dev Neo4j

- **Type**: plan
- **Category**: testing
- **Where**: `internal/testutil/testutil.go` (`TruncateEmbeddings` and a missing graph counterpart), fixtures in `internal/graph/graph_test.go:87,145`, `internal/graph/sleepcycle_test.go:162`, `internal/heuristics/heuristics_test.go:49`, `internal/mcpserver/tools_integration_test.go:51`, `internal/llm/abstract_test.go:70`
- **Why**: Integration tests truncate pgvector but never remove the Neo4j nodes they create, so fixtures accumulate against the shared dev graph indefinitely. Measured 2026-07-28: **169 of 194** Meta-Heuristic nodes were test residue — `"abstraction"` ×97, `"reducing the alert threshold restores latency without degrading recall"` ×25, `"reducing threshold restores latency"` ×24, `"[Primary Population Center] raises [System Output]"` ×23 — leaving only 25 real heuristics, and 4 residue nodes had live embeddings so they ranked in every user-facing search. Beyond the noise, this asymmetry (pgvector truncated, graph persisted) is the mechanism that manufactures *both* drift directions tracked above, so fixing it removes the main local source of the reconcile entry's recurrences. Options: a `TruncateGraph` counterpart called alongside `TruncateEmbeddings`, per-test `t.Cleanup` deleting seeded ids, or giving integration tests their own database rather than sharing the dev instance — the last also stops `make test` wiping embeddings out from under a running stack, which is how the 23 vector-less heuristics were produced.
- **Noted**: 2026-07-28
