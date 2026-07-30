---
spec: .turbo/specs/arborette-v2-dual-engine.md
depends_on: [arborette-v2-dual-engine-08-router-and-mcp]
---

# Plan: Web UI — Causal Surfaces

## Context

Analysts are the primary persona, and V2 invisible in the web UI undercuts the product: the analyst needs to see *why* a segment matters (causal vs merely correlated), trigger verification, read confounded/not-identifiable answers in plain language, and correct the discovered graph without being required to review it. This shell ships the full surface: causal-graph view with edge correction (reusing the HITL correction interaction pattern), verify affordances on finding cards, epistemic badges everywhere edges render, and verification results with the sufficiency caveat attached.

## Produces

- Causal-graph view (nodes/edges with provenance + confidence) with edge-direction flip / delete / add-confounder correction interactions wired to the correction endpoints; stale badges during re-verification
- Verify button on finding/triplet cards dispatching the explicit-verify endpoint
- Epistemic badges (`observational` / `causal_inferred`) wherever edges render, with the "causally supported (given the discovered model)" caveat copy
- Verification-result view: naive vs adjusted effect, adjustment set, refutation score, confounded / not-identifiable / unsupported outcomes in analyst language
- BFF proxy routes for the new orchestrator endpoints (causal graph, corrections, causal-verifications)

## Consumes

- Router endpoints (verify, causal-verifications listing, corrections) — from Shell 8
- Causal-graph read endpoint — from Shell 6 (reachable via Shell 8's chain)
- `web/` BFF proxy patterns, SSE handling, HITL correction interaction pattern, existing card components — from existing codebase

## Covers Spec Requirements

- R32
- R13 (partial: caveat carried on all web renderings)

## Implementation Steps (High-Level)

1. **Add BFF proxy routes** for graph, corrections, and causal-verifications.
2. **Build the causal-graph view** with correction interactions and stale/provenance indicators.
3. **Add verify affordances + epistemic badges** to finding/triplet surfaces.
4. **Build the verification-result view** with caveated analyst-language outcomes.
5. **Wire SSE** so verification transitions appear live on the goal view.

## Open Questions

None

## Expansion Deferred

The following are filled in when `/expand-shell` runs:

- Pattern survey against the codebase state at implementation time
- Concrete `file_path` references with named functions or symbols for each Implementation Step
- Verification section with specific test commands and smoke checks
- Context Files section with the files to read in full before editing
