# Feature Specification: Datasets as the Index Page

**Feature Branch**: `002-datasets-index-page`

**Created**: 2026-08-13

**Status**: Draft

**Input**: User description: "Change the index page to be the datasets listing page, sorted by last accessed. Remove the current index page and remove the 'Submit Goal' link from the header."

## Overview

Today the application's root URL (`/`) is the "Submit a goal" landing screen: a marketing-style hero describing the hypothesis loop beside the full goal-registration form. The data — the analyst's real work product — lives one click away on the Datasets inventory. The header reinforces that ordering with a **Submit goal** link that is the only route to `/`.

This feature flips that priority. The root URL becomes the dataset inventory, ordered by how recently each dataset was last accessed so the analyst lands on their most current work. The submit-goal landing page is removed, and so is the **Submit goal** header link; the remaining header navigation (Objectives, Datasets, Heuristics) plus the brand mark carry the analyst to every view. The dataset inventory's existing jobs — browse, search, create, and the detail drill-down — are unchanged; only where it lives and how it is ordered changes.

Because registering an objective was the landing page's whole job, that capability moves to where objectives already belong: the dataset's detail view. A dataset records when it was last opened, and opening a dataset presents its objectives as a deliberately sorted list plus a registration entry that creates a fresh objective bound to that dataset.

## Clarifications

### Session 2026-08-13

- Q: What does "sorted by last accessed" mean — what is the source of truth for the inventory ordering? → A: A new per-dataset **last-access** time, recorded each time the dataset's detail view is opened; the inventory ranks by it, most recently accessed first. Datasets that have never been accessed fall back to most-created-first ordering.
- Q: Where should registering an objective live once the index page is removed? → A: On the dataset's detail view; there is no standalone registration landing page anymore.
- Q: What order should a dataset's objectives be listed in? → A: Objectives with an active run first, then the rest, with most recently created first within each group.

### Implementation departures (2026-08-13)

- `GoalForm` bound mode (T012) keeps its `listDatasets` fetch even when `initialDatasetID` is set — it resolves the bound dataset's display name for the read-only source bar; the picker and upload/path ingest are still hidden and `dataset_id` is fixed.
- The US5 partition handler test (T016) lives in `internal/orchestrator/datasets_test.go` (`TestDatasetObjectiveOrdering`), not `submitdataset_test.go`, because it reuses `datasetTestServer` and `fakeRuns.latest`.
- `handleGetDataset` stamps the access event after synthesizing the objectives, so the response reflects the pre-touch `last_accessed_at`; the stamp persists for the next open and drives the inventory, which is the contract's only ordering consumer.

## Users

- **Analyst (primary)** — owns the data and the objectives that optimize it. Opens the console expecting to continue where they left off; the inventory ordered by last access is the natural landing.

## User Scenarios & Testing *(mandatory)*

### User Story 1 - Land on the dataset inventory (Priority: P1)

As an analyst, I want the application to open directly on my data — the dataset inventory — so that my most recent work is the first thing I see and I can reach any dataset, create one, or search without navigating first.

**Why this priority**: This is the core of the request — the root URL must present the dataset inventory exactly as the current Datasets page does, so the inventory becomes the de facto home of the console.

**Independent Test**: Can be fully tested by opening the root URL and confirming the dataset list renders with its search box and create entry, and that opening a dataset's detail works exactly as it does from `/datasets` today.

**Acceptance Scenarios**:

1. **Given** I open the console at its root URL, **Then** I see the dataset inventory — the list, the search box, and the "New dataset" create entry — not the submit-goal landing screen.
2. **Given** the inventory at the root URL, **When** I filter by name or open a dataset's detail, **Then** each behaves identically to the same action on the current Datasets page.
3. **Given** at least one dataset exists, **When** I reload the root URL, **Then** I land back on the same inventory, in the same order, with no error state.

---

### User Story 2 - See most-recently-accessed datasets first (Priority: P1)

As an analyst, I want the inventory ranked by how recently each dataset was last accessed, so the datasets I worked on most recently are at the top and I can resume without hunting.

**Why this priority**: The request names the ordering explicitly; together with US1 it defines the new home-page experience. It is independently shippable as a reordering of the existing list.

**Independent Test**: Can be fully tested by accessing two or more datasets in a known sequence and confirming that after each access the inventory presents the most recently accessed dataset first, with a stable ordering on reload.

**Acceptance Scenarios**:

