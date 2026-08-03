---
status: done
spec: .turbo/specs/arborette-v2-dual-engine.md
---

# Plan: Real Launchers & Worker Serve Modes

## Context

`StubLauncher` logs and returns 202 without starting anything, so the local stack cannot demo Phase 2 through its own interface — and V2 adds a second worker (the Verifier) that needs exactly the same invocation seam for batch auto-promotion. This plan makes the `JobLauncher` seam real: the Sleep-Cycle worker gains a serve mode, the orchestrator gains an `HTTPLauncher` selected by config, and the pattern is established for the Verifier (spec R24: its `POST /verifications` endpoint copies the serve-mode + launcher shape built here). The one-shot job invocation keeps working as the AWS Batch seam. Covers spec R24 (partial: Sleep-Cycle serve mode + HTTPLauncher/StubLauncher selection; the Verifier half lands in its own shell) and the Sleep-Cycle side of R27 (serve endpoint requires the shared secret).

Plan risks noted during expansion:

- A sleep-cycle run takes minutes, so `POST /runs` must be **async**: accept, spawn the run on a background context, return 202 — matching AWS Batch `SubmitJob` semantics, which return before the job runs. A synchronous handler would hold the orchestrator's trigger request open for the whole run.
- The Verifier shell (`depends_on` includes this shell) reuses both halves: keep `HTTPLauncher` free of sleep-cycle-specific naming (it posts the args map to a configured **full endpoint URL** — no path is baked into the type; the job name is log-only) so the Verifier can instantiate a second one pointed at its `POST /verifications` endpoint without redesign.

## Pattern Survey

### Analogous Features

- `internal/sandbox/server.go:59` — `Routes()` returns a `http.NewServeMux()` with `mux.HandleFunc("POST /introspect", …)` etc.; this is the exact serve-mode shape the Sleep-Cycle `POST /runs` endpoint should copy — a `Server` struct holding collaborators, a `Routes()` method, per-handler request handling with body caps and validation.
- `cmd/sandbox/main.go:31` — the internal-HTTP-service serve wiring precedent: wire connections, build a `Server`, then `service.RunHTTPServer("sandbox", ":"+cfg.Sandbox.Port, service.BearerAuth(cfg.Sandbox.InternalAuthToken, srv.Routes()))`. The sleepcycle serve mode should mirror this line-for-line.
- `cmd/mcpserver/main.go:31` — second serve-mode precedent; also shows the `service.BearerAuth(token, handler)` wrap and a per-service listen port from config (`cfg.MCP.Port`).
- `internal/orchestrator/sleepcycle.go:13` — `handleTriggerSleepCycle` invokes `s.jobs.Launch(ctx, s.sleepCycleJobName, map[string]string{"optimization_function_id": …})` and returns 202; the new `HTTPLauncher.Launch` must satisfy this existing call unchanged (interface contract: jobName + args map with key `optimization_function_id`).
- `internal/orchestrator/server.go:223` — `mux.Handle("POST /internal/audit", service.BearerAuth(s.internalAuthToken, http.HandlerFunc(s.handleAudit)))` is the established internal-auth wrap for a single internal route.
- `cmd/sleepcycle/main.go:26` — the current one-shot `-goal` flag wiring (flag defaults to `SLEEPCYCLE_GOAL_ID` env); the split must preserve this invocation as the AWS Batch seam while adding a serve branch.
- `internal/sleepcycle/worker.go:133` / `worker.go:182` — `NewWorker(...)` (wiring, validates config once) is already separated from `(*Worker) Run(ctx, goalID)` (one cycle per call). The wiring/run split is **already present at the package level**: serve mode wires one `Worker` and calls `Run` per request. The split needed is in `cmd/sleepcycle/main.go`, not in the worker package.

### Reusable Utilities

