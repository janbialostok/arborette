# Feature Specification: Dataset CRUD & Hierarchy Management

**Feature Branch**: `001-dataset-crud-lifecycle`

**Created**: 2026-08-13

**Status**: Draft

**Input**: User description: "There is no way via the UI to perform simple CRUD operations on the datasets present in the system. Using the AWS web console as an example, add the ability to manage the full dataset crud lifecycle. All objectives must be children of a dataset, all heuristics must be children of objectives."

## Overview

Today the dataset has no lifecycle. An analyst binds a data source to a goal at submission time and that binding is permanent, invisible, and unmanageable: there is no way to see every dataset in the system, name one, rename or annotate it, list what is stored under it, archive it, or remove it. Objectives and heuristics exist, but they are not presented as belonging to anything the user can manage. The result is a console where the most fundamental resource — the analyst's own data — cannot be organized, cleaned up, or even enumerated.

This feature makes the dataset a first-class, manageable resource, modeled on the AWS web console's treatment of S3 buckets: a management console where an analyst can browse every dataset, create one, inspect what it contains, edit its metadata, and delete it under controlled conditions. Along the way it fixes the ownership model so the hierarchy is explicit and enforceable: **every objective belongs to exactly one dataset, and every heuristic belongs to exactly one objective**. The console is organized around that tree — datasets list their objectives, objectives list their heuristics — and creation flows require the user to pick a parent, so orphans can no longer be produced.

## Clarifications

### Session 2026-08-13

- Q: How should the dataset console be reached from the application UI? → A: A dedicated **Datasets** link at the top of the header (app-wide navigation), present on every page, opening the dataset inventory console.
- Q: What does editing/updating a dataset cover in v1? → A: Metadata only — name, description, status. The data source binding stays fixed at registration.
- Q: What happens when the analyst tries to delete a dataset that still has objectives? → A: Block deletion while objectives remain; the analyst empties the dataset first (no force-delete option).

**Implementation departures (2026-08-13)**:

