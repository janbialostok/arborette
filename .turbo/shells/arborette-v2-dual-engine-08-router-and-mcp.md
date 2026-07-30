---
spec: .turbo/specs/arborette-v2-dual-engine.md
depends_on: [arborette-v2-dual-engine-04-retrieval-integrity-and-injection, arborette-v2-dual-engine-05-engine-a-hardening, arborette-v2-dual-engine-06-verifier-and-causal-discovery, arborette-v2-dual-engine-07-verification-and-refutation]
---

# Plan: Control Plane Router & MCP Surface

## Context

With verification able to run, the Router decides when it runs: intent classification at intake (explore vs verify, with claim extraction and reification of directly-constructed claims), an explicit per-finding verify affordance (REST + MCP), budgeted auto-promotion of top findings at Phase-1 completion, and the correction → staleness → re-verification flow that makes analyst domain knowledge actually propagate. The MCP surface rides here because R2's verify affordance and R33's `verify_finding` tool are the same contract, and downstream agents get epistemic labels (with the sufficiency caveat) on everything served.

## Produces

- `ClassifyGoalIntent` on `llm.Client` (fenced, flat schema) with claim extraction; goal registry `track` + claim + budget columns
- Verify-track flow: claim → validated filter conjunction → reified observational triplet (deterministic id, `claim_derived` flag) → verification dispatch; cannot-construct outcome reporting
- `POST /goals/{id}/findings/{fid}/verify` + `GET /goals/{id}/causal-verifications` endpoints (distinct from the HITL `/verifications` surface)
- Auto-promotion hook at Phase-1 completion (support-shrunk top-N, budgeted, kill switch); budget-exempt analyst-initiated paths per R3's accounting
- Correction endpoints (edge flip/delete/add with `analyst` provenance) → graph-version bump → staleness marking → capped re-verification dispatch
- MCP: `verify_finding` tool; `epistemic_source` + refutation confidence (+ caveat) in `get_optimized_heuristics` and `trace_causal_chain`

## Consumes

- Verification dispatch (records, budgets, caps, idempotency) and causal-evidence lookup — from Shell 7
- Causal-graph read/write + graph version — from Shell 6
- Prompt-fencing helper (R29) for `ClassifyGoalIntent` — from Shell 4
- Value-validation helper (R20) for claim-construction validation — from Shell 5
- `internal/mcpserver` tool patterns, orchestrator submit path — from existing codebase

## Covers Spec Requirements

- R1
- R2
- R3
- R7
- R13 (partial: caveat carried on MCP output)
- R33

## Implementation Steps (High-Level)

1. **Add intent classification** to the submit path with claim extraction; persist track/claim/budget on the goal.
2. **Implement verify-track reification** (validated construction, deterministic id, `claim_derived` provenance) and dispatch.
3. **Add the explicit verify endpoint + causal-verification listing** under the distinct namespace.
4. **Hook auto-promotion** into Phase-1 completion with ranking, budget, and kill switch.
5. **Build the correction flow**: correction endpoints, version bump, staleness marking, capped re-dispatch; SSE + audit.
6. **Extend MCP**: `verify_finding` tool, epistemic labels + caveat in existing tools.

## Open Questions

- Auto-promotion defaults: top-N, per-goal verification budget, staleness re-verification cap (spec Open Questions).

## Expansion Deferred

The following are filled in when `/expand-shell` runs:

- Pattern survey against the codebase state at implementation time
- Concrete `file_path` references with named functions or symbols for each Implementation Step
- Verification section with specific test commands and smoke checks
- Context Files section with the files to read in full before editing
