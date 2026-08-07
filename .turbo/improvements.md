# Improvements

Out-of-scope improvement opportunities captured during work sessions. Review periodically and pull items into active work when appropriate.

Entries are removed once shipped — this file lists only open work, in the order it was noted. An entry that shipped in part keeps only its unshipped remainder.

### Scope `data_source_ref` per goal/tenant at the Sandbox boundary

- **Type**: plan
- **Category**: reliability
- **Where**: `internal/sandbox/server.go`, `internal/store/goalregistry.go` (`DataSourceRefExists`)
- **Why**: The wired ref validator is only a global registry-existence check (`internal/store/goalregistry.go:154`), which binds a ref to "minted at ingest" but not to a specific goal or tenant — so any authenticated caller can introspect or execute against any registered object. The other three parts of the original boundary-hardening entry shipped in `ab9a5e3` (bearer auth on all routes, per-request concurrency cap, `MaxBytesReader` + pre-staging allowlist rejection); this is the remainder, and it is the one that needs a tenancy model rather than a middleware.
- **Noted**: 2026-07-21

### Extract LiveRun's decision logic into pure, unit-testable reducers

- **Type**: plan
- **Category**: testing
- **Where**: `web/components/LiveRun.tsx` — `resolveSilence`, `reconcileStatus`, `handleEvent`, `onStreamEnd`
- **Why**: The trickiest logic in the web UI — disambiguating a silent stream into complete/starting/neutral, the `loop_complete`→completed flip, the triggered-run reconnect, and reconciling server-persisted run status with the live SSE stream on reopen — is inlined into `useCallback`s closed over refs, so it is reachable only through a rendered component and is entirely untested. It was verified by trace + live preview + code review only. Lifting the pure decisions into `web/lib` helpers (following the `runState.ts` + `runState.test.ts` precedent) would make every branch testable with the existing Vitest harness, upholding four invariants: no neutral-flash duplicate-run, no crashed-running trap, no latency race, no stale failure Callout. Marked `plan` because the timer orchestration isn't cleanly extractable without a fake-timer harness the project lacks, so the extraction boundary needs deciding first. Deferred deliberately both times it came up: refactoring this immediately after the logic stabilized carried risk not worth taking inline.
- **Noted**: 2026-07-23 (two entries merged 2026-08-07)

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
- **Why**: The read path now skips similarity hits whose graph node is gone and retires their pgvector rows, gated on one live sibling in the same batch. Each risk below was surfaced in review and **deliberately accepted** — these are not oversights, and the two disputed windows are recorded so they are not re-litigated. (1) **The guard is per-batch, not per-goal.** `cmd/sleepcycle/main.go:30` requires `-goal`, so repair granularity is per-goal: re-run goal A but not goal B after a graph reset and a live hit from A corroborates permanently deleting B's embeddings. *Disputed, do not re-raise:* there is no `NEO4J_DATABASE` in the codebase, and a wrong `NEO4J_URI` gives a wholly foreign graph where `live == 0` and the guard correctly holds; replica lag is unreachable on the single-instance `neo4j:5`, and bookmarks would not help anyway since write and read live in different processes. (2) **TOCTOU on the delete.** The miss is observed at the top of the loop and the delete runs after the batch, without re-observing the miss, with nothing serializing sleepcycle against read traffic; deterministic `derivedID` means a concurrent repair's fresh embedding can be deleted, leaving `embedding_pending = false` with no vector. Fix is *not* a Go-side timestamp (Go and Postgres clocks skew) — return the observed `(node_id, updated_at)` from `SimilaritySearch` and delete `WHERE node_id = $1 AND updated_at = $2`. (3) **No back-fill.** `LIMIT` is applied before the graph filter, so orphans consume slots; sub-`k` returns are unconditional and an all-orphan top-`k` deadlocks (`live == 0` blocks the deletion that would clear it). Fix is over-fetch-and-filter, or push liveness into SQL. (4) **The GET is destructive on an unauthenticated surface** (bare mux, no middleware, `k` up to `maxSearchK` = 100), and 0008 grants DELETE to `arborette_orchestrator`, widening 0005's SELECT-only stance on that role — so prefetch, crawlers, and retries can trigger deletion. Accepted for the localhost-only trust model; revisit with the web-tier-hardening and orchestrator-auth entries above. The audit gap is **not** closable inside `heuristics.Service` — 0005 gives the service role no privilege on `audit_log` and `cmd/mcpserver` wires the Service as that role, so auditing belongs at the orchestrator handler (return retired ids from `Query`, let the Server record them).
- **Noted**: 2026-07-28

### Stop integration tests accumulating fixture nodes in the shared dev Neo4j

- **Type**: plan
- **Category**: testing
- **Where**: `internal/testutil/testutil.go` (`TruncateEmbeddings` and a missing graph counterpart), fixtures in `internal/graph/graph_test.go:87,145`, `internal/graph/sleepcycle_test.go:162`, `internal/heuristics/heuristics_test.go:49`, `internal/mcpserver/tools_integration_test.go:51`, `internal/llm/abstract_test.go:70`
- **Why**: Integration tests truncate pgvector but never remove the Neo4j nodes they create, so fixtures accumulate against the shared dev graph indefinitely. Measured 2026-07-28: **169 of 194** Meta-Heuristic nodes were test residue — `"abstraction"` ×97, `"reducing the alert threshold restores latency without degrading recall"` ×25, `"reducing threshold restores latency"` ×24, `"[Primary Population Center] raises [System Output]"` ×23 — leaving only 25 real heuristics, and 4 residue nodes had live embeddings so they ranked in every user-facing search. Beyond the noise, this asymmetry (pgvector truncated, graph persisted) is what manufactured the graph/pgvector drift the reconcile pass now heals, so fixing it removes the main local source of that drift. Options: a `TruncateGraph` counterpart called alongside `TruncateEmbeddings`, per-test `t.Cleanup` deleting seeded ids, or giving integration tests their own database rather than sharing the dev instance — the last also stops `make test` wiping embeddings out from under a running stack.
- **Noted**: 2026-07-28