1. **Given** two datasets, one accessed more recently than the other, **When** I open the inventory, **Then** the more recently accessed dataset appears above the other.
2. **Given** datasets whose last-accessed times are equal or absent, **When** I open the inventory, **Then** the ordering is deterministic — no ties resolve unpredictably on reload.
3. **Given** a dataset I open from the inventory, **When** I return to the inventory immediately afterward, **Then** that dataset now appears at the top of the list.

---

### User Story 3 - Clean header navigation (Priority: P2)

As an analyst, I want the **Submit goal** link gone from the top of every page while the rest of the header stays intact, so the navigation reflects the inventory-first console rather than pointing me at a page that no longer exists.

**Why this priority**: The removal is explicit and low-risk, but depends on the root-URL change (US1) so the header never advertises a removed page; it also depends on the goal-registration path decision below so no capability is lost.

**Independent Test**: Can be fully tested by viewing the header on any page and confirming only Objectives, Datasets, Heuristics, and the brand mark remain, and that every remaining link opens a real page.

**Acceptance Scenarios**:

1. **Given** any page of the console, **When** I look at the top header, **Then** there is no **Submit goal** link, and Objectives, Datasets, Heuristics, and the brand mark are all still present.
2. **Given** the removed **Submit goal** link, **When** I inspect every in-app reference to the old landing page, **Then** none points at a dead route — including affordances such as the objectives empty-state "Submit a goal" prompt.

---

### User Story 4 - Register an objective from the dataset's detail view (Priority: P2)

As an analyst, I want to register a new objective on the dataset I am already looking at, so the ability to create one survives the removal of the landing page and registration happens where it belongs — next to the dataset it binds to.

**Why this priority**: Removing the landing page without relocating registration would silently drop a core capability; moving it to the dataset detail keeps the console whole and reinforces the rule that every objective belongs to a dataset.

**Independent Test**: Can be fully tested by opening any active dataset's detail, registering an objective bound to it, and confirming the goal is created, appears under that dataset, and opens on its live view.

**Acceptance Scenarios**:

1. **Given** an active dataset's detail view, **When** I register an objective, **Then** it is created bound to that dataset, appears in the dataset's objectives list, in Objectives, and opens on its live view.
2. **Given** an archived dataset's detail view, **When** I attempt to register an objective, **Then** registration is refused with a clear reason — an archived dataset cannot accept new objectives, matching today's rule.
3. **Given** any entry point that previously led to the landing page (such as the objectives empty-state prompt), **When** I follow it, **Then** I am guided toward choosing a dataset and registering from its detail — never toward a dead route.

---

### User Story 5 - Browse a dataset's objectives in a defined order (Priority: P2/P3)

As an analyst, I want the objectives inside a dataset's detail presented in a deliberate, stable order, so I can tell at a glance which objectives matter on this dataset without the list reshuffling between views.

**Why this priority**: The request names a sorted list explicitly (order per Q3); it is a refinement of the existing detail view, not a new surface.

**Independent Test**: Can be fully tested by opening a dataset with several objectives and confirming the row order matches the defined rule and is identical on reload.

**Acceptance Scenarios**:

1. **Given** a dataset with two or more objectives, **When** I open its detail, **Then** the objectives appear in the defined order: those with an active run first, then the rest, with most recently created first within each group.
2. **Given** I reload an open dataset's detail, **When** the objectives render again, **Then** they are in the same order, with no ties resolving unpredictably.

---

### Edge Cases

- What happens when the app opens with no datasets at all? — The existing inventory empty state ("No datasets yet …") is the home page; it is informational, not an error, and the create entry remains the way forward.
- What ordering applies to datasets that have never been accessed? — A deterministic fallback (most recently created first) resolves them so the list is reproducible across reloads.
- What if two datasets report the same last-accessed time? — A deterministic tie-break (e.g., more recently created first) keeps the order stable.
- What about a user who bookmarks the old landing URL, or deep-links to `/datasets`? — The root URL serves the inventory; existing `/datasets` bookmarks (see Assumptions) continue to work via alias or redirect rather than returning 404.
- What about the objectives empty-state "Submit a goal" link that currently points at `/`? — It must be repointed to the registration path (choose a dataset, then register from its detail) or removed, never left pointing at the inventory where a user expects a form.
- What if a dataset has never been opened at all? — Its last-access time is absent; it ranks below every opened dataset and among its never-accessed peers falls back to most-created-first, so the ordering is still deterministic.
- How are objectives of a dataset with equal sort keys ordered? — Within a group, equal times resolve by creation date first (newest first); any remaining ties fall back to a stable secondary key so the list never reshuffles between reloads.
- What if the analyst registers an objective on a dataset mid-browse? — The new objective appears immediately in the dataset's objectives list in its defined position, and the dataset's inventory row reflects the change on the next load.
- Accessibility of navigation after removing the link — the brand mark and the three remaining links still cover every view; the root URL is reachable via the brand mark and the Datasets link.