- `internal/service/httpserver.go:32` — `RunHTTPServer(name, addr, handler)` — serve + graceful shutdown + Slowloris `ReadHeaderTimeout`; the sleepcycle serve mode calls this directly.
- `internal/service/auth.go:27` — `BearerAuth(token, next)` — shared-secret middleware from the service-seams plan; empty token fails open with a logged warning. Wrap the sleepcycle `Routes()` with `BearerAuth(cfg.SleepCycle.InternalAuthToken, …)` using the same `INTERNAL_AUTH_TOKEN` all services share.
- `internal/service/json.go` — `service.WriteJSON` / `service.WriteErr` — the JSON response writers every handler answers through.
- `internal/service/httpclient.go:27` — `DrainAndClose(resp)` — deferred body cleanup for HTTP clients; `HTTPLauncher` (an HTTP client to the worker) should use it.
- `internal/orchestratorclient/client.go` and `internal/sandboxclient/client.go` — the two existing internal HTTP client patterns `HTTPLauncher` should mirror: `Client{baseURL, client *http.Client, authToken}`, constructor takes `(baseURL, authToken, httpClient)` with nil client → default timeout, requests stamp `Authorization: Bearer <token>` only when the token is non-empty, response handling enforces the expected status and decodes an `{"error":…}` body into a typed error (`OrchestratorError`/`SandboxError`).
- `internal/config/config.go:351-392` — `env`/`intEnv`/`int64Env` helpers; new config fields (worker URL on the orchestrator side, listen port on the worker side) follow this convention.

### Convention Anchors

- **Interface + stub, production seam behind it**: `internal/orchestrator/joblauncher.go` defines `JobLauncher` (contract = jobName + args map) with `StubLauncher` as the fail-safe default. `HTTPLauncher` is a new implementer in this same file/package; selection happens in `cmd/orchestrator/main.go:82` where `orchestrator.StubLauncher{}` is currently passed to `NewServer`. The doc comment (`joblauncher.go:19-22`) already names the args contract the production launcher must honor.
- **Infra-constructor convention**: every `NewServer`/`NewClient`/`NewWorker` takes primitives and narrow interface types only (stated at `internal/sandbox/server.go:44`, `internal/orchestrator/server.go:160`); new constructors follow suit.
- **One Dockerfile per `cmd/<service>`, `ENTRYPOINT ["/binary"]`** (`cmd/sleepcycle/Dockerfile:12`): flags are appended as `command:` in compose. The sleepcycle image is `CGO_ENABLED=0` distroless-static — the serve mode must **not** pull in `internal/sandbox` (CGO DuckDB), consistent with the no-import rule documented at `internal/sandboxclient/client.go:5-11`.
- **Shared secret is one variable**: `INTERNAL_AUTH_TOKEN` is read by orchestrator, sandbox, and sleepcycle configs into `InternalAuthToken` (`internal/config/config.go:301,309,322`); both verifier and caller sides read the same env var by design. The sleepcycle serve mode verifies it; the orchestrator's `HTTPLauncher` presents it.
- **Config-driven local vs production selection**: precedent is `buildStageCache` (`cmd/sandbox/main.go:75`, zero budget → nil → fallback behavior) and the MCP `PublicURL`/token fatal check in `cmd/mcpserver/main.go`. The launcher selection ("worker URL set → `HTTPLauncher`, else `StubLauncher`") is the same empty-config-fallback idiom.
- **compose one-shot job vs long-running service**: the `sleepcycle` service uses `profiles: ["jobs"]` (`docker-compose.yml:174`) so `make up` skips it; long-running services (orchestrator, sandbox, mcpserver, web) publish a port and have no profile. A sleepcycle *serve* mode becomes a second, un-profiled service block while the profiled one-shot block stays for the Batch/`make sleep-cycle GOAL=…` path. The `x-app-environment` anchor (`docker-compose.yml:10`) supplies compose hostnames.
- **Web trigger path is already wired end-to-end**: `web/app/api/orchestrator/goals/[id]/sleep-cycle/route.ts:11` forwards `POST /goals/{id}/sleep-cycle` to the orchestrator, which calls `handleTriggerSleepCycle` → `jobs.Launch`. No web change is needed — swapping `StubLauncher` for `HTTPLauncher` makes the existing button real.

### Proposed Alignment

