---
status: done
spec: .turbo/specs/arborette.md
---

# Plan: Epistemic Provenance & Observational Relabel

## Context

The concept revision reframes V1 as a purely **observational** engine: what Phase 1 produces is a measured correlation, `P(Outcome | Segment)`, not do-calculus causality. The mechanics of Phase 1 are unchanged — the honesty of the labeling is what changes. This plan makes that reframe concrete and, more importantly, records each empirical edge's **epistemic provenance** on the graph so a future V2 interventional layer (physically-executed interventions, do-calculus `PRODUCED_EFFECT` edges — see spec V2 Roadmap) can coexist on the same graph without a rewrite.

This is a small, **additive** change layered onto already-built services (the graph repository and the Phase-1 triplet-write path). It adds one property to the `PRODUCED` edge and one convention — it does **not** rename `PRODUCED`, and it introduces no new node-level field (the reserved V2 physical-intervention type reuses the existing `Intervention.type` field). It is a prerequisite for the Sleep-Cycle Worker (spec R9), which reads `epistemic_source` and writes its MCTS-derived triplets with it.

## Pattern Survey

### Analogous Features

- `internal/graph/neo4j.go:186-201` — `CreateProduced` is the exact write to extend. It builds one Cypher string `MERGE (i)-[e:"+domain.Produced+"]->(o) SET e.effect_size = $effectSize, e.confidence = $confidence` and passes params as `map[string]any{...}`. Additive property = append `, e.epistemic_source = $epistemicSource` to the SET clause + one map key. Enums are stringified at the param boundary (`string(i.Type)`, `string(o.VerificationStatus)`), so an `EpistemicSource` typed string is passed as `string(...)`.
- `internal/graph/neo4j.go:99-110` — `CreateOutcome` is the multi-property additive-SET precedent (`SET n.verification_status = $status, n.value = $value, n.provenance = $provenance`).
- `internal/orchestrator/hypothesis.go:256` — the single Phase-1 write call site: `s.repo.CreateProduced(ctx, interventionID, outcomeID, domain.ProducedEdge{EffectSize: value - baseline, Confidence: 1.0})`. The one place to set `EpistemicSource`.

### Reusable Utilities

- `internal/graph/neo4j.go:400-403` — `stringProp(v any) string` — tolerant read-back helper returning `""` when a property is absent/non-string. The wrap point IF a PRODUCED-edge reader is ever added (`if src == "" { src = string(domain.EpistemicObservational) }`). No such reader exists today.
- `marshalProps`/`unmarshalProps` (neo4j.go:408-417) are NOT relevant — they JSON-serialize node `properties` bags. PRODUCED-edge scalars (`effect_size`, `confidence`) are written as native Neo4j scalar params; `epistemic_source` follows that scalar path, not the marshaled blob.

### Convention Anchors

- **Typed-string value enums** (domain.go): `InterventionType` (13-18, `"query"`/`"extract"`), `VerificationStatus` (24-32), `TargetDirection`, `ExpressionKind`, `ConstraintOp` — all `type Foo string` + `const ( FooX Foo = "x" )`. Edge *labels* (`Produced`, `PreConditionFor`, `AbstractedFrom`, domain.go:36-40) are a separate block of **bare untyped string** consts. A property *value set* uses the typed-string form, not the bare-label form.
- **By-value struct = stable interface.** `graph.Repository.CreateProduced` (repository.go:39) and impl take `edge domain.ProducedEdge` by value. Adding a field to `ProducedEdge` (domain.go:86-89) changes no signature and breaks no test double. `fakeRepo.CreateProduced` (server_test.go:192) ignores the edge param.
- **Schemaless edge property — no DDL.** `internal/graph/schema.go:15-30` (`InitSchema`) creates only per-node-label `id IS UNIQUE` constraints; no edge schema. Postgres migrations govern only the goal registry. `epistemic_source` needs no migration.

### Proposed Alignment

- Extend `CreateProduced` with the additive-comma SET; normalize an empty `EpistemicSource` to `observational` at the write boundary so V1 edges always carry a concrete value.
- Add `type EpistemicSource string` next to the other value enums (after `VerificationStatus`, domain.go:32) with `observational` + reserved `interventional`.
- Four `domain.ProducedEdge{...}` literals exist: production `hypothesis.go:256` (set the field) + tests `graph_test.go:44`, `internal/heuristics/heuristics_test.go:44`, `internal/mcpserver/tools_integration_test.go:48` (zero-value `""` → normalized to observational, so no change required; optionally set for clarity).
- **No read code to change.** `TraceCausalChain`/`tripletFromRecord` (neo4j.go:295-358) bind only nodes `s,i,o`, never the edge. `UpdateOutcomeVerification` (neo4j.go:285-293) MATCHes the edge and SETs only `e.confidence` — it does not read or clobber `epistemic_source`. So "missing ⇒ observational" is a documentation-only convention for V1.
- **Relabel is prose-only.** `CausalTriplet` (repository.go:18), `TraceCausalChain` (repository.go:60, neo4j.go:295), and MCP tool `trace_causal_chain` (mcpserver/tools.go:119, `traceCausalChainInput`/`Output`, `heuristics.Service.Trace`) are **cross-service API surface — out of scope to rename** (high blast radius). No existing comment asserts Phase-1 output is "causal"; the relabel target is the `writeTriplet` doc comment (hypothesis.go:219-222) plus the new enum/struct doc comments.