- Hierarchy invariant is enforced server-side with a client convenience, not a client mandate: `POST /goals` accepts an optional `dataset_id` (binds to that dataset's ref, no re-upload, `409` on unknown/archived/unreadable). API clients omitting it are **not** orphaned — the legacy ingest runs and an implicit dataset is minted around the fresh ref (`implicitDatasetName` = lowercased ref base filename + 6-hex md5, mirroring the 0015 backfill). The web form always selects an existing active dataset or ingests via file/path and lets the backend mint, so the UI itself always lands under a parent.
- Reconcile work is **not** only a migration side effect: the orchestrator runs `ReconcileDatasets` at boot (after the orphaned-runs reconciliation) to re-parent any residual NULL-parent rows on partially migrated stacks, minting missing datasets and auditing each reassignment as `dataset_reconcile` (`optimization_function_id` + `data_source_ref`). Migration 0015 still backfills and sets `NOT NULL`, so the boot pass is a safety net, not the primary mechanism.
- `ObjectiveSummary` renders a synthesized `status` ("no run" when the goal has no run yet) so the dataset detail never shows a blank objective; the value is the latest run's status.
- US6 goal delete is one transactional `GoalRegistry.Delete` (guard + child deletes + goal row) per the T035 departure; the dataset delete is refused with `409` while objectives remain.

## Users

- **Analyst (primary)** — owns the data, registers objectives against it, and reads the distilled heuristics. Needs to organize, curate, and retire datasets without developer help, and to navigate from a dataset down to the knowledge it produced.
- **Operator/administrator** — needs to see the full inventory and intervene when a dataset is obsolete, misnamed, or blocking on cleanup, with every destructive action recorded.

## User Scenarios & Testing *(mandatory)*

### User Story 1 - Browse the dataset inventory (Priority: P1)

As an analyst, I want to open the console from the Datasets link in the app header and see every dataset I own — each with a name, when it was created, its current status, and how many objectives live under it — so I can tell at a glance what is in the system without remembering IDs.

**Why this priority**: Without a list there is no lifecycle to manage; every other story builds on being able to find and select a dataset. This is the minimum viable slice of the whole feature.

**Independent Test**: Can be fully tested by clicking **Datasets** in the top header from any page and confirming that every dataset in the system appears with name, creation time, status, and objective count, that the list is searchable, and that selecting a dataset reveals its objectives.

**Acceptance Scenarios**:

1. **Given** at least one dataset exists in the system, **When** I click the Datasets link at the top of the header, **Then** the management console opens and every dataset is listed with its name, date created, status, and the number of objectives that belong to it.
2. **Given** I am anywhere in the application (objective detail, heuristics, run view), **When** I look at the top header, **Then** the Datasets link is always present and opens the dataset console from wherever I am.
3. **Given** a large number of datasets, **When** I type part of a dataset's name into the list search, **Then** only matching datasets are shown.
4. **Given** a dataset that has objectives, **When** I open its details, **Then** I see its metadata plus the list of objectives that belong to it, each linking to the objective's own view.

---

### User Story 2 - Create a dataset (Priority: P1)

As an analyst, I want to create a named dataset from a data source and add a description, so that my data is a first-class, findable resource before any objective is registered against it.

**Why this priority**: Creation is the entry point of the lifecycle; without it the console can only show data that was bound as a side effect of goal submission.

**Independent Test**: Can be fully tested by creating a dataset, giving it a name and description, and confirming it immediately appears in the inventory list with a status of "empty" (no objectives).

**Acceptance Scenarios**:

1. **Given** I am on the management console, **When** I create a dataset with a unique name and a data source, **Then** the dataset is registered, appears in the inventory, and has no objectives.
2. **Given** I attempt to create a dataset whose name is already in use, **When** I submit the form, **Then** creation is rejected with a message naming the conflict, and no duplicate is created.
3. **Given** a dataset whose data source cannot be read, **When** I open its details, **Then** I see a clear indication that the source is unavailable, but the dataset entry itself still exists and can be edited or removed.

---

### User Story 3 - Register an objective under a dataset (Priority: P1)

As an analyst, I want to register an objective by first choosing which dataset it optimizes, so that every objective is visibly a child of exactly one dataset and none can exist outside one.

**Why this priority**: This is the mechanical heart of the hierarchy requirement. It changes the existing registration flow so a parent dataset is mandatory, and guarantees the "all objectives are children of a dataset" invariant going forward.

**Independent Test**: Can be fully tested by attempting to register an objective without a dataset (must be impossible) and with one (must succeed and show under that dataset).

**Acceptance Scenarios**:

1. **Given** the objective registration flow, **When** I attempt to register an objective without choosing a parent dataset, **Then** the system refuses and requires a dataset selection; no objective can be created without one.
2. **Given** I register an objective and choose a parent dataset, **When** the registration succeeds, **Then** the objective is persisted as a child of that dataset and appears in the dataset's objectives list.
3. **Given** existing objectives that predate datasets (legacy records with no dataset), **When** the feature is launched, **Then** the system reconciles them so each is assigned a dataset and none remain orphaned, with the reconciliation reported for review.

---

### User Story 4 - Edit a dataset (Priority: P2)

As an analyst, I want to rename, update the description, or archive a dataset from its details view, so that datasets stay accurately labeled as the system grows. The data itself is never edited — the source is bound once at creation.

**Why this priority**: Editing makes the inventory trustworthy over time; it comes after create/browse/register because those are the load-bearing flows.

**Independent Test**: Can be fully tested by renaming a dataset (with a uniqueness check), changing its description, changing its status to archived, and confirming each change is reflected in the inventory and survives a reload.

**Acceptance Scenarios**:

1. **Given** a dataset's details view, **When** I edit its name, description, or status and save, **Then** the change is persisted and reflected in the inventory immediately.
2. **Given** I rename a dataset to a name already used by another dataset, **When** I save, **Then** the change is rejected with a conflict message and the original name is preserved.
3. **Given** a dataset I archive, **When** I browse the inventory, **Then** it is still listed but clearly marked archived, and archived datasets do not disappear from their objectives' details.

---

### User Story 5 - Browse and remove heuristics under an objective (Priority: P2)

As an analyst, I want to see the heuristics that belong to each objective and remove one that is obsolete, so that the knowledge layer is organized by its parent objective exactly as the hierarchy promises.

**Why this priority**: It is the second half of the "all heuristics are children of objectives" invariant made visible and manageable; heuristics are system-generated, so this story is about ordered, correct parenting plus a removal path — not authoring.

**Independent Test**: Can be fully tested by opening an objective, seeing exactly the heuristics that belong to it (not those of other objectives), and removing one with a confirmation, after which it no longer appears.

**Acceptance Scenarios**:

1. **Given** an objective that has heuristics, **When** I open the objective's heuristics list, **Then** I see exactly the heuristics that belong to it and no heuristic belonging to a different objective.
2. **Given** an objective with no heuristics, **When** I open its heuristics list, **Then** the list is empty with a clear "none yet" state rather than an error.
3. **Given** I choose to remove a heuristic, **When** I confirm the removal, **Then** the heuristic is permanently removed from the objective and no longer appears anywhere in the system, and the removal is recorded.

---

### User Story 6 - Delete an objective and retire a dataset (Priority: P3)

As an analyst, I want to delete an objective (taking its heuristics with it under a confirmation) and, once an objective's dataset is empty, delete the dataset too — with the system protecting me from destroying data that still has children.

**Why this priority**: Cleanup closes the lifecycle; it is safest last because it performs actual destruction. It follows the AWS bucket model the request names: you empty it before you remove it.

**Independent Test**: Can be fully tested by attempting to delete a dataset that still has objectives (must be blocked with guidance), deleting an objective, and then successfully deleting the now-empty dataset.

**Acceptance Scenarios**:

1. **Given** a dataset that still contains objectives, **When** I attempt to delete it, **Then** deletion is blocked with a message explaining that the dataset must be emptied first and listing what remains; no data is lost.
2. **Given** I delete an objective, **When** I confirm, **Then** the objective and all of its heuristics are removed together, the action is recorded, and the parent dataset's objective count updates.
3. **Given** a dataset with no objectives, **When** I confirm deletion, **Then** the dataset is removed from the inventory for good, and the action is recorded.
4. **Given** the property that every objective must have a dataset and every heuristic an objective, **When** any of the delete flows above completes, **Then** the hierarchy invariant still holds — no child is left orphaned.

---

### Edge Cases

- What happens when a dataset that still has objectives is deleted? — Blocked, with the offending objectives enumerated and a pointer to emptying them first.
- How does the system treat legacy objectives and heuristics created before datasets existed? — Reconciles them into the hierarchy at launch, assigning each a dataset, never deleting or silently merging them, and reporting what was done.
- What if two datasets point at the same underlying data source? — Allowed only if each keeps independent metadata and objectives; the UI surfaces the duplication rather than forbidding it.
- What if a dataset's underlying data source is missing, unreadable, or has been replaced? — The dataset remains manageable (rename, describe, delete) and its details flag the source as unavailable; objective registration against it is blocked with a clear reason.
- What if an objective under a dataset is still mid-run when the analyst renames or archives the dataset? — The run is unaffected; the objective simply displays the updated dataset metadata.
- What if a heuristic is being actively searched, traced, or served at the moment it is removed? — Removed heuristics cease to appear in search and trace results; in-flight requests complete against their last snapshot without erroring.
- What if the archive of a dataset collides with continued objective registration against it? — Archiving marks the dataset as read-only for new objectives: existing objectives keep working, new ones are refused until it is unarchived.
- What if the analyst deletes an objective whose heuristic is currently referenced in the trace of another objective? — Traces render gracefully with the removed heuristic shown as no-longer-available rather than breaking.
- How are duplicates avoided on rename when names are case-insensitive? — The uniqueness check is case-insensitive, so "Sales" and "sales" conflict.
- What if the deletion of a dataset is interrupted mid-way? — The operation is all-or-nothing: either the dataset is fully removed and recorded, or it remains fully present; no partial state.

## Requirements *(mandatory)*

### Functional Requirements

**Datasets — inventory & lifecycle**

- **FR-001**: System MUST let the analyst create a dataset with a unique name, an optional description, and a data source, and MUST add it to the inventory immediately.
- **FR-002**: System MUST list every dataset with at least its name, creation date, status (including "empty" vs "in use" vs "archived"), and objective count, searchable by name.
- **FR-003**: System MUST let the analyst open a dataset's details and see its metadata and the objectives that belong to it.
- **FR-004**: System MUST support editing a dataset's metadata — name, description, and status — enforcing the same uniqueness rule as creation and persisting the change. The data source binding is fixed at registration and is not editable.
- **FR-005**: System MUST support deleting a dataset once it has no objectives; deletion MUST require explicit confirmation and MUST be impossible while objectives remain.
- **FR-006**: System MUST record every dataset create, edit, and delete action for later accountability, including which dataset, what changed, and when.
- **FR-007**: System MUST expose a persistent **Datasets** link in the top header of every page, which opens the dataset inventory console from anywhere in the application.

**Hierarchy — objectives as children of datasets**

- **FR-008**: System MUST require a parent dataset when an objective is created; an objective MUST NOT be creatable or persistable without exactly one parent dataset.
- **FR-009**: System MUST display an objective's parent dataset wherever the objective is listed or opened, and MUST display the objective's heuristics beneath it.
- **FR-010**: System MUST reconcile legacy objectives (those created before this feature) so each is assigned a dataset and none remain orphaned, reporting the reconciliation for review.
- **FR-011**: System MUST support deleting an objective together with its heuristics after confirmation, and MUST preserve the parent dataset's accuracy (its objective count updates).
- **FR-012**: System MUST prevent any operation — create, delete, re-parent — from leaving an objective without a parent dataset or a heuristic without a parent objective.

**Hierarchy — heuristics as children of objectives**

- **FR-013**: System MUST associate every heuristic with exactly one objective and MUST scope heuristic browsing, search, and trace results to show only those belonging to a selected objective.
- **FR-014**: System MUST support removing a heuristic from its objective after confirmation, after which it MUST no longer appear in search or trace results.
- **FR-015**: System MUST preserve the association of heuristics to their objective through removals of neighboring heuristics and through dataset renames or archival.

### Key Entities

- **Dataset** — A first-class, user-managed container for one data source. Key attributes: unique name, description, status (active/archived/external-source-unavailable), date created, date last modified, and the objective count derived from its children. Parent of zero or more Objectives; dataset deletion is the lifecycle action the console exposes with the strictest rules.
- **Objective** — The optimization target an analyst registers, which this feature makes a strict child of exactly one Dataset. Key attributes: the goal/objective text, the parent dataset, its run status, and the set of heuristics beneath it. Adds the mandatory parent reference and the visibility of that parent everywhere.
- **Heuristic** — A distilled piece of reusable knowledge the system produces, now a strict child of exactly one Objective. Key attributes: the definition text, its parent objective, and its origin (the dataset and objective it was abstracted from). Gained an explicit, enforced parent and a removal path; remains system-generated rather than analyst-authored.

## Success Criteria *(mandatory)*

### Measurable Outcomes

- **SC-001**: An analyst can complete the full lifecycle of a dataset — create it, register an objective under it, see the hierarchy, rename it, and delete it — in under 5 minutes on first use without reference to documentation.
- **SC-002**: 100% of objectives in the system have exactly one parent dataset, and 100% of heuristics have exactly one parent objective, at launch and at any point afterward.
- **SC-003**: 100% of dataset inventory screens carry a search that returns results within 2 seconds on a corpus of up to 1,000 datasets.
- **SC-004**: 100% of destructive actions (dataset deletion, objective deletion, heuristic removal) are gated behind an explicit confirmation and are recorded with a timestamp and actor; 0 destructive actions occur without such a record.
- **SC-005**: 0 datasets are ever deleted while they still contain objectives; every blocked attempt surfaces the blocking objectives instead of losing data.
- **SC-006**: All pre-existing objectives and heuristics are reachable within the hierarchy after launch — none orphaned, dropped, or silently merged — with the reconciliation report auditable by an operator.

## Assumptions

- The AWS S3 web console is the governing analogy, specifically its bucket lifecycle: datasets are listed, selected, and emptied before removal. A non-empty dataset therefore cannot be deleted.
- Full CRUD ("the full dataset crud lifecycle") applies to **datasets**. Objectives gain a mandatory parent dataset plus delete (they are still authored through the existing registration flow); heuristics remain system-generated and gain enforced parenting plus removal. Manually authoring or editing heuristic content is out of scope.
- "Dataset" is treated as the user-facing name for the data source entity that already exists in the system; existing sources are grouped into named datasets as part of this feature's launch, so no analyst data is re-uploaded.
- A dataset binds to one data source, fixed at registration and never editable afterwards (matching today's immutable data-source binding); editing covers only name, description, and status. A dataset that needs different data is created fresh rather than re-pointed.
- Deleting a dataset removes the dataset record and its underlying data source together once the dataset is empty of objectives; no additional data-retention policy is imposed beyond what the operator already configures.
- Hierarchy integrity applies to the records the system manages. External sources the analyst owns outside the system are unaffected by dataset deletion.
- Re-parenting (moving an objective from one dataset to another) is not required for v1; objectives stay bound to the dataset chosen at registration, matching today's immutable data-source binding. If a dataset becomes obsolete, the objective is deleted with it rather than moved.