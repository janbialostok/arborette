---
spec: .turbo/specs/arborette.md
depends_on: [arborette-01-shared-infrastructure-data-layer, arborette-02-orchestrator-api-service, arborette-03-sandbox-execution-service]
---

# Plan: Multi-Modal Extraction

## Context

The original hypothesis loop assumes every data source is tabular — interventions are deterministic DuckDB queries. This shell extends that to unstructured documents (the concrete case: a contract PDF, where the goal is "accurately extract effective date, termination date, and SLOs"), where an intervention is instead an extraction/classification task. The key design constraint is that Citations (the natural way to get a precise source locator from Claude) and structured outputs (needed to keep the extracted value strictly typed) are mutually incompatible on the Claude API — so this shell's extraction path returns only a typed value and confidence, and provenance is computed separately via a deterministic post-hoc text search, not requested from the model.

## Produces

- Document-oriented `DataSource` implementation (PDF, MVP scope) alongside the existing file-based (CSV/Parquet) implementation, conforming to Shell 01's pluggable `DataSource` interface without changing its contract
- Schema/field introspection for a document `DataSource`: given a goal's text plus a sample pass over the document, returns candidate extractable fields (not a tabular column list)
- `extract` intervention-execution path in the Sandbox Execution service, alongside the existing `query` path, behind a `type` discriminated union on the intervention-execution contract
- Extraction call: document supplied via the Files API/PDF input, `output_config.format` (structured outputs) constraining the returned value to the target field's type — no Citations, no freeform text
- Deterministic provenance-locator computation: a standalone PDF-text-extraction pass over the document (independent of the Claude call), locating the extracted value's first matching occurrence and recording its page number and character span; null if no exact match is found
- Extensions to the `Outcome` node/`PRODUCED` edge (defined on Shell 01's shared Neo4j repository interface so every graph-touching service references one settled set): a `verification_status` field with the full five-value domain — `verified`, `unverified`, `confirmed`, `corrected`, `rejected`. **Its node-creation default is `verified`**, so a `query`-type Outcome written by Shell 02's existing triplet-write path is cluster-eligible with no change to that path (Shell 02 need not know the field exists); only the `extract` execution path explicitly writes `unverified`, and Shell 08's HITL resolution writes `confirmed`/`corrected`/`rejected`. Also adds a provenance-locator field, plus the `PRODUCED`-edge confidence weight fixed at 1.0 for `query`-type triplets. This shell defines the field and its complete domain; Shell 08 owns the write-side HITL transitions and Shell 04 reads the domain for its clustering filter, so both reference this shared definition rather than each declaring their own.

## Consumes

- From `arborette-01-shared-infrastructure-data-layer`: Go workspace, pluggable `DataSource` interface, Neo4j repository interface, object-storage client
- From `arborette-02-orchestrator-api-service`: hypothesis-loop tree/depth-level orchestration to hook the `extract`-intervention proposal and per-field sub-tree structuring into (the same loop seam Shell 08 hooks epoch-gating into), and goal-intake schema-introspection wiring to route to the document field-introspection path
- From `arborette-03-sandbox-execution-service`: existing `query`-path intervention-execution contract and file-based `DataSource` implementation, to extend rather than replace

## Covers Spec Requirements

- R2 (partial: document field-introspection — candidate extractable fields for a document `DataSource`)
- R16
- R17
- R18 (partial: the initial deterministic provenance-locator computation at extraction time — the queue, resolution, and UI slices of R18 are Shells 08 and 10)

## Implementation Steps (High-Level)

1. **Document `DataSource` implementation**
   - Reads PDF files from the object store; implements the Shell 01 pluggable interface alongside the existing file-based implementation.
2. **Field-introspection for documents**
   - Given the goal text and a sample pass over the document, returns candidate extractable fields — called during goal intake (R16) instead of the tabular schema-introspection path.
3. **`extract` intervention-execution path**
   - Generalizes the intervention-execution contract to a `query`/`extract` discriminated union; compiles an `extract` intervention (target field + extraction method) into a Claude API call with structured outputs, no Citations.
4. **Provenance-locator computation**
   - Standalone deterministic text-extraction pass over the document; locates the extracted value's first match and records page/character-span, or null on no match.
5. **`Outcome`/`PRODUCED` schema extensions**
   - Adds the `verification_status` field (full five-value domain: `verified`/`unverified`/`confirmed`/`corrected`/`rejected`, defaulting to `verified` at node creation) and the provenance-locator field to the `Outcome` node on Shell 01's shared repository interface; sets `query`-type `PRODUCED`-edge confidence to a fixed 1.0. The `verified` default keeps Shell 02's unchanged query-triplet writes cluster-eligible; the `extract` path (Step 3) explicitly writes `unverified`. Defining the complete domain here lets Shell 04's clustering filter and Shell 08's resolution writes reference one shared set.
6. **Per-field hypothesis sub-trees**
   - For a document goal with multiple target fields, each field is evaluated as its own sub-tree (breadth = competing extraction methods, depth = refinement iterations) rather than one tree spanning all fields.

## Open Questions

- Exact hypothesis-tree default parameters for extraction sub-trees (breadth, depth, stopping condition per field) — left to implementation-time tuning against real sample data.

## Expansion Deferred

The following are filled in when `/expand-shell` runs:

- Pattern survey against the codebase state at implementation time
- Concrete `file_path` references with named functions or symbols for each Implementation Step
- Verification section with specific test commands and smoke checks
- Context Files section with the files to read in full before editing
