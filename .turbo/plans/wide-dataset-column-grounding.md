---
status: done
---

# Plan: Wide-Dataset Column Grounding (arbitrary-width goal registration and runs)

## Context

Registering a goal against a wide dataset (e.g. Spaceship Titanic `train.csv`, ~14 columns) fails: Claude returns `400 invalid_request_error` — *"The compiled grammar is too large… Simplify your tool schemas or reduce the number of strict tools."* — which the orchestrator maps to a 502 "evaluation matrix generation failed". The root cause is in `internal/llm/schema.go`: both structured-output schemas embed the introspected column names as JSON-schema `enum` constraints at every column-reference site (`fieldProp(cols)`), and the matrix schema nests those enums inside a depth-3 objective-expression AST. Anthropic compiles the whole schema into a constrained-decoding grammar, and grammar size scales with enum cardinality × schema structure — so it exceeds Anthropic's ceiling at a low column count. This makes the schema-aware intake capability unusable beyond toy datasets.

The defect is broader than registration. `ProposeInterventionTree` shares the same `columnEnum`/`fieldProp` mechanism and is called at **every node** of the hypothesis tree (`hypothesis.go:125`, `hypothesis.go:191`). So a wide dataset that (hypothetically) registered would still fail every proposal call — no run could complete or emit triplets for the sleep cycle. The fix must cover both the intake matrix schema and the loop proposal schema.

The chosen direction (set in turboplan) is to **stop grounding column references via `enum`** — which scales with dataset width — and instead ground columns in the prompt (`columnSummary` already lists every column and type) plus validate deterministically after generation. This makes the compiled grammar independent of column count, so datasets of arbitrary width work. Anthropic's own error text ("simplify your tool schemas") points the same way. Two secondary benefits: the fixed-cardinality enums (aggregation, direction, constraint-op) stay, and the now-**static** schemas (identical for every dataset) become eligible for Anthropic's 24-hour structured-output compile cache, which per-dataset column enums currently defeat. The priority is completing runs and producing useful insights, not minimizing Claude API calls, so extra repair round-trips are acceptable.

Resolved design decisions:
- **Always drop the column enums** in both schemas (no width threshold) — one uniform path, truly arbitrary width, static cacheable schemas.
- **Registration: up to three schema-aware repair attempts** against the sandbox dry-run before returning 422 (currently one-shot).
- **Hypothesis loop: always repair** a proposal that references a non-existent column — re-propose against the validation error (bounded, then drop any still-invalid candidates so the run always terminates).

## Pattern Survey

### Analogous Features

- `internal/orchestrator/submit.go:81-99` — the **only existing "deterministic dry-run + one-shot repair" seam**. `handleSubmitGoal` runs `dryRunObjective`, tests with `isObjectiveValidationFailure`, calls `RepairEvaluationMatrix` once, re-runs the dry-run, maps a second failure to 422 (`"could not fit the goal to the data source: "+verr.Error()`), a sandbox fault to 502, success to 201-then-persist. This is the seam registration's repair loop extends from one attempt to three.
- `internal/orchestrator/hypothesis.go:328-335` — `dryRunObjective` validates a matrix by `pinObjective` + a filter-less `Execute` (the exact request the root baseline runs). Returns nil (compiles) or the pin error / `SandboxError`. **This already validates the objective's columns** via the sandbox compile path, so registration needs no new column validator.
- `internal/orchestrator/hypothesis.go:341-347` — `isObjectiveValidationFailure` classifies a repairable failure: `errNoObjective`/`errMissingAggregation` (pin-time) **or** a `*SandboxError` with `Status == http.StatusBadRequest`. Any 4xx≠400, 5xx, or transport error is a fault. The registration repair loop keys on this unchanged.
- `internal/sandbox/compile.go:254-330,450-474` — `compileExpr`/`resolveColumn`/`matchColumns` — the sandbox's own column-vs-schema validation, **case-insensitive via `strings.EqualFold`** (0 matches → unknown field, >1 → ambiguous). It lives behind the CGO boundary and **cannot be imported** by the CGO-free orchestrator/llm/domain (see `internal/orchestrator/sandboxclient.go:19-25`). The new domain validator must re-implement the same `EqualFold` rule, not import it.
- **Candidate flow — nothing is dropped today.** `hypothesis.go:150-152` (root) and `:202-204` (children) iterate every `Proposal.Candidate`; each is unconditionally passed to `processCandidate` (`:159`), which `Execute`s it at `:163`. An invalid-column candidate reaches the sandbox and its 400 becomes a non-terminal `branchFailure` (`:165`, `:277-286`). The new deterministic pre-check + repair slots in **before** `Execute` at these two loops.

