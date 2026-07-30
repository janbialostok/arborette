# Arborette

Arborette discovers which **segments of a dataset** maximize or minimize a numeric objective, and
abstracts the findings that survive scrutiny into reusable, queryable **Meta-Heuristics** that
downstream AI agents consume over MCP. An analyst describes a goal in plain English and points the
system at a file; everything after that is measurement — Claude proposes where to look, but on the
tabular path every number in the resulting graph was computed from the data by a read-only query,
never asserted by a model.

An objective has a fixed shape: an **aggregation over a value expression, plus a direction** —
`maximize avg(order_value)`, or `maximize avg(is_fraud = True)` for a rate. Discovery proceeds by
adding **filter predicates** that carve sub-segments out of the dataset and re-measuring the same
objective inside each one. A finding is therefore always a pair: a set of filters, and what the
objective measured under them.

It is **not** prediction, feature selection, or model training — the output is a ranked set of data
filters and their measured effect, with no fitted model, no held-out set, and nothing to score
against unseen rows. It will not tell you which customers will churn; it tells you which slice of
your customers already churns most, and how much worse that slice is than the whole.

## What it does not do (yet)

- **V1 is observational.** It records `P(Outcome | Segment)` — a correlation, measured exactly as
  the data reports it. It does **not** claim the segment *causes* the outcome. Do-calculus and
  physically-executed interventions are the V2 direction (see [Roadmap](#roadmap)), and V1's
  epistemic-provenance tagging exists so V2 can land on the same graph additively.
- **`Intervention` is a name, not an action.** The word is retained across the data model, the
  graph triplets, and the REST surface, but in V1 an `Intervention` node is a reified *read-only
  data-segment definition* — a filter — and nothing is ever enacted.
- **An extracted value is a typed model assertion until a human says so.** On the document path the
  outcome and its confidence are what Claude reported, not what a query computed, so it is written
  `unverified` and stays out of Phase 2 until an analyst confirms or corrects it. Confidence only
  decides what gets queued for review; it never promotes an extraction to evidence.
- **Document goals run Phase 1 only.** A PDF data source is supported by the hypothesis loop's
  per-field extraction path, but the Sleep Cycle rejects a document goal outright: such a goal has no
  numeric objective to optimize and no conjoinable segment to search. That is structural: human
  review of the extracted values does not change it.

## How it works

Discovery runs in two phases, both manually triggered. They are complementary rather than
sequential refinements of each other: Phase 1 is a greedy, LLM-guided descent that finds strong
single directions fast; Phase 2 is a budget-bounded search over combinations of what Phase 1 found,
which is exactly the space a greedy descent prunes past.

### Phase 1 — Active Hypothesis Loop

The Orchestrator pins the objective from the goal's Evaluation Matrix, introspects the data source,
asks Claude for candidate filter predicates, and then measures a root baseline with no filters. Each
candidate is then measured empirically through the Sandbox Execution service at its **cumulative**
filter set — its own predicates plus every ancestor's — and written to the graph as a `State →
Intervention → Outcome` triplet. A candidate that improves on its parent is expanded further with a
fresh proposal; one that does not is recorded and left alone.

Three properties of the traversal are easy to assume wrongly:

- **Expansion is depth-first pre-order recursion**, not level-wise. A candidate's children are
  processed inside its own sibling loop, so the loop descends a branch to the depth cap before
  touching the next sibling. There is no level queue and no per-level barrier.
- **Improving-branch pruning is the real bound on fan-out**, not the depth cap. Breadth and depth
  are hardcoded constants — `defaultBreadth = 3` and `defaultDepth = 4` in
  `internal/orchestrator/server.go` — so the nominal worst case is 3 + 9 + 27 + 81 nodes, but a
  branch only expands when it improves on its parent *and* clears the matrix's hard constraints.
  A handful of triplets on a small dataset is a correctly pruned run, not a broken one.
- **Pruned does not mean discarded.** The triplet is written *before* the improve check, so a
  non-improving candidate is still measured, still recorded, and still available to Phase 2 — it is
  simply not expanded. Triplet count therefore exceeds expanded-node count.

Effect size on a Phase-1 triplet is **parent-relative and marginal**: the candidate's value minus
its parent segment's value, not minus the global baseline. That makes a depth-3 triplet's effect
the marginal contribution of its newest predicate, which is what you want when reading a branch,
and what you must not confuse with the segment's total lift.

Two things that both look like "filters" are separate flows that happen to share one type.
**Intervention filters** are compiled to SQL by the Sandbox and are what carves the segment.
**Matrix hard constraints** are checked in the Orchestrator against the measured objective value,
and only when the objective is a bare column reference — for a compound value expression there is
no single column to constrain, so the check is a no-op. Expression-based constraints are deferred.

A failure at the root (pinning the objective, introspection, the root proposal, the root baseline)
is terminal and marks the run `failed` with its real reason; a per-candidate failure is recorded and
its siblings continue. Because the proposal precedes the baseline, a root failure has usually
already paid for its Claude call. The loop is bounded by a 30-minute timeout, as is the Sleep Cycle
— both run on a background context decoupled from any client connection, so without a deadline a
wedged dependency would hang the work indefinitely.

Four things to know before writing an SSE client, none of them guessable from the wire:

- **Frames are unnamed.** The server emits `data: <json>` with no `event:` line, so the discriminator
  is the JSON payload's own `type` — `triplet`, `branch_failure`, `confidence_distribution`, or a
  terminal `loop_complete`. A `confidence_distribution` frame is a whole snapshot superseding the
  last rather than a delta, and it is not a liveness signal: resolving a verification republishes one
  onto the goal's stream from outside any run. An
  `addEventListener('triplet', …)` handler would never fire. Note `type` alone is not quite enough:
  a `triplet` from a tabular goal and one from a document goal carry different payloads, and the
  overlap is the dangerous part — both have a `value`, a number on one path and a string on the
  other. Only the goal's kind, which the wire never states, tells you which to expect.
- **The stream is keyed by goal, not run.** Two runs triggered on one goal share a buffer and a
  subscriber set, and the first to finish tears the subscription down for both. Let a run finish
  before re-triggering it.
- **`branch_failure` is not a survival signal.** Terminal root failures emit the same frame, with the
  same `{error}` payload, as a per-candidate failure. Nothing on the wire distinguishes them, and
  `loop_complete` carries no payload — a run's real outcome comes from `GET /goals`, which serves the
  persisted status and failure reason.
- **Subscribing after a run ends attaches to silence.** An unknown goal is a 404, and the response
  head is flushed on subscribe so a status code is always available — but a *known* goal whose run
  already finished attaches to a run that will never emit another event, and nothing closes the
  connection. Keepalive comment frames arrive every 30s, so the connection stays healthy rather than
  being reaped; they carry no events. Mid-run reconnects are safe and replay recent history. Prefer a
  manual reader over `EventSource`, which would reconnect into that silence every time a run
  completes.

### Phase 2 — Sleep Cycle

The Sleep-Cycle Worker is a one-shot batch job that searches for **macro-segments** — conjunctions
of the predicates Phase 1 surfaced — that beat anything Phase 1 found on its own, then decides what
is worth publishing as knowledge. In order, one run:

1. Settles the previous run's leftovers: sweeps Meta-Heuristics whose evidence was since rejected,
   and re-embeds any left with a pending embedding by a mid-write crash.
2. Introspects the data source, pins the objective, and measures the **global unfiltered baseline** —
   the reference every derived finding's effect size is relative to.
3. Loads the goal's eligible findings and picks `S*` — the best raw objective value any of them
   achieved, not the support-shrunk score that ranks publications later. `S*` is the best
   *finding*, not the best single predicate: a depth-2 Phase-1 branch is an equally valid `S*`, and
   it sets a correspondingly higher bar.
4. Builds the atom vocabulary — every distinct predicate any finding introduced — and runs a
   level-wise **beam search over the conjunction lattice** with anti-monotone (Apriori) support
   pruning. Conjoining predicates can only shrink the matched row set, so a candidate below the
   support floor is pruned along with every superset it could reach. Every surviving candidate is
   measured **empirically** through the Sandbox, objective and row count in one query.
5. Writes each materially-better macro-segment back as a full triplet against the global baseline,
   flagged as sleep-derived, under an id derived deterministically from the goal and the
   canonicalized filter set so a re-run after a crash rewrites identical nodes instead of
   duplicating them.
6. Selects publications over the **union of Phase-1 findings and this run's derived winners**:
   deduplicate identical predicate sets, rank on a support-shrunk score, then drop restatements of
   one relationship at several resolutions, then cap. Ranking precedes the restatement pass because
   that pass has to know which candidate of a nested pair is the stronger one to keep.
7. Abstracts each selection into a Meta-Heuristic with one Claude call — stripping dataset payloads
   into a domain-agnostic structural vocabulary — writes it to Neo4j with links back to its
   supporting triplets, and embeds it into pgvector.

`SLEEPCYCLE_SEARCH_MIN_SUPPORT` is an **absolute matched-row floor** and the broader of the two
knobs, reaching four stages: it prunes search candidates, holds `S*` to the same bar so the
gate is never set by evidence it would itself reject, drops publication candidates beneath it, and
doubles as the shrinkage constant that ranks whatever survives. `SLEEPCYCLE_SEARCH_MIN_LIFT` is
narrower — the relative improvement over `S*` a macro-segment must clear — and it gates **write-back
only**, deliberately, because a bar phrased relative to `S*` gets harder to clear the better Phase 1
performed, and that must not decide whether the goal publishes anything at all. So a run that writes
back zero macro-segments can still publish Meta-Heuristics; a run publishes nothing when no
candidate clears selection, or when every selected one fails to abstract.

### The services

Three long-running services, three one-shot binaries, all Go, all under `cmd/`:

- **`orchestrator`** — the REST API, the Phase-1 loop, the SSE stream, the goal registry, and the
  only holder of audit-table credentials, which is why the Sleep-Cycle Worker records through its
  internal audit API rather than writing to the table directly.
- **`sandbox`** — stateless execution of read-only DuckDB queries and document extractions against
  data staged from the object store. It owns no per-goal state and holds no graph or Postgres
  connection.
- **`mcpserver`** — the read-side MCP interface over Streamable HTTP, proxying goal registration to
  the Orchestrator rather than writing anything itself.
- **`sleepcycle`** — the Phase-2 worker. `-goal <optimization_function_id>`, one cycle, then exit.
- **`migrate`** — applies (`up`, the default) or rolls back (`down`) the Postgres migrations as the
  owner role.
- **`dbbootstrap`** — idempotently provisions the runtime LOGIN roles before migrations run.

One architectural constraint will trip up a contributor eventually: **`internal/sandbox` is the only
package that touches DuckDB, and it requires CGO.** Every CGO-free service reaches the Sandbox
through `internal/sandboxclient`, which carries its own copy of the wire contract for exactly this
reason. Callers must not import `internal/sandbox`.

Storage is split three ways, which is also why cross-store consistency shows up in the roadmap:
Neo4j holds the triplet graph and the Meta-Heuristic nodes, Postgres holds the goal registry, the
runs table, the append-only audit log, and — via pgvector — the Meta-Heuristic embeddings, and
MinIO/S3 holds every staged dataset.

## Quickstart

This walks the bundled 20-row sample end to end through both phases. Several steps carry traps where
correct behavior reads as a broken system; all are called out inline.

### Prerequisites

Docker with Compose, and an Anthropic API key. `ANTHROPIC_API_KEY` is the **only variable with no
working default**, so `cp .env.example .env` and replacing that one placeholder is the entire
configuration step for a default local run.

Everything in the Quickstart runs through Docker. The host-side Make targets need their own
toolchains: Go 1.24 plus a C compiler for `build`, `test`, and the `migrate-*` targets (see the CGO
constraint above), and Node 22 for the `web-*` targets, where `web/node_modules` is not committed so
`make web-dev` needs `npm ci` (or one `make web-build`) first.

`make up` also needs ports 5432, 7474, 7687, 9000, 9001, and 11434 free on the host. Those mappings
are literals in `docker-compose.yml` with no variable behind them, so a local Postgres or a native
Ollama install — both common, and Ollama's default port is the same one the embedding sidecar
publishes — fails the very first command with `port is already allocated`.

**Run this on a trusted network.** Compose publishes every service with a short-form port mapping,
which Docker binds to all interfaces rather than to loopback. Two different exposures follow. The
datastores carry the placeholder credentials you just declined to replace, so anyone on the subnet
can open a Bolt session against Neo4j on `:7687`, reach Postgres on `:5432` as superuser, or browse
every staged dataset through MinIO on `:9001`, each with a password published in this repository.
The application and embedding ports carry no credentials at all — they authenticate nobody — so for
those the loopback binding is the only control there is.

Hardening it is therefore not just a password change: bind the published ports to `127.0.0.1` by
editing `docker-compose.yml` (no variable controls this), replace every `change-me-*` value, move
`POSTGRES_SSLMODE` off `disable`, and run `make web-dev` as `next dev -H 127.0.0.1` if you use it.

**Rotate the credentials before the first `make up`, not after.** Neo4j and Postgres read their
passwords only when their volume is first initialized, so changing one on an existing stack leaves
the datastore itself on the old password while everything that connects to it uses the new one. The
two failures look nothing alike: Neo4j's healthcheck authenticates, so it simply never reports
healthy and every service gated on it waits forever, whereas Postgres reports healthy — its
healthcheck does not authenticate — and the init chain dies instead at `db-bootstrap`, which blocks
the migration gate. Confusingly, the two Postgres *runtime* roles do rotate correctly, since they are
re-issued on every run, which makes the whole thing look arbitrary. If you have already started the
stack, `docker compose down -v` first and accept losing the data.

### 1. Bring up the stack

`make up` builds and starts everything: the four infra services (Neo4j, Postgres/pgvector, MinIO,
Ollama), a completion-gated init chain (`ollama-pull` pulls the embedding model; `db-bootstrap`
provisions the runtime roles, then `migrate` applies the schema), and then the three Go services and
the web UI. Nothing authenticates before its roles and tables exist, which is why the chain is gated
on completion rather than on start.

The `sleepcycle` job sits behind the Compose `jobs` profile, so `make up` deliberately neither
builds nor starts it.

The first run is slow: it builds six images and pulls the embedding model. `make up` returns once
the gated init chain has completed, which is the readiness signal — no service exposes a health
endpoint, so `docker compose ps` and `docker compose logs -f orchestrator` are the way to watch.
Note that `make down` keeps the four named volumes, so goals, triplets, and published heuristics
survive a restart and accumulate across repeated passes; `docker compose down -v` is the clean
slate.

### 2. Register an objective

```
curl -sS -X POST http://localhost:8080/goals \
  -F 'goal=Maximize the average order value' \
  -F 'import_path=orders.csv'
```

Two request details matter more than they look:

- The endpoint parses a **multipart form**, with fields `goal` and either `import_path` or a `file`
  upload. Use `-F`, not `-d`; a JSON body is rejected with a 400.
- `import_path` is **relative to the import mount**, which is the repo's own `local-import/`
  directory bind-mounted read-only. The literal working value for the bundled sample is
  `orders.csv` — drop your own files in `local-import/` and name them the same way. An
  absolute-looking `/import/orders.csv` is re-rooted under the mount and fails to resolve, with an
  error that does not explain itself.

Three extensions are accepted: `.csv` and `.parquet` on the tabular path, and `.pdf` on the document
path. Anything else — `.tsv`, `.xlsx`, `.json` — is rejected, so a spreadsheet export needs
converting first. A `.pdf` takes a different intake entirely: instead of fitting an objective it
asks Claude which fields are extractable from your goal text, so phrase a document goal as the
values you want pulled out rather than as something to maximize, or it comes back 422 with nothing
to extract. No sample PDF ships with the repo.

Registration ingests the file into the object store, introspects its schema, fits the Evaluation
Matrix to the real columns with Claude, **dry-runs the fitted objective against the Sandbox**, and
persists the goal — returning 201 with `{"optimization_function_id": "<uuid>"}`. That id is what
every later step consumes: `{id}` in the loop and stream routes, and `GOAL=` for `make sleep-cycle`.

Registration does **not** start Phase 1. It also fails *here* rather than at run time if the goal
cannot be fitted to the data — an objective that does not compile against the schema comes back as
a 4xx with the reason, after the system has already tried to repair it. One failure at this step is
worth recognizing on sight: a `502 "evaluation matrix generation failed"` almost always means the
Anthropic call did not go through, because nothing validates `ANTHROPIC_API_KEY` at startup and the
shipped placeholder brings the stack up perfectly happily. Check the key, the model id, and the
account's billing before suspecting the data.

### 3. Run Phase 1

Trigger the loop and follow it:

```
curl -sS -X POST http://localhost:8080/goals/<id>/hypothesis-loop
curl -N http://localhost:8080/goals/<id>/stream
```

The stream carries the frames described under [Phase 1](#phase-1--active-hypothesis-loop).

**Expect a high branch-failure rate on this sample, and possibly a run with no triplets at all.**
Candidate proposals are grounded against the schema's column *names* — a filter naming a column that
does not exist is caught and re-proposed — but not against the categorical *values* those columns
actually hold. Claude therefore tends to invent plausible-sounding categories (`customer_segment =
'Enterprise'`, `region IN ('North America', 'Europe')`) that match zero rows in a 20-row sample of
regional retail orders, and an aggregate over zero rows is not a number, so the branch fails with
`sandbox returned a non-numeric objective value`. The run still reports `completed`, because a
per-candidate failure is not a terminal one. If you get no triplets, re-trigger the loop once the
first run has finished — the proposal is re-sampled each time, and re-triggering appends to the
goal's accumulated findings rather than replacing them.

The trigger returns 202 and the loop runs detached, so confirm it finished before moving on:
`curl -sS http://localhost:8080/goals` reports each objective's persisted status and failure reason.

### 4. Run Phase 2

**`make sleep-cycle GOAL=<id>` is the only way to execute a Sleep Cycle against the local stack.**
The REST route `POST /goals/{id}/sleep-cycle` and the web UI button both log, audit, return 202, and
run nothing: that path is designed to dispatch a real job runner in a deployed environment, and no
such runner exists locally.

Before running it, **lower the support floor**. The bundled sample has 20 data rows and
`SLEEPCYCLE_SEARCH_MIN_SUPPORT` defaults to 30, an absolute matched-row count no segment of 20 rows
can clear. At the default the run still searches, but every publication candidate is dropped beneath
the floor and it correctly reports that no candidate cleared publication selection — a no-op that
reads exactly like a failure. Set `SLEEPCYCLE_SEARCH_MIN_SUPPORT=5` in `.env`, then:

```
make sleep-cycle GOAL=<id>
```

Two follow-ups:

- **Restore `SLEEPCYCLE_SEARCH_MIN_SUPPORT=30` before pointing the stack at real data.** The
  `sleepcycle` job reads `.env` on every run, so this edit persists until undone, and a floor of 5
  left in place quietly publishes Meta-Heuristics backed by a handful of rows — the exact outcome
  the default exists to prevent.
- **Expect zero winners from the conjunction search.** The lattice can only conjoin predicates Phase
  1 proposed and this sample yields too few, so the run publishes from Phase-1 findings directly —
  correct behavior on thin evidence, not a misconfiguration.
- **The counts that matter are not on stdout.** The job logs its measurement count and, when it
  abstracts nothing, the reason — but `winners` and `published` live only in the run's
  `sleepcycle_run_complete` audit row, so confirming either means querying the `audit_log` table
  directly.

### 5. Query the results

```
curl -sS 'http://localhost:8080/heuristics/search?q=raising+average+order+value'
curl -sS http://localhost:8080/heuristics/<meta-heuristic-id>/trace
```

Search is semantic over the published Meta-Heuristics' embeddings; it returns
`[{"id", "definition"}]`, and that `id` is what the trace call takes. The MCP endpoint on `:8082` is
the agent-facing equivalent of both.

Little of this walkthrough actually requires curl. The web UI's landing page is the goal-submission
form, `/goals` lists registered objectives with their run status, and `/heuristics` browses what was
published. A goal page carries three tabs, mirrored into `?tab=` so any of them is linkable: **Run**
follows the live stream, **Verify** works the review queue against the source excerpts each value
was read from, and **Preview agent** chats with the accumulated graph. Only step 4 is Make-only, for
the reason given there.

### Working on the web UI

`make web-dev` serves your local `web/` edits on `:3000` against the Orchestrator. The Compose `web`
service on `:8083` serves a pre-built standalone image and will **not** reflect local edits — bring
the backend up with `make up`, but drive the frontend through `make web-dev`.

## Reference

### Services and ports

| Service | Host port | Purpose |
|---|---|---|
| `orchestrator` | 8080 | REST API, SSE, Phase-1 loop. |
| `sandbox` | 8081 | DuckDB / extraction execution. |
| `mcpserver` | 8082 | MCP over Streamable HTTP. |
| `web` | 8083 | Next.js UI. `WEBUI_PORT` picks the host port; the container is fixed at 8083. |
| `neo4j` | 7474, 7687 | HTTP browser, Bolt. |
| `postgres` | 5432 | pgvector image. |
| `minio` | 9000, 9001 | S3 API, console. |
| `ollama` | 11434 | Embedding sidecar. |

Twelve Compose services in all: the eight above, three init one-shots, and the profile-gated
`sleepcycle` job.

### Make targets

Nine targets:

| Target | Purpose |
|---|---|
| `build` | `go build ./...`. |
| `test` | Integration suite against a running stack: sources `.env`, sets the localhost host overrides, runs `go test -p 1 ./...`. **Destructive** — it truncates the embeddings table and seeds fixture nodes into the graph. Affected heuristics stay in the graph marked complete, so the resume pass skips them and they never become searchable again without manual re-embedding. |
| `up` | `docker compose up -d --build`. |
| `down` | `docker compose down`. |
| `sleep-cycle` | Runs one Sleep Cycle: `make sleep-cycle GOAL=<optimization_function_id>`. |
| `migrate-up` / `migrate-down` | Applies or rolls back migrations from the host. |
| `web-dev` | Next dev server on `:3000` against `ORCHESTRATOR_URL`. |
| `web-build` | `npm ci && npm run build` in `web/`. |

### HTTP API

The Orchestrator's thirteen routes:

| Method and path | Purpose |
|---|---|
| `POST /goals` | Register a goal (multipart: `goal` plus a file upload or `import_path`; optional `confidence_threshold` — above 0, at most 1 — and `epoch_mode=speculative\|blocking` override the review defaults, and a bad value is a 400). Ingests, introspects, fits and dry-runs the Evaluation Matrix, persists, returns 201. Does not start Phase 1. |
| `GET /goals` | List registered objectives with each one's latest run status (synthetic `no run` when never triggered). |
| `POST /goals/{id}/hypothesis-loop` | Trigger Phase 1. |
| `GET /goals/{id}/stream` | SSE progress for a goal's run. |
| `POST /goals/{id}/chat` | One turn of the agent preview, streamed back as SSE. The browser holds the transcript and posts it each turn. Returns 503 until the MCP connector is configured — see `MCP_PUBLIC_URL` in `.env.example`. |
| `POST /goals/{id}/sleep-cycle` | Trigger Phase 2. **Locally a stub: logs, audits, returns 202, runs nothing** — use `make sleep-cycle`. |
| `GET /goals/{id}/verifications` | The human-review queue for a goal (`status=pending\|resolved` narrows it), plus the goal's effective threshold and epoch mode. |
| `POST /goals/{id}/verifications/{outcomeID}` | Submit a review: `{"action":"confirm"\|"correct"\|"reject","corrected_value":"…"}`. 409 if the outcome was already resolved. |
| `GET /goals/{id}/outcomes` | Every extracted value for a goal, not just the queued ones, so any result can be pulled up for review. |
| `GET /goals/{id}/outcomes/{outcomeID}/excerpt` | The source text behind an extracted value — the located span, or the whole document when the value cannot be pinpointed in it. |
| `GET /heuristics/search` | Semantic search over published Meta-Heuristics (`q` required; `k` defaults 10, clamped at 100). |
| `GET /heuristics/{id}/trace` | Trace a Meta-Heuristic back to its supporting triplets. |
| `POST /internal/audit` | The internal audit-write API other services record through. |

The Sandbox exposes three routes, all called by other services rather than by an analyst:
`POST /introspect`, `POST /execute`, and `POST /document/text`.

### MCP tools

Three tools, registered at startup and served over Streamable HTTP on `:8082`:

| Tool | Purpose |
|---|---|
| `get_optimized_heuristics` | Return the Meta-Heuristics most relevant to an operational-state description, ranked by semantic similarity. |
| `trace_causal_chain` | Trace a Meta-Heuristic back to the State/Intervention/Outcome triplets that support it. |
| `submit_analyst_goal` | Register an analyst optimization goal against a data source on the import mount, proxied to the Orchestrator. |

MCP is read-side consumption plus goal registration: a pure-MCP consumer can register a goal but
**cannot trigger either phase**: advancing a goal through discovery is a human-driven REST action.

### Configuration

The credentials worth knowing about. `.env.example` ships working placeholders for all of them
except `ANTHROPIC_API_KEY` — when the rest must be replaced, and why before the first `make up`, is
under [Prerequisites](#prerequisites).

| Variable | Purpose |
|---|---|
| `ANTHROPIC_API_KEY` | Claude access for matrix fitting, tree proposal, extraction, abstraction, and the agent chat preview. |
| `MCP_PUBLIC_URL` | Where Anthropic's infrastructure dials the MCP server for the agent chat. It connects inbound, so the in-network `http://mcpserver:8082` cannot serve — locally this is a tunnel to port 8082. Unset, a goal's Preview agent tab answers "agent preview is not configured". |
| `MCP_AUTHORIZATION_TOKEN` | Bearer token the MCP server checks when set; empty disables auth, leaning on the same trusted-network assumption every other published port makes. Setting `MCP_PUBLIC_URL` without a token makes the MCP server refuse to start. |
| `NEO4J_USER` / `_PASSWORD` | Graph credentials. Compose pins the user to `neo4j`, so only the password is really free. |
| `POSTGRES_OWNER_USER` / `_PASSWORD` | Superuser: provisions the runtime roles and owns the migrated tables. |
| `POSTGRES_ORCHESTRATOR_USER` / `_PASSWORD` | The Orchestrator's runtime role — the only one with audit-table privileges. |
| `POSTGRES_SERVICE_USER` / `_PASSWORD` | The runtime role the MCP server and sleep-cycle job authenticate as. The Sandbox uses no Postgres role. |
| `S3_ACCESS_KEY` / `_SECRET_KEY` | Object-store credentials; locally these are also the MinIO root user and password. |

Sleep-Cycle tuning:

| Variable | Default | What it gates |
|---|---|---|
| `SLEEPCYCLE_SEARCH_MAX_MEASUREMENTS` | 200 | A run's Sandbox query volume — every candidate costs one call. |
| `SLEEPCYCLE_SEARCH_BEAM_WIDTH` | 10 | Candidates carried forward per lattice level. |
| `SLEEPCYCLE_SEARCH_MAX_ORDER` | 3 | Maximum conjunction order; below 2 no conjunction is formable and the search is skipped. |
| `SLEEPCYCLE_SEARCH_MIN_SUPPORT` | 30 | Absolute matched-row floor. Reaches four stages — see [Phase 2](#phase-2--sleep-cycle). |
| `SLEEPCYCLE_SEARCH_MIN_LIFT` | 0.05 | Relative improvement over `S*` required for write-back. |
| `SLEEPCYCLE_MAX_PUBLICATIONS` | 20 | How many segments one run abstracts into Meta-Heuristics. Selection runs after the search, which is why the name carries no `SEARCH_` segment. |

Human review of extracted values:

| Variable | Default | What it gates |
|---|---|---|
| `HITL_CONFIDENCE_THRESHOLD` | 0.8 | Confidence below which an extraction is queued for review. A goal can override it at registration (`confidence_threshold`). This tunes queue volume only — what makes an extraction eligible for Phase 2 is an analyst's verdict, never its confidence. |
| `HITL_BLOCKING_LOOP_TIMEOUT_MINUTES` | 1440 | Deadline for a run whose goal registered with `epoch_mode=blocking`. That mode waits for a verdict before refining below a queued extraction, and the loop is a single sequence, so one wait stalls everything behind it — hence a human-scale deadline instead of the usual 30 minutes. The default `speculative` mode never waits. |

`.env.example` is the source of truth for every other variable — staging limits, the embedding
model, the analyst-identity stub. It deliberately omits the host names (`NEO4J_URI`,
`POSTGRES_HOST`, `S3_ENDPOINT`, `OLLAMA_URL`), which Compose injects per service and which you
override only when pointing the stack at something external. Treat the port variables as the
exception: Compose publishes `8080`, `8081`, and `8082` as literals, so changing
`ORCHESTRATOR_PORT`, `SANDBOX_PORT`, or `MCP_PORT` moves a container's listen port without moving
the mapping that reaches it. `WEBUI_PORT` is the one that behaves as it reads.

## Roadmap

### In flight

Open themes, each with the reason it is open:

- **Objective expressiveness.** The query surface has no `GROUP BY`, window, or partition grammar,
  so every row is independent and any signal defined relative to an entity's own history —
  transaction velocity, amount versus that account's trailing mean, geo-impossibility between
  consecutive events — is currently inexpressible. Precomputing such features as plain columns is
  the working path.
- **Search quality.** The beam spends slots on same-column threshold conjunctions that are
  semantically degenerate (`x >= 4 ∧ x >= 12` *is* `x >= 12`), displacing the cross-column
  combinations that can actually win. Separately, similarity search is unbounded k-NN with no
  relevance floor, so an off-topic query still gets its nearest neighbours back, ranked, with
  nothing marking them as unrelated.
- **Cross-store consistency.** A Meta-Heuristic lives in Neo4j and its embedding in pgvector with no
  shared transaction, so the two can drift — a node whose embedding vanished still reports itself
  complete while being invisible to the search that actually serves consumers.
- **Performance.** Every Sleep-Cycle measurement re-stages the dataset from the object store and
  re-parses it, so a 200-measurement run pays that cost ~200 times over an identical file. The
  Meta-Heuristic read path also fetches nodes one per similarity hit.
- **Service-boundary and auth hardening.** Only the MCP server authenticates, and only because the
  agent preview requires dialing it from outside the network; every other boundary, the
  Orchestrator's own API included, carries no authentication and no rate limiting. This needs a
  deliberate pass before anything is exposed beyond a laptop.

### V2 — the dual-engine architecture

The near-term input to V2 is a **schema-derived atom vocabulary** for the Sleep Cycle. Today the
beam can only conjoin predicates Phase 1 happened to propose, so a column the hypothesis loop never
touched is invisible to Phase 2 entirely — the reachable search space is capped by incidental
coverage. Enumerating atoms from the schema directly (enum values as-is, quantile cuts for numerics)
removes that cap, and inverts the LLM from proposer to **critic**: one call to flag leakage,
tautological, and post-outcome columns that support and lift gates cannot catch by construction.

V2 proper turns the observational engine into one that can *prove* causality. A **Control Plane
Router** dispatches each evaluation by intent to one of two engines: **Engine A** is V1 unchanged —
fast observational discovery writing correlation edges — while **Engine B** executes physical
interventions in isolated ephemeral sandboxes and writes do-calculus causal edges, using A\* guided
by existing Meta-Heuristics to choose which expensive experiment to run first. The Sleep Cycle's
search policy swaps from the level-wise beam to **UCT-MCTS** for the resulting mixed graph, scoring
paths validated by a causal edge far above merely correlated ones.

None of this is a rewrite. V1 already tags every empirical edge with its epistemic provenance and
reserves the interventional values, so V2 lands as new writers, a router, and a new policy behind
the existing search seam.

## Repository layout

- **`cmd/`** — the six binaries described under [The services](#the-services).
- **`internal/`** — eighteen packages, in five groups:
  - *Infrastructure seams*, each the single place its dependency is touched so it can be swapped
    behind an interface: `store` (Postgres and migrations), `graph` (Neo4j), `objectstore`
    (S3/MinIO), `datasource`, `embedding`.
  - *Service packages*: `orchestrator`, `sandbox`, `mcpserver`, `sleepcycle`.
  - *Clients*: `sandboxclient` and `auditclient`.
  - *Shared query seams*: `objective`, which pins a matrix to one measurement, and `heuristics`,
    which both the REST handler and the MCP tool call so the two cannot diverge.
  - *Plumbing*: `domain`, `config`, `service`, `llm`, `testutil`.
- **`web/`** — the Next.js UI, kept as its own deployable with its own image and lockfile so
  front-end iteration does not couple to the Go backend's release cadence.
- **`local-import/`** — the read-only import mount the Orchestrator ingests on-disk sources from,
  containing the bundled `orders.csv` sample.
- **`.turbo/`** — specs, plans, shells, and the running improvements log.
- **`context/`** — the original concept documents, superseded by the specs and kept for provenance.

## Further reading

- [`AGENTS.md`](AGENTS.md) — build pins, dependency constraints, and the testing gotchas that bite
  when running the integration suite against a live stack.
- [`.turbo/specs/arborette.md`](.turbo/specs/arborette.md) — the product source of truth:
  requirements, architecture, data model, key flows, and the V2 roadmap this README compresses.
- [`.turbo/specs/objective-intake-and-navigation.md`](.turbo/specs/objective-intake-and-navigation.md)
  — the objective-intake and navigation design; supersedes the parent spec on tabular intake
  ordering.
