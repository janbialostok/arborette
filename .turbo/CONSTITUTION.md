# Arborette Constitution

## Core Principles

### I. Non-Hallucination: Every Measurement Is Empirical

The system must never invent, estimate, or fabricate a data measurement. Every finding's
effect — whether Phase 1 observational, Phase 2 macro-segment, or Engine B adjusted — is
computed by executing a deterministic, read-only query through the Sandbox Execution
service against the wired dataset. A finding carries exactly the value the data produced.
Confidence scores, model self-reports, and LLM abstractions are recorded with their
provenance and are never treated as measurements. This is the non-negotiable contract:
Arborette measures data; it does not imagine it.

### II. Epistemic Honesty: Every Edge Declares Its Provenance

Every `PRODUCED` edge carries an `epistemic_source` property (`observational`,
`causal_inferred`, or reserved `interventional`). Correlation is never presented as
causation, and causal inference is never presented as proof — it is labeled with its
assumptions (discovered model, no-latent-confounder). Every read path, every UI surface,
and every MCP response distinguishes these sources. When an edge carries `causal_inferred`,
it is rendered with the caveat "causally supported (given the discovered model)", never as
"proven."

### III. Boundary Integrity: One Service, One Concern, One Credential

Service boundaries are load-bearing: the Sandbox is the only CGO package (it alone
touches DuckDB), the Orchestrator is the only audit-table writer (it alone holds
audit credentials), the MCP Server is read-only (it writes nothing), and the graph
repository is the only Neo4j-touching package. A credential, a CGO dependency, and the
ability to mutate a datastore each live in exactly one service. Crossing a boundary
requires a typed client, a shared vocabulary of `domain.Prop*` constants, and an explicit
authorization token.

### IV. Idempotent Writes, Crash-Safe Progress

Every write that can be replayed after a crash uses a deterministic, content-derived id
(canonicalized filter → `domain.DerivedID`; intervention id + graph version → causal
outcome id) and writes through repo MERGE-on-id semantics. The Sleep Cycle persists
progress by `embedding_pending` state; the Verifier leases and heartbeats records.
A restart resumes, never re-creates. Work that can be split across sessions (graph writes,
multi-step corrections) is designed so re-runs converge to the same state, not duplicate
data.

### V. Typed Contracts, Not Convention

Enums that cross service boundaries are defined as typed string constants in `domain`
(`EpistemicSource`, `VerificationStatus`, `InterventionType`, `EdgeDirection`). Node
property keys that cross boundaries are `domain.Prop*` constants. A spelling divergence
is a compile error, not a silent read miss. The same rule applies across languages: the
web frontend's typed orchestrator client mirrors the Go-side wire contracts exactly, and
client types in `web/lib/` are the single source of truth for the frontend's view of the
API shape.

### VI. Test at the Boundary, Not Through the Stack

Interfaces live at the consumer — every handler, every search policy, every launcher
defines its own narrow interface listing only the methods it uses. Test doubles implement
that interface, not a superset. A double that stands in for a store respects context
cancellation (returns `ctx.Err()` when done). Integration tests are gated on
`ARBORETTE_INTEGRATION` and share a database — never assert table-global counts, never
depend on a clean slate. Pure logic lives in dependency-free packages (`domain`) or
`lib/` modules (frontend) and is tested with no infrastructure.

### VII. Observability as Infrastructure

The audit log is append-only, immutable, and keyed on a cross-service vocabulary
(`optimization_function_id`, `data_source_ref`). Every service writes through the
orchestrator's internal audit API. Driver events (intervention, outcome, heuristic
injection, verification dispatch, branch failure, HITL resolution) are recorded.
The audit log is a product surface (dashboard queries, traceability), not a debug
artifact — its detail-map keys are a codebase-wide contract.

### VIII. Additive Evolution, Not Rewrite