### Reusable Utilities

- `internal/domain/domain.go:164-181` — `Expression` fat-node union, and `renderExpr` (`:193-224`) — the dependency-free AST-walk template. **Not needed for this change**: `CandidateIntervention` carries `Filters []domain.Constraint` only (no objective AST), so the loop validator inspects `Constraint.Field` (`domain.go:255-259`), not an Expression tree.
- `internal/orchestrator/hypothesis.go:427-433` — `toSandboxSchema` maps `schemaDTO` → `llm.SandboxSchema`; the column set a deterministic validator consumes (via `.Columns[].Name`).
- `internal/llm/client.go:80-99` — `RepairEvaluationMatrix` — the repair-call template to mirror for a new `RepairInterventionTree`.
- `internal/llm/client.go:228-234` — `columnSummary` — already renders `- name (type)` per column into every prompt; the prompt-grounding half of the change strengthens the system prompts to lean on this list.
- `internal/llm/schema.go:219-228` — `columnEnum` — the function being deleted; its `len==0 → nil` behavior is pinned by `TestColumnEnumUnconstrainedWhenEmpty` (that test is removed/replaced).
- `internal/orchestrator/sandboxclient.go:83-88` — `SandboxError{Status, Message}` — the typed error `isObjectiveValidationFailure` classifies via `errors.As`.

### Convention Anchors

- **Schema builders** in `internal/llm/schema.go` are small combinators (`object`, `arrayOf`, `enumSchema`, `fieldProp`, `constProp`, `constraintItem`). Tests (`internal/llm/schema_test.go`) navigate the raw `map[string]any` via helpers (`requiredKeys`, `properties`, `enumValues`, `anyOfVariants`, `resolveRef`). Removing the column enums changes `TestEvaluationMatrixSchema` (asserts a 2-value direction enum stays but the column ref is now a plain string), `TestInterventionTreeSchema`, and deletes/replaces `TestColumnEnumUnconstrainedWhenEmpty`.
- **Anthropic SDK fake** (`internal/llm/client_test.go:18-54`): `fakeMessages` implements the one-method `messagesClient`; canned `*anthropic.Message` are built by `message(t, stop, blocks...)` which **JSON round-trips** (marshal a map → unmarshal into `anthropic.Message`) so `AsText` reads from `.raw`. New llm tests (e.g. for `RepairInterventionTree`) must use this construction; struct literals don't populate `.raw`.
- **Orchestrator test doubles** (`internal/orchestrator/server_test.go`): `fakeClaude` (`:120-142`) records `gotSchema`/`gotNodes` and counts `repairCalls`; `fakeSandbox` (`:194-217`) scripts per-call `execResps`/`execErrs` indexed by `execCalls`. Wired via `newTestServer`/`newTestServerRepo`. Adding a `RepairInterventionTree` method to the `claudeClient` interface (`server.go:43-47`) **requires** adding it to `fakeClaude`.
- **Error sentinels + wrapping**: `domain`/`sandbox` declare `errors.New` sentinels; the orchestrator wraps and logs `log.Printf("orchestrator: <context>: %v", err)` at every failure site. A new deterministic-validation error message is a plain string fed to the repair prompt (like the sandbox error text is today), not a new sentinel.
- **CGO boundary is load-bearing** (`sandboxclient.go:19-25`): the orchestrator must not import `internal/sandbox`. The dependency-free `internal/domain` package (already holds `Constraint`, `Expression`, `strconv`/`strings` usage) is the idiomatic home for a CGO-free column validator both `internal/orchestrator` and (if ever needed) `internal/llm` can call.

### Proposed Alignment