### Bound and authenticate the agent chat endpoint

- **Type**: plan
- **Category**: reliability
- **Where**: `internal/orchestrator/chat.go` (`handleChat`), `internal/service/httpserver.go` (`RunHTTPServer`)
- **Why**: `POST /goals/{id}/chat` is effectively an open LLM proxy on the operator's `ANTHROPIC_API_KEY`. It accepts a caller-authored transcript — including forged `assistant` turns — and streams raw model text back, reachable by anyone who can reach :8080 with a goal id from the unauthenticated `GET /goals`. Each request authorizes up to `maxChatResumes+1` generations of `chatMaxTokens` and holds a connection for the whole stream, while `RunHTTPServer` sets only `ReadHeaderTimeout` and no write timeout; the sole limit is the 1 MiB body cap. Deliberately deferred when the chat backend shipped (2026-07-28) because no orchestrator route has auth and the localhost trust model is intentional — see the README roadmap's service-boundary bullet. Wants a per-IP or global concurrency cap on this handler, a per-message content cap, and a write timeout, alongside whatever auth model the orchestrator eventually adopts.
- **Noted**: 2026-07-28

### Guard handleTriggerLoop against concurrent runs for one goal

- **Type**: direct
- **Category**: reliability
- **Where**: `internal/orchestrator/hypothesis.go` (`handleTriggerLoop`), `internal/store/runs.go`
- **Why**: The trigger creates a run row and launches the loop goroutine unconditionally, so two triggers for one goal (double-click, scripted retry) produce two live runs whose `triplet`/`loop_complete` frames interleave on the shared goal-keyed hub stream, and any goal-keyed in-memory state collides (the arborette-08 plan's histogram registry defends itself with compare-and-delete deregistration, but the underlying collision is pre-existing). Check the runs table for a `running` row and return 409 at the entry point. Surfaced during arborette-08 plan review, deliberately left out of that plan's scope.
- **Noted**: 2026-07-29

### Migrate the stored provenance property to one wire shape

- **Type**: plan
- **Category**: dx
- **Where**: `internal/graph/neo4j.go` (`marshalProvenance`), `internal/domain/domain.go` (`ProvenanceLocator`)
- **Why**: The SSE half of this is done — the `triplet` frame now publishes through `toProvenanceDTO`, so every wire surface is snake_case. The stored Neo4j `o.provenance` property still holds Go field names (`{"Page":0,"CharStart":42,"CharEnd":55}`) because `marshalProvenance` serializes the tagless `ProvenanceLocator` straight in. Adding json tags to the struct is still unsafe on its own: it rewrites the on-disk keys and breaks reads of every existing Outcome. Needs a migration decision (backfill vs. tolerant reader) rather than a tag change.
- **Noted**: 2026-07-29

### Close the four seams left by the HITL verification backend

- **Type**: plan
- **Category**: reliability
- **Where**: `internal/orchestrator/{server.go,hypothesis.go,verification.go}`, `internal/graph/neo4j.go`, `internal/store/verificationqueue.go`
- **Why**: Four review findings deliberately deferred as too broad for that commit. (1) `NewServer` now takes 17 positional params including two adjacent strings — a `Deps` struct would make every argument self-labelling and let `verificationPoll`/`keepaliveInterval` be real optional fields rather than post-construction pokes in tests. (2) A blocking-mode goal swaps the *whole run's* deadline to 24h, so sandbox/Claude/graph calls inherit it too — a wedged dependency hangs 24h instead of failing at 30 min; bounding the waits separately needs the run-context handling restructured. (3) `compensateClaim` checks the graph then un-claims as two steps across two stores, so a repairer landing the verdict in between leaves graph=resolved/queue=pending — reads as unreviewed while already search-eligible, and a later different verdict would overwrite it (needs a failed graph write *plus* a concurrent duplicate resolution to reach; the code comment now states the window rather than claiming it closed). (4) Dedups: `GetExtractionOutcome` reimplements `getNode`'s collect/zero-one-many/`ErrNotFound` protocol including its driver-quirk rationale; `store.marshalLocator` duplicates `graph.marshalProvenance`; and the run-confidence histogram sits in `verification.go` though the tabular loop uses it and HITL does not.
- **Noted**: 2026-07-29

### Bound and cache the document page text behind the HITL excerpt/correction endpoints

- **Type**: plan
- **Category**: performance
- **Where**: `internal/orchestrator/verification.go:178,392`, `internal/sandbox/documentsource.go` (`pdfPages`), `internal/sandboxclient/client.go` (`DocumentText`), `internal/orchestratorclient/client.go` (`do`)
- **Why**: The sandbox is stateless, so every HITL excerpt view and every correction re-downloads and re-parses the whole PDF in a synchronous cross-service round trip — an analyst paging an N-entry queue pays N full parses of an immutable document. It is also unbounded in memory, not just slow: `pdfPages` accumulates every page's full text with no cap (`documentSample` bounds only the intake response), the clients' `json.Decode` calls read uncapped (`service.DrainAndClose` bounds only discarded bytes), and source PDFs are allowed up to 512 MiB of Flate-compressed content — so concurrent excerpt GETs can balloon the process holding the audit-writing Postgres role and the Anthropic key. A plain `io.LimitReader` cap was considered and rejected during the V2 seam work: any constant below 512 MiB newly breaks excerpt for documents that register fine today. Fix both at once — a page-range parameter on `/document/text` makes the response bounded by construction, and a goal-scoped page-text cache (documents are content-addressed by `data_source_ref` and never change) removes all but the first parse.
- **Noted**: 2026-07-29 (updated 2026-07-31)

### Extract the duplicated presentational patterns in web/components

- **Type**: plan
- **Category**: refactor
- **Where**: `web/components/{ui.tsx,VerificationQueue.tsx,HeuristicBrowser.tsx,ObjectivesList.tsx,GoalForm.tsx,AgentChat.tsx,CausalGraph.tsx,CausalVerifications.tsx}`
- **Why**: Five patterns are copied rather than shared: the segmented control, the text-input/select class string, the master/detail list scaffold with its loading/empty/placeholder panels, the fetch-on-mount `cancelled`-guard effect, and the status badge. The badge is the one with teeth — components render the same status under divergent tone maps, so one outcome can show neutral on one screen and positive on another. Deferred deliberately as its own refactor, since it reverses an established local-helper convention across files.
- **Field class string, now at seven call sites** (the causal surfaces added the seventh) with its cost demonstrated: a Tailwind v4 `outline-none` → `outline-hidden` sweep had to touch six by hand. Extract a size-free `FIELD_BASE` constant holding only the verbatim-identical part (`rounded-lg border border-line bg-surface text-fg outline-hidden transition-colors focus:border-signal/60 focus:ring-2 focus:ring-signal/20`), consumed as `cn(FIELD_BASE, …)` with padding/width/placeholder left per call site — mechanical, zero visual change. Avoid an `Input`/`Select` component with a size prop: the seven sites carry five distinct padding combinations across three element kinds, so a size enum deliberately changes spacing at some of them, and `cn` has no tailwind-merge, so a base carrying padding resolves conflicts by stylesheet order rather than class order.
- **Noted**: 2026-07-30, field-string detail added 2026-08-05

### Boot-time internal-auth probe in the Sleep-Cycle worker

- **Type**: direct
- **Category**: reliability
- **Where**: `cmd/sleepcycle/main.go`, `internal/sleepcycle/worker.go:416-422`
- **Why**: `worker.report` intentionally logs-and-swallows audit `Append` failures, so a misconfigured/mismatched `INTERNAL_AUTH_TOKEN` silently drops the entire audit trail — including `sleepcycle_run_complete`, the only carrier of Phase-2 winners. One authenticated probe call at worker startup would fail loudly at boot instead. Belongs with worker boot/serve wiring, deliberately kept out of the shell that introduced the token.
- **Noted**: 2026-07-30

### Eval framework measuring agent accuracy uplift from heuristics/causal traces vs baseline LLM

- **Type**: plan
- **Category**: feature
- **Where**: agent preview / chat surface (`internal/orchestrator/chat.go`) + MCP read tools (`get_optimized_heuristics`, `trace_causal_chain`); a new eval harness + labeled question sets per dataset (e.g. Spaceship Titanic transport questions)
- **Why**: There is no way to quantify whether the discovered heuristics and causal traces actually improve answer quality. An eval harness that runs a fixed question set through the agent (with heuristic/causal-trace retrieval) and through the bare baseline LLM, scoring accuracy on each, would measure uplift over time and guard against regressions — e.g. transport-related questions on the Spaceship Titanic corpus should score higher with the heuristics than without. Enables tracking model/prompt/heuristic changes against a baseline rather than eyeballing.
- **Update (2026-08-07)**: raised in priority by a whole-repo assessment. This is the only measurement that tests the product thesis, and nothing else in the backlog substitutes for it. The one quantitative result the project has is the R14 calibration gate (UCT vs beam, ~33% more winners), which was measured on a single ground-truth dataset with cross-goal grounding contributing nothing — so *no* measurement anywhere shows that a heuristic from goal A improved goal B, or that an agent answers better with the corpus than without. Until this exists, "search quality compounds as knowledge accumulates" (V2 spec Overview) is an untested claim, and the two entries below it here — the distance floor and heuristic demotion — cannot be evaluated either, since neither has a success metric to move.
- **Noted**: 2026-08-02

### Index ontology-term mapping / goal text so cross-goal heuristic search can rank by domain query

- **Type**: plan
- **Category**: feature
- **Where**: `internal/sleepcycle` (abstraction persists concrete↔ontological pairs — R15), `internal/embedding` + `internal/heuristics` (Query), `internal/orchestrator/heuristics.go` (search)
- **Why**: Meta-Heuristic definitions are domain-abstracted (`[Categorical Attribute]`, `[System Output Rate]` — never "GPA"/"sleep"), so a domain query matches nothing on-topic: measured cosine distances compress into a flat ~0.38–0.52 band and the larger legacy corpus wins the top-k by sample size, not relevance (153 legacy rows buried a goal's 13 on-topic ones). Goal-scoping is the immediate fix, but cross-goal browse-by-question needs the abstract definition to carry a concrete surface — index the persisted ontology-term concrete↔ontological pairs and/or the goal text (e.g. a second embedding) so domain queries have something to land on.
- **Noted**: 2026-08-03

### Distance floor (0.5) cuts off the entire abstracted heuristic corpus for reasonable domain queries

- **Type**: plan
- **Category**: reliability
- **Where**: `internal/config` (`EMBEDDING_DISTANCE_FLOOR`, 0.5 provisional), `internal/store/embeddingstore.go` (`SimilaritySearchScored`, floor applied in both scope branches), `internal/sleepcycle/grounding.go` (`groundProposals`), relates to the ontology-term-mapping entry above
- **Why**: A goal-scoped search for "Does sleep impact GPA" on a goal whose 13 heuristics are literally the sleep→GPA ones returns ZERO results: measured, all 13 sit at 0.527+ cosine distance (0/13 under the 0.5 floor), while the reworded "biggest impacts on GPA" sits at 0.41 (13/13 under). Abstracted definitions have no shared domain terms with natural-language queries, so they live at high absolute distances and a floor tuned for concrete text produces empty results for valid queries — a near-miss (0.527 vs 0.5). Levers: recalibrate/relax the floor for the abstracted corpus (or drop it when goal-scoped, where there's nothing to protect against), and/or index concrete terms (see ontology-term entry) to pull distances under the floor. Calibrate against the R30 dataset.
- **Update (2026-08-07, stakes are higher than the search box)**: the floor is applied inside `SimilaritySearchScored` on **both** scope branches (`internal/store/embeddingstore.go:266-278`), and that is the same method `groundProposals` calls (`internal/sleepcycle/grounding.go:71`) — so the floor gates the Phase-2 knowledge-reuse path, not just the analyst/MCP read path. Grounding embeds **goal text** and matches it against **abstracted definitions**: precisely the vocabulary mismatch that produced the 0.527 measurement. If that distance is representative, `refs` comes back empty, `groundProposals` returns nil, and R15/R16 — grounded proposals, similarity priors, causal-evidence weighting — are a silent no-op on every real goal, while the run still reports success. No test catches it because every sleepcycle test uses a fake embeddings store that ignores the floor. **Measure first, tune second**: embed a few real goal texts and query the live corpus cross-goal, and record whether the reuse path returns anything at all today. That measurement decides whether this is a tuning nit or the reason the compounding-knowledge thesis has never been observed.
- **Noted**: 2026-08-03

### Make the heuristic-browser goal selector a typed autocomplete combobox

- **Type**: plan
- **Category**: feature
- **Where**: `web/components/HeuristicBrowser.tsx` (scope `<select>`)
- **Why**: The scope selector renders every goal as a native `<select>` option; with many goals that's a long, clunky list to scroll. A typed autocomplete/combobox (filter-as-you-type over goal text, keyboard-navigable, accessible) keeps selection fast as the goal count grows, and matches how analysts think about goals (by phrase, not position).
- **Noted**: 2026-08-03

### Correction-triggered staleness cannot invalidate what an added confounder breaks

- **Type**: plan
- **Category**: reliability
- **Where**: `internal/orchestrator/corrections.go` (`reverifyStale`), `internal/store/causalverifications.go` (`MarkStale`), incidence data in `internal/verifier/adjust.go` (`adjustmentColumns`)
- **Why**: `MarkStale` flags records whose *existing* `adjustment_set` names a corrected column, but an added confounder invalidates exactly the records whose adjustment set **lacks** it — that missing column is why they were wrong. So `add` (R7's flagship op, "add a known confounder edge the tests missed") and any `flip` that *creates* a new parent of a treatment column mark nothing stale and re-dispatch nothing, while the affected findings keep serving `causal_inferred` edges computed without the confounder. Only `delete` and parent-removing flips are caught. The fix needs the corrected edge's incidence with each record's treatment columns (re-derived from its intervention's filters), not set overlap — a new derivation plus a graph read per candidate record, which is why it was deferred rather than patched.
- **Noted**: 2026-08-04

### Extract the shared observational-triplet writer used by the Verifier and the Sleep Cycle

- **Type**: plan
- **Category**: refactor
- **Where**: `internal/verifier/claim.go` (`reifyClaim`), `internal/sleepcycle/writeback.go` (`writeSegment`), destination likely `internal/graph`
- **Why**: The two are near-verbatim copies: same baseline `State` property map, same `Intervention` keys, same `Outcome` shape, same five-write ordering, same `EffectSize = value − baseline, Confidence 1.0, EpistemicObservational` edge — differing only in role constants and the provenance flag. The ordering is load-bearing and documented in only one of them: a zero-row Cypher `MATCH` is not an error, so an edge write against a node that failed to write silently links nothing. A sixth write or a changed edge semantic must now land in two packages, and a miss is invisible because `adjust.go`, `promote.go`, and `sleepcycle/publish.go` all read these property maps by key. The id half of this duplication was already consolidated into `domain.CanonicalFilters`/`DerivedID`; this is the write half. Deferred because extracting it modifies shipped Sleep-Cycle write-back code and its tests.
- **Noted**: 2026-08-04

### Chain Neo4j bookmarks across the multi-session graph read-modify-writes

- **Type**: plan
- **Category**: reliability
- **Where**: `internal/graph/neo4j.go` (`read`/`write` session construction), affecting `CorrectCausalEdge` and the Verifier's discovery `persist` (columns→edges→meta)
- **Why**: Every multi-step graph write spans several independent sessions built with a bare `neo4j.SessionConfig{AccessMode: …}` — no `Bookmarks`, no `BookmarkManager` — and the driver guarantees read-your-own-writes only *within* one session. Inert today (single `neo4j:5` container, direct `bolt://` URI, so every session lands on the same instance), but against a `neo4j://` routing URI or a cluster, `CorrectCausalEdge`'s copy-forward could read a pre-correction edge set and silently drop an earlier analyst's edit — the exact failure its advisory lock exists to prevent — and could commit a version-bumped meta on a member that has not applied the edge writes, so `GetCausalGraph` serves a version with no edges. The Postgres advisory lock serializes writers but cannot order Neo4j sessions. Fix is repo-level: thread `session.LastBookmarks()` or a repository-scoped `BookmarkManager` through the read→write chain.
- **Noted**: 2026-08-04

### Bound causal-graph version growth from repeated corrections

- **Type**: plan
- **Category**: performance
- **Where**: `internal/graph/causal.go` (`CorrectCausalEdge`), `internal/orchestrator/corrections.go`
- **Why**: Each accepted correction copies the entire edge set forward at `version+1` and nothing ever reclaims the prior version: `deleteCausalEdgesAtVersion` only clears torn writes at the *target* version, and `DeleteCausalGraphVersion` is reached solely from discovery, which early-returns once a meta exists. So every correction permanently adds `|E|` `CAUSES` relationships (plus a `causal_verifications` row per re-dispatch, since the version bump defeats coalescing). Alternating flips (`A→B`, then `B→A`) are always "matched" and so always accepted, giving an unbounded growth path on an unauthenticated endpoint; on a 100-edge graph, 10k corrections is a million relationships. Copy-forward is the plan's deliberate mechanism (it is what makes a deleted edge durable without a tombstone), so the fix is a retention policy — delete `version-1`'s edges once the new meta commits, or cap retained versions — not a change to the write shape.
- **Noted**: 2026-08-04

### Subsample refutation is broken for every finding with a non-empty adjustment set

- **Type**: direct
- **Category**: reliability
- **Where**: `internal/sandbox/compile.go:1198-1205` (the `AnalyzeSampledEffect` inner scan), error path through `internal/verifier/refute.go:54`
- **Why**: The inner scan appends `USING SAMPLE …` to the table function and only then appends `WHERE`, producing `FROM read_csv(…) USING SAMPLE 70.0000% (reservoir) WHERE "Z" IS NOT NULL` — which DuckDB rejects with `Parser Error: syntax error at or near "WHERE"`. `nullPreds` is populated once per `req.Adjust` entry, so the `WHERE` clause exists **exactly when the adjustment set is non-empty**: the K=20 subsample stability refutation therefore fails for precisely the confounded findings causal verification exists to test, while Z-less findings pass, which is why every run on the seed data looked healthy. The verification surfaces as `failed: internal error` and is then lease-reaped, so the analyst sees no verdict at all rather than a refutation score. Observed live 2026-08-04 on `causal-demo.csv` after adding a `Z→A` confounder edge to a verify-track goal. Committed in `c0f1809` (shell 07), independent of the router change. Fix is a subquery or a `WHERE`-before-`USING SAMPLE` reordering, plus a compile test that pins a sampled effect with a non-empty adjustment set — the existing analyze tests only cover the Z-less shape.
- **Update (2026-08-07, re-verified and worse than "one refutation degrades")**: still present at `compile.go:1198-1205`, unchanged. Traced the blast radius: `stability` returns the error and `refute` propagates it at `refute.go:54` with no degrade-to-partial arm, so this does not cost one refutation score — it **fails the whole verification**. Net effect on the product: Engine B returns a verdict only for findings with an empty adjustment set, i.e. exactly the cases where backdoor adjustment is a no-op and the adjusted effect equals the naive one. The spec's headline capability ("a spurious correlation Engine B should kill", R30) is therefore unexercised in production. This is `direct`-typed and three days old; it should jump the queue ahead of every other entry in this file.
- **Noted**: 2026-08-04

### Extract a Pin-and-bind helper so a windowed objective cannot lose its window columns

- **Type**: direct
- **Category**: refactor
- **Where**: `internal/verifier/claim.go`, `internal/verifier/adjust.go`, `internal/sleepcycle/worker.go`, `internal/orchestrator/hypothesis.go` (two sites)
- **Why**: `objective.Pin(goal.EvaluationMatrix)` followed by two manual assignments of `EntityKeyColumn`/`TimeColumn` from the goal row is repeated at five sites across three packages — twice inside `internal/verifier` alone. The bindings live on the goal rather than the matrix, so `Pin` cannot see them, and a caller that forgets the two lines gets a windowed objective that compiles against nothing and fails at the sandbox rather than at the type checker. A `Pin`-adjacent helper taking `(matrix, entityKey, timeColumn)` makes the next dispatch kind correct by construction.
- **Noted**: 2026-08-04

### Settle one trust rule for the dispatched datasource_ref across Verifier dispatch kinds

- **Type**: plan
- **Category**: reliability
- **Where**: `internal/verifier/claim.go` (`VerifyClaim`), `internal/verifier/verify.go` (`VerifyOne`), `internal/verifier/worker.go` (`discover`)
- **Why**: `VerifyClaim` overwrites the wire `datasourceRef` with the goal row's and documents the override as load-bearing ("measuring a goal's claim against another goal's data would persist a finding on this goal's graph that describes neither"). Its two sibling dispatch paths take the wire ref at face value and use it for graph-version resolution, introspection, and every persisted `DataColumn`/`CausalEdge`/`CausalGraphMeta` key — `discover` even loads the same goal row and could compare. Either the argument holds for all three (in which case the override belongs where the goal is loaded, once) or for none; as written a reader cannot tell which rule a next dispatch kind should follow.
- **Noted**: 2026-08-04

### Type the goal Track and group NewServer's collaborators into a struct

- **Type**: plan
- **Category**: refactor
- **Where**: `internal/domain/claim.go`, `internal/store/goalregistry.go`, `internal/llm/classify.go`, `internal/orchestrator/server.go`, `cmd/orchestrator/main.go`
- **Why**: Two convention gaps the router change widened. `Track` is an untyped string while every other cross-boundary enum `domain` owns is a defined string type (`TargetDirection`, `VerificationStatus`, `EpistemicSource`, `EdgeDirection`) and `store` follows suit with `EpochMode` — so a field whose whole purpose is routing carries no type-level signal about its constant set. And `NewServer` now takes 22 positional parameters including two adjacent `JobLauncher` values: swapping them in `main.go` compiles cleanly and silently routes sleep-cycle jobs to the Verifier and back. `RouterConfig` and `server_test.go`'s `testServer` already apply the remedy one level down; production wiring is the only place still exposed.
- **Noted**: 2026-08-04

### Compose the claim filter-shape prompt from filterShapeGuide instead of restating it

- **Type**: direct
- **Category**: reliability
- **Where**: `internal/llm/classify.go` (`claimShapeGuide`, `classifyGoalIntentSystem`), `internal/llm/client.go` (`filterShapeGuide`, `interventionTreeSystem`)
- **Why**: `claimShapeGuide` restates `filterShapeGuide` almost word for word — the `{"field","op","value"}` shape sentence and the whole per-column-type operator/value sentence — and the column-grounding clause duplicates `interventionTreeSystem`'s. Both prompts feed the same shared `filterWire` decoder, which `decodeClaimFilters` documents as the reason for sharing ("a second decoder here would let the two drift apart"); the decoder is shared but the prose that grounds it is now forked, so an operator or value-encoding change updates one prompt and leaves the other describing the old contract to the same model.
- **Noted**: 2026-08-04

### Promote canonical_filter and value to domain.Prop constants

- **Type**: direct
- **Category**: reliability
- **Where**: `internal/domain/domain.go` (`Prop*` block), `internal/verifier/claim.go`, `internal/sleepcycle/writeback.go`, `internal/sleepcycle/abstract.go`, `internal/sleepcycle/search.go`
- **Why**: The `Prop*` block exists so a node property key crossing a service boundary is a compile error to misspell, and `PropClaimDerived` was just added to it. The adjacent keys in the very same property maps — `"canonical_filter"` and `"value"` — stay bare literals, now written from six sites and, since the claim reification, from a second service. The claim triplet and the sleep-cycle triplet agree only by coincidence of typing, and a drift degrades silently rather than failing.
- **Noted**: 2026-08-04

### Align auto-promotion's eligibility gate with publication's

- **Type**: plan
- **Category**: reliability
- **Where**: `internal/orchestrator/promote.go` (`rankFindings`), `internal/sleepcycle/publish.go` (`candidatesFromFindings`)
- **Why**: `domain.ShrunkScore` was hoisted so "publication selection and verification auto-promotion must agree on which findings are the strongest". The two callers still diverge on which findings are rankable at all: publication drops anything failing `objective.Improves(baseline, value, direction)` and anything under `MinSupport`, while promotion drops only `support <= 0` and undecodable filters. A goal producing fewer than `AutoPromoteTopN` improving segments therefore spends verification budget on segments that move the objective the wrong way — ones publication would never surface. The surrounding loop is structurally identical in both, which makes the divergence easy to miss on inspection.
- **Noted**: 2026-08-04

### Close the remaining router coverage gaps (config sentinels, body cap, claim-decode asymmetry)

- **Type**: direct
- **Category**: testing
- **Where**: `internal/orchestrator/promote.go` (`topN`), `internal/orchestrator/corrections.go`, `internal/store/goalregistry.go`, `internal/verifier/claim.go`
- **Why**: Four unpinned behaviours. `topN` treats `n < 0` as unlimited and `n == 0` as nothing, mirrored by `reverifyStale`'s `limit >= 0` guard, and both values come straight from unclamped operator env (`ORCHESTRATOR_AUTOPROMOTE_TOP_N`, `ORCHESTRATOR_STALE_REVERIFY_CAP`) with only positive values ever tested — a mutation swapping the sentinels goes undetected. `Insert`'s `claim_error` parameter is never read back through the real registry. The correction endpoint's `MaxBytesReader` can be deleted with the suite green, on a route that takes no credential. And an undecodable stored claim is a hard dispatch error in `VerifyClaim` but a logged "absent" in `decodeClaim`, with neither behaviour tested, so it is unclear which is intended.
- **Noted**: 2026-08-04

### Goal-intent classification is unreliable enough that verify-track goals often silently become explore

- **Type**: investigate
- **Category**: reliability
- **Where**: `internal/llm/classify.go` (`decodeGoalIntent`, `classifyGoalIntentSystem`), `internal/orchestrator/submit.go` (`classifyIntent`)
- **Why**: Observed live 2026-08-04: registering the *same* verify-track goal text three times against `causal-demo.csv` produced verify only once. One attempt failed with `parse claim filters: invalid character 'p' looking for beginning of value` (the model emitted prose in the JSON-encoded `claim_filters` string), another returned verify with an empty `claim_filters` — both are hard errors that fall open to explore, which is the deliberate design but is indistinguishable to the analyst from "your goal was not a claim". Separately, an ungrounded claim (naming a missing column or value) reliably comes back as verify-with-empty-filters rather than as ungrounded filters, so the cannot-construct outcome and its `claim_error` almost never fire at intake — the Verifier's dispatch-time re-validation is the path that actually reaches it. Worth measuring the classifier's verify-recall before adding a repair round-trip or reporting the fall-open more loudly.
- **Noted**: 2026-08-04

### Rename the verifyGoal() test helper — it returns an explore-track goal

- **Type**: direct
- **Category**: testing
- **Where**: `internal/orchestrator/causalverify_test.go` (declaration), with call sites in `corrections_test.go` and `promote_test.go`
- **Why**: The name refers to the causal *verification* surface the helper was written for, not the routing track, but to anyone working on the router it reads as "a verify-track goal" — and it returns `Track: store.TrackExplore`. That produced a vacuous test in this session: a case asserting the claimless-verify-track promotion fallback used it without setting `Track`, so the branch under test was never entered and the mutation that should have killed the test survived; only re-running the mutation caught it. `verifyTrackGoal(t, field)` in `promote_test.go` is the helper that actually sets the track. A track-neutral name (`registeredGoal()`) removes the trap; ~a dozen mechanical call sites.
- **Noted**: 2026-08-04

### Consolidate the duplicated schema/column-value adapters into internal/sandboxclient

- **Type**: plan
- **Category**: refactor
- **Where**: `internal/sleepcycle/grounding.go` (`llmSchema`, `distinctValues`), `internal/orchestrator/hypothesis.go` (`toSandboxSchema`, `columnValues`), `internal/verifier/claim.go` (inline names+values loop in `groundingFailure`)
- **Why**: Three near-identical copies of two adapters. The orchestrator's `schemaDTO` is a type alias for `sandboxclient.Schema`, so its adapter and sleepcycle's have identical signatures *and* byte-identical bodies under two names; the value indexer exists three times, named `distinctValues` in one package and `columnValues` in another. Both feed `domain.UnknownFilterColumns`/`UnknownFilterValues`, which live in `domain` precisely because several services must agree on the grounding check — the adapters that build their arguments belong together too. Verified there is no import-cycle obstacle: `internal/llm` imports only `domain` and `config`, so hosting them in `internal/sandboxclient` (adding a `sandboxclient → llm` edge) is acyclic. `internal/domain` is not viable — it would have to name `llm.SandboxSchema`, breaking its dependency-free rule, which is why `UnknownFilterValues` takes a plain `map[string][]string`. Deferred from the knowledge-guided sleep-cycle change because consolidating would widen it into two more services.
- **Noted**: 2026-08-04

### Reuse heuristics.Service for the Sleep Cycle's retrieve-then-hydrate instead of copying it

- **Type**: plan
- **Category**: refactor
- **Where**: `internal/sleepcycle/grounding.go` (`groundProposals`), `internal/heuristics/service.go` (`Query`, `retireOrphans`), `internal/store/embeddingstore.go` (`SimilaritySearchScored`)
- **Why**: `groundProposals` reproduces `Service.Query` statement for statement — embed the query, similarity-search, batch `GetMetaHeuristics`, index by id, walk in retrieval order rather than graph order — down to restating the same "walk the search's order, not the batch's" comment in both places. The copy also drops the self-healing the original has: `Query` retires a pgvector row whose graph node is gone (gated on one live sibling), while grounding skips the `!ok` lookup and leaves the orphan to be retrieved again on every future run — so the reuse path re-pays for a dead row indefinitely and the corpus never converges. `Service` already owns both collaborators and `SimilaritySearchScored` now exists, so adding a scored variant there and calling it from the Sleep Cycle gives the reuse path orphan retirement for free and leaves one implementation. Deferred from the knowledge-guided sleep-cycle change because it restructures the grounding path and moves collaborator ownership.
- **Noted**: 2026-08-04

### Reject DuckDB composite (LIST/ARRAY) column types in the shared classifier, and decide on INTERVAL

- **Type**: direct
- **Category**: reliability
- **Where**: `internal/datasource/datasource.go` (`IsNumericType`, `IsTemporalType`, and the test table in `datasource_test.go`), consumed at `internal/sandbox/compile.go` (aggregate target check, numeric-threshold filter guard, `columnCoarseType`)
- **Why**: Verified against the pinned engine (DuckDB 1.4.1): a list column's `DESCRIBE` type is literally `DECIMAL(10,2)[]` / `TIMESTAMP[]`, `filesource.go` maps every DESCRIBE row through with no composite filter, and both spellings pass the `HasPrefix` tests. The compiler then admits the column and DuckDB rejects it at execution — `Binder Error: No function matches ... 'avg(DECIMAL(10,2)[])'` — which `writeStageErr` masks as a 500, where the whole point of compile-time validation is an analyst-fixable 400. Fix: reject a type containing `[` before the prefix tests. Separately `duckdb_types()` puts INTERVAL in the DATETIME category, which `IsTemporalType`'s DATE/TIME prefixes miss, so an INTERVAL column is refused as a window `ORDER BY` at registration though DuckDB orders intervals fine (Parquet-only; `read_csv_auto` never infers it) — either add it or say the omission is deliberate. Both predate the consolidation but belong here now that this package is the single documented owner and its test claims exhaustiveness. Deferred because closing them changes what the sandbox compiler accepts.
- **Noted**: 2026-08-04

### Bundle selectPolicy's run-scoped inputs into a struct

- **Type**: direct
- **Category**: refactor
- **Where**: `internal/sleepcycle/policy.go` (`selectPolicy`), with call sites in `worker.go`, `grounding_test.go`, `knowledge_run_test.go`
- **Why**: Nine positional parameters, two of which carry the same input (`findings`, and `findingAtoms` which is `buildAtoms(findings)`), one consulted for a single field (`obj.Direction`), and a `target` that already holds the goal id and data-source ref — so the callee re-splits what the caller joined. The cost is at the call sites: the production call runs past 120 columns and the test calls are positional soup. The package already has the model in `searchTarget` ("run-invariant addressing"), so a sibling `searchInputs` built once in `Run` and passed as `(ctx, target, inputs)` fixes it; a small `policyChoice{policy, atoms, skipReason}` return would additionally make the documented "a nil policy always comes with a reason" invariant expressible in the type. Raised in two independent review rounds and skipped both times as shape rather than defect — the semantic ambiguity in the middle return value was fixed separately. Same class as the `NewServer` positional-parameter entry above, different package and remedy.
- **Noted**: 2026-08-04

### Annotate causal-graph columns so an analyst can orient by them

- **Type**: plan
- **Category**: feature
- **Where**: `internal/orchestrator/causalgraph.go` (`causalColumnDTO`, `toCausalGraphDTO`), consumed by `web/components/CausalGraph.tsx` (`Node`, edge list) and `web/lib/causal.ts` (`layoutGraph`)
- **Why**: The graph endpoint exposes only `{name, kind}` per column, so the diagram can say nothing about a column beyond its name — and the node the objective actually measures is indistinguishable from its confounders. On an opaque schema (`A`, `X`, `Z` in the confounder fixture, but equally a warehouse table of coded columns) the analyst has nothing to orient by. Fix: carry the objective's columns (and ideally a short per-column description, which registration's schema introspection already has in hand) on the graph DTO, then mark the outcome node and annotate the rest. Raised while previewing the causal tab and deliberately deferred: the surfaces that would show it are web-only, the data to show is not.
- **Noted**: 2026-08-04

### Auto-promoted verifications can complete without persisting a record

- **Type**: direct
- **Category**: reliability
- **Where**: `internal/verifier/verify.go` (`VerifyOne`'s `ErrInflightCapReached` arm), `internal/store/causalverifications.go` (`inflightSlots`), with the knob pair in `internal/config/config.go` (`ORCHESTRATOR_AUTOPROMOTE_TOP_N` 5 vs `VERIFIER_INFLIGHT_CAP` 4)
- **Why**: On a fresh goal, auto-promotion dispatched 5 verifications and only 3 rows landed in `causal_verifications`. The two missing ones logged `started` and `complete` seconds *before* inline discovery finished, and left no row, no audit line and no error behind. The shipped defaults explain it exactly: `TOP_N` is 5, `VERIFIER_INFLIGHT_CAP` is 4, and a budgeted dispatch is held one slot below the cap (`inflightSlots`) to reserve a slot for an analyst — so three promotions can hold slots, the first of them running discovery inline for far longer than the others wait, and the remaining two are refused with `ErrInflightCapReached`. That refusal is invisible everywhere an operator would look: `VerifyOne` publishes a `verification_rejected` frame and returns nil, `PublishLive` drops the frame because no client is subscribed to a goal whose run just ended, and nothing is logged or audited. The analyst-visible symptom is a run that promotes N findings while the Causal tab shows fewer, with nothing explaining the gap. The fix is observability rather than a bigger cap — log or audit the refusal — plus a decision on whether promoting more findings than the cap can ever run is worth the wasted dispatches. Observed while smoke-testing the causal web surfaces; the UI matched the API, so this is orchestrator/verifier-side.
- **Noted**: 2026-08-05

### Execute the analyze surface's harder shapes in tests, not just its simplest ones

- **Type**: plan
- **Category**: testing
- **Where**: `internal/sandbox/analyze_test.go` (`TestAnalyzeEffectStaging`, `TestAnalyzeWireParity`), against the kinds compiled in `internal/sandbox/compile.go`
- **Why**: `TestAnalyzeEffectStaging` does run compiled SQL against real DuckDB through the server route, which is the right shape — but only for the easy cells of the request matrix. `SampleFraction` appears in exactly one test (`TestAnalyzeWireParity:37`), which compares struct fields and never executes, so **no test has ever run a `sampled_effect` query**, with or without an adjustment set; `RandomStratifierBins` is in the same position. That is precisely how a hard DuckDB parser error (the `USING SAMPLE`/`WHERE` entry above) shipped with a green suite and stayed green for three days. The gap is systemic rather than one missing case: the request matrix is (kind × adjustment-set empty/non-empty × sampling on/off × random-stratifier on/off) and executing coverage exists only where the first two are simplest. Add an executing case per non-trivial cell against the staged fixture — cheap, since the harness already stages an object and posts through `Routes()`. Worth deciding at the same time whether the compile-only assertions earn their keep once execution covers the same shapes.
- **Noted**: 2026-08-07

### Nothing demotes a Meta-Heuristic whose grounded proposals keep measuring badly

- **Type**: plan
- **Category**: feature
- **Where**: `internal/sleepcycle/grounding.go` (`groundProposals`, `similarityPrior`), `internal/sleepcycle/uct.go` (`proposalPrior`), staleness at `internal/graph/neo4j.go` (`MarkStaleMetaHeuristics`)
- **Why**: The reuse path has no negative feedback edge. A grounded proposal that measures below the floor, or measures fine but never clears `materiallyBetter`, simply loses — nothing is written back to the proposing heuristic, so the next run retrieves it at the same similarity prior and spends the same share of the expansion budget (`GroundingFraction`, 0.3 of the root's expansions) re-measuring it. The only demotion mechanism that exists, `MarkStaleMetaHeuristics`, triggers on *analyst-rejected* supporting evidence, never on measurement outcomes. So a heuristic that transfers badly to a new data source is an unbounded recurring cost at unchanged priority, and the corpus cannot get better at proposing — which is half of what "search quality compounds as knowledge accumulates" is supposed to mean. Cheapest shape that keeps the audit story: record per-heuristic proposal outcomes (adopted / below-floor / lost) and fold a decay term into `proposalPrior`, leaving the retrieval itself untouched. Needs a decision on whether that signal is per-(heuristic, data-source) or global — a heuristic can transfer badly here and well elsewhere, and a global counter would punish it everywhere.
- **Noted**: 2026-08-07

### Heuristic reuse reaches only the Phase-2 search; Phase 1 and Engine B never consult the corpus

- **Type**: plan
- **Category**: feature
- **Where**: `internal/orchestrator/hypothesis.go` (tree proposal), `internal/sleepcycle/policy.go` (`vocabulary` → `CritiqueAtoms`), `internal/verifier/` (discovery + verification), against `internal/heuristics/service.go`
- **Why**: The accumulated corpus is read at exactly one automated site — `groundProposals` in the Sleep Cycle's UCT policy. Phase 1's `ProposeInterventionTree` gets goal text and schema and nothing else, so a column another goal proved load-bearing is rediscovered from scratch (or missed) every time; the atom critic (`CritiqueAtoms`) likewise ranks columns from the schema alone, though its whole job is ordering the search by what matters; and PC discovery has no prior at all, despite a corpus that records which columns carry effects. Both Phase-1 sites already hold the goal text and schema the retrieval needs, so the seam is additive — the missing piece is a decision about what a heuristic contributes at proposal time (a column prior? a candidate filter set? prompt context?) and how to keep an LLM-visible corpus from becoming an injection surface at a *second* set of sites (`llm.Fence` is the existing answer). Sequence this after the distance-floor measurement above: extending reuse to more phases is worthless if the retrieval that feeds the one existing consumer returns nothing.
- **Noted**: 2026-08-07
