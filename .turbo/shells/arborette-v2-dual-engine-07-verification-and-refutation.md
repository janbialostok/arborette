---
spec: .turbo/specs/arborette-v2-dual-engine.md
depends_on: [arborette-v2-dual-engine-01-service-seams-and-internal-auth, arborette-v2-dual-engine-05-engine-a-hardening, arborette-v2-dual-engine-06-verifier-and-causal-discovery]
---

# Plan: Verification, Refutation & the Epistemic Contract

## Context

Engine B's second stage: given a finding and the discovered graph, compute the backdoor-adjusted do() effect, stress-test it with the refutation battery, and write the result honestly — `causally_verified`, `confounded`, `not_identifiable`, or `unsupported_objective` are all first-class answers. This shell owns the whole atomic verification lifecycle: the adjustment math and its treatment/outcome mapping, refutation, the `causal_inferred` epistemic value with its read-path contract (observational collection filters + the exempt causal-evidence lookup), deterministic causal-Outcome ids with supersession on any completed outcome, and the leased, idempotent `causal_verifications` records with crash reaping and the budget/cap accounting later shells rely on.

## Produces

- Sandbox `stratified_effect` and `sampled_effect` analysis kinds (GROUP-BY adjustment, USING SAMPLE)
- Adjustment computation per R8's mapping (treatment = joint segment indicator; Z = parents(T) \ (T ∪ O)); windowed objectives short-circuit to `unsupported_objective`
- Refutation battery (placebo, subsample stability, random-confounder) + refutation score → edge confidence
- `causal_inferred` `EpistemicSource` value; `coalesce`-filtered observational collection reads (`ListEligibleFindings` etc.); the exempt causal-evidence lookup (latest non-superseded edge per intervention)
- Causal Outcome writes: deterministic id (intervention + graph version), `verification_status: verified`, supersession on ANY completed newer-version outcome (standalone retraction for non-confirming results)
- `causal_verifications` Postgres table (unique key, lease deadline, statuses incl. `causally_verified`) with charge-at-dispatch-accept accounting, lease reaping/refund, re-lease transitions, and the per-goal in-flight cap
- Verification SSE frames + audit events through the authenticated internal API

## Consumes

- Discovered causal graph + `/analyze` + Verifier scaffold — from Shell 6
- Windowed-objective shape (entity/time bindings, windowed AST constructs) for the `unsupported_objective` short-circuit — from Shell 5
- Ground-truth dataset (planted confounder, true effect) for refutation-sanity regression — from Shell 6
- Authenticated audit write path — from Shell 1

## Covers Spec Requirements

- R8
- R9
- R10
- R11
- R12
- R13 (partial: domain value, read-path filter contract, caveat contract definition)
- R30 (partial: refutation-sanity regression — planted confounder killed, true effect survives adjustment — and Engine-B-at-defaults exercisability)

## Implementation Steps (High-Level)

1. **Add `stratified_effect`/`sampled_effect` kinds** to `/analyze`.
2. **Implement adjustment**: derive Z from the graph per R8, compute adjusted vs naive effect; sparse strata → `not_identifiable`; unoriented-backdoor → `not_identifiable`; windowed → `unsupported_objective`.
3. **Implement the refutation battery** and score→confidence mapping.
4. **Extend the epistemic domain**: `causal_inferred` value; add the coalesce filter to observational collection reads; build the exempt causal-evidence lookup.
5. **Write causal Outcomes** with deterministic ids and universal supersession semantics.
6. **Build `causal_verifications`** with unique key, leases, reaping/refund, re-lease, per-goal in-flight cap, and transactional budget accounting.
7. **Stream + audit** every verification transition.
8. **Regression-test refutation sanity** against the Shell 6 ground-truth dataset: the planted confounder's spurious effect reports confounded, the true effect survives adjustment and refutation, placebo shows ~zero — Engine B exercisable at default config.

## Open Questions

- Refutation battery composition weights, subsample count K, and the refutation-score → confidence mapping (spec Open Questions).
- Verification lease duration default (spec Open Question).

## Expansion Deferred

The following are filled in when `/expand-shell` runs:

- Pattern survey against the codebase state at implementation time
- Concrete `file_path` references with named functions or symbols for each Implementation Step
- Verification section with specific test commands and smoke checks
- Context Files section with the files to read in full before editing
