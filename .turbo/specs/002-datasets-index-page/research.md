# Research Notes — Datasets as the Index Page

Resolves the unknowns in `plan.md` Technical Context. Everything is grounded in the
existing codebase: migrations `0001–0015`, the orchestrator dataset surface
(`internal/orchestrator/datasets.go`, `server.go`), the store layer
(`internal/store/datasets.go`, `goalregistry.go`), and the `web/` App-Router BFF.

## R1 — Where the last-access time lives

**Decision**: A new nullable `datasets.last_accessed_at timestamptz` column (migration
`0016`). `NULL` means "never accessed". No separate table, no pgvector/graph involvement.

**Rationale**: `datasets` already carries the lifecycle metadata the inventory renders and
already has `SELECT`/`UPDATE` grants for the orchestrator role, so a single additive
column ships with **no grant change**. The recency signal is one value per dataset; a
timestamps column is the smallest faithful storage. `//go:embed *.sql`
(`migrations.go`) picks up the new files with no code change.

**Alternatives considered**:
- *Derive recency from an audit event* — rejected: access is intentionally not an
  accountability action (AGENTS audit vocabulary is for destructive slips/management);
  deriving ordering from `audit_log` would couple the home page to an append-only log.
- *Track per-goal access and fold up* — rejected: the ordering is per-dataset; per-goal
  tracking is a second concept the spec never asks for (YAGNI).

## R2 — Inventory ordering rule

**Decision**: `DatasetStore.List` orders
`ORDER BY last_accessed_at DESC NULLS LAST, created_at DESC, id`.
Never-accessed datasets sit below every accessed one; among equal/absent last-access times
the newest-created wins, and `id` is the final deterministic tie-break. The web client
renders in server order (it already does — `filterDatasets` preserves order).

**Rationale**: Matches the spec's resolved Q1/A and FR-002/FR-003. The existing list
already orders server-side (`created_at DESC`, datasets.go); extending that query keeps one
source of truth. `NULLS LAST` implements the never-accessed fallback directly in SQL, and
the `id` tail guarantees the "identical on consecutive reloads" requirement (SC-002) with
no ties.

**Alternatives considered**:
- *Reorder client-side in `DatasetsList`* — rejected: duplicates the rule in JS where the
  data already arrives ordered; the backend must stay the single source for SC-002.
- *Add a covering index on `(last_accessed_at DESC NULLS LAST, created_at DESC)`* —
  deferred: at the stated scale (≤1,000 rows, SC-001) a sort of a few hundred rows is
  well inside the 2 s budget; an index is an optimization we'd add when a measurement says
  so (YAGNI).

## R3 — Defining the access event (side-effecting detail GET)

**Decision**: `GET /datasets/{id}` is the access event. `handleGetDataset` best-effort
stamps `last_accessed_at = now()` via a new `DatasetStore.Touch(ctx, id)` before serving
the detail; a failed stamp logs and still serves the detail (an access mark must never
turn an open into a 500). Reads that are not the detail view — `GET /datasets`,
`POST /goals` binding, `GoalForm` inventory loads — never touch.

**Rationale**: The spec's Q1/A resolution names "the dataset's detail view was last
opened" as the event, and `DatasetDetail`'s mount performs exactly one `getDataset(id)` —
so the UI's open maps 1:1 onto the detail GET. The spec's "external clients / background
processes do not count" assumption is acknowledged as a best-effort approximation: any
GET of the detail stamps. That is the correct trade — last-access is a recency hint for
ordering, not audit.

**Alternatives considered**:
- *Separate `POST /datasets/{id}/touch` from the client* — rejected: adds an endpoint and
  a round trip to express an event the detail read already expresses; the side effect is
  documented in the contract (contracts/rest-api.md) rather than hidden.
- *Touch on `List` too* — rejected: merely listing a dataset is not opening it; would
  scramble any meaningful order (every list would re-stamp everything).

## R4 — Objective registration bound to a dataset

**Decision**: Registration relocates to the dataset detail view. `GoalForm` gains an
`initialDatasetID` prop; when bound it renders only the objective field, skips the
inventory load/picker/upload controls, and submits `submitGoal(f)` carrying just
`dataset_id` — the exact body the backend already binds (submit.go:167), which 409s an
archived dataset ("dataset is archived and cannot accept new objectives"). Success
`router.push`es to `goals/{id}` unchanged. On an archived dataset the detail hides the
entry and shows a reason instead (the backend gate stays the authority).

**Rationale**: Reuses the tested submit path and the existing archived gate rather than
inventing a second registration route; keeps FR-007/FR-008's behavior identical while
changing only *where* the form lives. `GoalForm` previously loaded the picker on mount —
the bound mode skips that fetch so no wasted `listDatasets`. This is the least-new-code
way to satisfy the resolved Q2 while hosting registration next to its parent dataset.

**Alternatives considered**:
- *A brand-new `RegisterObjective` component* — rejected: duplicates form state, error
  mapping, and the submit call that `GoalForm` already owns.
