# Tasks: Dataset Insights (Refactoring)

**Input**: Design documents from `/specs/001-dataset-insights/`

**Prerequisites**: plan.md (required), spec.md (required for user stories), research.md, data-model.md, contracts/

**Tests**: Test tasks are included due to the constitutional principle of Test-First (TDD mandatory). Tests are non-negotiable.

**Organization**: Tasks are grouped by user story to enable independent implementation and testing of each story.

## Format: `[ID] [P?] [Story] Description`

- **[P]**: Can run in parallel (different files, no dependencies)
- **[Story]**: Which user story this task belongs to (e.g., US1, US2, US3)
- Include exact file paths in descriptions

---

## Phase 1: Setup (Refactoring Preparation)

**Purpose**: Prepare the project for the refactoring effort

- [x] T001 Create backup branch and document current entity map (objectives → questions, heuristics → insights, etc.) in docs/migration-map.md
- [x] T002 Audit all existing references to old names (objectives, heuristics, etc.) across Go files, SQL files, and web UI in docs/old-name-audit.md
- [x] T003 [P] Update Go module dependencies for time-series and visualization libraries

---

## Phase 2: Foundational (Blocking Prerequisites)

**Purpose**: Core infrastructure that MUST be complete before ANY user story can be implemented

**⚠️ CRITICAL**: No user story work can begin until this phase is complete

- [x] T004 Create database migration to rename tables/columns (objectives → questions, heuristics → insights, etc.) in internal/store/migrations/0015_rename_dataset_insights.up.sql
- [x] T005 [P] Create database migration for time-series tables (dataset_versions) in internal/store/migrations/0016_create_timeseries.up.sql
- [x] T006 [P] Create database migration for visualization tables in internal/store/migrations/0017_create_visualizations.up.sql
- [x] T007 Implement error handling utilities for refactored services in pkg/utils/errors.go
- [x] T008 Setup structured logging configuration for all refactored services in pkg/utils/logging.go
- [x] T009 Run database migrations to apply new schema

**Checkpoint**: Foundation ready - user story implementation can now begin in parallel

---

## Phase 3: User Story 1 - Replace Existing Top-Level Entity with Dataset (Priority: P1) 🎯 MVP

**Goal**: Replace the existing top-level entity (what was previously there) with the new Dataset entity, maintaining existing functionality

**Independent Test**: Create a dataset via API and verify it's stored in Neo4j with correct fields, including time-series support

### Tests for User Story 1

> **NOTE: Write these tests FIRST, ensure they FAIL before implementation**

- [x] T010 [P] [US1] Contract test for dataset creation endpoint in tests/contract/test_dataset.go
- [x] T011 [P] [US1] Integration test for dataset CRUD operations in tests/integration/test_dataset.go
- [x] T012 [P] [US1] Unit test for Dataset model validation in internal/dataset/models/dataset_test.go

### Implementation for User Story 1

- [x] T013 [P] [US1] Create Dataset model with time-series fields in internal/dataset/models/dataset.go
- [x] T014 [P] [US1] Implement DatasetStorage interface in pkg/storage/neo4j/dataset_store.go
- [x] T015 [P] [US1] Implement PostgreSQL storage for datasets in pkg/storage/postgres/dataset_store.go
- [x] T016 [US1] Create Dataset service with CRUD operations in internal/dataset/services/dataset_service.go
- [ ] T017 [US1] Implement dataset handlers (create, read, update, delete, list) in internal/dataset/handlers/dataset_handler.go
- [x] T018 [US1] Update main entry point with dataset routes in cmd/orchestrator/server.go
- [x] T019 [US1] Remove old top-level entity code and update references throughout codebase
- [x] T020 [US1] Add cascading deletion logic for datasets in internal/dataset/services/dataset_service.go

**Checkpoint**: Dataset entity should be fully functional, replacing the old top-level entity

---

## Phase 4: User Story 2 - Replace Objectives with Questions (Priority: P2)

**Goal**: Replace the existing objectives functionality with the new Question entity

**Independent Test**: Create a question for an existing dataset and verify it's stored in Neo4j with correct relationship

### Tests for User Story 2

- [x] T021 [P] [US2] Contract test for question creation endpoint in tests/contract/test_question.go
- [x] T022 [P] [US2] Integration test for question creation and relationship management in tests/integration/test_question.go
- [x] T023 [P] [US2] Unit test for Question model validation in internal/question/models/question_test.go

### Implementation for User Story 2

- [x] T024 [P] [US2] Create Question model in internal/question/models/question.go
- [x] T025 [P] [US2] Implement QuestionStorage interface in pkg/storage/neo4j/question_store.go
- [x] T026 [P] [US2] Implement PostgreSQL storage for questions in pkg/storage/postgres/question_store.go
- [x] T027 [US2] Create Question service with CRUD operations in internal/question/services/question_service.go
- [x] T028 [US2] Implement question handlers (create, read, update, delete, list) in internal/question/handlers/question_handler.go
- [x] T029 [US2] Update main entry point with question routes in cmd/orchestrator/server.go
- [x] T030 [US2] Remove old objectives code and update references throughout codebase
- [x] T031 [US2] Add cascading deletion logic for questions in internal/question/services/question_service.go

