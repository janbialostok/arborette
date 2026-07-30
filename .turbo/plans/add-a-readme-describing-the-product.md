---
status: done
---

# Plan: Add a README describing the product, usage, process, and roadmap

## Context

The repository has no README. Verified by `find . -iname 'readme*'`, there is no user-facing
prose anywhere outside `.turbo/` and `context/` — the product is currently explained only by
two specs written at requirement altitude, an agent guide (`AGENTS.md`) scoped to build pins
and testing gotchas, and package doc comments. Anyone arriving at the repo — including the
author after a gap — has no entry point that says what arborette is, how to run it, or where
it is going.

This plan writes a single top-level `README.md` for a **developer-operator**: someone cloning
the repo to run the local stack. It leads with the product mental model (a causal-segment
*optimization* engine, not prediction or feature selection), walks the bundled sample dataset
end-to-end through both discovery phases, documents the operational and API surface as
reference tables, and closes with a roadmap tiered from specced-but-unbuilt work through
open improvement themes to the V2 dual-engine direction.

Two accuracy hazards drive specific decisions below. First, the bundled `local-import/orders.csv`
has **20 data rows** while `SLEEPCYCLE_SEARCH_MIN_SUPPORT` defaults to **30** — an absolute
matched-row floor — so Phase 2 on the sample can only ever report "no candidate cleared
publication selection"; the quickstart must lower the floor at that step or it demos a no-op
that reads as broken. Second, the specs hold two load-bearing framings that a careless README
would contradict: V1 records **correlation**, `P(Outcome | Segment)`, not causation, and
`Intervention` is retained as a schema and API name even though V1 executes nothing. Both must
be stated explicitly rather than glossed.

## Pattern Survey

### Analogous Features

Existing prose that already explains parts of this product. There is **no README anywhere in the repo** (verified: `find . -iname 'readme*'` returns nothing outside `web/node_modules`), and **no `doc.go` files** — package prose lives in the doc comment of a representative `.go` file.

- `.turbo/specs/arborette.md:1-218` — the product source of truth. §Overview (`:5-9`) is the single best existing statement of the product framing (observational V1, two-phase discovery, MCP exposure, V2 additive). §Users `:13-14`; requirements R1-R25 `:20-119`; §Design/Architecture `:125-135` (the four services, one paragraph each); §Tech Stack `:139-154`; §Data Model `:160-177`; §Key Flows `:181-186` (six numbered flows — goal intake, Phase 1, HITL, agent preview, Phase 2, MCP query); §MVP Scope `:190-194`; **§V2 Roadmap `:196-207`** (Control Plane Router, Engine A/B, UCT-MCTS sleep cycle); §Open Questions `:211-218`.
- `.turbo/specs/objective-intake-and-navigation.md:1-87` — the second spec, supersedes the parent's *tabular* intake ordering. §Overview `:5-7` explains the analyst-usability framing; §Key flows `:74-76` gives the three-step registration/run/browse loop in the most README-ready form in the repo; §Data model `:69-70` (runs table).
- `AGENTS.md:1-94` — operator-facing prose, but agent-scoped, not user-scoped: build/dependency pins (§`:3-24`), testing (§`:26-93`). Contains the only written explanation of `make sleep-cycle` vs the REST trigger (`:78-87`) and of `make web-dev` vs the `:8083` container (`:88-93`). Both facts belong in a README's "how to use it".
- `context/CONCEPT.md:1-103` — the original concept spec (ACAE), superseded by the specs but the source of the tagline "A system which automatically builds domain expert-level knowledge Agents" (`:3`) and the original MCP tool schemas (`:86-103`).
- `context/CONCEPT_REVISION_V1.md:1-81` — the V1/V2 phasing revision; §5 `:65-81` is the original V2 dual-engine text the spec's V2 Roadmap was distilled from ("Episodic Memory Layer" naming lives here, not in the spec).
- `docker-compose.yml:1-6` — a 6-line header comment that already summarizes the whole topology ("four infra services, a completion-gated init chain (db-bootstrap -> migrate), three long-running Go services, the Next.js web UI, and the sleepcycle one-shot job").
- Package doc comments (each 3-8 lines, role-first): `internal/orchestrator/server.go` (top), `internal/sandbox/filesource.go`, `internal/sleepcycle/worker.go:1-9`, `internal/mcpserver/orchestratorclient.go`, `internal/graph/repository.go`, `internal/heuristics/service.go`, `internal/objective/objective.go`, `internal/llm/client.go`, `internal/embedding/provider.go`, `internal/datasource/datasource.go`, `internal/domain/domain.go`, `internal/objectstore/client.go`, `internal/sandboxclient/client.go`, `internal/auditclient/client.go`, `internal/config/config.go`, `internal/service/shutdown.go`, `internal/testutil/testutil.go`. **`internal/store` has no package doc comment** — the only internal package without one.
- `cmd/*/main.go` doc comments (`cmd/dbbootstrap`, `cmd/mcpserver`, `cmd/migrate`, `cmd/orchestrator`, `cmd/sandbox`, `cmd/sleepcycle`) each state in 2-6 lines what the binary is and whether it is a service or a one-shot. These are lift-ready one-liners for a binary table.

### Reusable Utilities

Factual surfaces the README can lift near-verbatim.

**Binaries — `cmd/` (6).** Long-running services: `cmd/orchestrator` (REST/API + hypothesis loop + SSE + sole audit writer), `cmd/sandbox` (stateless DuckDB/extract execution), `cmd/mcpserver` (read-side MCP over Streamable HTTP). One-shot: `cmd/sleepcycle` (`-goal <id>`, runs one cycle and exits), `cmd/migrate` (`up`|`down`, `up` default), `cmd/dbbootstrap` (provisions runtime LOGIN roles, idempotent).

