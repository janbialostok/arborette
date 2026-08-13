# Implementation Plan: Datasets as the Index Page

**Status**: done (implemented 2026-08-13; departures logged in spec.md "Implementation departures")

**Branch**: `002-datasets-index-page` | **Date**: 2026-08-13 | **Spec**: [spec.md](spec.md)

**Input**: Feature specification from spec.md (datasets inventory becomes the root URL, ordered by last access; the submit-goal landing page and its header link are removed; objective registration relocates to the dataset detail view alongside a deliberately-ordered objectives list).

## Summary

Make the dataset inventory the console's home page. The root URL serves the same inventory the Datasets page serves today, now ordered by a new per-dataset **last-access** time (recorded each time a dataset's detail view is opened, never-accessed datasets falling back to newest-created-first). The submit-goal landing page and the **Submit goal** header link are removed; the objectives empty-state prompt is repointed. Registration of objectives moves to the dataset detail view (a bound goal submber sends only `dataset_id`, reusing the existing `POST /goals` path and its archived 409 gate), and the detail's objectives list is ordered active-run-first, then newest-created-first within each group. Concretely: migration `0016` adds `datasets.last_accessed_at`; `DatasetStore.List` reorders by it (`DESC NULLS LAST, created_at DESC, id`); `DatasetStore.Touch` stamps it on the detail GET; the orchestrator's `datasetDTO` and `objectivesWithStatus` gain the field and the running-first ordering; `web/app/page.tsx` renders `DatasetsList`; `AppNav` drops the Submit goal link; `GoalForm` gains a bound mode; `DatasetDetail` hosts registration. See `research.md`, `data-model.md`, `contracts/rest-api.md`, `quickstart.md`.

## Technical Context

**Language/Version**: Go 1.24 (pinned `GOTOOLCHAIN`); web frontend is TypeScript on
Next.js ≥15 (App Router), Node 22.

**Primary Dependencies**: None new — plain SQL column + reorder on the existing `pgx/v5`
store; no web dependency changes (typed client only). Keep `pgx/v5` ≤ v5.7.x per AGENTS.md.

**Storage**: Postgres. Migration `0016` adds `datasets.last_accessed_at timestamptz NULL`
(no grant change: the orchestrator role already holds `SELECT`/`UPDATE` on `datasets`).
Neo4j / S3 / embeddings are untouched by this feature.

**Testing**: Go: `go test ./...` (integration gated on `ARBORETTE_INTEGRATION=1`), `make
test` (`-p 1` + localhost overrides), `go test -race ./internal/orchestrator` where the
detail GET and registration paths are touched, `go vet`. Web: `npm test` (vitest) from
`web/`. AGENTS.md rules apply: per-row assertions only (never table-global counts),
`OrderBy`/timestamp tests `time.Sleep` a few ms between creates so ordering is
deterministic, and the shared integration DB means another run's `last_accessed_at` writes
persist — ordering tests must touch/assert their own rows.

**Target Platform**: Local docker compose stack (orchestrator :8080, web :3000 dev /
:8083 standalone) — an internal analyst-facing web app.

**Project Type**: Web application (Go orchestrator backend + Next.js frontend).

**Performance Goals**: Spec SC-001 — root URL inventory renders in under 2 s up to 1,000
datasets. The reorder is a single indexed-friendly `ORDER BY` over `datasets`; no N+1.

**Constraints**: `GET /datasets/{id}` gains the documented side effect of stamping
`last_accessed_at` (a best-effort `UPDATE`, never a read failure). Ordering must be
deterministic (never-accessed and equal keys resolve by `created_at DESC`, then `id`).
Registration on an archived dataset stays a 409 (unchanged `submit.go` gate). No new
audit events: access is not an accountability action. `audit_log` vocabulary and the
integration-suite sharing rules from AGENTS.md apply untouched.

**Scale/Scope**: Single analyst persona; a few hundred datasets/goals. Changes touch
`internal/store`, `internal/orchestrator`, and `web/` only — no graph/object-store/verifier
contract changes.

## Constitution Check

*GATE: Must pass before Phase 0 research. Re-check after Phase 1 design.*

The project `.specify/memory/constitution.md` is the unfilled repository template (no custom
principles ratified). The governing conventions come from `AGENTS.md`; all gates pass:

- **Toolchain/dependency discipline** — Go 1.24 pin kept; zero new dependencies (a bare SQL
  column and a Go sort); no `go.mod` movement.
- **Migrate pattern** — new additive migration `0016` for the column; no grant changes
  needed (the orchestrator already reads/updates `datasets`); `audit_log` untouched.
