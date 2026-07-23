---
status: done
spec: .turbo/specs/objective-intake-and-navigation.md
---

# Plan: Run Status & Objectives-List Backend

## Context

Once an analyst leaves the runner UI a hypothesis run is unreachable — there is no list of objectives and no persisted record of whether a run is running, finished, or failed (the progress hub is in-memory and evicts a run on completion). This shell adds the backend for navigation and observability: a runs table the Orchestrator writes across the Phase-1 loop lifecycle, a `GET /goals` list endpoint returning each objective with its latest run status, and — so failures are intelligible after the fact — run-time propagation of the *real* failure reason. Terminal status writes must survive the loop's own cancellation, so a deadline-terminated run is marked `failed` rather than stranded at `running`.

**Reconciliation with shell 02 (already landed).** The real-failure-reason propagation to the live stream is *already done*: `SandboxClient.post` parses the sandbox's `{error}` body into `SandboxError.Message` (`internal/orchestrator/sandboxclient.go:141-148`), and `branchFailure` already publishes `cause.Error()` to the SSE stream (`internal/orchestrator/hypothesis.go:238`). The Orchestrator no longer collapses errors into `sandbox /execute returned status N`. So the remaining R7 work in *this* shell is **persisting the terminal reason on the run row** (the durable half of R8), not re-implementing SSE propagation. The plan verifies the SSE path is intact and does not touch it.

## Pattern Survey

### Analogous Features

**Migrations (up/down pairs, numbering, PK style)**
- `internal/store/migrations/0002_goal_registry.up.sql:1` — `goal_registry` with a `uuid PRIMARY KEY` + `created_at timestamptz NOT NULL DEFAULT now()`; the closest analog for the `runs` table's uuid PK and `started_at` default. Down is a bare `DROP TABLE IF EXISTS ...` (`0003_audit.down.sql:1`).
- Numbering is zero-padded `000N_<slug>.{up,down}.sql`; highest is `0005_grants` → new work is `0006_runs.{up,down}.sql`. All embedded via `//go:embed *.sql` in `internal/store/migrations/migrations.go`; applied by `internal/store/migrate.go:20` (`Migrate`) / `:26` (`MigrateDown`) with the owner DSN.

**Grants / least-privilege write-owner pattern**
- `internal/store/migrations/0005_grants.up.sql:12-19` — the pattern to mirror: `GRANT USAGE ON SCHEMA public` per role, then per-table grants. `GRANT INSERT ON audit_log TO arborette_orchestrator` (line 13) is the sole-writer template; `GRANT SELECT ON goal_registry TO arborette_service` (line 19) is the runtime-role read template. Runs needs `INSERT, UPDATE` for `arborette_orchestrator` (it writes via `Create`/`SetStatus`) and `SELECT` for both runtime roles (spec data-model: "Read by the runtime roles; written by the Orchestrator role only").
- `0005_grants.down.sql:3-11` — down is symmetric `REVOKE`, no role drops.
- **Decision point (no prior art):** 0005 is keyed to "runs after every table exists." Since the runs table arrives in 0006 (after 0005), its grants must be co-located in `0006_runs.up.sql`, not retro-edited into 0005.

**Store types (constructor, pgx usage, error wrapping)**
- `internal/store/goalregistry.go:22-29` — `type GoalRegistry struct { pool *Pool }` + `NewGoalRegistry(pool *Pool)`. Exact constructor style for the new `Runs` store. `Insert` (`:32`) uses `pool.Exec` with `$1..$N`; `Get` (`:49`) uses `pool.QueryRow(...).Scan(...)`. Errors wrapped `fmt.Errorf("...: %w", err)` with the key.
- `internal/store/auditlog.go:17-48` — `AuditLog`/`NewAuditLog`; the insert-only type whose doc comment names the two-layer least-privilege boundary. Prose template for the sole-writer runs store.
- `internal/store/embeddingstore.go:60-82` (`SimilaritySearch`) — **the only multi-row `pool.Query` + `rows.Scan` loop in the store package**: `rows, err := e.pool.Query(...)`, `defer rows.Close()`, `for rows.Next() { rows.Scan(&x) }`, then the crucial `if err := rows.Err(); err != nil`. The exact idiom `GoalRegistry.List` and `Runs.LatestByGoal` must mirror.

