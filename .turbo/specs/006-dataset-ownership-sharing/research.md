# Research: Dataset Ownership & Sharing

Phase 0 output. This feature ships inside one known codebase, so "research" is a combination of the spec's resolved design decisions and targeted codebase analysis of the seams the design threads through. Every decision is recorded Decision / Rationale / Alternatives so planning can revisit.

## 1. Ownership root and inheritance model

**Decision**: The dataset is the single ownership root. A child object (goal, and through it every run, verification, heuristic/embedding, and graph/outcome row) is accessible exactly when the user can access its dataset. Access = owner OR an explicit `dataset_shares` row. Goals additionally record a `created_by` for the child-management rule (FR-014), not for visibility.

**Rationale**: `goal_registry.dataset_id` is `NOT NULL` and the entire child tree hangs off goals (`runs.optimization_function_id`, `verification_queue.optimization_function_id`, `causal_verifications.goal_id`, `meta_heuristic_embeddings.goal_id`, Neo4j nodes keyed by `goal_id` + `datasource_ref` in `internal/graph/causal.go`). Enforcing at one level (datasets) makes every descendant surface correct by construction and avoids per-table ACL wiring across two stores (Postgres + Neo4j).

**Alternatives considered**: Per-object ACL on every child table — rejected as a combinatorial change across both stores with no use case; groups/roles sharing — deferred (spec assumption says per-account sharing only); ownership on goals with dataset as a mere grouping — rejected because the working-access model (FR-009) is dataset-scoped.

## 2. Owner identity source and system/legacy ownership

**Decision**: `datasets.owner_id` is `NULL`-able and references `users(id)`. Human-created datasets stamp the signed-in session identity (`AnalystsFromContext`, already stashed by the session guard at `internal/orchestrator/session.go:90`). Pre-existing rows and rows created by background processes receive `owner_id = NULL` and are attributed to an **admin caretaker**: the migration backfills to the earliest active admin (`ORDER BY created_at, id LIMIT 1`); a runtime self-heal (mirroring the `003` first-registrant-seeds-admin rule at `internal/store/usersessions.go:80`) attributes any still-NULL rows to the first admin when one registers, and boot reconciliation does the same. A NULL-owner dataset is visible to nobody until attributed — a safe transient.

**Rationale**: Historical activity was stamped with a stub identity (`ARBORETTE_ANALYST_ID`, default `analyst-stub`), so true creators of pre-existing data are unknowable; the audit `actor` cannot be trusted for attribution and is unreadable by runtime roles. A NULL-owner-until-admin rule keeps Q1 (admin caretaker) and Q2 (admins follow the same rules as everyone) simultaneously true: the caretaker sees their attributed data like any owner; other admins see nothing unless shared.

**Alternatives considered**: A dedicated non-user "system" owner — rejected because shares and UI expect a real account; grandfathered-visibility — rejected because it violates the requirement's absolute wording; attributing by audit `actor` — rejected because the stub identity predates accounts.

## 3. Sharing model

**Decision**: A dedicated `dataset_shares (dataset_id, user_id, created_at, created_by)` table, `PRIMARY KEY (dataset_id, user_id)`, `ON DELETE CASCADE` on both FKs. Sharing is literal per-account grants made by the dataset owner; revocation deletes the row. Removed accounts lose grants automatically (FR-012); deactivated accounts are stopped by the session guard's `active` re-check at the next request.

**Rationale**: A table is simpler and more queryable than a JSONB array, matches the project's relational conventions (`0017` users/sessions), and gives CASCADE cleanup for free. `created_by` records the sharing actor for audit.

**Alternatives considered**: JSONB `shared_with` array on `datasets` — harder to index/query and to CASCADE on user delete; sharing to a role/group — out of scope.

## 4. Access predicate and enforcement points

**Decision**: One predicate, expressed in SQL and reused everywhere:

```
accessible(d, u) := d.owner_id = u
                 OR EXISTS (SELECT 1 FROM dataset_shares sh
                            WHERE sh.dataset_id = d.id AND sh.user_id = u)
```

Store reads gain user-scoped variants (`DatasetStore.ListAccessible/GetAccessible/CanAccess`, `GoalRegistry.ListAccessible/ListByDatasetAccessible/GetAccessible`); handlers resolve the acting user from context and either filter (lists) or gate (single-object/child routes). Enforcement is **always** at the orchestrator; the web client only hides/marks affordances and is never authoritative.