**Checkpoint**: Question entity should be fully functional, replacing objectives

---

## Phase 5: User Story 3 - Replace Heuristics with Insights (Priority: P3)

**Goal**: Replace the existing heuristics functionality with the new Insight entity

**Independent Test**: Create an insight for an existing question and verify it's stored in Neo4j with correct relationship

### Tests for User Story 3

- [x] T032 [P] [US3] Contract test for insight creation endpoint in tests/contract/test_insight.go
- [x] T033 [P] [US3] Integration test for insight creation and relationship management in tests/integration/test_insight.go
- [x] T034 [P] [US3] Unit test for Insight model validation in internal/insight/models/insight_test.go

### Implementation for User Story 3

- [x] T035 [P] [US3] Create Insight model with time-series fields in internal/insight/models/insight.go
- [x] T036 [P] [US3] Implement InsightStorage interface in pkg/storage/neo4j/insight_store.go
- [x] T037 [P] [US3] Implement PostgreSQL storage for insights in pkg/storage/postgres/insight_store.go
- [x] T038 [US3] Create Insight service with CRUD operations in internal/insight/services/insight_service.go
- [x] T039 [US3] Implement insight handlers (create, read, update, delete, list) in internal/insight/handlers/insight_handler.go
- [x] T040 [US3] Update main entry point with insight routes in cmd/orchestrator/server.go
- [x] T041 [US3] Remove old heuristics code and update references throughout codebase
- [x] T042 [US3] Ensure individual insights can be deleted without cascading

**Checkpoint**: Insight entity should be fully functional, replacing heuristics

---

## Phase 6: User Story 4 - Implement Time-Series Dataset Support (Priority: P4)

**Goal**: Support time-series datasets that get updated on a regular cadence with versioning and trend analysis

**Independent Test**: Create a time-series dataset, add multiple versions, and verify versioning and comparison features work correctly

### Tests for User Story 4

- [ ] T043 [P] [US4] Contract test for time-series endpoints in tests/contract/test_timeseries.go
- [ ] T044 [P] [US4] Integration test for dataset versioning in tests/integration/test_timeseries.go
- [ ] T045 [P] [US4] Unit test for DatasetVersion model in internal/timeseries/models/version_test.go

### Implementation for User Story 4

- [ ] T046 [P] [US4] Create DatasetVersion model in internal/timeseries/models/version.go
- [ ] T047 [P] [US4] Implement TimeSeriesStorage interface in pkg/storage/timeseries/store.go
- [ ] T048 [P] [US4] Create PostgreSQL time-series optimized tables in pkg/storage/postgres/timeseries_store.go
- [ ] T049 [US4] Create TimeSeries service with versioning in internal/timeseries/services/timeseries_service.go
- [ ] T050 [US4] Implement time-series handlers (create version, compare, trends) in internal/timeseries/handlers/timeseries_handler.go
- [ ] T051 [US4] Update main entry point with time-series routes in cmd/orchestrator/server.go
- [ ] T052 [US4] Implement trend analysis engine in pkg/analytics/trend_detection.go
- [ ] T053 [US4] Implement version comparison logic in internal/timeseries/services/comparison.go

**Checkpoint**: Time-series datasets should be fully supported with versioning, comparison, and trend analysis

---

## Phase 7: User Story 5 - Implement Analyst-Focused UI with Visualizations (Priority: P5)

**Goal**: Update the web UI to target analyst audience with simplified dashboards and visualizations tied to insights

**Independent Test**: Create a time-series dataset with versions, generate insights, and verify that visualizations are properly rendered in the analyst interface

### Tests for User Story 5

- [ ] T054 [P] [US5] Contract test for visualization endpoints in tests/contract/test_visualization.go

### Implementation for User Story 5

- [ ] T055 [P] [US5] Create Visualization model in internal/visualization/models/visualization.go
- [ ] T056 [P] [US5] Create Visualization service in internal/visualization/services/visualization_service.go
- [ ] T057 [US5] Implement visualization handlers in internal/visualization/handlers/visualization_handler.go
- [ ] T058 [US5] Update main entry point with visualization routes in cmd/orchestrator/server.go
- [ ] T059 [P] [US5] Update all web UI views to use new terminology (dataset, question, insight) in web/app/**/*.tsx
- [ ] T060 [P] [US5] Implement simplified analyst dashboard in web/app/dashboard/page.tsx
- [ ] T061 [P] [US5] Implement time-series visualization components (line charts, trend graphs) in web/components/visualizations/
- [ ] T062 [US5] Implement calendar view for time-series period comparison in web/components/visualizations/calendar-view.tsx
- [ ] T063 [US5] Integrate Plotly.js visualization library in web/package.json and web/components/visualizations/
- [ ] T064 [US5] Implement insight-driven visualization generation (visualizations tied to insights, not general dashboard) in web/app/insights/[id]/page.tsx

