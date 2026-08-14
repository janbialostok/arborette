# Contract: `cleanup` CLI

The operator-invoked action (spec FR-001..004) and the post-suite gate
(FR-008..010). A one-shot binary at `cmd/cleanup`, mirroring the `cmd/migrate` /
`cmd/dbbootstrap` precedent: load `.env`-style config, run to completion, exit.

## Usage

```text
cleanup clean    # wipe every dataset, objective, and derived record/artifact
cleanup check    # verify the stores are clean; exit non-zero if not
```

- `cleanup` with no arguments prints usage and exits non-zero.
- Both modes connect as the **owner** Postgres role (only role with DELETE on
  every table, including `audit_log`) plus the configured Neo4j and object-store
  clients.

## `clean` behavior

1. Wipe the record store via `store.ResetAll` (one transaction, FK-safe order —
   see `data-model.md`).
2. Wipe the graph via `graph.Wipe` (whole-graph `DETACH DELETE`).
3. Wipe the object store via `objectstore.Wipe` (list + batch delete).
4. Log a summary line: per-table rows removed, graph nodes removed, object keys
   removed.
5. Exit `0`.

Idempotent: any or all stores may already be empty; the command still succeeds
and logs zeroes (FR-003). Connection failure to any store is fatal (exit `1`)
with a message naming the store — a half-wiped state must never read as
"cleaned".

## `check` behavior (the gate)

1. For each PG table, count rows (`datasets`, `goal_registry`,
   `data_source_registry`, `runs`, `verification_queue`, `causal_verifications`,
   `meta_heuristic_embeddings`, `sessions`, `users`, `audit_log`).
2. Count graph nodes (`MATCH (n) RETURN count(*)`).
3. Count object-store keys.
4. If every count is zero: print a one-line clean confirmation and exit `0`
   (FR-010 — a freshly `clean`-ed environment passes).
5. If any count is non-zero: print each offending table with its row count and a
   sample of ids, plus the graph/object counts, and exit `1` (FR-008 — the
   offenders are named).

## Environment

Same variables as every service (`config.Load`): Postgres owner DSN parts,
`NEO4J_URI`/`NEO4J_USER`/`NEO4J_PASSWORD`, `S3_ENDPOINT`/`S3_BUCKET`/`S3_REGION`/
`S3_ACCESS_KEY`/`S3_SECRET_KEY`/`S3_PATH_STYLE`.

## Exit codes

| Code | Meaning |
|------|---------|
| 0 | Wipe completed, or check found the stores clean |
| 1 | Wipe failed, or check found residue (offenders printed) |
| 2 | Usage error (unknown mode) |