**Rationale**: Every analyst route already passes the session guard, so the acting user is always in `AnalystsFromContext`; central gating at the handler is where the existing `adminRequired` pattern (`internal/orchestrator/accountadmin.go:52`) already proves out.

**Alternatives considered**: Postgres Row-Level Security — rejected as a larger trust shift (two runtime roles, service-to-service paths, and tests revolve around grant-based access, not RLS); a middleware-level gate — rejected because access is goal→dataset derived and needs per-object reads.

## 5. Not-found vs forbidden on inaccessible single objects

**Decision**: An inaccessible single object (dataset, goal, heuristic, or any goal-keyed sub-resource) answers the uniform **404**; a dataset the user *can* access but does not *own* answers **403** to owner-only writes (PATCH/DELETE of the dataset, share management); a goal they can access but did not create answers **403** to its deletion unless the caller is the dataset owner.

**Rationale**: Matches the existing anti-existence-oracle posture (session guard's generic 401 at `session.go:106`); a 404 "dataset not found" leaks nothing. 403 only after the object's existence is already legitimately known to the caller.

**Alternatives considered**: 403 for everything — leaks existence; redirect-to-login for data reads — wrong layer (page redirects are the web middleware's job, and FR-007 keeps data responses machine-readable).

## 6. SSE streams and mid-stream revocation (FR-008)

**Decision**: The stream open validates access (goal → dataset → `CanAccess`). Each forwarded frame consults the access predicate again inside the stream handler's write loop before writing to the browser; when access is lost the handler ends the stream (subscriber dropped). The `Hub` stays generic (anonymous channels, `internal/orchestrator/hub.go`) — the re-check lives where the user identity is known, not in the hub.

**Rationale**: The hub publishes for loop goroutines without analyst context; threading identities through it is invasive. The write-loop re-check converts "next data action" into the next frame — the practical reading of FR-008 for a long-lived stream — with zero hub churn.

**Alternatives considered**: Per-publish checks inside `Hub.Publish`/`PublishLive` — moves user identity and a store call into the pub/sub core; killed-at-access-lost only when the next HTTP call happens — leaves a stale stream rendering revoked data.

## 7. Similarity search and heuristics surfaces

**Decision**: The analyst-facing heuristic search gains a user scope: results are limited to goals whose dataset the user can access (a goal → dataset → access join in the embedding query), and legacy NULL-goal rows (the pre-dataset corpus) are visible only to the owning admin caretaker. The internal knowledge-reuse paths (Sleep-Cycle `groundProposals`, `internal/sleepcycle/grounding.go`) scope their corpus read to the goal's own dataset plus system-owned rows, so one user's heuristics never leak into another user's run output or proportionals.

**Rationale**: `internal/store/embeddingstore.go:258` already supports goal-scoped vs cross-goal modes with a known post-filter cost; adding the access join keeps the browsing surface honest (FR-006 explicitly names "similarity/cross-goal retrieval") and the cross-goal grounding sees only what the acting principal may.

**Alternatives considered**: Leaving `SLEEPCYCLE_SEARCH_CROSS_GOAL_GROUNDING` (default true) reading the whole corpus — rejected: it is exactly a cross-user retrieval surface and would leak a collaborator's heuristic content into a run's grounding; ignoring the NULL-goal legacy corpus — rejected: the caretaker owner still needs it reachable.

## 8. Admin scope composites with existing account management

**Decision**: No new role semantics. The `/users` roster stays as-is (accounts, not data access); admin-follows-same-rules means a caretaker admin is simply the `owner_id` of backfilled/system rows and gains nothing else.

**Rationale**: `adminRequired` already gates account surfaces; introducing an admin data-view would contradict the resolved Q2.

## 9. DTO and UI surface

**Decision**: `datasetDTO`/`DatasetSummary` gain `access: "owner" | "shared"` (derived server-side from the predicate) and `owner_id`; the detail adds `owner_username`. `GET /datasets/{id}/shares` + `PUT/DELETE` share endpoints are owner-only. The web UI shows a "shared with you" marker on shared rows, hides Metadata-edit/Danger/Registration? (no — registration stays: collaborators create goals; only delete + edit + share management hide), and renders a share-management panel for owners.

**Rationale**: The client needs the private boolean `access` to make the collaborator experience distinct (US1.3, US6) without re-deriving it; the server derives it so the client never asserts entitlements.

## Unresolved items

None. The three spec questions (Q1 caretaker, Q2 admins follow rules, Q3 working access without dataset administration) are resolved and encoded above; no new NEEDS CLARIFICATION emerged from the design review.