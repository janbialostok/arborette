---
spec: .turbo/specs/arborette.md
depends_on: [arborette-01-shared-infrastructure-data-layer]
---

# Plan: Sandbox Execution Service

## Context

The Sandbox Execution service is where the system's core empirical claim gets proven or disproven: every LLM-proposed intervention becomes a deterministic, read-only query over the analyst's actual data, never a hallucinated guess. It's deliberately a separate, stateless, horizontally-scalable service from the Orchestrator — it owns the only DuckDB-over-object-store access path in the system, used both for schema introspection at goal intake and for executing intervention queries during the hypothesis loop. Keeping intervention execution deterministic (parameters compiled into a query, not LLM-generated SQL) is what makes it safe to run untrusted-shaped LLM output against real data.

## Produces

- Schema-introspection API endpoint: reads a file from the object store and returns its schema plus variables relevant to a goal's optimization targets
- Intervention-execution API endpoint: compiles LLM-proposed structured intervention parameters (filter/threshold/aggregation) into a deterministic DuckDB query and executes it read-only, returning the measured result
- File-based `DataSource` implementation behind the shared pluggable interface (CSV/Parquet over the object store)

## Consumes

- From `arborette-01-shared-infrastructure-data-layer`: Go workspace, object-storage client, pluggable `DataSource` interface

Note: DuckDB is this service's own embedded dependency (added directly to its module), not something Shell 1 produces — Shell 1's infrastructure covers Neo4j, PostgreSQL, MinIO, and Ollama only.

## Covers Spec Requirements

- R2 (partial: tabular schema introspection — column/schema information for file-based sources)
- R3 (partial: the file-based `DataSource` implementation proving the interface is genuinely pluggable)
- R4 (partial: reads uploaded/on-disk files from object storage for both introspection and execution)
- R6 (partial: compiles intervention parameters into a deterministic read-only DuckDB query and executes it)

## Implementation Steps (High-Level)

1. **File-based `DataSource` implementation**
   - Reads CSV/Parquet files from the object store (by the key/path the Orchestrator recorded in the goal registry), implementing the Shell 1 pluggable interface.
2. **Schema-introspection endpoint**
   - Given a data-source reference, returns column/schema information and a mapping of variables relevant to a goal's optimization targets — called by the Orchestrator during goal intake (R2).
3. **Intervention-parameter-to-query compilation**
   - Translates structured intervention parameters (from the Orchestrator's LLM proposal) into a deterministic DuckDB SQL query or aggregation — never executes LLM-generated SQL directly, keeping execution injection-safe.
4. **Read-only execution endpoint**
   - Runs the compiled query against the wired file and returns the measured result to the Orchestrator, which writes the resulting causal triplet.

## Open Questions

- None.

## Expansion Deferred

The following are filled in when `/expand-shell` runs:

- Pattern survey against the codebase state at implementation time
- Concrete `file_path` references with named functions or symbols for each Implementation Step
- Verification section with specific test commands and smoke checks
- Context Files section with the files to read in full before editing