**"Latest row per key" ordered read**
- No existing `DISTINCT ON` / window / latest-per-key query. `SimilaritySearch`'s `ORDER BY ... LIMIT` (`embeddingstore.go:62`) is the only ordered multi-row read. The latest-run-per-goal SQL is **net-new** — only the row-iteration mechanics are borrowable.

**HTTP GET handler + list response shape**
- `internal/orchestrator/heuristics.go:53-96` (`handleHeuristicSearch`, `handleHeuristicTrace`) — the template for `GET /goals`: read inputs, call a collaborator, build a snake_cased DTO slice with `out := make([]xxxDTO, 0, len(...))` (non-nil empty slice → marshals `[]` not `null`), then `writeJSON(w, http.StatusOK, out)`. Domain/store types carry no json tags (would leak PascalCase) — a dedicated DTO is required.
- `internal/orchestrator/server.go:153/161/149` — `writeJSON` / `writeErr` / `errorResponse`. `GET /goals` uses these; it lists (not fetch-by-id), so it does NOT use `lookupGoal`.

**Route registration**
- `internal/orchestrator/server.go:121-131` (`Routes()`) — method-prefixed `mux.HandleFunc(...)`. Add `mux.HandleFunc("GET /goals", s.handleListGoals)`. There is currently only `POST /goals`, so the GET is non-conflicting.

**Loop lifecycle & terminal bookkeeping** (`internal/orchestrator/hypothesis.go`)
- `:44-55` (`handleTriggerLoop`) — looks up the goal (404), launches `go func(){ ctx, cancel := context.WithTimeout(context.Background(), loopTimeout); defer cancel(); s.runLoop(ctx, goal) }()`, returns 202. Where the `running` row is created (decision below).
- `:59-106` (`runLoop`) — the single `defer` (`:61-64`) publishes `loop_complete` + `hub.Complete`. **Terminal (root) failures** are the five early `return`s after `s.branchFailure(ctx, id, nil, err)` (lines 69/76/85/94/99) — each passes `filters == nil`. **Normal completion** is falling through the `for _, cand := range root.Candidates` loop (`:103`). The single defer cannot see whether the run root-failed → explicit bookkeeping (a terminal-error local set at each root site) is required.
- `:112-158` (`processCandidate`) — per-candidate `branchFailure` calls (lines 118/123/129/137/152) are **non-terminal** (they pass `effective` filters, siblings continue) and must NOT flip run status. They live in a *separate function* with no access to `runLoop`'s terminal-error local, so this is structurally enforced.
- `:230-239` (`branchFailure`) / `sandboxclient.go:88,141-148` — the terminal reason string is `cause.Error()`, already the parsed `SandboxError.Message`.

