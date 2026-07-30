---
spec: .turbo/specs/arborette-v2-dual-engine.md
depends_on: [arborette-v2-dual-engine-02-sandbox-substrate]
---

# Plan: Engine A Hardening — Value Grounding & Windowed Objectives

## Context

Engine A remains V2's fast track unchanged in shape, but it still silently produces zero-triplet "completed" runs when Claude invents categorical values that exist in no row — the last remaining cause of goals that report success while producing nothing. Separately, entity-relative signals (velocity, trailing means — the native vocabulary of the fraud use case) are inexpressible because the sandbox emits only flat SELECTs. This shell grounds Phase-1 proposals in real column values and adds windowed objective support with its own threat pass, since the compiler's narrowness is currently a load-bearing injection defense.

## Produces

- Introspection returning capped distinct values for low-cardinality columns
- Value-grounded tree-proposal prompt plus a deterministic post-check (value analogue of `UnknownFilterColumns`) feeding the existing repair loop; zero-row segments become a distinct non-fatal outcome
- Entity-key + time-column bindings at goal registration; windowed AST constructs (lag, trailing aggregate) compiling to `PARTITION BY entity ORDER BY ts` window SQL
- Threat-pass results for the widened grammar (quoting, binding, allowlists extended to the window surface)
- The value-validation helper later reused by claim construction and grounding validation

## Consumes

- Hardened sandbox with early validation — from Shell 2
- `internal/sandbox` introspection/compile, `internal/orchestrator/hypothesis.go` proposal/repair loop, `internal/domain` Expression AST, `internal/llm` tree-proposal prompt — from existing codebase

## Covers Spec Requirements

- R20
- R21

## Implementation Steps (High-Level)

1. **Return distinct values from introspection** for low-cardinality columns (capped, config threshold).
2. **Ground the proposal prompt** on those values and add the deterministic value post-check + repair wiring; make zero-row segments a retryable non-fatal outcome.
3. **Add entity/time bindings to registration** and windowed constructs to the AST with depth/shape guards.
4. **Compile windows in the sandbox** and run the threat pass on the widened surface before enabling.

## Open Questions

- Low-cardinality cutoff for distinct-value return (spec Open Question; likely shared with Shell 6's CI-test binning).

## Expansion Deferred

The following are filled in when `/expand-shell` runs:

- Pattern survey against the codebase state at implementation time
- Concrete `file_path` references with named functions or symbols for each Implementation Step
- Verification section with specific test commands and smoke checks
- Context Files section with the files to read in full before editing
