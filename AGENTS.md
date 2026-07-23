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
- To smoke-test a backend service change over HTTP without rebuilding its (stale)
  compose container, build and run the binary against the live stack: `go build`
  the `cmd/<svc>` binary, then run it with the same localhost overrides `make test`
  uses plus `SANDBOX_URL=http://localhost:8081` and a free `ORCHESTRATOR_PORT` (the
  compose container already holds 8080), sourcing `.env` for credentials — then
  `curl` the endpoint. Boot-time work (migrations already applied, reconciliation
  sweeps) runs too, so the local process exercises the real startup path.