- *Keep a standalone `/goals/new` page* — rejected: the user asked for configuration on the
  dataset detail; a separate page reintroduces the landing-page pattern just removed.

## R5 — Objectives ordering (active-run first)

**Decision**: After `objectivesWithStatus` synthesizes each objective's latest run status,
stable-partition the slice so objectives whose latest run status is `running` come first;
within each group the existing `created_at DESC` order (from `ListByDataset`) is
preserved, and `optimization_function_id` is the final tie-break. The same partition is
applied to the non-empty-delete 409's blocking list for consistency.

**Rationale**: The spec's resolved Q3/B ("active run first, then most recently created
within each group"). "Active run" is unambiguously the `running` lifecycle status
(`store.RunRunning`, the only in-flight status; `completed`/`failed` are terminal and
`no run` is synthesized). Because the feed is already `created_at DESC`, a stable
partition — not a full re-sort — produces the required order with no secondary key logic.
The status is already computed server-side in `objectivesWithStatus`, so the ordering is
one source of truth (rationale mirrors R2).

**Alternatives considered**:
- *Sort client-side in `DatasetDetail`* — rejected: the rule needs the latest-run status,
  which only the orchestrator computes; duplicating it in JS would split the rule across
  Go and vitest with no consumer to justify it.
- *Include `completed` as "active"* — rejected: a settled objective is not in-flight; the
  correct resume-at-the-top behavior is running work first (US5's "which objectives
  matter").

## R6 — Root URL, header, and leftover links

**Decision**: `web/app/page.tsx` renders `<DatasetsList />` in place of the submit-goal
hero; `AppNav` drops the `{ href: "/", label: "Submit goal" }` entry and the now-dead
`pathname === "/"` active-check special case; the Objectives empty-state "Submit a goal"
link and copy are repointed to the inventory (`/datasets` — the canonical path), telling
the analyst to open a dataset to register against it. The Datasets header link stays
`/datasets`, and `/datasets` keeps rendering the same component, so every existing
bookmark and `router.push("/datasets")` (DatasetDetail back-link, post-delete push)
resolves unchanged.

**Rationale**: The instruction is "index page = datasets listing"; a root page rendering
the one existing inventory component satisfies it exactly. Keeping `/datasets` as an alias
(not a redirect and not deletion) preserves every existing reference with zero churn — the
spec's assumption allowed alias or redirect. The nav now has no `/` entry, so eliminating
the special case keeps the active-state logic uniform (`pathname.startsWith(href)`).

**Alternatives considered**:
- *`/datasets` redirects to `/`* — rejected: a redirect adds a hop for every back-link and
  post-delete push for no user value; two App-Router pages sharing one component is the
  standard tiny-duplication trade.
- *Point the Datasets header link at `/`* — rejected: for an internal tool two routes to
  the same inventory are harmless, and leaving the canonical link untouched minimizes
  behavioral surface (both navigate to the inventory either way).

## R7 — Wire surface & typed client

**Decision**: `datasetDTO` gains `LastAccessedAt *time.Time json:"last_accessed_at"`
(nullable), copied from `store.Dataset.LastAccessedAt` in `toDatasetDTO`; the web
`DatasetSummary` gains `last_accessed_at: string | null`. `GET /datasets` and
`GET /datasets/{id}` both return it. No new endpoints, no BFF proxy changes (both routes
are pass-through), and `POST /goals` is unchanged.

**Rationale**: FR-004 requires the recorded time to come back with inventory reads (so the
ordering provably uses server-recorded times). Nullable keeps "never accessed" distinct
from an old date. The BFF proxies forward JSON verbatim, so only the typed interface and
the vitest fixture need the new field.

**Alternatives considered**:
- *Keep the field server-side only (not in the DTO)* — rejected: FR-004 is explicit that
  inventory reads carry the recorded time, and exposing it lets a later UI render "accessed
  at" without an API change.

## R8 — Test seams (side effects in a shared DB)

**Decision**: Go integration tests assert **per-row** effects and never global order across
other tests' datasets: stamping is checked on the row the test created (`GET
/datasets/{id}` then `List` positions that row first), `time.Sleep` hosts the created_at
ordering determinism, and the objectives partition is asserted on a fixture dataset's own
rows. Orchestrator unit tests record `Touch` on the `fakeDatasetStore` and assert the DTO
partition; the fake returns `ctx.Err()` when its context is done (AGENTS.md), so the
handler that passes a request context reads as correct instead of passing for the wrong
reason.

**Rationale**: The shared integration database means one run's `last_accessed_at` writes
persist into the next (AGENTS.md: never compare observations taken before/after a gate
run); asserting on owned rows sidesteps it. `make test` truncates
`meta_heuristic_embeddings`, which is irrelevant here — this feature touches no embedding
rows.

**Alternatives considered**: *Flush `last_accessed_at` in testutil truncation* — rejected:
truncating one more column would destroy recency signal for a real operator's session
running the gate; per-row assertions need no such ceremony.