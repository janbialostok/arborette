# Arborette — Agent Guide

## Build & Dependencies

- The Go toolchain is pinned to **1.24** (`GOTOOLCHAIN`). Pin every new dependency
  to a Go-1.24-compatible version. In particular `github.com/jackc/pgx/v5` must
  stay ≤ v5.7.x — v5.8+ requires Go 1.25 and will fail `go mod tidy` under the
  pinned toolchain.
- `golang-migrate/migrate/v4`'s S3 source driver transitively pulls in an
  ancient `aws-sdk-go-v2/service/s3` (predating `Options.BaseEndpoint`). Keep
  `service/s3` explicitly required at a current version, or the object-store
  client's `BaseEndpoint`/`UsePathStyle` (MinIO/S3 selection) will not compile.
- `github.com/modelcontextprotocol/go-sdk` (the MCP server) must stay at **v1.3.1**
  — the version `github.com/anthropics/anthropic-sdk-go` v1.58.0 transitively
  requires. Pinning it lower (e.g. v0.8.0) makes `go mod tidy` silently downgrade
  `anthropic-sdk-go` to v1.40.0, breaking the orchestrator's structured-output
  usage. The MCP APIs in use (`AddTool`, `ToolHandlerFor`, the streamable handler,
  in-memory transports) are identical across v0.8.0 and v1.3.1.
- The web UI (`web/`) is a separate Node/Next.js deployable, not part of the Go
  module. It is pinned to **Next.js ≥15 (App Router) on Node 22** with a
  committed `web/package-lock.json`. Keep Next at ≥15: the TypeScript
  `next.config.ts` and the uncached-by-default GET Route Handlers both break on
  Next 14. It ships as its own image (`web/Dockerfile`, `output: 'standalone'`)
  with a `./web` build context — never the repo root.

## Testing

- Integration tests are gated on the `ARBORETTE_INTEGRATION` env var and skip
  cleanly when it is unset, so `go build`, `go vet`, and `go test ./...` stay
  green without any infrastructure.
- Run the integration suite against a live stack: `make up` (compose) then
  `make test`, which sets the localhost host overrides plus
  `ARBORETTE_INTEGRATION=1` and runs `go test -p 1`. Use `-p 1`: the packages
  share one Postgres database and must not run concurrently.
- Running a scoped subset (`go test ./internal/<pkg>/`) from the host still needs
  those localhost overrides — especially `S3_ENDPOINT=http://localhost:9000`.
  `.env` leaves `S3_ENDPOINT` unset (compose injects the in-network `minio:9000`),
  so without the override the S3 client targets real AWS and fails with a
  misleading `InvalidAccessKeyId` rather than an endpoint error.
- The integration suite shares one Postgres database (hence `-p 1`), so tests must
  tolerate rows other tests leave behind: never assert a table-**global** count
  (e.g. a status-sweep's affected-row count) — assert per-row effects instead. When
  asserting an `ORDER BY` over `now()`-defaulted timestamps, `time.Sleep` a couple
  ms between inserts so the ordering is deterministic.
- `make test` **truncates** `meta_heuristic_embeddings` (`internal/testutil`), so a
  run against the live stack destroys the pgvector rows of every Meta-Heuristic a
  real Sleep-Cycle run published. The graph side is untouched, so those nodes keep
  `embedding_pending = false` and the resume pass skips them — they stay reachable
  by `trace_causal_chain` while returning nothing from `get_optimized_heuristics`.
  Never compare an embeddings-table observation taken before the gate with one
  taken after; re-publish (or re-embed) before drawing conclusions.
- The graph side is not left alone either: the suite seeds fixture
  `MetaHeuristic` nodes into the shared dev Neo4j and never removes them, so
  residue accumulates run over run (a real instance reached 169 of 194 nodes,
  from the `"abstraction"` and `"reducing threshold restores latency"` fixtures
  among others). Combined with the truncate above, one `make test` leaves the two
  stores diverged in **both** directions — nodes with no embedding, and stale
  embeddings whose fixture node was seeded by an earlier run. When judging drift,
  filter fixture definitions out first; when verifying against the live stack,
  expect to restore the corpus after every gate run.
- Audit records carry their detail as `map[string]any`, so a nil slice or a typed
  nil pointer stored in one is never `== nil`. Assert on content (length, a
  specific id) rather than `detail["k"] != nil`, which passes even when the value
  is the nil the assertion means to catch.
- A test double that ignores its `context.Context` cannot catch a context bug. The
  real pools and drivers fail a call on a cancelled context, so a fake that does
  not is why a handler passing the wrong context (the request's, where a detached
  one is required) reads as correct. Have doubles that stand in for a store return
  `ctx.Err()` when the context is done — the moment they do, a test asserting the
  wrong thing fails loudly rather than passing for the wrong reason.
- Scripted-response test doubles (`fakeSandbox`, `fakeClaude`) return a nil error /
  empty success once their scripted slice is exhausted. A test for a bounded retry
  loop must therefore script one failure per attempt **plus** the initial one
  (K repairs → K+1 scripted errors): script too few and a later iteration reads past
  the script, gets the defaulted success, and the loop exits early — persisting the
  goal and passing the assertion for the wrong reason.
- To smoke-test a backend service change over HTTP without rebuilding its (stale)
  compose container, build and run the binary against the live stack: `go build`
  the `cmd/<svc>` binary, then run it with the same localhost overrides `make test`
  uses plus `SANDBOX_URL=http://localhost:8081` and a free `ORCHESTRATOR_PORT` (the
  compose container already holds 8080), sourcing `.env` for credentials — then
  `curl` the endpoint. Boot-time work (migrations already applied, reconciliation
  sweeps) runs too, so the local process exercises the real startup path.
- The Sleep-Cycle Worker is a one-shot job behind the compose `jobs` profile, so
  `make up` neither builds nor starts it, and a bare `docker compose run` reuses
  whatever image already exists — `make sleep-cycle GOAL=<id>` passes `--build` for
  exactly that reason. The REST/UI trigger (`POST /goals/{id}/sleep-cycle`) is
  backed by `StubLauncher` locally: it logs, audits, returns 202, and runs nothing,
  so `make sleep-cycle` is the only way to execute a run against the local stack.
  To run it from the host instead, use the same overrides as the HTTP smoke-test
  plus `ORCHESTRATOR_URL=http://localhost:8080` (it reaches the audit table only
  through that API) and `-goal <optimization_function_id>`; it exits when the run
  finishes rather than serving.
- To preview or smoke-test a `web/` frontend change against the live stack, run
  `make web-dev` — it starts the Next.js dev server on :3000 with your local edits,
  pointed at the orchestrator on :8080 (`ORCHESTRATOR_URL`). The compose `web`
  service (:8083) serves the pre-built standalone image and will **not** reflect
  local `web/` edits, so bring the backend up with `make up` but drive the frontend
  through `make web-dev`, not the :8083 container.
