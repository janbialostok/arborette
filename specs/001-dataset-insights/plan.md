# Implementation Plan: Dataset Insights

**Branch**: `001-dataset-insights` | **Date**: 2026-08-12 | **Spec**: [spec.md](spec.md)

**Input**: Feature specification from `/specs/001-dataset-insights/spec.md`

## Summary

This feature replaces the existing hierarchy with a new model where datasets are the top-level entities, objectives are replaced with questions, and heuristics are replaced with insights. The system targets an analyst audience rather than data scientists or engineers, with special support for time-series datasets that get updated on a regular cadence. Users can ask questions of datasets, which turn into insights, with automated insight generation from periodic data changes.

## Technical Context

**Language/Version**: Go 1.24

**Primary Dependencies**: 
- Neo4j for graph database storage
- pgvector for embeddings storage
- Visualization library for time-series charts and graphs

**Storage**: 
- Neo4j (graph database) for dataset/question/insight relationships
- PostgreSQL with pgvector for storing embeddings
- Time-series optimized tables for efficient storage of time-series data

**Testing**: Go testing package with testify for assertions

**Target Platform**: Linux server environment

**Project Type**: web-service

**Performance Goals**: 
- 1000 req/s for dataset operations
- 500 req/s for question/insight operations
- Real-time visualization updates for time-series data

**Constraints**: 
- <200ms p95 for read operations
- <500ms p95 for write operations
- <100ms for visualization rendering

**Scale/Scope**: 
- 100k datasets (70% time-series)
- 1M questions
- 10M insights
- Support for weekly/monthly data updates

## Constitution Check

*GATE: Must pass before Phase 0 research. Re-check after Phase 1 design.*

1. Library-First: Every feature starts as a standalone library; Libraries must be self-contained, independently testable, documented; Clear purpose required - no organizational-only libraries
2. CLI Interface: Every library exposes functionality via CLI; Text in/out protocol: stdin/args → stdout, errors → stderr; Support JSON + human-readable formats
3. Test-First (NON-NEGOTIABLE): TDD mandatory: Tests written → User approved → Tests fail → Then implement; Red-Green-Refactor cycle strictly enforced
4. Integration Testing: Focus areas requiring integration tests: New library contract tests, Contract changes, Inter-service communication, Shared schemas
5. Observability: Text I/O ensures debuggability; Structured logging required

## Project Structure

### Documentation (this feature)

```text
specs/001-dataset-insights/
├── plan.md              # This file (/speckit.plan command output)
├── research.md          # Phase 0 output (/speckit.plan command)
├── data-model.md        # Phase 1 output (/speckit.plan command)
├── quickstart.md        # Phase 1 output (/speckit.plan command)
├── contracts/           # Phase 1 output (/speckit.plan command)
└── tasks.md             # Phase 2 output (/speckit.tasks command - NOT created by /speckit.plan)
```

### Source Code (repository root)

```text
internal/
├── dataset/
│   ├── models/
│   ├── services/
│   └── handlers/
├── question/
│   ├── models/
│   ├── services/
│   └── handlers/
├── insight/
│   ├── models/
│   ├── services/
│   └── handlers/
├── timeseries/
│   ├── models/
│   ├── services/
│   └── handlers/
├── visualization/
│   ├── models/
│   ├── services/
│   └── handlers/

pkg/
├── storage/
│   ├── neo4j/
│   ├── postgres/
│   └── timeseries/
├── utils/
├── analytics/

cmd/
├── dataset-service/
├── question-service/
├── insight-service/
├── timeseries-service/
├── visualization-service/

tests/
├── contract/
├── integration/
└── unit/
```

**Structure Decision**: The project follows a modular structure with separate internal packages for each entity (dataset, question, insight) and additional packages for time-series functionality and visualization. Each entity has its own models, services, and handlers to maintain clear separation of concerns. Time-series functionality is separated into its own module to handle the specialized requirements for periodic data updates and analysis.

## Constitution Check Re-evaluation

After Phase 1 design, all constitutional principles continue to be satisfied:

1. **Library-First**: Each entity (dataset, question, insight) plus new time-series and visualization modules are implemented as standalone libraries
2. **CLI Interface**: Each service will expose functionality via CLI with REST API endpoints
3. **Test-First**: TDD will be followed for all implementations, including new time-series and visualization features
4. **Integration Testing**: Integration tests will cover new service contracts, time-series comparisons, and visualization rendering
5. **Observability**: Structured logging will cover all services, including time-series data updates and visualization generation

The new time-series and visualization components are designed to be self-contained modules that integrate with the existing architecture without violating any principles.

## Complexity Tracking

> **Fill ONLY if Constitution Check has violations that must be justified**

No violations identified. The design follows all constitutional principles with additional modules for time-series and visualization functionality.
