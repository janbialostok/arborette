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

## Testing

- Integration tests are gated on the `ARBORETTE_INTEGRATION` env var and skip
  cleanly when it is unset, so `go build`, `go vet`, and `go test ./...` stay
  green without any infrastructure.
- Run the integration suite against a live stack: `make up` (compose) then
  `make test`, which sets the localhost host overrides plus
  `ARBORETTE_INTEGRATION=1` and runs `go test -p 1`. Use `-p 1`: the packages
  share one Postgres database and must not run concurrently.