**Internal packages — `internal/` (18).** `auditclient` (audit writes over the orchestrator's internal API), `config` (env config, all services), `datasource` (pluggable source interface + Schema), `domain` (dependency-free shared model: Expression AST, Constraint, Target), `embedding` (`EmbeddingProvider` seam + Ollama impl), `graph` (Neo4j repository seam, Neptune-portable openCypher subset), `heuristics` (shared string-in/matches-out query seam for both the REST handler and the MCP tool), `llm` (Anthropic SDK wrapper: matrix generation, tree proposal, abstraction), `mcpserver`, `objective` (pins the matrix to one measurement + shapes sandbox requests; shared by loop and sleep cycle), `objectstore` (S3/MinIO), `orchestrator`, `sandbox` (the only DuckDB access path; CGO-isolated), `sandboxclient` (CGO-free wire copy — callers must not import `internal/sandbox`), `service` (`RunHTTPServer`, shutdown), `sleepcycle`, `store` (Postgres + migrations), `testutil`.

**Make targets** — `Makefile`:

| Target | Line | What |
|---|---|---|
| `build` | `:10` | `go build ./...` |
| `test` | `:15` | sources `.env`, sets `ARBORETTE_INTEGRATION=1` + localhost host overrides, `go test -p 1 ./...` |
| `up` | `:19` | `docker compose up -d --build` |
| `down` | `:22` | `docker compose down` |
| `sleep-cycle` | `:28` | `docker compose run --rm --build sleepcycle -goal $(GOAL)` |
| `migrate-up` / `migrate-down` | `:31`/`:34` | `go run ./cmd/migrate up\|down` from host |
| `web-dev` | `:39` | Next dev server on :3000 against `ORCHESTRATOR_URL` (default `http://localhost:8080`) |
| `web-build` | `:42` | `npm ci && npm run build` in `web/` |

**There are nine targets, not eight** — `Makefile:1` declares `build test up down sleep-cycle migrate-up migrate-down web-dev web-build`. The table above shows eight rows only because it merges `migrate-up` and `migrate-down` into one. Do not carry "eight make targets" into the README.

`HOST_ENV` (`Makefile:5-8`): `POSTGRES_HOST=localhost NEO4J_URI=bolt://localhost:7687 S3_ENDPOINT=http://localhost:9000 OLLAMA_URL=http://ollama→localhost:11434`.

**Ports (all verified in `docker-compose.yml`).** orchestrator `8080:8080` (`:135-136`); sandbox `8081:8081` (`:159-160`); **mcpserver `8082:8082` (`:197-198`)**; web `${WEBUI_PORT:-8083}:8083` (`:225-226`, container port pinned to 8083 internally at `:222`); neo4j `7474`/`7687` (`:23-24`); postgres `5432` (`:44-45`); minio `9000`/`9001` (`:63-65`); ollama `11434` (`:77-78`).

**Compose services (12 + 4 volumes).** Infra: `neo4j:5`, `pgvector/pgvector:pg16`, `minio/minio:RELEASE.2023-09-04T19-57-37Z` (pinned for the curl healthcheck), `ollama/ollama`. Init chain (all `restart: "no"`, gated by `service_completed_successfully`): `ollama-pull` → pulls `${OLLAMA_MODEL}`; `db-bootstrap` → `migrate`. Services: `orchestrator`, `sandbox`, `mcpserver`, `web`. Job: `sleepcycle` under `profiles: ["jobs"]` (`:171`) so `make up` skips it. **Import mount:** only the orchestrator mounts `./local-import:/import:ro` (`docker-compose.yml:137-138`), matching `ARBORETTE_LOCAL_IMPORT_DIR=/import`. `local-import/orders.csv` is a 20-row sample (`region,customer_segment,channel,order_value`).

**Env vars — `.env.example`.** Grouped as the file already groups them:
- Neo4j `:7-9` — `NEO4J_USER`, `NEO4J_PASSWORD` (required).
- Postgres `:11-22` — `POSTGRES_DB`, `POSTGRES_SSLMODE`, and **three** credential sets: `POSTGRES_OWNER_USER/PASSWORD`, `POSTGRES_ORCHESTRATOR_USER/PASSWORD`, `POSTGRES_SERVICE_USER/PASSWORD` (required).
- Object store `:24-29` — `S3_BUCKET`, `S3_REGION`, `S3_ACCESS_KEY`, `S3_SECRET_KEY`, `S3_PATH_STYLE`. `S3_ENDPOINT` is deliberately **not** in `.env` (compose injects it; host runs must override — `AGENTS.md:36-39`).
- Embeddings `:31-33` — `OLLAMA_MODEL=nomic-embed-text`, `EMBEDDING_DIMENSION=768`.
- Sandbox `:35-39` — `SANDBOX_PORT=8081`, `SANDBOX_MAX_OBJECT_BYTES=536870912`, `SANDBOX_MAX_TEMP_DIR_SIZE=2GiB` (must carry a unit).
- Orchestrator `:41-47` — `ORCHESTRATOR_PORT=8080`, `SANDBOX_URL`, `ARBORETTE_ANALYST_ID=analyst-stub`, `SLEEPCYCLE_JOB_NAME`, `ARBORETTE_LOCAL_IMPORT_DIR=/import`.
- MCP `:49-52` — `MCP_PORT=8082`, `ORCHESTRATOR_URL`.
- Sleep Cycle tuning `:54-67` — `SLEEPCYCLE_SEARCH_MAX_MEASUREMENTS=200`, `SLEEPCYCLE_SEARCH_BEAM_WIDTH=10`, `SLEEPCYCLE_SEARCH_MAX_ORDER=3`, `SLEEPCYCLE_SEARCH_MIN_SUPPORT=30`, `SLEEPCYCLE_SEARCH_MIN_LIFT=0.05`, `SLEEPCYCLE_MAX_PUBLICATIONS=20` (the last carries no `SEARCH_` segment on purpose — selection runs after the search, `:59-61`).
- Web `:69-72` — `WEBUI_PORT=8083`.
- Claude `:74-78` — `ANTHROPIC_API_KEY` (**the only var with no working default** — `:75`), `ANTHROPIC_MODEL=claude-opus-4-8`.

**Orchestrator HTTP routes** — `internal/orchestrator/server.go:145-152` (found via `find … -exec grep`, since gitignore-respecting search skips this dir):

| Method + path | Line | Purpose |
|---|---|---|
| `POST /goals` | `:145` | Register a goal (multipart: `goal` text + file upload **or** `import_path`). Ingest → introspect → fit matrix → sandbox dry-run validate → persist → 201. Does not start Phase 1. |
| `GET /goals` | `:146` | List registered objectives with latest run status (synthetic `no run` when none). |
| `POST /goals/{id}/hypothesis-loop` | `:147` | Trigger Phase 1 for a goal. |
| `GET /goals/{id}/stream` | `:148` | SSE progress stream for a goal's run. |
| `POST /goals/{id}/sleep-cycle` | `:149` | Trigger Phase 2. **Locally backed by `StubLauncher`: logs, audits, returns 202, runs nothing** (`AGENTS.md:80-83`) — `make sleep-cycle` is the only local execution path. |
| `GET /heuristics/search` | `:150` | Semantic (pgvector) search over Meta-Heuristics; `k` defaults 10, clamped at 100 (`internal/orchestrator/heuristics.go:14-15`). |
| `GET /heuristics/{id}/trace` | `:151` | Trace a Meta-Heuristic back to its supporting triplets. |
| `POST /internal/audit` | `:152` | Internal audit-write API (the only path other services write audit through). Carries an open hardening entry in `.turbo/improvements.md` — **do not restate that entry's specifics in the README**, per Resolved Decisions § security. |

**SSE event types** — `triplet`, `branch_failure`, `loop_complete` (`internal/orchestrator/hypothesis.go:408,495,529,108`).

**Sandbox routes** — `internal/sandbox/server.go:33-35`: `POST /introspect`, `POST /execute`, `POST /document/text`.

**MCP tools** — `internal/mcpserver/tools.go:114-125`, registered at startup, served over Streamable HTTP on 8082:
- `get_optimized_heuristics` (`:115`) — "Return the Meta-Heuristics most relevant to an operational-state description, ranked by semantic similarity."
- `trace_causal_chain` (`:119`) — "Trace a Meta-Heuristic back to the State/Intervention/Outcome triplets that support it."
- `submit_analyst_goal` (`:123`) — "Register an analyst optimization goal against a data source on the import mount, proxied to the orchestrator." MCP cannot trigger either phase (intentional — spec R12, `.turbo/specs/arborette.md:61`).

**Web UI surface** — three pages behind `AppNav` (`web/components/AppNav.tsx:8-10`): `/` "Submit goal" (`web/app/page.tsx`, `GoalForm`), `/goals` "Objectives" (`ObjectivesList`), `/heuristics` "Heuristics" (`HeuristicBrowser`); plus `/goals/[id]` live-run view (`LiveRun`). BFF proxy routes mirror the orchestrator 1:1 under `web/app/api/orchestrator/…`.

**Phase-1 loop mechanics (facts, all verified):**
- Breadth/depth defaults are **hardcoded constants, not env vars**: `defaultBreadth = 3`, `defaultDepth = 4` at `internal/orchestrator/server.go:33-34`; `loopTimeout = 30 * time.Minute` at `internal/orchestrator/hypothesis.go:38`; `maxProposalRepairs = 3` (`:50`); registration repair bound `maxObjectiveRepairs = 3` (`internal/orchestrator/submit.go:32`).
- The tree expands per candidate: each candidate is a **filter predicate** added to its parent's cumulative filter set; a node stops at `depth >= defaultDepth` (`hypothesis.go:209`, `:313`). Effect size is **parent-relative marginal** — a child is measured against its parent's value, which is passed down as the new baseline (`hypothesis.go:225`).
- **Traversal is depth-first pre-order, not breadth-first.** `runLoop` iterates the root candidates calling `processCandidate(..., depth: 1)` (`hypothesis.go:162-164`), and `processCandidate` recurses into each child inside its own sibling loop (`:224-226`). There is no level queue and no per-level barrier. `defaultBreadth` is the **fan-out proposed per node** (`TreeContext.Breadth`, `:214`), not a traversal discipline. Note `.turbo/specs/arborette.md:182` (Key Flow 2) describes level-wise progression, which the code does **not** match — the code wins.
- **Improving-branch pruning is the dominant fan-out bound**, not the breadth/depth constants: a candidate expands only if it improves on its parent (`if !objective.Improves(...) { return }`, `hypothesis.go:202-203`) and satisfies the matrix hard constraints (`:205-208`). `internal/orchestrator/server.go:25-30` states this explicitly — the pruning "bounds the fan-out well below" the nominal 3+9+27+81. Every measured candidate still yields a triplet: `writeTriplet` runs at `:196`, **before** the improve check, so a non-improving candidate is recorded and simply not expanded.
- **Intervention filter vs matrix hard constraint** are separate flows sharing `domain.Constraint`: intervention filters are compiled to SQL by the sandbox; matrix hard constraints are checked in the orchestrator (`constraintsSatisfied`, `hypothesis.go:205,653`) and only against the objective field — a no-op for compound-expression objectives (`hypothesis.go:639-642`, spec `objective-intake-and-navigation.md:59`).
- Document goals branch to a per-field extraction loop, breadth = prompt-strategy variants, depth = refinement iterations (`hypothesis.go:234-313`).
- Run status: `running` on trigger, `failed`+reason on terminal (root) failure, `completed` otherwise; the terminal write uses a detached context (`statusWriteTimeout = 5s`, `hypothesis.go:44`).

**Sleep-cycle mechanics (facts, verified in `internal/sleepcycle/worker.go` `Run`, `:165-285`):** ordered — sweep stale → resume embeddings → introspect → pin objective → measure **global unfiltered baseline** → `ListEligibleFindings` → `bestSingleSegment(findings, obj, MinSupport)` (S\* held to the same floor since `00dccce`) → `buildAtoms` → if `len(atoms) < 2 || MaxOrder < 2` skip search with `"no conjunction formable"` → level-wise beam over the conjunction lattice with anti-monotone (Apriori) support pruning, every candidate measured **empirically** through the sandbox (value + row count in one query) → `materiallyBetter` gates **write-back only** → `writeWinners` (full `State→Intervention→Outcome` triplet, deterministic id from goal id + canonicalized filter, `sleep_derived` flag) → `selectPublications` over the **union of Phase-1 findings and derived winners** (`publish.go:181` → `dedupeByCanonical:211` → `dropContainedRestatements:248` → cap at `MaxPublications`) → `abstractAll` (one Claude call per selection, ontological naming, `ABSTRACTED_FROM` edges, `embedding_pending: true` then pgvector then cleared). `runTimeout = 30m` (`worker.go:34`). Document goals are rejected (`errDocumentGoal`, `:45`). Only remaining `no_abstraction` cause is "no candidate cleared publication selection" (`worker.go:281`).

**Roadmap source — `.turbo/improvements.md` (28 entries as of this writing — re-derive, the file grows).** Only **two carry an explicit `Shipped` marker**:
- `:160-167` "Hold S\* to the same support floor" — Shipped 2026-07-28 in `00dccce`.
- `:185-192` "Decouple Meta-Heuristic publication from the lattice's materially-better gate" — Shipped 2026-07-28 in `932e1b9`.

That leaves **26 open**: the 22 grouped by theme immediately below, plus the four stale entries flagged after them.
- **V2 / product direction:** `:177-183` **"Schema-derived atom vocabulary for the Sleep Cycle (V2 spec input)"** — augment `buildAtoms` with schema-enumerated atoms (enum values, quantile cuts) behind a flag and invert the LLM to a critic role; explicitly labelled *"Core input to the V2 dual-engine spec (beam in the episodic layer, UCT in the sleep cycle)."* This is the one improvement entry that feeds the V2 dual-engine direction; the V2 architecture itself is in `.turbo/specs/arborette.md:196-207`, not in improvements.md. `:152-158` "Support entity-relative and windowed objectives (grouped time-series segments)" — no `GROUP BY`/`OVER`/window grammar today, blocks card-fraud-style velocity/geo signals; interim path is precomputing features as columns.
- **Search quality:** `:144-150` "Reject same-column threshold conjunctions in the Sleep-Cycle beam" (73% of order-3 candidates measured degenerate); `:210-216` "Give the heuristic similarity search a relevance floor (or drop free-text search)" (unbounded k-NN; gibberish scores 0.4986 vs on-topic 0.4236-0.4634).
- **Reliability/consistency:** `:127-134` "Reconcile Meta-Heuristic embeddings across Neo4j and pgvector" (23 of 25 heuristics vector-less, measured 2026-07-28); `:218-224` "Stop integration tests accumulating fixture nodes in the shared dev Neo4j" (169 of 194 nodes were residue); `:202-208` "Harden the heuristics read-path self-heal (four risks accepted knowingly)".
- **Security:** `:14-20` "Harden the Sandbox Execution service boundary"; `:22-28` "Authenticate the Orchestrator's internal audit-write endpoint"; `:136-142` "Harden the abstraction prompt against stored prompt injection"; `:70-76` "Web-tier hardening if the UI is ever exposed beyond localhost".
- **Performance:** `:169-175` "Amortize sandbox dataset staging across Sleep-Cycle measurements" (~199 redundant staging cycles per 200-measurement run); `:5-12` "Batch the Meta-Heuristic graph fetch (N+1)"; `:194-200` "Label the id lookups in `CreateMetaHeuristic`".
- **Housekeeping/refactor/DX/UI:** `:30-36`, `:54-60`, `:62-68`, `:95-101`, `:103-109`, `:111-117`, `:119-125`, `:226-232` (ship a bundled sample large enough to exercise the Sleep Cycle at defaults — the fix for the quickstart workaround in step 5.4; internal DX, not README-roadmap material).

**Stale entries — flag before using as roadmap.** Four entries describe problems that landed fixes but were never marked shipped, verified against `git log`: `:38-44` "Cast BOOLEAN columns to 0/1" and `:46-52` "Rebind the hypothesis objective field" (both superseded by `ad5daef`/`f1d09a8` schema-aware intake + objective expression AST); `:78-84` "Harden the hypothesis proposal→execute path (zero-triplet runs)" and `:86-93` "Wide-dataset goal registration fails: grammar limit" (superseded by `b9f40b9` column grounding + `152c66c` expression prompt-grounding). **A README roadmap must not list these as upcoming.**

**Unbuilt shells (verified).** `.turbo/shells/` contains exactly three files, and none has a matching `.turbo/plans/` entry or commit: `arborette-08-hitl-verification-backend.md` (HITL queue + REST + `verification_status` write-through + confidence-bin SSE), `arborette-09-agent-chat-backend.md` (orchestrator chat endpoint via Claude's native MCP connector, `claude-sonnet-5`, public token-auth MCP endpoint), `arborette-10-web-ui-hitl-confidence-agent-chat.md` (HITL UI, confidence histogram, iMessage-style chat component, "preview agent" tab). Shells 01-07 and 11 all have plans and landed commits.

### Convention Anchors

- **Prose lives in `.turbo/` and `context/`, never in package dirs.** No `doc.go`, no per-package README, no `web/README.md`. A root README would be the first user-facing doc in the repo — nothing to conflict with, and nothing to mirror structurally.
- **Heading depth: `#` title → `##` section → `###` subsection.** Specs stop at `###` (`.turbo/specs/arborette.md`); `AGENTS.md` uses only `#`/`##`; `context/CONCEPT.md` numbers its `##` sections (`## 1.`, `## 2.`).
- **Voice: dense declarative prose in full paragraphs, with the *reason* attached to the fact.** The house style never states a rule without its rationale — every `AGENTS.md` bullet, every compose comment (`docker-compose.yml:27-30,88-89,148,167-169,212-215`), and every package doc comment explains why, not just what. E.g. `Makefile:25-27` explains why `--build` is passed. A README that lists facts without rationale would read as foreign here.
- **Bullets over tables.** There is **not a single markdown table** in any repo doc — specs, AGENTS.md, improvements.md, and shells are all `-` bullets and numbered lists. Tables would be a deviation (defensible for a route/env reference, but it *is* a deviation).
- **Code fences are rare.** `AGENTS.md` uses inline backticks for commands (`` `make up` ``, `` `make sleep-cycle GOAL=<id>` ``) rather than fenced blocks; the only fenced-style block in a repo doc is `context/CONCEPT.md`'s JSON tool schemas.
- **Cross-references are by requirement id and file path.** Specs cite `(R7)`, `(R9)`, `Design § Data Model`; improvements cite `internal/pkg/file.go:LINE`.
- **Terminology is load-bearing and consistent.** "Evaluation Matrix", "observational triplet", "macro-segment", "Meta-Heuristic", "Active Hypothesis Loop" / "Phase 1", "Sleep Cycle" / "Phase 2", "epistemic provenance", "Sandbox Execution service", "Orchestrator". `Intervention` is retained as a schema/API name even though V1 executes nothing (`.turbo/specs/arborette.md:36`) — a README must explain that or it will read as a lie.
- **Two framings are consistently held and must not be blurred:** (1) V1 records **correlation**, `P(Outcome | Segment)`, not causation — do-calculus is V2 (`arborette.md:37`); (2) it is a **segment-optimization** engine (maximize/minimize an aggregate over a value expression under filter interventions), not prediction or feature selection.

### Proposed Alignment

Follow the house voice — declarative prose with rationale attached, `#`/`##`/`###` depth, bullets, inline-backticked commands — and treat `.turbo/specs/arborette.md:5-9` (product framing), `:181-186` (key flows), `:196-207` (V2 roadmap) and `objective-intake-and-navigation.md:74-76` (register → run → browse) as the text to compress and align with rather than restate independently; contradicting either spec is the main risk since they are the source of truth and one supersedes the other on tabular intake. Deviate on two points deliberately: use tables for the route/env/port/make-target reference sections (no repo doc has a table, but a README's reference material is exactly where a bullet list stops scanning), and pitch the register-run-browse walkthrough at analyst altitude rather than the specs' requirement altitude. For the roadmap, draw from `.turbo/improvements.md` plus the three unbuilt shells (08/09/10) and the V2 Roadmap section — but exclude the four stale entries listed above (`:38-52`, `:78-93`) whose fixes have landed unmarked, and lead the V2 material with `improvements.md:177-183` since it is the only entry explicitly scoped as V2 dual-engine spec input.

## Resolved Decisions

Settled with the user before drafting; the steps below assume these and should not re-litigate them.

- **Audience: developer-operator.** Someone cloning the repo to run the local stack. Not a public-OSS pitch page, not an analyst manual.
- **Scope: `README.md` only.** `AGENTS.md` is not edited. The README states the `StubLauncher` gap inline (the quickstart walks straight into it) and links `AGENTS.md` for build pins and testing gotchas, accepting one duplicated fact rather than widening the task.
- **Quickstart: bundled sample, end-to-end.** Walk `local-import/orders.csv` through both phases, lowering `SLEEPCYCLE_SEARCH_MIN_SUPPORT` at the Phase 2 step and explaining that the default `30` is a real-data floor.
- **Roadmap: three tiers.** Specced-but-unbuilt shells 08/09/10 → open improvement themes → V2 dual-engine as the headline. The four stale entries are excluded.
- **Security items: generalized.** One "service-boundary and auth hardening" theme line; do not name which endpoint is unauthenticated or describe the sandbox boundary weakness.
- **Env reference: partial by design.** Only the required secrets and the Sleep-Cycle tuning knobs get a table; everything else points at `.env.example`, which is already grouped and commented and stays the source of truth.
- **Model IDs are cited; beta headers are not.** `claude-opus-4-8` (`.env.example:78`, `internal/config/config.go:248`) and `claude-sonnet-5` (shell 09) are both current and correct as written. The MCP connector beta header in `.turbo/specs/arborette.md:144` is also current, but belongs in the spec, not the README — headers drift and the README should stay at capability altitude.

## Implementation Steps

1. **Create `README.md` with the title and framing opener**
   - New file at the repo root: `/Users/janbialostok/Developer/github/arborette/README.md`. Nothing else in the repo is modified by this plan.
   - `# Arborette`, then two or three paragraphs compressing `.turbo/specs/arborette.md:5-9`: arborette discovers which **segments of a dataset** maximize or minimize a numeric objective, and abstracts the repeated findings into reusable, queryable **Meta-Heuristics** exposed to agents over MCP.
   - State the objective shape concretely, since it is the single most load-bearing concept: an objective is an **aggregation over a value expression with a direction** (e.g. maximize `avg(order_value)`), and discovery proceeds by adding **filter predicates** that carve out sub-segments.
   - Say what it is *not*: not prediction, not feature selection, not model training. Attach the reason — the output is a ranked set of data filters and their measured effect, not a fitted model.

2. **Add "What it does not do (yet)"**
   - A short `##` section, placed early and deliberately — omitting it makes the rest of the document inaccurate.
   - V1 is **observational**: it records `P(Outcome | Segment)` — correlation, not causation. Do-calculus and interventional identification are V2 (`.turbo/specs/arborette.md:37`).
   - `Intervention` is a **schema and API name**, retained across the data model, the graph triplets, and the REST surface, even though V1 executes no intervention. Naming it here prevents every later use of the word from reading as a false claim (`.turbo/specs/arborette.md:36`).
   - Document goals are supported by Phase 1's per-field extraction loop but **rejected by the Sleep Cycle** (`errDocumentGoal`, `internal/sleepcycle/worker.go:45`).

3. **Write "How it works" — the two phases**
   - `##` section with a `###` per phase, compressing `.turbo/specs/arborette.md:181-186` and the verified mechanics from the Pattern Survey.
   - **Phase 1 — Active Hypothesis Loop.** Claude proposes candidate filter predicates; each candidate is measured empirically through the Sandbox; each measured candidate is recorded as a triplet, and improving candidates are expanded further. State that breadth and depth are **hardcoded constants** (`defaultBreadth = 3`, `defaultDepth = 4`, `internal/orchestrator/server.go:33-34`) rather than env vars, because a reader will look for the knob and not find one. Note the 30-minute `loopTimeout` (`internal/orchestrator/hypothesis.go:38`), and that effect size is **parent-relative marginal**, not absolute — a triplet's effect is measured against its parent segment, not the global baseline.
   - Get the traversal and the bounds right; both are easy to state wrongly and neither is checkable by reading the README back:
     - Expansion is **depth-first pre-order recursion** — `processCandidate` recurses into a candidate's children inside its own sibling loop (`hypothesis.go:224-226`). Do **not** write "breadth-first": there is no level queue and no per-level barrier. `defaultBreadth` is the number of children proposed **per node**, not a traversal order. `.turbo/specs/arborette.md:182` describes level-wise progression and is out of date against the code; follow the code.
     - The dominant bound is **improving-branch pruning**, not the depth cap: a candidate is expanded only if it improves on its parent (`hypothesis.go:202-203`) and clears the matrix hard constraints (`:205-208`), which as `server.go:25-30` puts it bounds the fan-out well below the nominal 3+9+27+81. Say so — otherwise the README implies a full 120-node sweep, and a quickstart reader who gets a handful of triplets on the 20-row sample cannot tell a correctly pruned run from a broken one.
     - Non-improving candidates are still **recorded**, not discarded: `writeTriplet` runs before the improve check (`:196`). The README should not imply that a pruned branch produces nothing.
   - Distinguish **intervention filters** (compiled to SQL by the Sandbox) from **matrix hard constraints** (checked in the Orchestrator by `constraintsSatisfied`, `hypothesis.go:205,653`). These share `domain.Constraint` but are separate flows; the constraint check applies only to the objective field and is a no-op for compound-expression objectives (`hypothesis.go:639-642`).
   - **Phase 2 — Sleep Cycle.** Describe the ordered pipeline from `internal/sleepcycle/worker.go` `Run` (`:165-285`) at prose altitude: measure a global unfiltered baseline → pick `S*`, the best-scoring eligible Phase-1 finding → build atoms → level-wise **beam search over the conjunction lattice** with anti-monotone (Apriori) support pruning → measure every surviving candidate empirically through the Sandbox → write back materially-better winners as `sleep_derived` triplets → select publications from the **union of Phase-1 findings and derived winners** → abstract each into a Meta-Heuristic via one Claude call, embed into pgvector.
   - Do **not** call `S*` the "best single segment" or otherwise imply it is a one-predicate filter. `bestSingleSegment` (`worker.go:318-338`) maximizes over everything `ListEligibleFindings` returns, whose Cypher filters only on `goal_id`, `sleep_derived`, and `verification_status` (`internal/graph/neo4j.go:342-346`) — no order or depth predicate. A Phase-1 finding is measured at its **cumulative** filter set, so a depth-2 finding carrying several predicates is an equally valid `S*`. The spec's "better than any single Phase-1 segment alone" (`.turbo/specs/arborette.md:43`) means one *finding*, not one *predicate*; phrase it as "the best-scoring segment Phase 1 found, which may itself combine several filters" so the README does not teach the wrong model of what the search must beat.
   - Name the two gates that a reader will need when nothing publishes: `SLEEPCYCLE_SEARCH_MIN_SUPPORT` (an absolute matched-row floor, applied to `S*` as well since `00dccce`) and `SLEEPCYCLE_SEARCH_MIN_LIFT`. Note that `materiallyBetter` gates **write-back only**, not publication — publication is ranked and capped separately by `SLEEPCYCLE_MAX_PUBLICATIONS`.

4. **Write "How it works" — the services**
   - A `###` subsection listing the three long-running services and three one-shot binaries from `cmd/`, one line each, lifted from their `main.go` doc comments: `orchestrator` (REST API, hypothesis loop, SSE, sole audit writer), `sandbox` (stateless DuckDB execution — the only DuckDB access path, CGO-isolated), `mcpserver` (read-side MCP over Streamable HTTP); `sleepcycle` (`-goal <id>`, one cycle then exits), `migrate`, `dbbootstrap`.
   - Explain the one architectural constraint a contributor will trip over: `internal/sandbox` is the only package that touches DuckDB and requires CGO, so CGO-free services use the wire-DTO copy in `internal/sandboxclient` — **callers must not import `internal/sandbox`**.
   - Note the storage split, since it explains the two-store reconciliation work in the roadmap: Neo4j holds the triplet graph and Meta-Heuristic nodes, Postgres/pgvector holds the objective registry, runs, audit log, and Meta-Heuristic embeddings, MinIO/S3 holds staged datasets.

5. **Write the Quickstart**
   - `##` section with numbered `###` steps. Every command inline-backticked per house style; use a fenced block only where a multi-line `curl` genuinely needs one.
   - **Prerequisites** — Docker with compose, and an Anthropic API key. Call out that `ANTHROPIC_API_KEY` is the **only variable with no working default** (`.env.example:75`), so `cp .env.example .env` followed by replacing that one placeholder is the whole configuration step for a default local run.
   - **1. Bring up the stack** — `make up`. Describe what comes up: four infra services, a completion-gated init chain (`ollama-pull`, `db-bootstrap` → `migrate`), then the three Go services and the web UI. Note that the `sleepcycle` job sits behind the compose `jobs` profile and is deliberately **not** started by `make up` (`docker-compose.yml:171`).
   - **2. Register an objective** — `POST /goals` against `http://localhost:8080`. Two request details must be stated exactly, because getting either wrong strands the reader at step 2 of 5:
     - The endpoint parses a **multipart form** (`r.ParseMultipartForm`, `internal/orchestrator/submit.go:50`) with fields `goal` (the objective text) and `import_path`. A JSON body returns 400. The README's `curl` must use `-F`, not `-d`.
     - `import_path` is **mount-relative**: the literal working value for the bundled sample is `orders.csv`, not `/import/orders.csv`. `ingestLocal` (`internal/orchestrator/submit.go:262-281`) computes `filepath.Clean("/" + importPath)` and then `filepath.Join(s.localImportDir, rel)`, so an absolute-looking path re-roots under the mount — `/import/orders.csv` becomes `/import/import/orders.csv` and fails `EvalSymlinks` with an error that does not explain itself. Do **not** describe the mount in a way that invites the absolute form; state the literal value and, if the mount is mentioned at all, say the path is relative to it.
     - Name the id in the 201 response: the body is `{"optimization_function_id": "<uuid>"}` (`internal/orchestrator/submit.go:133`), and that value is what every later step consumes — `{id}` in the Phase 1 and SSE routes, and `GOAL=` for `make sleep-cycle` (`Makefile:27`). Naming it once here is what makes the five-step walkthrough chain without the reader having to run it to find out.
     - Give a concrete goal string matched to the sample's real columns (`region`, `customer_segment`, `channel`, `order_value`) — e.g. maximizing average order value. Explain what registration does and does not do: it ingests, introspects the schema, fits the Evaluation Matrix with Claude, **dry-runs it against the Sandbox**, and persists — returning 201 — but it does **not** start Phase 1. Note that a bad fit fails here at registration rather than at runtime.
   - **3. Run Phase 1** — `POST /goals/{id}/hypothesis-loop`, then follow `GET /goals/{id}/stream` for SSE progress. Name the three event types a reader will see: `triplet`, `branch_failure`, `loop_complete`. Mention the web UI at `http://localhost:8083` as the alternative to curl for this step.
   - **4. Run Phase 2** — this step carries the two traps, both stated inline:
     - `make sleep-cycle GOAL=<id>` is the **only** way to execute a run against the local stack. The REST route `POST /goals/{id}/sleep-cycle` and the web UI button are backed by `StubLauncher` locally: they log, audit, return 202, and run nothing. Attach the reason — the REST path is designed to launch a real job runner in a deployed environment, which does not exist locally.
     - The bundled sample has **20 data rows** and `SLEEPCYCLE_SEARCH_MIN_SUPPORT` defaults to **30**, an absolute matched-row floor. Instruct the reader to set it lower (e.g. `SLEEPCYCLE_SEARCH_MIN_SUPPORT=5`) in `.env` before this step, and explain that the default exists so real runs do not publish findings backed by a handful of rows. Without this, the run completes and publishes nothing — the reader would reasonably read a correct no-op as a failure. The mechanism is worth one clause: the floor blocks publication through `candidatesFromFindings` (`internal/sleepcycle/publish.go:67`), which drops any Phase-1 finding whose support is under it, so at the default the candidate union is empty before the search is even consulted.
     - Tell the reader to **restore `SLEEPCYCLE_SEARCH_MIN_SUPPORT=30` before pointing the stack at real data.** The `sleepcycle` service reads `.env` on every run (`env_file: [.env]`, `docker-compose.yml:175`), so the quickstart's edit is permanent until undone — a floor of 5 left in place silently publishes Meta-Heuristics backed by a handful of rows, exactly what the default prevents.
     - Set the expectation that **the conjunction search will publish via Phase-1 findings, not via its own winners.** Even at `MIN_SUPPORT=5`, no segment in the 20-row sample clears `materiallyBetter` (`internal/sleepcycle/worker.go:350-365`), which requires beating `S*` by at least `SLEEPCYCLE_SEARCH_MIN_LIFT` (5%) relative. Taking `S*` at its lowest plausible value, `customer_segment = repeat` (12 rows, avg ≈ 136.76): the best conjunction clearing support 5 is `repeat ∧ mobile` (5 rows, ≈ 141.25 — a 3.3% lift), and stronger combinations such as `repeat ∧ West` (4 rows) are support-pruned. The conclusion is robust to which finding actually becomes `S*` — since `S*` may be a multi-predicate Phase-1 finding (see step 3), a different one can only be **higher**, which raises the bar and keeps the winner count at zero. It cannot flip the result in the other direction. So the run writes back **zero** macro-segments and every published Meta-Heuristic comes from a Phase-1 finding. Without this the README headlines a beam search over the conjunction lattice that the reader's own run never exercises — the same "correct behavior reads as broken" failure this quickstart exists to avoid. Two constraints on how to write it:
       - **State the mechanism, not the numbers.** Put in the README the reason — on a sample this small the support floor prunes the strongest conjunctions, so publication comes from Phase-1 findings — and keep the specific averages out of it. They are properties of the bundled CSV, and every reader's Phase 1 is a fresh LLM run. The figures above are working evidence for the plan, not README copy.
       - **Hedge on both dependencies, not just one.** The prediction rests on `S*` *and* on which atoms Phase 1 produced: `buildAtoms` (`internal/sleepcycle/search.go:112-143`) applies no support filter, so predicates from sub-floor findings still enter the lattice, and a proposed `order_value` threshold predicate (a known open issue, `.turbo/improvements.md:144`) would shift both `S*` and the reachable space. Write it as what to expect, never as a guarantee.
   - **5. Query the results** — `GET /heuristics/search` for semantic search over published Meta-Heuristics (`k` defaults 10, clamped at 100), `GET /heuristics/{id}/trace` to walk one back to its supporting triplets, and the `/heuristics` page in the web UI. Mention the MCP endpoint on `:8082` as the agent-facing equivalent.
   - **Working on the web UI** — one short note: `make web-dev` serves local `web/` edits on `:3000` against the orchestrator, while the compose `web` service on `:8083` serves a pre-built image and will not reflect local edits.

6. **Write the Reference section**
   - `##` Reference with `###` subsections, using tables — the one deliberate deviation from house style, because this is exactly the material a bullet list stops scanning well.
   - **Services and ports** — orchestrator 8080, sandbox 8081, mcpserver 8082, web 8083, Neo4j 7474/7687, Postgres 5432, MinIO 9000/9001, Ollama 11434. All verified against `docker-compose.yml`.
   - **Make targets** — the **nine** targets declared at `Makefile:1`, one line each (`migrate-up` and `migrate-down` may share a row, but the count in any prose is nine).
   - **HTTP API** — the eight orchestrator routes from `internal/orchestrator/server.go:145-152` with the one-line purposes captured in the Pattern Survey. Keep the `StubLauncher` caveat on the sleep-cycle row — a reader needs it. For `POST /internal/audit`, carry **only** its purpose (the internal audit-write path other services use); do **not** carry the Pattern Survey's hardening annotation into the README, per Resolved Decisions § security. Add the three Sandbox routes (`internal/sandbox/server.go:33-35`) as a short second table or a trailing note.
   - **MCP tools** — the three registered tools with their real descriptions from `internal/mcpserver/tools.go:114-125`. State that MCP is read-side plus goal registration and **cannot trigger either phase** — that is intentional (`.turbo/specs/arborette.md:61`), not a gap.
   - **Configuration** — a table of the required secrets (`ANTHROPIC_API_KEY`, the Neo4j pair, the three Postgres credential sets) and the six Sleep-Cycle tuning knobs with their defaults and what each gates. End with a pointer to `.env.example` as the complete, grouped, commented source of truth rather than duplicating the remaining variables.

7. **Write the Roadmap**
   - `##` Roadmap with three `###` tiers, ordered near-term → themes → V2.
   - **Next up (specified, not yet built)** — the three unbuilt shells, described by capability rather than by shell number alone: human-in-the-loop verification of triplets (queue, REST surface, `verification_status` write-through, confidence-binned progress); a conversational agent over the published Meta-Heuristics, using Claude's native MCP connector against the MCP server; and the web UI surfacing all of it (HITL review, a confidence histogram, and a chat/preview-agent tab). Cite `.turbo/shells/` as where each is specified.
   - **In flight** — the open improvement themes, each one or two lines with the *why* attached per house style: objective expressiveness (no `GROUP BY`/window grammar today, which blocks entity-relative and time-windowed objectives such as velocity signals); search quality (degenerate same-column threshold conjunctions in the beam; no relevance floor on the similarity search, so an off-topic query still returns its k nearest); cross-store consistency (Meta-Heuristic embeddings can drift between Neo4j and pgvector); performance (dataset staging is repeated per Sleep-Cycle measurement rather than amortized; an N+1 on the Meta-Heuristic graph fetch); and **service-boundary and auth hardening**, stated at exactly that altitude with no specifics.
   - Do **not** list the four stale entries (`.turbo/improvements.md:38-52`, `:78-93`) — their fixes have landed and are only unmarked in the improvements file.
   - **V2 — the dual-engine architecture** — the headline. Compress `.turbo/specs/arborette.md:196-207`: a Control Plane Router dispatching between two engines, with the Sleep Cycle's search moving to UCT/MCTS. Lead into it with `.turbo/improvements.md:177-183`, the one entry explicitly scoped as V2 spec input: give the Sleep Cycle a **schema-derived atom vocabulary** (enum values directly, quantile cuts for numerics) instead of depending entirely on which columns Phase 1's proposals happened to surface, and invert the LLM from proposer to **critic** (flagging leakage and tautological columns that support and lift gates cannot catch). State the payoff plainly — today the beam's reachable space is capped by Phase 1's incidental coverage.

8. **Add repository layout and further reading**
   - A short `##` section mapping the top-level directories: `cmd/` (binaries), `internal/` (the 18 packages, named with a few words each — not all 18 individually, group them: domain/config/service plumbing, the store and graph seams, the LLM and embedding seams, the per-service packages), `web/` (separate Node/Next.js deployable, not part of the Go module), `local-import/` (the read-only import mount), `.turbo/` (specs, plans, shells, improvements), `context/` (superseded concept docs).
   - Close with links: `AGENTS.md` for build pins, dependency constraints, and testing gotchas; `.turbo/specs/arborette.md` as the product source of truth; `.turbo/specs/objective-intake-and-navigation.md` for the objective intake and navigation flow, noting it supersedes the parent spec on tabular intake ordering.

9. **Verify every cited fact against source before finishing**

   **The invariant: re-derive, never transcribe.** Every count, ordering, and mechanism claim in the README must be computed from the source file at writing time. The Pattern Survey above is a research snapshot and is *not* a citable authority — it has already been wrong four times, in two separate review rounds, on exactly the facts it looked most confident about:

   | Claim as first written | Actual | Where it came from |
   |---|---|---|
   | "eight make targets" | nine (`Makefile:1`) | survey table merged two rows |
   | "19 improvement entries", "remaining 17" | 27 entries / 25 open when caught; **28 / 26 by the end of planning** | survey miscounted — and the count then drifted again as the file grew, which is why it must be recomputed at writing time rather than read from here |
   | "9 compose services" | 12 (`neo4j:18` … `web:211`) | survey miscounted; its own adjacent enumeration said 12 |
   | "the tree expands breadth-first" | depth-first pre-order (`hypothesis.go:224-226`) | inferred from the spec, which is out of date |

   Each was caught by review rather than by writing, and each would have shipped as a public false statement. Treat any bare number or traversal/ordering word in the drafted README as unverified until re-derived.

   - **Counts:** recompute rather than copy. `grep -c '^### ' .turbo/improvements.md` for entries; the `.PHONY` line at `Makefile:1` for targets; the top-level service keys in `docker-compose.yml` for services. If a table merges rows, state the true count in prose alongside it.
   - **Orderings and mechanisms** (traversal order, gate sequence, what precedes what): read the function, do not infer from the spec. `.turbo/specs/arborette.md:182` is a live example of a spec that contradicts the code; where they disagree, the code wins and the README follows the code.
   - **Identifiers:** re-check each number, port, path, route, env var name, and default against its file — the eight routes and their order (`internal/orchestrator/server.go:145-152`), the port list (`docker-compose.yml`), the six `SLEEPCYCLE_*` names and defaults (`.env.example:54-67`), `defaultBreadth`/`defaultDepth` (`server.go:33-34`), and the three MCP tool names (`internal/mcpserver/tools.go:114-125`).
   - Use `find <dir> -type f -name '*.go' -exec grep -n '<pattern>' {} +` for anything under `internal/orchestrator` or `cmd/orchestrator`. A bare `orchestrator` line in `.gitignore` shadows those directories, so gitignore-respecting search tools silently skip them and return a confidently wrong answer.

## Verification

The change is a single documentation file, so verification is factual accuracy plus one live walkthrough. There are no tests to run and no observable runtime behavior.

- **Walk the quickstart on a clean stack.** From a fresh `cp .env.example .env` with a real `ANTHROPIC_API_KEY`, run every command in the Quickstart in order — copy-pasted from the rendered README, not retyped from memory, so a wrong `curl` shape or `import_path` value is caught here. Expected: `make up` reaches a healthy stack; `POST /goals` returns **201** with an objective id; `POST /goals/{id}/hypothesis-loop` returns and `GET /goals/{id}/stream` emits `triplet` events and terminates with `loop_complete`; after lowering `SLEEPCYCLE_SEARCH_MIN_SUPPORT`, `make sleep-cycle GOAL=<id>` **publishes at least one Meta-Heuristic** while writing back **zero** macro-segment winners (see below); `GET /heuristics/search` returns the published heuristic. Any step that does not behave as the README claims is a README bug, not a stack bug — fix the prose.
- **Confirm the `import_path` and form-encoding claims by using them.** The registration `curl` must be run exactly as written. A 400 means the README used a JSON body instead of `-F` multipart; an `EvalSymlinks`-flavored failure means it published the absolute `/import/orders.csv` form instead of the mount-relative `orders.csv`. Both are silent-until-run errors, which is why this is a walkthrough check rather than a read-through one.
- **Expect zero write-back winners at the lowered floor — and observe it deliberately.** The winner count is **not** visible in `make sleep-cycle` output: it is recorded only in the `sleepcycle_run_complete` audit detail (`internal/sleepcycle/worker.go:207-219`), and `writeback.go:50` logs on failure only. Use one of two observation methods rather than inferring from a successful-looking run:
  - Query the audit record for the run and read its `winners` and `published` fields. Connect with the `POSTGRES_OWNER` credentials from `.env`. Note this repo's audit-table quirk when writing the `WHERE` clause: the short token (`"job"`) and the long event name (`"sleepcycle_run_complete"`) sit in the opposite columns from what their names suggest, so match on the value rather than assuming which column holds it.
  - Or check the graph for `Intervention` nodes carrying the `sleep_derived` flag for this goal — simpler, and zero such nodes is the same signal.

  Expected: `winners` is 0 while `published` is at least 1. That is correct behavior on a 20-row sample, not a failure, and the README must have said so in advance. If winners are unexpectedly non-zero, the `materiallyBetter` explanation in quickstart step 4 is wrong for this data and the prose needs correcting — do not treat the run as anomalous.
- **Confirm the Phase 2 floor instruction is load-bearing.** Run `make sleep-cycle` once at the default `SLEEPCYCLE_SEARCH_MIN_SUPPORT=30` against the 20-row sample and confirm it publishes nothing (expected reason: no candidate cleared publication selection). This proves the README's instruction to lower the floor is necessary rather than decorative. If it unexpectedly publishes, the support-floor explanation in quickstart step 4 is wrong and must be corrected.
- **Confirm the `StubLauncher` claim.** `POST /goals/{id}/sleep-cycle` and observe a 202 with no run executed — matching what the README says. This is the single most important claim to get right, because a reader who does not believe it will conclude the product is broken.
- **Spot-check the reference tables against source.** Diff the route table against `internal/orchestrator/server.go:145-152` (via `find … -exec grep`, not a gitignore-respecting search), the port table against `docker-compose.yml`, the make-target table against `Makefile`, and the MCP tool names against `internal/mcpserver/tools.go:114-125`.
- **Check for spec contradiction.** Re-read `.turbo/specs/arborette.md:5-9` and `objective-intake-and-navigation.md:74-76` against the README's framing and quickstart. The specs are the source of truth; where the README compresses them it must not change their claims — especially the correlation-not-causation framing and the `Intervention`-is-a-name framing.
- **Confirm the roadmap excludes shipped work.** Verify none of `.turbo/improvements.md:38-52` or `:78-93` appears in the roadmap, and that no security item names a specific unauthenticated endpoint or boundary weakness.
- **Render check.** View the file on GitHub or in a Markdown preview and confirm the tables render, heading depth is `#`/`##`/`###`, and no line wraps badly in a narrow viewport.

## Context Files

- `.turbo/specs/arborette.md` — the product source of truth. §Overview `:5-9`, §Key Flows `:181-186`, and §V2 Roadmap `:196-207` are the passages the README compresses; the correlation-vs-causation (`:37`) and `Intervention`-naming (`:36`) statements are the ones it must not contradict.
- `.turbo/improvements.md` — the roadmap source. Read in full to pick up the theme groupings, and to see which four entries are stale despite carrying no `Shipped` marker.
- `AGENTS.md` — the boundary document. The README links to it and must not duplicate its build-pin and testing content; its `:78-93` paragraphs are the two operator facts the README does restate.
- `docker-compose.yml` — authoritative for the topology, ports, the `jobs` profile, and the `./local-import:/import:ro` mount. The 6-line header comment is the best one-paragraph summary of the stack.
- `.env.example` — authoritative for every variable name and default; the README's configuration table is a curated subset of it and points back to it.
- `Makefile` — the nine targets declared at `:1`, and the comments explaining why `--build` is passed to the sleep-cycle run.
- `internal/orchestrator/server.go` — route registration at `:145-152` and the hardcoded `defaultBreadth`/`defaultDepth` at `:33-34`. Read with `find … -exec grep` or the Read tool; gitignore-respecting search silently skips this directory.
- `internal/sleepcycle/worker.go` — `Run` at `:165-285` is the Phase 2 pipeline the README describes in prose, including the support-floor and publication-selection ordering.
- `internal/mcpserver/tools.go` — the three registered tools and their real descriptions at `:114-125`.
- `local-import/orders.csv` — the 20-row sample the quickstart walks; its four column names anchor the concrete goal string.