## Implementation Steps

1. **Add the `EpistemicSource` typed-string enum**
   - In `internal/domain/domain.go`, after the `VerificationStatus` block (~line 32), add `type EpistemicSource string` with `const ( EpistemicObservational EpistemicSource = "observational"; EpistemicInterventional EpistemicSource = "interventional" )`. Doc-comment it: V1 writes `observational` (a measured correlation, not causation); `interventional` is **reserved for V2** (do-calculus edges) and is never written in the MVP. Note in the comment that a physically-executed `Intervention.type` value is likewise reserved for V2, reusing the existing `InterventionType` field (no new node field).
2. **Add the `EpistemicSource` field to `ProducedEdge`**
   - In `internal/domain/domain.go:86-89`, add `EpistemicSource EpistemicSource` to the `ProducedEdge` struct. Extend the struct's doc comment to state the edge records observational provenance in V1.
3. **Write `epistemic_source` in `CreateProduced` with a write-boundary default**
   - In `internal/graph/neo4j.go:186-201`, before building params, normalize: `src := edge.EpistemicSource; if src == "" { src = domain.EpistemicObservational }`. Append `, e.epistemic_source = $epistemicSource` to the SET clause and add `"epistemicSource": string(src)` to the param map. This keeps the additive-comma pattern of `CreateOutcome` and guarantees no empty-string values are written (so "missing ⇒ observational" only ever covers pre-existing/legacy edges).
4. **Wire the Phase-1 write path**
   - In `internal/orchestrator/hypothesis.go:256`, set `EpistemicSource: domain.EpistemicObservational` in the `domain.ProducedEdge{...}` literal. This is the only current V1 `PRODUCED` writer; future writers (e.g. the `extract` path, spec R17) will set it too, and the write-boundary default (Step 3) protects any that forget.
5. **Observational relabel (prose only)**
   - Update the `writeTriplet` doc comment (`internal/orchestrator/hypothesis.go:219-222`) to frame the persisted triplet as an **observational/correlational** finding (`P(Outcome | Segment)`), not causal — the edge's `epistemic_source: observational` records this. Do **not** rename `CausalTriplet`, `TraceCausalChain`, or the MCP tool `trace_causal_chain` (cross-service API surface). Keep the relabel confined to comments and the new domain doc comments from Steps 1-2.
6. **Document the read-side convention**
   - In the `EpistemicSource` enum doc comment (Step 1) and/or a comment near `stringProp` (`internal/graph/neo4j.go:400-403`), record the convention: any consumer that reads a `PRODUCED` edge's `epistemic_source` treats an absent/empty value as `observational` and must not assume `interventional` exists. No read code changes today (none reads the edge); this is the guardrail for the Sleep-Cycle Worker (spec R9) and any future reader.

## Verification

- `go build ./...` and `go vet ./...` — the additive struct field and enum compile with no signature changes; no test-double edits required.
- `go test ./internal/graph/...` — `TestCreateProduced`/graph round-trip (`graph_test.go`) still pass; if a Neo4j-backed test reads the edge back, assert `epistemic_source == "observational"`. If graph tests are integration-gated (require a live Neo4j), note that and rely on build + the orchestrator unit tests below.
- `go test ./internal/orchestrator/...` — `writeTriplet` tests (`server_test.go`, the `fakeRepo` recording nodes) still pass; `fakeRepo.CreateProduced` ignores the edge, so behavior is unchanged. Optionally extend a fake to record the edge and assert `EpistemicSource == domain.EpistemicObservational` is passed from `hypothesis.go:256`.
- `go test ./internal/heuristics/... ./internal/mcpserver/...` — the two other `ProducedEdge{...}` literal sites compile and pass with the zero-value field (normalized to observational on write).
- Manual/integration spot-check (if a live Neo4j is available, e.g. the local docker-compose stack): after a Phase-1 loop run, query `MATCH ()-[e:PRODUCED]->() RETURN DISTINCT e.epistemic_source` and confirm every edge is `observational`; run an `UpdateOutcomeVerification` (HITL confirm) against one and re-query to confirm `epistemic_source` **survives** the confidence update (the MATCH-SET at neo4j.go:285-293 must not clobber it).
- Edge cases to spot-check: a `ProducedEdge{}` with an unset `EpistemicSource` writes `observational` (not `""`); `TraceCausalChain` output is unchanged (it never bound the edge).

## Context Files

- `internal/domain/domain.go` — where `EpistemicSource` and the `ProducedEdge` field are added; read the existing typed-string enums (`InterventionType` :13, `VerificationStatus` :24) and edge-label consts (:36) to match convention.
- `internal/graph/neo4j.go` — `CreateProduced` (:186), the `stringProp` helper (:400), `UpdateOutcomeVerification` (:285), and `TraceCausalChain`/`tripletFromRecord` (:295) to confirm the read paths that do/don't touch the edge.
- `internal/graph/repository.go` — the `graph.Repository` interface and the `CausalTriplet` return type; confirms the by-value signature stays stable and marks the API-surface names not to rename.
- `internal/orchestrator/hypothesis.go` — `writeTriplet` (:219) and the `CreateProduced` call (:256): the single V1 write site to wire and the doc comment to relabel.
- `internal/graph/schema.go` — confirms no edge DDL/constraint exists, so no migration is needed.