**Detached-context pattern**
- **No `context.WithoutCancel` anywhere in the repo.** The only "survive-cancellation" idiom is `context.WithTimeout(context.Background(), ...)`: `internal/service/httpserver.go:48` (shutdown) and `hypothesis.go:50` (the loop's own trigger context). Since the loop ctx *is* a `Background`-rooted timeout, the terminal-status write mirrors the same idiom with a short independent deadline (recommended over introducing `WithoutCancel`).

### Reusable Utilities
- `internal/store/pool.go:18/23` — `Pool` / `NewPool`; the runs store takes `*Pool`.
- `internal/orchestrator/server.go:153/161` — `writeJSON` / `writeErr`.
- `github.com/google/uuid` `uuid.NewString()` (`hypothesis.go:165`, `submit.go:100`) — for `run_id`.
- `internal/testutil` `RequireIntegration`, `SetupPostgres` (provisions roles + migrates; idempotent), `NewID` — the store integration harness.
- `internal/config/config.go` `OwnerDSN()` / `OrchestratorDSN()` / `ServiceDSN()` — the DSNs a grants-boundary test connects with (`store_test.go` uses per-role pools).

### Convention Anchors
- **Store file placement**: one file per table/concern (`goalregistry.go`, `auditlog.go`, `embeddingstore.go`) → new `internal/store/runs.go`. Exported struct + `NewXxx(pool *Pool)` + `ctx`-first methods, `%w`-wrapped errors.
- **Consumer-defined interfaces**: `server.go:36-71` defines narrow interfaces (`goalStore`, `auditStore`, …) at the consumer; `Server` holds them (`:74-87`), `NewServer` takes them positionally (`:91-103`), `cmd/orchestrator/main.go:56-68` injects concrete stores. Add a `runStore` interface + a `runs` field + `NewServer` param + `main.go` construction; extend `goalStore` with `List`.
- **Test constructor churn**: a new `NewServer` param forces updating the two helper constructors `newTestServer` (`server_test.go:193`) and `newTestServerRepo` (`server_test.go:199`) **and every direct positional `NewServer(...)` literal** — `server_test.go:377`, `server_test.go:400`, and `handlers_more_test.go:93` (this last one bypasses the helpers, so it is easy to miss). `runLoop`'s new `runID` param forces updating `hypothesis_test.go` calls (`:123/:156/:182`). A fake `runStore` plugs in through the helpers; and because `goalStore` gains `List`, the existing `fakeGoals` double must add a `List(ctx) ([]store.Goal, error)` method or the package stops compiling.
- **DTO-per-response** + **non-nil empty slices** (`heuristics.go:75/91`).
- **Migration grants prose**: every grants SQL carries a header comment explaining the least-privilege rationale and the "roles are cluster-global, provisioned outside migrations" invariant (`0005_grants.up.sql:1-4`). The runs migration carries the same.
- **Grants-boundary integration tests**: `TestAuditBoundaryOrchestrator` and `TestGoalRegistryGrants` in `store_test.go` are the exact templates for `TestRunsGrants` (orchestrator writes; runtime role reads; runtime write denied). Loop/handler tests are in-package (`package orchestrator`) with fakes + `httptest`, driving the loop synchronously via `srv.runLoop(...)` (`hypothesis_test.go`).

### Proposed Alignment
Model `runs.go` on `goalregistry.go`/`auditlog.go` (struct + `NewRuns(pool *Pool)`, `Exec` for writes, `QueryRow`/`Query` for reads, the `embeddingstore.go:60-82` `Query`/`rows.Next`/`rows.Err` loop for `List` and the latest-status read). Model `0006_runs` on `0002_goal_registry` (uuid PK) with a grants block mirroring `0005_grants` (INSERT+UPDATE for orchestrator, SELECT for both runtime roles) and a symmetric `REVOKE`+`DROP` down. Add a `runStore` consumer interface and thread it through `Server`/`NewServer`/`main.go`/test constructors exactly as the other stores; shape `GET /goals` on `handleHeuristicSearch` with a dedicated DTO. Two deliberate (no-prior-art) choices: the latest-run-per-goal SQL (`DISTINCT ON`), and the detached terminal write (`context.WithTimeout(context.Background(), short)`).

## Implementation Steps

1. **Add the runs table + grants migration (`0006`)**
   - Create `internal/store/migrations/0006_runs.up.sql`: `CREATE TABLE runs (run_id uuid PRIMARY KEY, optimization_function_id uuid NOT NULL REFERENCES goal_registry(optimization_function_id), status text NOT NULL, started_at timestamptz NOT NULL DEFAULT now(), ended_at timestamptz, failure_reason text)`. Add a btree index `CREATE INDEX runs_goal_started_idx ON runs (optimization_function_id, started_at DESC)` to back the latest-per-goal read. In the same file, mirror `0005_grants.up.sql`'s prose header and append the grants: `GRANT SELECT, INSERT, UPDATE ON runs TO arborette_orchestrator; GRANT SELECT ON runs TO arborette_service;`. (Grants co-located here, not retro-edited into 0005 — see Pattern Survey.) The `REFERENCES` FK is *created* under the owner DSN during migration (`migrate.go` runs as the table owner), which satisfies the required ownership/`REFERENCES` on `goal_registry`. Its *insert-time* RI check runs as the referenced-table owner via an internal system trigger, so it does **not** depend on the `arborette_orchestrator` role holding `SELECT` on `goal_registry` — `TestRunsGrants` must not assume revoking that grant would block a runs insert.
   - Create `internal/store/migrations/0006_runs.down.sql`: symmetric `REVOKE ... ON runs FROM ...` then `DROP TABLE IF EXISTS runs;` (index drops with the table).
   - No `//go:embed` change needed (`migrations.go` globs `*.sql`).

2. **Add the runs store (`internal/store/runs.go`)**
   - Define a small status type: `type RunStatus string` with `RunRunning`/`RunCompleted`/`RunFailed` consts ("running"/"completed"/"failed").
   - `type Run struct { RunID, OptimizationFunctionID string; Status RunStatus; StartedAt time.Time; EndedAt *time.Time; FailureReason *string }` (pointers for the nullable columns; pgx scans SQL NULL into `*T`).
   - `type Runs struct { pool *Pool }` + `NewRuns(pool *Pool) *Runs` (mirror `NewGoalRegistry`).
   - `Create(ctx, runID, optimizationFunctionID string) error` — `INSERT INTO runs (run_id, optimization_function_id, status) VALUES ($1,$2,'running')` (started_at defaults). `%w`-wrapped error.
   - `SetStatus(ctx, runID string, status RunStatus, failureReason string) error` — `UPDATE runs SET status=$2, ended_at=now(), failure_reason=$3 WHERE run_id=$1`; pass `NULLIF($3,'')` or bind `*string` (nil when `failureReason == ""`) so a completed run stores NULL, not empty string.
   - `LatestByGoal(ctx, goalIDs []string) (map[string]Run, error)` — one query, `SELECT DISTINCT ON (optimization_function_id) run_id, optimization_function_id, status, started_at, ended_at, failure_reason FROM runs WHERE optimization_function_id = ANY($1) ORDER BY optimization_function_id, started_at DESC`; iterate with the `embeddingstore.go:60-82` `Query`/`rows.Next`/`rows.Err` idiom; return a map keyed by `optimization_function_id` (goals absent from the map have no run → the handler synthesizes `no run`). Passing goal IDs (rather than a bare "all latest") keeps it to a single query composed with `List` and avoids N+1. An empty `goalIDs` slice binds `= ANY('{}')`, which matches no rows and returns an empty map without error.
   - `FailOrphaned(ctx, reason string) (int64, error)` — `UPDATE runs SET status='failed', ended_at=now(), failure_reason=$1 WHERE status='running'`; returns the affected row count. Called once at orchestrator boot (Step 4) to settle runs abandoned by a prior process crash/SIGTERM/restart — the case the in-process detached write cannot reach, because a killed process never runs the loop's `defer`. **Single-instance assumption (load-bearing):** this is correct only while exactly one orchestrator instance runs (the MVP compose topology); with multiple replicas it would wrongly fail a peer's live run. Multi-replica reconciliation (heartbeat/lease) is explicitly deferred — do not add replicas without replacing this sweep.

3. **Add `GoalRegistry.List` (`internal/store/goalregistry.go`)**
   - `List(ctx) ([]Goal, error)` — `SELECT optimization_function_id, goal_text, evaluation_matrix, datasource_ref, created_at FROM goal_registry ORDER BY created_at DESC`; iterate rows with the `embeddingstore.go` idiom, unmarshalling `evaluation_matrix` per row exactly as `Get` (`:60-63`). Reuses the existing `Goal` struct.

4. **Wire the runs store + `List` through the Server (`internal/orchestrator/server.go`, `cmd/orchestrator/main.go`)**
   - Extend the `goalStore` interface (`server.go:54-57`) with `List(ctx context.Context) ([]store.Goal, error)`.
   - Add a `runStore` interface: `Create(ctx, runID, optID string) error`; `SetStatus(ctx, runID string, status store.RunStatus, reason string) error`; `LatestByGoal(ctx, goalIDs []string) (map[string]store.Run, error)`.
   - Add a `runs runStore` field to `Server` (`:74-87`); add the param to `NewServer` (`:91-103`) and assign it.
   - In `cmd/orchestrator/main.go:56`, construct `runs := store.NewRuns(pool)` and pass it into the `NewServer(...)` call (`:63-67`).
   - **Boot-time orphan reconciliation:** after `runs` is constructed and before `service.RunHTTPServer(...)` (`main.go:71`), call `n, err := runs.FailOrphaned(ctx, "orchestrator restarted")`; log the count (`log.Printf("orchestrator: reconciled %d orphaned run(s) to failed", n)`). A reconciliation error is logged but **non-fatal** — boot proceeds (a stale `running` row is a display nuisance, not a reason to refuse to serve). This delivers R12's shutdown/crash clause that the detached in-process write cannot: `RunHTTPServer` (`internal/service/httpserver.go:50`) drains only in-flight HTTP handlers, never the fire-and-forget `runLoop` goroutines, so a SIGTERM/crash abandons an in-flight run before its `defer` fires and strands the row at `running` until this sweep settles it on the next boot.

5. **Create the `running` row on trigger (`internal/orchestrator/hypothesis.go`)**
   - In `handleTriggerLoop` (`:44-55`), after `lookupGoal` succeeds: `runID := uuid.NewString()`; `if err := s.runs.Create(r.Context(), runID, goal.OptimizationFunctionID); err != nil { log + writeErr(w, 500, "internal error"); return }` — created synchronously on the request context **before** launching the goroutine (per the confirmed decision: eliminates the immediate-subscriber race and surfaces a create failure as a 5xx). Then launch the goroutine passing `runID`: `s.runLoop(ctx, goal, runID)`. Leave the 202 body unchanged.

6. **Terminal status bookkeeping across `runLoop` (`internal/orchestrator/hypothesis.go`)**
   - Change the signature to `runLoop(ctx context.Context, goal store.Goal, runID string)`.
   - Introduce a terminal-error local: `var termErr error` (nil ⇒ completed). At each of the five root-failure sites (`:69/76/85/94/99`) set `termErr = err` (or the sentinel for the non-numeric baseline case) immediately before the existing `s.branchFailure(ctx, id, nil, err)` + `return`. Non-terminal `processCandidate` failures are untouched (separate function, no access to `termErr`).
   - Rework the single `defer` (`:61-64`) to settle the run **on a detached context, before publishing `loop_complete`** so a client reacting to that event sees the settled status:
     ```
     defer func() {
         // A panic unwinds THROUGH this defer with termErr still nil; recover so a
         // crashed run is marked failed rather than mislabeled completed — and so one
         // run's panic cannot kill the whole orchestrator process. Otherwise, a
         // timeout/cancellation that fired ANYWHERE in the run (including
         // mid-candidate-expansion, where per-candidate failures are non-terminal and
         // leave termErr nil) also means the run did not complete.
         if r := recover(); r != nil {
             termErr = fmt.Errorf("hypothesis loop panicked: %v", r)
         } else if termErr == nil && ctx.Err() != nil {
             termErr = ctx.Err()
         }
         writeCtx, cancel := context.WithTimeout(context.Background(), statusWriteTimeout)
         defer cancel()
         status, reason := store.RunCompleted, ""
         if termErr != nil { status, reason = store.RunFailed, termErr.Error() }
         if err := s.runs.SetStatus(writeCtx, runID, status, reason); err != nil {
             log.Printf("orchestrator: hypothesis loop %q: set run status: %v", id, err)
         }
         s.hub.Publish(id, Event{Type: "loop_complete"})
         s.hub.Complete(id)
     }()
     ```
   - Add `const statusWriteTimeout = 5 * time.Second` near `loopTimeout` (`:27`), and add `fmt` to the imports (`hypothesis.go` does not currently import it). Scope of what the detached write buys, precisely:
     - **Deadline/cancellation of the loop's own ctx (in scope):** the `context.Background()`-rooted `writeCtx` detaches the terminal write from the loop's expired `loopTimeout`/ctx (mirrors `httpserver.go:48`) — a `SetStatus` on the already-Done loop ctx would fail immediately, so the detached write is what lets a *timed-out* run still record its terminal status.
     - **Timeout fired mid-expansion (in scope):** the `else if ... ctx.Err() != nil` promotion turns a deadline/cancellation that fired *after* the root baseline — where failures route through `processCandidate`/`branchFailure` and are non-terminal (termErr stays nil) — into a `failed` run, instead of the misleading `completed` a bare fall-through would write (satisfies R12's timeout clause everywhere in the run, not just at the root).
     - **Panic (in scope):** `recover()` marks a crashed run `failed` and contains the blast radius (an unrecovered panic in this background goroutine would otherwise crash the entire orchestrator).
     - **Process kill / SIGTERM (out of scope here):** a killed process never runs this `defer` at all, so no in-process mechanism can settle the row — that case is handled by the boot-time `FailOrphaned` sweep (Step 4).
   - `termErr.Error()` is the already-parsed real reason (`SandboxError.Message`), satisfying the R7/R8 persistence half.

7. **Expose `GET /goals` (`internal/orchestrator/server.go` + a handler)**
   - Register `mux.HandleFunc("GET /goals", s.handleListGoals)` in `Routes()` (`:121-131`).
   - Add `handleListGoals` (new handler, styled on `handleHeuristicSearch`): call `s.goals.List(ctx)`; collect the goal IDs; call `s.runs.LatestByGoal(ctx, ids)`; build `out := make([]goalListItemDTO, 0, len(goals))`, one DTO per goal with `optimization_function_id`, `goal_text`, `created_at`, `status` (the latest run's status, or the synthetic string `"no run"` when the goal is absent from the map), and `failure_reason` (`omitempty`, from the latest run when present — surfaces the persisted reason for R8/downstream shell 04). `writeJSON(w, http.StatusOK, out)`; masked 500 on a store error.
   - Define `goalListItemDTO` with snake_case json tags alongside the handler.

## Verification

- **Runs store round-trip (integration):** add `TestRunsLifecycle` to `internal/store/store_test.go` (gated by `RequireIntegration`+`SetupPostgres`). `Create` a run, assert `LatestByGoal([id])` returns it `running` with nil `EndedAt`/`FailureReason`; `SetStatus(completed, "")` and assert `ended_at` set + `failure_reason` NULL; create a second run for the same goal, assert `LatestByGoal` returns the newer one (started_at DESC); assert a goal id with no runs is absent from the map; assert `LatestByGoal(nil)` returns an empty map, no error. Run: `ARBORETTE_INTEGRATION=1 go test ./internal/store/ -run TestRunsLifecycle`.
- **Orphan reconciliation (integration):** add `TestRunsFailOrphaned` — insert one `running` run and one already-`completed` run, call `FailOrphaned(ctx, "orchestrator restarted")`, assert it returns `1`, the `running` row is now `failed` with that reason and a non-null `ended_at`, and the `completed` row is untouched.
- **Grants boundary (integration):** add `TestRunsGrants` modeled on `TestGoalRegistryGrants`/`TestAuditBoundaryOrchestrator` — orchestrator-role pool can `INSERT`/`UPDATE`/`SELECT`; service-role pool can `SELECT` but its `INSERT`/`UPDATE` is denied. Run: `ARBORETTE_INTEGRATION=1 go test ./internal/store/ -run TestRunsGrants`.
- **Loop transitions (unit, `internal/orchestrator/hypothesis_test.go`):** drive `srv.runLoop(ctx, goal, runID)` with a fake `runStore` recording each `SetStatus` call's `(status, reason)` **and** its `ctx.Err()` at call time (needed for the detachment assertion below). Assert: (a) a clean run ⇒ `SetStatus(runID, completed, "")`; (b) a **root** failure (e.g. fake sandbox `Introspect` returns a `*SandboxError{Status:400, Message:"field not present in schema: X"}`) ⇒ `SetStatus(runID, failed, "field not present in schema: X")` — the real reason, not a bare status; (c) a run whose baseline succeeds but every candidate `branchFailure`s (siblings all fail, none via ctx timeout) ⇒ status stays `completed` (per the confirmed classification), `SetStatus` never called with `failed`. Update the existing `runLoop` call sites in this file for the new `runID` arg.
- **Mid-expansion timeout ⇒ `failed` (unit):** script a successful root baseline, then a candidate whose `Execute` returns a `context.DeadlineExceeded`-class error, with the loop `ctx` already expired; assert the run settles `failed` (the defer's `ctx.Err()` promotion), **not** `completed` — this is the R12-timeout case a root-only test misses.
- **Terminal write runs on a live (detached) context (unit):** because the fake `runStore.SetStatus` records its own `ctx.Err()`, drive `runLoop` with an already-cancelled loop `ctx` and assert `SetStatus` was invoked with `ctx.Err() == nil` — proving the write ran on the `context.Background()`-derived `writeCtx`, not the dead loop ctx. This is the assertion that actually catches a regression reusing the loop ctx (a ctx-ignoring fake would silently pass otherwise).
- **`GET /goals` (unit, `internal/orchestrator/server_test.go`):** via `newTestServer` with fake `goalStore.List` returning two goals and a fake `runStore.LatestByGoal` returning a run for only one; assert the JSON body is a non-nil array, the goal with a run shows its status (+`failure_reason` when failed), and the goal without a run shows `"no run"`. Update `newTestServerRepo` and the `NewServer(...)` literals for the new `runs` param + fake.
- **Build + full package tests:** `go build ./...`; `go test ./internal/orchestrator/...`; `go vet ./...`.
- **Edge cases to spot-check:** `SetStatus` on a completed run writes NULL `failure_reason` (not `""`); `LatestByGoal` with an empty id slice returns an empty map without error; `FailOrphaned` on a table with no `running` rows returns `0` and is a no-op; a goal-registry `List` on an empty table returns `[]`/`null`-safe `[]`; the `running`-row `Create` failure path returns 500 and does not launch the goroutine.

## Context Files

- `internal/orchestrator/hypothesis.go` — `handleTriggerLoop`, `runLoop`, `processCandidate`, `branchFailure`, the single `defer`, `loopTimeout`; the five root-failure sites and where terminal bookkeeping + the detached write belong.
- `internal/orchestrator/sandboxclient.go` — `SandboxError` (Status/Message) and the `{error}`-body parse; confirms the terminal reason string is already the real message.
- `internal/orchestrator/server.go` — consumer interfaces (`goalStore`, …), `Server` fields, `NewServer`, `Routes()`, `writeJSON`/`writeErr`; where `runStore` + the `GET /goals` route are added.
- `internal/orchestrator/heuristics.go` — GET-handler + DTO-slice response template for `handleListGoals`.
- `internal/store/goalregistry.go` — store constructor/`Exec`/`QueryRow` conventions and the `Goal` unmarshal path `List` reuses.
- `internal/store/auditlog.go` — sole-writer store prose + least-privilege boundary comment to mirror in `runs.go`.
- `internal/store/embeddingstore.go` — the only multi-row `Query`/`rows.Next`/`rows.Err` loop; the idiom for `List` and `LatestByGoal`.
- `internal/store/migrations/0005_grants.up.sql` + `0002_goal_registry.up.sql` — grants block and uuid-PK table templates for `0006_runs`.
- `internal/store/migrate.go` / `internal/store/migrations/migrations.go` — how migrations embed and apply (no code change; confirms `0006` is picked up automatically).
- `internal/store/store_test.go` — `TestGoalRegistryGrants` / `TestAuditBoundaryOrchestrator` templates for `TestRunsGrants`, and the per-role-pool harness.
- `internal/orchestrator/hypothesis_test.go` + `server_test.go` — how the loop is driven synchronously and how fakes/`newTestServer` are constructed; the call sites needing the new `runID`/`runs` params.
- `cmd/orchestrator/main.go` — store construction + `NewServer` injection site for `store.NewRuns(pool)`, and the boot slot for the `FailOrphaned` reconciliation sweep.
- `internal/service/httpserver.go` — `RunHTTPServer`/`Shutdown` drains only in-flight HTTP handlers, not `runLoop` goroutines; the reason process-kill survival is covered by the boot-time `FailOrphaned` sweep rather than the detached in-process write.