## Requirements *(mandatory)*

### Functional Requirements

**Home page & ordering**

- **FR-001**: System MUST serve the dataset inventory at the root URL — the same list, search, create entry, and detail drill-down as the current Datasets page — with no submit-goal landing content rendered there.
- **FR-002**: System MUST order the dataset inventory by when each dataset was last accessed, most recently accessed first, where "last accessed" is the time the dataset's detail view was last opened; datasets that have never been accessed rank by creation date, newest first.
- **FR-003**: System MUST break ties deterministically when datasets share an equal or absent last-access time (resolve by creation date, newest first), so the order is stable across reloads.
- **FR-004**: System MUST record the time a dataset's detail view is opened, MUST treat that open as the access event, and MUST return a per-dataset last-access time with every inventory read so the ordering in FR-002 is based on the recorded times, not on client-supplied values.

**Header cleanup**

- **FR-005**: System MUST remove the **Submit goal** entry from the top header on every page; Objectives, Datasets, Heuristics, and the brand mark MUST remain, and the brand mark MUST continue to return to the root URL.
- **FR-006**: System MUST ensure no in-app link targets the removed landing page; the objectives empty-state prompt that currently links to it MUST guide the analyst toward the registration path (choose a dataset, then register from its detail) or be removed.

**Goal registration survivability**

- **FR-007**: System MUST provide a registration entry point on the dataset's detail view that creates an objective bound to that dataset, so removing the landing page does not remove the ability to register an objective in the console.
- **FR-008**: System MUST route a completed registration to the objective's live view, unchanged from today, and MUST refuse registration for archived datasets with a clear reason, consistent with today's rule that archived datasets accept no new objectives.
- **FR-009**: System MUST present the objectives of a dataset's detail view with those whose latest run is active first and then the remainder, with most recently created first within each group, so the order is deterministic and identical across reloads.

### Key Entities

- **Dataset** — The analyst-managed data source container listed on the home page. Key attributes: unique name, description, status, data source ref, date created, date last modified, date last accessed, and objective count derived from its children. Gained a "date last accessed" attribute (recorded when its detail view is opened) that ranks the inventory; it is the parent from whose detail view new objectives are now registered.
- **Objective** — The optimization target registered against a dataset. Key attributes: the objective text, its parent dataset, its run status, and the date created. Gained a defined listing order within its parent dataset's detail view and a registration entry point colocated there.

## Success Criteria *(mandatory)*

### Measurable Outcomes

- **SC-001**: An analyst opening the root URL sees the dataset inventory in under 2 seconds on a corpus of up to 1,000 datasets, with no loading failure.
- **SC-002**: The dataset accessed most recently is always the first row shown, datasets never accessed fall back to newest-created-first, and the full ordering is identical on consecutive reloads — 100% of the time.
- **SC-003**: 100% of datasets appear in the inventory; none is hidden or dropped by the reorder.
- **SC-004**: 0 in-app links point at the removed landing page; every previously reachable view (objectives, datasets, heuristics, dataset and objective details) remains reachable from the header, verified per page.
- **SC-005**: Registering an objective from a dataset's detail view works and a newly registered objective appears in that dataset's list and in Objectives and opens on its live view, matching today's registration behavior.
- **SC-006**: A dataset's objectives render in the defined order on every load of that dataset's detail, with identical order across consecutive reloads.

## Assumptions

- The dataset inventory's existing behavior — its list fields, search, create entry, and detail navigation — is unchanged; only its location (root URL) and ordering are new.
- "Last accessed" is a UI-access concept: opening a dataset's detail view in the console is the access event that refreshes that dataset's last-access time. Access by background processes or external clients does not count.
- The most recently created dataset is the deterministic fallback for datasets with no or equal last-access time, and within the dataset detail the objectives list keeps a single defined order — active-run objectives first, then the rest, most recently created first within each group — so both orders are stable across reloads.
- The `/datasets` route continues to resolve (as an alias of the root inventory or a redirect to it) so existing bookmarks do not 404; the header Datasets link targets the root URL.
- The brand mark in the header continues to link to the root URL (now the inventory).
- Registering an objective from a dataset's detail view reuses today's registration behavior and rules — including binding to the dataset's already-ingested source, no re-upload — and archived datasets still refuse new objectives.