# Feature Specification: Clean Test Data

**Feature Branch**: `004-clean-test-data`

**Created**: 2026-08-14

**Status**: Draft

**Input**: User description: "Our testing has created many datasets that are not useful and are cluttering our interface. Clean out all the existing datasets and objectives. Ensure all current and future tests clean up after themselves and don't leave test datasets, objectives or other objects in the database. DO NOT COMMIT TO GIT BEFORE YOU ARE INSTRUCTED TO DO SO. Use the AGENTS.md file as your constitution."

## User Scenarios & Testing *(mandatory)*

### User Story 1 - Start from a clean slate (Priority: P1)

An operator of the system notices the interface's Datasets and Objectives lists are cluttered with countless datasets and objectives created by months of testing. They invoke a cleanup action that removes every existing dataset, objective, and everything derived from them, leaving the interface empty and easy to navigate. Fresh work begins from a clean, trustworthy state.

**Why this priority**: This is the immediate pain driving the work — the clutter is visible today and must be gone before any future-hygiene rule can be judged. It is the cheapest story and unlocks everything else.

**Independent Test**: Invoke the cleanup action against a populated system, then open the Datasets and Objectives lists. Both must show zero entries, and no leftover references to the removed items may surface anywhere in the interface.

**Acceptance Scenarios**:

1. **Given** a system contains datasets, objectives, and derived activity from prior testing, **When** an operator runs the cleanup action, **Then** the Datasets and Objectives lists in the interface show zero entries and no orphaned content remains.
2. **Given** a clean (empty) system, **When** the cleanup action is run again, **Then** it completes without errors and the system stays empty (idempotent).
3. **Given** datasets and objectives exist, **When** cleanup removes them, **Then** nothing that depended on them (records, references, stored artifacts) is left behind in an unreachable or dangling state.

---

### User Story 2 - Tests leave no trace (Priority: P1)

A developer runs the test suite repeatedly against the shared development environment. Every test that creates a dataset, objective, or any other persisted object removes exactly what it created once the test finishes — whether the test passed, failed, or panicked. Running the suite a second (or hundredth) time starts from the same state as the first, and the interface never accumulates test junk again.

**Why this priority**: This is the root-cause fix. The current clutter happened because tests wrote persistent objects and never removed them; without this story the cleanup in Story 1 is undone by the next test run.

**Independent Test**: Run the full test suite twice back-to-back against the shared environment, verifying after each run that no test-created datasets, objectives, or other objects remain and that the second run's end-state matches the first.

**Acceptance Scenarios**:

1. **Given** a test that creates a dataset or objective, **When** the test completes successfully, **Then** everything it created has been removed and none of it appears in the interface.
2. **Given** a test that creates a dataset or objective, **When** the test fails or panics mid-way, **Then** everything it created is still removed afterwards.
3. **Given** the full test suite, **When** it is run twice consecutively, **Then** the end-state is identical both times — no datasets, objectives, or other objects accumulate across runs.
4. **Given** tests share a single environment, **When** one test creates fixtures, **Then** those fixtures are uniquely attributable to that test so any leftover is identifiable and other tests cannot collide with them.

---

### User Story 3 - The gate catches regressions (Priority: P2)

A developer invokes the standard test command and the harness, besides running the tests, verifies the environment is clean afterwards. If any test left a dataset, objective, or other object behind, the check reports exactly what remains and the run is considered failed — so hygiene regressions are caught the moment they are introduced instead of silently accumulating until the interface is cluttered again.

**Why this priority**: This keeps Story 2 enforced rather than aspirational. It is lower priority than the two P1 stories because it adds no new user-facing capability, but it is what makes the guarantee durable.

**Independent Test**: Run the test gate after deliberately introducing a test that leaves one dataset behind; the gate must fail and identify the leftover. Then remove the defect and confirm the gate passes again.

**Acceptance Scenarios**:

1. **Given** a test run that leaves a dataset, objective, or other object behind, **When** the cleanliness check runs as part of the gate, **Then** the gate reports the run as failed and identifies what was left behind.
2. **Given** a test run that removed everything it created, **When** the cleanliness check runs, **Then** it passes and the run is reported as successful.
3. **Given** the cleanup work in Story 1 has already run, **When** the gate's cleanliness check runs, **Then** it considers the clean state normal and reports no findings.