**Checkpoint**: Web UI should be updated with analyst-focused interface and visualizations tied to insights

---

## Phase 8: Polish & Cross-Cutting Concerns

**Purpose**: Improvements that affect multiple user stories

- [x] T065 [P] Update AGENTS.md to reflect new terminology (dataset, question, insight, time-series, visualization) in AGENTS.md
- [x] T066 [P] Update README.md with new project nomenclature in README.md
- [x] T067 [P] Update CONSTITUTION.md with new terminology in .specify/memory/constitution.md
- [x] T068 [P] Update all documentation references from old to new names in docs/
- [x] T069 Run quickstart.md validation scenarios
- [x] T070 Security audit of all endpoints
- [x] T071 Performance testing for time-series data operations
- [x] T072 Final code cleanup and removal of any remaining old terminology

---

## Dependencies & Execution Order

### Phase Dependencies

- **Setup (Phase 1)**: No dependencies - can start immediately
- **Foundational (Phase 2)**: Depends on Setup completion - BLOCKS all user stories
- **User Stories (Phase 3+)**: All depend on Foundational phase completion
- **Polish (Final Phase)**: Depends on all desired user stories being complete

### User Story Dependencies

- **User Story 1 (P1)**: Can start after Foundational (Phase 2) - No dependencies on other stories
- **User Story 2 (P2)**: Can start after Foundational (Phase 2) - May reference US1 but independently testable
- **User Story 3 (P3)**: Can start after Foundational (Phase 2) - May reference US1/US2 but independently testable
- **User Story 4 (P4)**: Depends on US1 for dataset support - Can start after US1 completion
- **User Story 5 (P5)**: Depends on US3 for insight support and US4 for time-series data - Can start after US3/US4 completion

### Within Each User Story

- Tests MUST be written and FAIL before implementation
- Models before services
- Services before endpoints
- Core implementation before integration
- Story complete before moving to next priority

### Parallel Opportunities

- All Setup tasks marked [P] can run in parallel
- All Foundational tasks marked [P] can run in parallel (within Phase 2)
- US1, US2, US3 can start in parallel after Phase 2
- All tests for a user story marked [P] can run in parallel
- Models within a story marked [P] can run in parallel
- Storage implementations within a story marked [P] can run in parallel

---

## Parallel Example: User Story 1

```bash
# Launch all tests for User Story 1 together:
Task: "Contract test for dataset creation endpoint in tests/contract/test_dataset.go"
Task: "Integration test for dataset CRUD operations in tests/integration/test_dataset.go"
Task: "Unit test for Dataset model validation in internal/dataset/models/dataset_test.go"

# Launch all models and storage implementations for User Story 1 together:
Task: "Create Dataset model with time-series fields in internal/dataset/models/dataset.go"
Task: "Implement DatasetStorage interface in pkg/storage/neo4j/dataset_store.go"
Task: "Implement PostgreSQL storage for datasets in pkg/storage/postgres/dataset_store.go"
```

---

## Implementation Strategy

### MVP First (User Stories 1-3)

1. Complete Phase 1: Setup
2. Complete Phase 2: Foundational (CRITICAL - blocks all stories)
3. Complete Phase 3: User Story 1 (Dataset replaces top-level entity)
4. Complete Phase 4: User Story 2 (Questions replace objectives)
5. Complete Phase 5: User Story 3 (Insights replace heuristics)
6. **STOP and VALIDATE**: Test all three core entities independently
7. Deploy/demo if ready

### Incremental Delivery

1. Complete Setup + Foundational → Foundation ready
2. Add US1 + US2 + US3 → Core refactoring complete (MVP!)
3. Add US4 → Time-series support for analysts
4. Add US5 → Analyst-focused UI with visualizations
5. Each story adds value without breaking previous stories

### Parallel Team Strategy

With multiple developers:

1. Team completes Setup + Foundational together
2. Once Foundational is done:
   - Developer A: User Story 1 (Dataset)
   - Developer B: User Story 2 (Questions)
   - Developer C: User Story 3 (Insights)
3. After US1 completes:
   - Developer A moves to User Story 4 (Time-series)
4. After US3 and US4 complete:
   - Developers move to User Story 5 (UI/Visualizations)

---

## Notes

- [P] tasks = different files, no dependencies
- [Story] label maps task to specific user story for traceability
- Each user story should be independently completable and testable
- Verify tests fail before implementing
- Commit after each task or logical group
- Stop at any checkpoint to validate story independently
- Avoid: vague tasks, same file conflicts, cross-story dependencies that break independence
- **CRITICAL**: This is a refactoring effort - preserve existing functionality while updating naming and structure
- All old terminology (objectives, heuristics) must be completely removed from the codebase