- **Testability** — integration suite stays infrastructuraless-green; per-row assertions;
  deterministic `ORDER BY` tests sleep between inserts; the side-effecting detail GET is
  modeled as a best-effort `UPDATE` so a cancelled context fails the store call loudly
  rather than masking a bug.
- **Web conventions** — pure ordering is enforced server-side as the single source of
  truth (matching the existing server-ordered list and objective feed), so no duplicate
  rule is split across Go and web; the typed client gains only the new nullable field.
- **No unwarranted complexity** — no new endpoint, no per-goal access tracking, no new
  service, no redirect middleware: two App-Router pages share one component so existing
  `/datasets` bookmarks keep resolving.

## Project Structure

### Documentation (this feature)

```text
specs/002-datasets-index-page/
├── plan.md              # this file
├── research.md          # Phase 0 — last-access decision, ordering, side effect
├── data-model.md        # Phase 1 — datasets.last_accessed_at + ordering rules
├── quickstart.md        # Phase 1 — end-to-end validation guide
├── contracts/
│   └── rest-api.md      # Phase 1 — dataset DTO/ordering/registration contracts
└── tasks.md             # created by /speckit.tasks (next phase)
```

### Source Code (repository root)

```text
internal/store/
├── migrations/
│   └── 0016_datasets_last_accessed.up.sql / .down.sql   # ADD / DROP last_accessed_at
├── datasets.go                      # Dataset + LastAccessedAt *time.Time; List ordering
│                                    #   last_accessed_at DESC NULLS LAST, created_at DESC,
│                                    #   id; + Touch(ctx, id) UPDATE last_accessed_at=now()
└── goalregistry.go                  # unchanged (ListByDataset stays created_at DESC feed)

internal/orchestrator/
├── datasets.go                      # datasetDTO + LastAccessedAt; toDatasetDTO copies it;
│                                    #   handleGetDataset stamps Touch (best-effort) before
│                                    #   serving; objectivesWithStatus: stable partition
│                                    #   active-run (latest status "running") first, keeping
│                                    #   created_at DESC within each group, id tie-break
├── server.go                        # datasetStore interface + Touch(ctx, id) error
├── datasets_test.go                 # + Touch recorded on GET /datasets/{id}; no read
│                                    #   failure when UPDATE fails
├── submitdataset_test.go            # + detail objectives ordered running-first
└── (fakes) server_test.go           # fakeDatasetStore + Touch
```

```text
web/
├── app/page.tsx                     # renders <DatasetsList /> (submit-goal hero removed)
├── app/datasets/page.tsx            # unchanged — alias of the root inventory, so
│                                    #   bookmarks and router.push("/datasets") keep working
├── components/AppNav.tsx            # remove { href: "/", "Submit goal" }; drop the
│                                    #   `pathname === "/"` active-check special case
├── components/DatasetDetail.tsx     # + registration entry (bound GoalForm) for active
│                                    #   datasets; archived → hidden with reason text;
│                                    #   objectives render in API order (no client re-sort)
├── components/GoalForm.tsx          # + initialDatasetID prop: bound mode renders only the
│                                    #   objective field, skips listDatasets/picker/upload,
│                                    #   and submits dataset_id (unchanged submitGoal path)
├── components/ObjectivesList.tsx    # empty-state copy + link → guide to a dataset's
│                                    #   detail (the new registration point), not a gone
│                                    #   landing page
├── lib/orchestrator.ts              # DatasetSummary + last_accessed_at: string | null;
│                                    #   comment update on DatasetDetail.objectives order
└── lib/datasets.test.ts             # ds() fixture gains last_accessed_at (type churn only)
```

**Structure Decision**: The repo is a single Go module plus the `web/` deployable; this
feature is additive within the existing layers — a store column + reorder + touch method,
an orchestrator DTO field + handler stamp + objectives sort, and web page/component
edits. The one genuinely new grouping is the registration entry bound to a dataset inside
`DatasetDetail`, implemented as a `GoalForm` bound mode rather than a new page. Both `/`
and `/datasets` render the same component (an App Router alias, not a redirect) so no
bookmark or internal `router.push` breaks. No re-architecture is warranted.

## Complexity Tracking

> No `AGENTS.md` / constitution violations justified — no added projects, no new
> repository pattern, no schema versioning. The side-effecting detail GET is an explicit,
> documented contract change (research R3), not hidden behavior; the objectives sort is a
> stable in-memory partition on data the handler already fetches, not a new query. The
> table is therefore intentionally empty.