The graph and the V1→V2 layer are designed for additivity. V1's observational edges
are never modified; V2's causal edges are new `PRODUCED` edges on the same Intervention
nodes, not replacements. The `epistemic_source` discriminator is additive — a missing
value means `observational`, so a V2 upgrade does not break V1 read paths. Causal edges
have their own lifecycle (supersession) that never touches observational edges. All
consumers that collect findings filter on `observational` unless they explicitly want
causal edges. The `SearchPolicy` interface and `JobLauncher` interface isolate V2
changes behind a seam.

### IX. Minimal Dependencies, Pinned Versions

Every dependency must justify its presence. The Go toolchain is pinned to 1.24.
Dependency versions are chosen for compatibility with the pinned toolchain, not for
latest features. The web UI is a separate deployable with its own lockfile and Node
version. A dependency's transitive pulls are actively managed — required versions are
explicitly declared to prevent silent downgrades, and the rationale for each pin is
documented in AGENTS.md.

### X. Configuration Is Environment-Driven, Not Hardcoded

Every tunable is a config field read from the environment with a documented default.
Knobs are grouped in nested config structs by service ownership. Compose injects
infrastructure hostnames; `.env` holds credentials. The `.env.example` is the
documented source of truth; every field carries a comment explaining its purpose.
A config field whose absence must be distinguishable from its zero value uses a
type-aware sentinel, never implicit behavior.

## Development Workflow

### Source of Truth

The spec files in `.turbo/specs/` are the product-level source of truth — every
requirement carries an R-id, every one is testable, and the spec defines what ships,
not how. Implementation plans in `.turbo/plans/` define the how. When an implementation
departs from the plan, the plan is corrected in the same commit.

### Shells and Plans

A planned change produces an implementation shell in `.turbo/shells/`. A commit that
implements the shell carries its plan at `status: done` and deletes the consumed shell.
The plan file is the durable record of what shipped.

### The Improvements Backlog

`.turbo/improvements.md` is the backlog for deliberately deferred work. Entries state
the problem, the mechanism, and the fix — not the session that found them. An entry is
removed once it ships. An entry that ships in part keeps its unshipped remainder. Every
entry carries a type (`direct` for a straightforward fix, `plan` for work that needs
design, `investigate` for open questions) and a `Noted` date.

### Code Review Standards

A review verifies that:
- No measurement is fabricated (Principle I)
- Every edge carries its epistemic source (Principle II)
- New service boundaries don't leak credentials or CGO (Principle III)
- Writes are idempotent and crash-safe (Principle IV)
- New cross-boundary constants are in `domain` (Principle V)
- Tests exercise the boundary, not the stack, and respect integration gating (Principle VI)
- Audit records use the shared vocabulary (Principle VII)
- Changes to existing edges are additive (Principle VIII)
- New dependencies are justified and version-pinned (Principle IX)
- New knobs are environment-driven with documented defaults (Principle X)

### Testing Gate

Before merging, all of the following must pass:
- `go build ./...` (zero errors)
- `go vet ./...` (zero diagnostics)
- `go test ./...` (zero failures without infrastructure)
- `make test` (integration suite, with infrastructure — zero failures)
- `npm test` from `web/` (frontend unit suite — zero failures)

### Commit Convention

Commits carry a description of what changed and why. A commit that implements a shell
links the shell id. A commit that fixes a problem documented in `improvements.md`
removes the entry. A commit that defers work adds an entry to `improvements.md`.

## Governance

This constitution defines the non-negotiable principles that govern all contributions
to Arborette. When a principle conflicts with a spec requirement, the constitution
wins — spec requirements are the product's goals, not its constraints; a spec that
contradicts a principle is corrected. When a principle conflicts with implementation
pragmatism, the conflict is documented in the plan and the resolution is recorded in
the plan's trade-offs section.

Amendments to this constitution require a documented rationale and must not contradict
the spec's product vision. The constitution is versioned alongside the code it governs.
