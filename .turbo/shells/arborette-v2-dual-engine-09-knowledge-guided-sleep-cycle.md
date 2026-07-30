---
spec: .turbo/specs/arborette-v2-dual-engine.md
depends_on: [arborette-v2-dual-engine-04-retrieval-integrity-and-injection, arborette-v2-dual-engine-05-engine-a-hardening, arborette-v2-dual-engine-06-verifier-and-causal-discovery, arborette-v2-dual-engine-07-verification-and-refutation]
---

# Plan: Knowledge-Guided Sleep Cycle (UCT-MCTS)

## Context

The Sleep Cycle's search stops being bounded by whatever vocabulary Phase 1 happened to surface: schema-derived atoms make the lattice genuinely large, and the growing Meta-Heuristic graph supplies both expansion proposals (grounded heuristics — the deferred Cold-Start reuse path) and selection priors, with causal evidence multiplying value estimates. The result is the compounding property that is V2's search thesis: the more knowledge accumulates, the better search gets. Everything lands behind the existing `SearchPolicy` seam — driver, measurement, budgets, and write-back are unchanged — and the beam ships as default until UCT passes the calibration gate against the ground-truth dataset.

## Produces

- `uctPolicy` implementing `SearchPolicy` (Select/Update/Done), config-selected (`SLEEPCYCLE_POLICY=beam|uct`, beam default until gate passes)
- Schema-derived atom vocabulary behind a flag: enum values + quantile cuts; `CritiqueAtoms` LLM critic (leakage/tautology exclusion, first-level ranking); enumerated-atom provenance
- Grounding moves: retrieval (goal-scoped or explicit cross-goal from Shell 4) → instantiation via persisted ontology terms (same-dataset) or `GroundHeuristic` LLM call (cross-dataset/pre-V2), validated, dropped-not-repaired
- Persisted `OntologyTerms` + origin (goal id, data-source ref) on Meta-Heuristic nodes at abstraction time
- UCT priors from similarity, evidence-weighted values, causal multiplier via Shell 7's causal-evidence lookup
- `SearchPolicy` contract invariant (no same-(field,op) conjunction) documented + tested for beam and uct
- Grounded-winner provenance (heuristic id on derived Interventions) surfaced in trace
- Calibration-gate regression: UCT matches/beats beam winners on the ground-truth dataset at default budget; default flip wired to it

## Consumes

- Goal-scoped retrieval + distance floor + fencing helper — from Shell 4
- Value-validation helper (R20) for grounding validation — from Shell 5
- Causal-evidence lookup + `causal_inferred` edges — from Shell 7
- Ground-truth dataset — from Shell 6
- `internal/sleepcycle` (`SearchPolicy`, driver, `buildAtoms`, write-back), `internal/llm` abstraction path — from existing codebase

## Covers Spec Requirements

- R14
- R15
- R16
- R17
- R18
- R19
- R13 (partial: Sleep-Cycle scoring treats unknown/missing epistemic values as observational)
- R30 (partial: UCT-vs-beam calibration regression and Sleep-Cycle-at-defaults validation)

## Implementation Steps (High-Level)

1. **Persist ontology terms + origin** at abstraction time (additive node properties).
2. **Build schema-derived atoms** behind a flag with the critic call and provenance rules.
3. **Enforce the policy-contract invariant** in candidate generation for both policies, with tests.
4. **Implement `uctPolicy`**: UCT selection with similarity priors, atom + grounding expansion, evidence-weighted backprop with the causal multiplier, budget/frontier termination.
5. **Implement grounding**: term-map instantiation, LLM fallback, deterministic validation, provenance threading to write-back.
6. **Run the calibration gate** on the ground-truth dataset; wire the default flip.

## Open Questions

- UCT constants: exploration c, causal multiplier value, prior-vs-visit weighting, atom/grounding budget split (spec Open Questions — calibrated here).

## Expansion Deferred

The following are filled in when `/expand-shell` runs:

- Pattern survey against the codebase state at implementation time
- Concrete `file_path` references with named functions or symbols for each Implementation Step
- Verification section with specific test commands and smoke checks
- Context Files section with the files to read in full before editing