Follow the existing patterns closely; this task is almost entirely composition of established seams rather than new design. Add `HTTPLauncher` alongside `StubLauncher` in `internal/orchestrator/joblauncher.go` modeled on `orchestratorclient`/`sandboxclient` (configured full endpoint URL + shared-secret bearer + `DrainAndClose` + typed error), select it in `cmd/orchestrator/main.go` by a new worker-URL config field (empty → `StubLauncher`). Give `cmd/sleepcycle/main.go` a serve branch that wires the `Worker` once and calls the existing reentrant `Run(ctx, goalID)` per `POST /runs` request, wrapped in `service.BearerAuth` + `service.RunHTTPServer`, copying `cmd/sandbox/main.go` almost verbatim; keep the `-goal` one-shot path as the Batch seam. Two constraints: (1) the wiring/run split already exists inside `internal/sleepcycle` — do not refactor the worker, only `cmd/sleepcycle/main.go`; (2) the sleepcycle binary must stay CGO-free. The `POST /runs` handler and its auth wrap are the exact template the Verifier's `POST /verifications` shell will copy.

## Implementation Steps

1. **Add serve-mode config fields**
   - In `internal/config/config.go`, add `Port string` to `SleepCycleConfig` (loaded as `env("SLEEPCYCLE_PORT", "8084")` — 8080–8083 are taken by orchestrator/sandbox/mcp/web) and `SleepCycleWorkerURL string` to `OrchestratorConfig` (loaded as `os.Getenv("SLEEPCYCLE_WORKER_URL")` — deliberately no default, because empty means "keep the `StubLauncher`/AWS Batch seam").
   - Extend the two structs' doc comments: `SleepCycleWorkerURL` documents the selection rule (set → `HTTPLauncher` dispatches to the worker's serve mode; empty → `StubLauncher`, the AWS Batch seam remains the production path) and that the value is the **full endpoint URL including the `/runs` path** (the launcher POSTs to it verbatim — that's what keeps the type reusable for the Verifier's differently-pathed endpoint); `SleepCycleConfig.Port` documents that serve mode is the local/long-running alternative to the one-shot Batch job.
   - Add both variables to `.env.example` with comments matching its existing style.

2. **Build the Sleep-Cycle serve surface (`internal/sleepcycle/server.go` + `server_test.go`)**
   - New `Server` struct holding a narrow `runner` interface (`Run(ctx context.Context, goalID string) error` — satisfied by `*Worker`, fakeable in tests) and `maxBodyBytes int64`; constructor `NewServer(runner, maxBodyBytes)` per the infra-constructor convention.
   - `Routes() http.Handler` returning a `http.NewServeMux()` with one route, `mux.HandleFunc("POST /runs", s.handleRun)`, mirroring `internal/sandbox/server.go:59`.
   - `handleRun`: cap the body with `http.MaxBytesReader` (as `internal/sandbox/server.go:150` does), decode `{"optimization_function_id": string}`, reject a missing/empty id with `service.WriteErr(w, http.StatusBadRequest, …)`.
   - **Async dispatch**: guard with a per-goal in-flight map (`sync.Mutex` + `map[string]struct{}`); a duplicate trigger for a goal already running returns `409 Conflict` (a deliberate local-mode divergence from Batch, which would queue — document it on the handler). Otherwise spawn `go func() { … s.runner.Run(ctx, id) … }` on `context.Background()` (the run must outlive the HTTP request), log start/completion/error, clear the in-flight entry, and answer `service.WriteJSON(w, http.StatusAccepted, map[string]any{"optimization_function_id": id})` — the same 202 shape `handleTriggerSleepCycle` returns.
   - **Shutdown disposition (deliberate)**: the run goroutine is fire-and-forget — `RunHTTPServer`'s graceful drain covers in-flight HTTP requests only, so SIGTERM kills a mid-run cycle, exactly as it would kill the one-shot Batch job. Document this on the handler; do not add run-tracking shutdown machinery.
   - Tests (table-driven, `httptest`, fake runner): 202 + runner invoked with the decoded goal id (synchronize on a channel from the fake); 400 on missing id and on malformed JSON; 409 while a first run for the same goal is still in flight, and a fresh 202 after it finishes; body-cap rejection. Auth is not tested here — `BearerAuth` is applied in `main` and has its own tests (`internal/service/auth_test.go`).

