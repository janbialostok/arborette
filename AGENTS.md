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

## Testing

- Integration tests are gated on the `ARBORETTE_INTEGRATION` env var and skip
  cleanly when it is unset, so `go build`, `go vet`, and `go test ./...` stay
  green without any infrastructure.
- Run the integration suite against a live stack: `make up` (compose) then
  `make test`, which sets the localhost host overrides plus
  `ARBORETTE_INTEGRATION=1` and runs `go test -p 1`. Use `-p 1`: the packages
  share one Postgres database and must not run concurrently.