Blend, don't invent. Remove the width-scaling `enum` from both schemas and keep the fixed-cardinality enums; route the objective's column validation through the **existing** dry-run + repair seam (extended to three attempts); add **one small CGO-free validator in `internal/domain`** (case-insensitive `EqualFold` match, mirroring `compile.go:450` since the real one is CGO-locked) used only by the hypothesis loop to pre-check candidate filter columns before `Execute`, with a new `RepairInterventionTree` call modeled on `RepairEvaluationMatrix`. Extend the established test scaffolds (`schema_test.go`, the `client_test.go` JSON round-trip fake, the scripted `fakeSandbox`/`fakeClaude`) — no new test infrastructure.

## Implementation Steps

1. **Drop the column enums from both structured-output schemas** (`internal/llm/schema.go`)
   - Delete `columnEnum` (`:219-228`) and stop threading `cols []any` through `evaluationMatrixSchema`, `expressionRef`, `expressionDef`, `interventionTreeSchema`, and `constraintItem`.
   - In `expressionDef`, the `column_ref` variant's `column` property becomes `stringProp()` (was `fieldProp(cols)`).
   - In `constraintItem`, the `field` property becomes `stringProp()` (was `fieldProp(cols)`); keep `op` as `enumSchema(constraintOpEnum())` and `value` as the number schema.
   - Keep every fixed-cardinality enum untouched: `direction` (`directionEnum`), `aggregation` (`aggregationEnum`), `op` (`constraintOpEnum`). These do not scale with dataset width.
   - `fieldProp` is now unused → remove it (or leave `stringProp` inline). The `$defs`/`$ref` bounded-DAG expression schema is unchanged apart from losing the `cols` argument.
   - Net effect: both `evaluationMatrixSchema()` and `interventionTreeSchema()` produce a schema **independent of `SandboxSchema.Columns`** (static per code version) → grammar size no longer grows with width, and the schema becomes compile-cacheable.

2. **Strengthen the system prompts to ground columns via the prompt** (`internal/llm/client.go`)
   - Since the `enum` no longer enforces the column set, update `evaluationMatrixSystem`, `evaluationMatrixRepairSystem`, and `interventionTreeSystem` to state explicitly: reference **only** columns present in the provided "Available columns" list, using their exact names as written. `columnSummary` already injects that list into every user message (`GenerateEvaluationMatrix`, `RepairEvaluationMatrix`, `treePrompt`), so no new prompt plumbing is needed — just firmer instruction language.

3. **Add a CGO-free filter-column validator** (`internal/domain/`)
   - Add a pure function, e.g. `func UnknownFilterColumns(filters []Constraint, columns []string) []string`, returning the `Field` values that match no entry in `columns` under `strings.EqualFold` (deduplicated, first-seen order). This mirrors `matchColumns`' case-insensitive rule without importing the CGO-locked sandbox.
   - Home it in `internal/domain` (dependency-free; already imports `strings`). It takes `[]string` column names (not `llm.SandboxSchema`) so `domain` keeps zero package dependencies; the orchestrator extracts names from `llm.SandboxSchema`.

4. **Add `RepairInterventionTree` to the Claude client** (`internal/llm/client.go`, `internal/llm/schema.go` reuse)
   - Mirror `RepairEvaluationMatrix`: `func (c *Client) RepairInterventionTree(ctx, goalText string, matrix domain.EvaluationMatrix, schema SandboxSchema, node TreeContext, prior Proposal, validationErr string) (Proposal, error)`.
   - Build a repair user message from `treePrompt(...)` plus the rejected candidates (marshaled) and the `validationErr` (e.g. "these filter columns are not in the schema: …; re-propose using only the listed columns"). Use a repair system prompt emphasizing exact column names. Reuse `interventionTreeSchema()` and the existing `complete(...)` + `proposalWire` unmarshal path.
   - Register the method on the `claudeClient` interface (`internal/orchestrator/server.go:43-47`).

5. **Extend registration to three repair attempts** (`internal/orchestrator/submit.go`)
   - Replace the single `RepairEvaluationMatrix` block (`:81-95`) with a bounded loop: after the initial `GenerateEvaluationMatrix` + `dryRunObjective`, while `isObjectiveValidationFailure(verr)` and attempts `< 3`, call `RepairEvaluationMatrix(ctx, goal, schema, matrix, verr.Error())`, set `matrix`, re-run `dryRunObjective`. A repair-call transport error still maps to 502; after three exhausted attempts return 422 (`"could not fit the goal to the data source: "+verr.Error()`); a non-validation dry-run error still routes to `writeIntakeErr`. Add a `const maxObjectiveRepairs = 3`.