3. **Add the serve branch to `cmd/sleepcycle/main.go`**
   - Add a `-serve` bool flag alongside `-goal`. Extract the current wiring block (neo4j, postgres, embedding provider, LLM client, `sleepcycle.NewWorker(...)`) into a helper both branches share. The block's `defer repo.Close(ctx)` / `defer pool.Close()` must NOT move into the helper (they would fire at the helper's return, handing serve mode a worker with dead connections): the helper returns `(*sleepcycle.Worker, cleanup func(), error)` and `main` defers the cleanup so the connections live for the whole serve loop.
   - Serve branch: build `sleepcycle.NewServer(worker, maxBodyBytes)` (reuse the 1 MiB default idiom; a request body here is one UUID, so no new config knob — hardcode `1<<20` with a comment), then `service.RunHTTPServer("sleepcycle", ":"+cfg.SleepCycle.Port, service.BearerAuth(cfg.SleepCycle.InternalAuthToken, srv.Routes()))`, copying `cmd/sandbox/main.go:66-70`.
   - One-shot branch: byte-for-byte the current behavior (`-goal`/`SLEEPCYCLE_GOAL_ID` required, run once, exit) — this stays the AWS Batch entry point. `-serve` and `-goal` together is a fatal usage error (the modes are exclusive) — but detect it via `flag.Visit` (was `-goal` explicitly passed?), NOT via the resolved value: `-goal` defaults to `os.Getenv("SLEEPCYCLE_GOAL_ID")`, and the serve container inherits `env_file: [.env]`, so a resolved-value check would fatal serve boot whenever that env var happens to be set. Serve mode ignores the `SLEEPCYCLE_GOAL_ID` env fallback.
   - Update the package doc comment (currently "is a one-shot batch job, not a service") to describe both modes and name the Batch seam as the production path.