---

### Edge Cases

- A test fails, panics, or is interrupted mid-setup: cleanup must still run to completion.
- Test fixtures that must outlive a single test (shared across tests within a run) are removed by the end of the suite — never left over after the run concludes.
- Removal order matters: an objective cannot be removed while records still reference it, nor a dataset while its objectives reference it; cleanup must respect these dependencies.
- Two tests creating same-named datasets must not collide; unique, attributable identities are required.
- Cleanup and test-hygiene apply across every storage layer the system uses — records, the graph, and stored data artifacts — not just the primary record store.
- A hard kill of the test process (power loss, forced termination) cannot remove in-flight data; this is accepted and out of scope, as the gate will flag any residue on the next run.

## Requirements *(mandatory)*

### Functional Requirements

- **FR-001**: System MUST provide a repeatable, operator-invoked cleanup action that removes all existing datasets and objectives.
- **FR-002**: The cleanup action MUST also remove everything derived from or referencing the removed datasets and objectives (activity records, verification records, audit references, graph records, and stored data artifacts) so no dangling or orphaned content survives.
- **FR-003**: The cleanup action MUST be idempotent — running it on a system that is already clean completes without errors and changes nothing.
- **FR-004**: The cleanup action MUST respect dependencies between records so it completes without integrity errors regardless of the order items were created.
- **FR-005**: Every test that creates a dataset, objective, or other persisted object MUST remove exactly what it created when the test finishes, whether the test passes, fails, or panics.
- **FR-006**: Tests MUST create fixtures with unique, attributable identities across all storage layers so any residue left by a test can be recognized and attributed.
- **FR-007**: Running the test suite MUST be reproducible — a run leaves the environment in the same state it found it, so consecutive runs produce identical end-states with no accumulation.
- **FR-008**: The standard test gate MUST include a cleanliness verification that fails the run and identifies any test-created datasets, objectives, or other objects that remain.
- **FR-009**: Test cleanup obligations in FR-005 MUST apply to every storage layer a test writes to, including the graph and stored data artifacts, not only the primary record store.
- **FR-010**: The cleanliness verification in FR-008 MUST treat an already-clean environment (e.g. immediately after the Story 1 cleanup) as a passing, normal state.

### Key Entities *(include if feature involves data)*

- **Dataset**: A user-visible container a data source lives in, shown in the interface's Datasets list. Depends on a data source reference.
- **Objective**: A user-visible optimization goal tied to a dataset, shown in the interface's Objectives list.
- **Data source reference**: The underlying registration a dataset/objective's source depends on; must be removed with its dependents.
- **Derived records**: Activity, verification, and audit records that reference datasets/objectives and must not outlive their parents.
- **Graph records**: Connected records describing the relationships a dataset/objective produced, which must be removed with their originating dataset/objective.
- **Stored data artifacts**: External stored data objects associated with a dataset's source, which must be removed with it.

## Success Criteria *(mandatory)*

### Measurable Outcomes

- **SC-001**: After the cleanup action runs against a cluttered environment, the interface shows exactly zero datasets and zero objectives.
- **SC-002**: The cleanup action runs start to finish without errors on both a populated and an already-empty environment (idempotent).
- **SC-003**: Two consecutive full test-suite runs leave identical end-states — a verified zero net change in datasets, objectives, and other objects after each run.
- **SC-004**: The gate's cleanliness verification passes after every clean suite run and fails, with specific offenders named, whenever test-created objects remain.
- **SC-005**: 100% of tests that create persisted objects include cleanup that executes on both success and failure, verified by the gate rather than by inspection.

## Assumptions

- The shared development environment is used exclusively for testing; every existing dataset and objective in it is a test artifact and safe to remove.
- The cleanup of existing clutter is an operator-invoked, repeatable action, not an automatic background process.
- The interface renders the Datasets and Objectives lists directly from the stores, so removing the records empties the lists; no separate interface change is required.
- Test hygiene applies to every automated test that writes to a persistent store — present and future suites alike — following the constitution's guidance in AGENTS.md.
- Cleanup guarantees hold per test run; a hard kill of the test process mid-run is out of scope, with any residue it leaves flagged by the next gate run.
- No commit is made until explicitly instructed to do so.