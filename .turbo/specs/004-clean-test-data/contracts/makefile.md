# Contract: Makefile targets

The operator entry points for this feature. `make clean-data` performs the
one-time wipe (Story 1); `make test` now ends with the cleanliness gate
(Story 3) after the suite (Story 2) has run.

## `make clean-data`

```text
make clean-data
```

- Runs `cleanup clean` against the live stack, sourcing `.env` for credentials
  and applying the `HOST_ENV` localhost overrides already used by `make test`
  (`S3_ENDPOINT=http://localhost:9000` etc.), since MinIO is reached from the
  host.
- Idempotent; safe to run twice.
- Prerequisite: `make up` (the stack the binary talks to).

## `make test` (modified)

```text
make test    # existing suite + new gate
```

- Existing behavior unchanged: `set -a; . ./.env; set +a; ARBORETTE_INTEGRATION=1
  go test -p 1 ./...` with the localhost overrides.
- A new final step appends `cleanup check` with the same env. The whole target
  fails if either the tests or the gate fail.
- Documented prerequisite: the stores must be clean to begin with — run
  `make clean-data` once on a cluttered stack before relying on the gate
  (FR-010 / SC-001).
- Bare `go test ./...` (no infra) stays untouched: the gate lives only in the
  Make target, matching the `ARBORETTE_INTEGRATION` gating convention.

## Ordering guarantee

The gate is a *separate command after* `go test`, never a Go test in the
`./...` set: `go test ./...` orders packages by import path, so a "cleanliness"
package would run near the front and see fixtures from suites that run later.
Only a post-suite command can observe the true end state.