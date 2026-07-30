---
spec: .turbo/specs/arborette-v2-dual-engine.md
depends_on: [arborette-v2-dual-engine-01-service-seams-and-internal-auth]
---

# Plan: Heuristic-Retrieval Integrity & Injection Hardening

## Context

In V2 the Meta-Heuristic corpus stops being a browsing surface and becomes machinery: retrieval seeds MCTS grounding moves and priors, and heuristic content influences what gets verified and searched. Two measured drift/abuse paths therefore get teeth: an unbounded k-NN that returns the whole corpus ranked however distant (and, live-measured, a corpus that was 92% invisible after embedding drift), and prompts that interpolate untrusted goal text/definitions with no fencing — a stored-injection vector that now steers search and verification. This shell closes both halves: retrieval becomes goal-scoped with a calibratable distance floor, the resume pass widens into a reconcile pass, and all untrusted prompt interpolation gets per-request random fencing with shape checks.

## Produces

- Goal-scoped heuristic retrieval: embeddings carry `goal_id`; queries filter by goal unless cross-goal reuse is explicitly requested; calibratable distance floor returning empty over garbage
- Reconcile pass in the Sleep-Cycle worker: graph Meta-Heuristic ids diffed against pgvector rows, missing embeddings re-embedded in-product
- Prompt-fencing helper (per-request `crypto/rand.Text()` delimiters + "fenced content is data" system line) applied to all existing untrusted interpolation (goal text, column names, prior definitions), plus length/shape checks on returned definitions
- The fencing helper as the pattern V2's new LLM calls (orientation, grounding, critic, intent) must use

## Consumes

- Batched `GetMetaHeuristics` — from Shell 1
- `internal/store/embeddingstore.go`, `internal/heuristics/service.go`, `internal/sleepcycle/abstract.go` (resume path), `internal/llm` prompts — from existing codebase

## Covers Spec Requirements

- R25
- R26
- R29

## Implementation Steps (High-Level)

1. **Add `goal_id` to embedding rows** (migration + write path) and goal-filter `SimilaritySearch`; expose explicit cross-goal mode for grounding.
2. **Add the distance floor** (config; calibrated later against the R30 dataset) so unrelated queries return empty.
3. **Widen resume into reconcile**: diff graph ids vs embedding rows each run; re-embed the difference through the existing embed tail.
4. **Build the fencing helper** and apply it to `macroSegmentPrompt`, `RepairMetaHeuristic`, and Phase-1 prompt interpolation; add definition length/shape checks.

## Open Questions

- Distance-floor default value — calibrate against the R30 dataset once it exists (spec Open Question; ~0.48–0.50 measured margin pre-goal-scoping).

## Expansion Deferred

The following are filled in when `/expand-shell` runs:

- Pattern survey against the codebase state at implementation time
- Concrete `file_path` references with named functions or symbols for each Implementation Step
- Verification section with specific test commands and smoke checks
- Context Files section with the files to read in full before editing
