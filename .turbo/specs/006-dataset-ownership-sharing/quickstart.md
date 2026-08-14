# Quickstart: Validating Dataset Ownership & Sharing

Phase 1 output. Runnable end-to-end validation for the feature, against the local compose stack. This is a validation guide, not implementation — contract details live in [contracts/api.md](./contracts/api.md) and [contracts/ui.md](./contracts/ui.md); the entity rules live in [data-model.md](./data-model.md).

## Prerequisites

- `make up` brings up the full stack (orchestrator :8080, sandbox :8081, web :8083, sleepcycle-serve :8084, verifier-serve :8085, Postgres, Neo4j, MinIO).
- Two accounts exist (e.g. register `alice` and `bob` through the sign-in screen — the first registrant seeds admin). All console routes require sign-in.
- A clean-ish stack is convenient but not required: `make clean-data` wipes everything first if you want a fresh start.

## Automated gate

Run the suite before/after the feature to prove no regression and that the new rules hold:

```
make up
make test        # ARBORETRIET_INTEGRATION=1, -p 1, localhost overrides, post-suite cleanliness gate
```

Expected: all packages pass, the post-suite gate reports every table (including `dataset_shares`) empty. The ownership scenarios below are additionally covered by dedicated integration tests (see tasks.md): per-row teardown through `testutil`, never truncating shared tables.

## Manual end-to-end scenarios

### Scenario 1 — A new dataset is private by default (US1, US3)

1. Sign in as `bob`; create a dataset "bob-data" from a file/import path.
2. Sign out, sign in as `alice` (a different account).
3. Open `/` and `/datasets`.
4. **Expected**: `alice` sees no `bob-data` row, no placeholder, no count hinting at it. `GET /datasets` returns only `alice`'s own rows.
5. Direct-navigate to `/datasets/<bob-data-id>` (copy the id from bob's session).
6. **Expected**: the not-found state, not a partial dataset frame.

### Scenario 2 — Children inherit the boundary (US2)

1. As `bob`, register a goal against `bob-data` and trigger a hypothesis loop (or sleep-cycle) so runs/verifications/heuristics exist.
2. As `alice`, open `/goals`, the heuristics browser, and a `search` query.
3. **Expected**: none of bob's goals/heuristics appear, and searching for them returns nothing.
4. As `alice`, directly navigate to `/goals/<bob-goal-id>` and `/goals/<id>/verifications`, `/outcomes`, `/causal-graph`.
5. **Expected**: every one answers the not-found state; no run, verification, or graph content renders.

### Scenario 3 — Share grants working access, not administration (US4, US6, FR-009/FR-013/FR-014)

1. As `bob`, open `bob-data` → Sharing → add `alice`.
2. As `alice`, reload `/datasets`. **Expected**: `bob-data` appears with a "shared with you" marker; its detail opens; the objective form is available; the metadata editor, delete control, and sharing panel are absent.
3. As `alice`, register a goal into `bob-data`; as `alice`, delete that goal.
   **Expected**: the goal registers and later deletes (the creator can manage their own child).
4. As `alice`, attempt to PATCH or DELETE the dataset, or to read `/datasets/bob-data/shares`.
   **Expected**: 403 on each; the UI offers none of these affordances.

### Scenario 4 — Revocation takes effect immediately (US5, FR-008/FR-010)

1. As `alice`, keep `bob-data` open (ideally with a goal stream/chat running).
2. As `bob`, revoke `alice` in the Sharing panel.
3. As `alice`, refresh the inventory.
   **Expected**: the dataset is gone from the list.
4. As `alice`, reload the open detail or stream.
   **Expected**: the detail answers not-found; an open SSE stream is closed on the next frame rather than continuing to render revoked data.

### Scenario 5 — Administrators follow the same rules (US7, FR-015)

1. With `bob` (a member) owning `bob-data`, sign in as the admin account.
2. **Expected**: the admin sees `bob-data` only if `bob` shared it with them — otherwise not on `/datasets`, not by direct URL, not through any child surface. Admin-only data surfaces are identical to a member's.

### Scenario 6 — Pre-existing and system-owned data lands with the caretaker (FR-003, SC-009)

1. On a stack that already had datasets/goals before this feature, sign in as the earliest admin.
2. **Expected**: those datasets appear in that admin's inventory (they are the caretaker owner).
3. Sign in as any other account.
4. **Expected**: none of the pre-existing data appears unless the caretaker shares it — and the legacy goal-less heuristic corpus is reachable only to the caretaker.

### Scenario 7 — Implicit minting stays private (US3.3)

1. As `bob`, submit a goal with a fresh data source (no existing dataset for its ref) via `/goals` unbound.
2. **Expected**: the minted implicit dataset is owned by `bob`; `alice` cannot see it or its goal anywhere.

## Web dev smoke test (optional)

To preview the UI deltas against the live stack without rebuilding the compose `web` image:

```
make up
make web-dev    # Next dev server on :3000 pointed at the orchestrator on :8080
```

Repeat Scenarios 1–4 in the browser on :3000.