6. **Pre-validate + repair candidate columns in the hypothesis loop** (`internal/orchestrator/hypothesis.go`)
   - Add a helper, e.g. `func (s *Server) proposeValidCandidates(ctx, goal store.Goal, schema llm.SandboxSchema, node llm.TreeContext) ([]llm.CandidateIntervention, error)`:
     - Call `ProposeInterventionTree`; on transport error return it (callers keep the terminal-at-root / non-terminal-in-child distinction).
     - Extract `schemaCols := names(schema.Columns)`. For each candidate, compute `domain.UnknownFilterColumns(cand.Filters, schemaCols)`. Partition into an **accumulated valid set** and the invalid candidates.
     - **Always repair** while any candidate is invalid and repair attempts `< maxProposalRepairs` (a small const, e.g. 3, mirroring registration): call `RepairInterventionTree(...)` passing **only the invalid candidates** plus a message naming the unknown columns (never re-proposing the already-valid ones). Validate each returned candidate and **merge** the newly-valid ones into the accumulated valid set; anything still invalid becomes the invalid set for the next round. This accumulate-valid / re-propose-only-invalid loop guarantees earlier-round valid candidates are neither dropped nor duplicated into duplicate triplets.
     - **Repair-call transport error:** if `RepairInterventionTree` itself returns a transport error mid-loop, return it as the helper's error (do not swallow it) — mirroring §5's registration intent that a repair-call fault is not silently absorbed. The caller then treats it terminally at the root and as a non-terminal `branchFailure` at a child, matching the `ProposeInterventionTree` transport-error path.
     - After the bound, **drop** any still-invalid candidates and return the accumulated valid set. Whenever any candidate was dropped — not only when the valid set fully empties — emit a `branchFailure(ctx, id, node.ParentFilters, err)` naming the dropped unknown columns (visible audit + SSE, matching the codebase's non-silent `branchFailure`/siblings-continue convention). If the valid set is empty, return an empty slice — the run continues and terminates.
   - Replace the two `ProposeInterventionTree` call sites: root (`:125-132`) and child (`:191-201`) call `proposeValidCandidates` and iterate its returned slice. Root proposal transport failure stays terminal (`termErr`); child stays non-terminal (`branchFailure`) — unchanged.

7. **Update and add tests** (see Verification for the full matrix)
   - `internal/llm/schema_test.go`: rewrite `TestEvaluationMatrixSchema`/`TestInterventionTreeSchema` to assert the `column`/`field` props are plain strings (no `enum`) while `direction`/`aggregation`/`op` enums remain; delete `TestColumnEnumUnconstrainedWhenEmpty`; add a test that building each schema with 2 columns vs 200 columns yields byte-identical schemas (pins width-independence).
   - `internal/domain`: unit-test `UnknownFilterColumns` (case-insensitive match, unknown detection, dedup, empty-schema behavior).
   - `internal/llm/client_test.go`: add a `RepairInterventionTree` test using the JSON-round-trip `message(...)` fake.
   - `internal/orchestrator/submit_test.go`: the three-attempt loop **repurposes an existing test** — `TestSubmitGoalDryRunFailsTwiceIsUnprocessable` (named for the old one-shot semantics) scripts exactly two 400 `execErrs` and asserts `repairCalls == 1` + 422. It becomes the exhaustion case for the 3-repair loop; there is **no separate "three-attempts" test** — this one test *is* the exhaustion scenario. Rewrite and rename it (e.g. `TestSubmitGoalRepairsExhaustedIsUnprocessable`) to script **four** 400 `execErrs` and assert `repairCalls == maxObjectiveRepairs` (i.e. 3) with a 422. The count follows the codebase's K-repair → (K+1)-scripted-400s pattern: the §5 loop runs one initial dry-run plus one dry-run after each of the three repairs, so all four dry-runs must return 400 for the loop to exit still-failing (if only three 400s are scripted, the fourth dry-run reads past `execErrs`, `fakeSandbox.Execute` (`server_test.go:205-214`) returns a nil error, the loop exits with `verr == nil`, and the goal wrongly persists as 201/`repairCalls == 3`). Keep `TestSubmitGoalDryRunRepairThenSuccess` (one 400 then success → `repairCalls == 1`, 201) and `TestSubmitGoalRepairErrorIsBadGateway` (repair-call error → 502) passing unchanged.
   - `internal/orchestrator/server_test.go`: add `RepairInterventionTree` to `fakeClaude` (record calls + script responses). Add cases: (a) loop repairs an invalid-column candidate — script a proposal with a bad column then a repaired proposal with a valid column, assert the bad candidate never reaches `Execute` and the valid one writes a triplet; (b) loop node with only invalid columns after the repair bound emits a `branch_failure` and the run still completes.

## Verification

How to verify the change works end-to-end after implementation:

- **Unit/integration tests** — `go test ./internal/llm/... ./internal/orchestrator/... ./internal/domain/...` all pass, including:
  - `schema_test.go`: column/field props carry no `enum`; fixed enums intact; schema is byte-identical at 2 vs 200 columns.
  - `domain`: `UnknownFilterColumns` returns the expected unknown fields case-insensitively.
  - `submit_test.go`: the repurposed/renamed exhaustion test (four scripted 400s → `repairCalls == maxObjectiveRepairs` (3) → 422) passes; `TestSubmitGoalDryRunRepairThenSuccess` and `TestSubmitGoalRepairErrorIsBadGateway` still pass unchanged in behavior.
  - `server_test.go`: the loop repairs a bad-column candidate (bad candidate never hits `Execute`), and a node left with no valid candidates emits `branch_failure` while the run completes.
- **Build** — `go build ./...` succeeds; the CGO-free orchestrator still builds (the new validator adds no imports beyond `strings` in `internal/domain`, and the orchestrator does not import `internal/sandbox`).
- **Wide-dataset repro (the original failure)** — register the Spaceship Titanic `train.csv` (~14 columns) via `POST /goals`: previously 502 "evaluation matrix generation failed"; now returns 201 with an `optimization_function_id`. Then trigger the run (`POST /goals/{id}/trigger`) and confirm via the SSE stream / `GET /goals` that it reaches `completed` and emits at least one `triplet` (not just a root `branch_failure`).
- **Arbitrary-width spot check** — register a synthetic ~200-column CSV; confirm no `400 invalid_request_error: "The compiled grammar is too large…"` surfaces (the failure mode that motivated this change) and registration succeeds or returns a clean 422 fit failure, never a 502 grammar error.
- **Regression** — a narrow dataset (2–3 columns) still registers and runs, producing triplets, confirming prompt-grounding + validation did not regress the common case.

## Context Files

Files to read in full before starting implementation:

- `internal/llm/schema.go` — the schemas being de-enumed; `columnEnum`, `fieldProp`, `constraintItem`, `evaluationMatrixSchema`, `interventionTreeSchema`, and the `$defs` expression builders.
- `internal/llm/client.go` — `GenerateEvaluationMatrix`, `RepairEvaluationMatrix` (repair template), `ProposeInterventionTree`, `complete`, `columnSummary`, `treePrompt`, and the three system-prompt constants to strengthen.
- `internal/orchestrator/submit.go` — `handleSubmitGoal`'s dry-run + one-shot repair block to convert to a three-attempt loop.
- `internal/orchestrator/hypothesis.go` — `runLoop`, `processCandidate`, the two `ProposeInterventionTree` call sites, `dryRunObjective`, `isObjectiveValidationFailure`, `branchFailure`, `toSandboxSchema`.
- `internal/orchestrator/server.go:43-52` — the `claudeClient` interface `RepairInterventionTree` is added to.
- `internal/domain/domain.go` — `Constraint`, `Expression`, and existing dependency-free helpers, to place `UnknownFilterColumns` idiomatically.
- `internal/orchestrator/server_test.go` and `internal/llm/{schema_test.go,client_test.go}` — the test doubles (`fakeClaude`, `fakeSandbox`, the JSON-round-trip `message` helper) and schema-navigation helpers the new tests extend.