4. **Implement `HTTPLauncher` in `internal/orchestrator/joblauncher.go` (+ `joblauncher_test.go`)**
   - `HTTPLauncher` struct `{endpointURL string, authToken string, client *http.Client}` with `NewHTTPLauncher(endpointURL, authToken string, client *http.Client) *HTTPLauncher` (nil client → short default timeout, ~10s: `Launch` is a dispatch acknowledgment, the worker answers 202 before running — mirror the constructor shape of `internal/orchestratorclient/client.go`). `endpointURL` is the **full** launch endpoint (e.g. `http://sleepcycle-serve:8084/runs`) — no path is appended inside the type.
   - `Launch(ctx, jobName, args)` POSTs `args` as the JSON body to `endpointURL` verbatim, stamps `Authorization: Bearer <token>` when the token is non-empty, defers `service.DrainAndClose(resp)`, and treats anything but `202` as an error that includes the status and the decoded `{"error":…}` message (follow `orchestratorclient`'s typed-error shape). `jobName` is log-only — the configured endpoint URL, not the name, selects the worker, which is what lets the Verifier shell instantiate a second launcher pointed at its `POST /verifications` endpoint without touching this type.
   - Keep `StubLauncher` untouched; extend the file's doc comments so the three-way story is explicit: `StubLauncher` (nothing configured), `HTTPLauncher` (local serve mode), AWS Batch `SubmitJob` (future production implementer, still behind the same interface).
   - Tests with `httptest.NewServer`: configure the launcher with `server.URL + "/runs"` and assert method and path `/runs` arrive as configured (pinning that the type appends nothing), bearer header present (and absent when token empty), body equals the args map; 202 → nil; 409/500 with `{"error":…}` body → error carrying the message; connection-refused → error.

5. **Select the launcher in `cmd/orchestrator/main.go`**
   - At `cmd/orchestrator/main.go:82`, replace the unconditional `orchestrator.StubLauncher{}` with the empty-config-fallback idiom: `cfg.Orchestrator.SleepCycleWorkerURL != ""` → `orchestrator.NewHTTPLauncher(url, cfg.Orchestrator.InternalAuthToken, nil)`, else `orchestrator.StubLauncher{}`. Log which launcher was selected (matching the existing "wired …" boot log style).

6. **Wire docker-compose and Makefile**
   - In `docker-compose.yml`, add an un-profiled `sleepcycle-serve` service: same `build`/`env_file`/`x-app-environment` block as the existing `sleepcycle` job (`docker-compose.yml:173-191`), plus `command: ["-serve"]`, port `8084:8084`, and the same `depends_on` set. Keep the profiled one-shot `sleepcycle` block untouched (it documents and exercises the Batch seam; update its adjacent comment to say so).
   - Add `SLEEPCYCLE_WORKER_URL: http://sleepcycle-serve:8084/runs` (full endpoint URL, `/runs` included) to the orchestrator service's `environment` block so the local stack selects `HTTPLauncher`.
   - No Makefile target changes required (`make up` now includes `sleepcycle-serve` automatically; `make sleep-cycle GOAL=…` keeps hitting the one-shot block) — but extend the Makefile/compose header comments that currently describe sleepcycle as one-shot-only.

## Verification

- `go build ./...` and `go vet ./...` pass; `CGO_ENABLED=0 go build ./cmd/sleepcycle` still builds (guards the no-CGO constraint).
- `go test ./internal/sleepcycle/ ./internal/orchestrator/ ./internal/config/` — new `server_test.go` (202/400/409/body-cap/in-flight-clear) and `joblauncher_test.go` (path, auth header, status handling) pass alongside existing suites.
- Local smoke (per the stack's standard ops flow): `make up`, register a goal against the bundled dataset, then `curl -X POST localhost:8080/goals/<optimization_function_id>/sleep-cycle` (or the web UI button). Expected: orchestrator answers 202 and logs the HTTP launch (not the stub log line); `docker compose logs sleepcycle-serve` shows the run starting and completing; the audit log gains `sleep_cycle_trigger` and, on completion, the worker's terminal `sleepcycle_run_complete` record. Note: with the bundled 20-row `orders.csv` the run completes but publishes no winners (MinSupport floor) — completion is read from the audit trail, not from published Meta-Heuristics.
- Edge cases to spot-check: double-press the trigger button while a run is live → second orchestrator call returns 500 ("failed to launch sleep cycle") from the worker's 409 — acceptable for now, note it if it bothers; stop `sleepcycle-serve` and trigger → orchestrator 500 with a logged connection error, stack otherwise healthy; comment out the `SLEEPCYCLE_WORKER_URL` line in the orchestrator's compose `environment` block and recreate the service (compose `environment` wins over `env_file`, so unsetting it in `.env` is not enough) → boot log shows the stub selected and the old 202-noop behavior returns; wrong `INTERNAL_AUTH_TOKEN` on one side → worker answers 401 and the launch fails loudly (not silently).

## Context Files

- `internal/orchestrator/joblauncher.go` — the interface and stub being extended; its doc comment carries the args contract.
- `internal/orchestrator/sleepcycle.go` — the caller of `Launch`; its behavior must not change.
- `cmd/sleepcycle/main.go` — the file being split into one-shot vs serve branches; all worker wiring lives here.
- `internal/sleepcycle/worker.go` (`NewWorker`, `Run`) — confirms the worker is already wire-once/run-many; the serve mode must not refactor it.
- `cmd/sandbox/main.go` — the serve wiring template (`RunHTTPServer` + `BearerAuth` + `Routes()`).
- `internal/sandbox/server.go` — the `Server`/`Routes()`/handler shape (body caps, `WriteErr`/`WriteJSON`) the new `internal/sleepcycle/server.go` copies.
- `internal/orchestratorclient/client.go` — the internal HTTP client pattern (`newRequest` bearer stamping, typed error decode) `HTTPLauncher` mirrors.
- `internal/config/config.go` — config structs, env helpers, and the `INTERNAL_AUTH_TOKEN` one-variable convention.
- `docker-compose.yml` — the profiled one-shot block being kept and the service-block conventions the new `sleepcycle-serve` entry follows.
