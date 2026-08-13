# Old Name Audit

Comprehensive audit of all old-name references that need updating.
Generated for the dataset-insights refactoring.

## Goals → Datasets

### Go files (high-signal)
- `internal/store/goalregistry.go` — `GoalStatus`, goal types, goal registry
- `internal/orchestrator/server.go` — goal routes, runStore
- `internal/orchestrator/submit.go` — goal submission
- `internal/orchestrator/promote.go` — goal verification
- `internal/orchestrator/chat.go` — goal in chat
- `internal/orchestrator/submitintent_test.go` — document goal
- `internal/sleepcycle/*.go` — goal references throughout
- `internal/verifier/*.go` — goal reader, goal verification
- `cmd/orchestrator/main.go` — entry point
- `internal/mcpserver/tools.go` — goal scoping

### Web UI files
- `web/app/goals/page.tsx` — goals page
- `web/components/ObjectivesList.tsx` — objectives list on goals page
- `web/components/AppNav.tsx` — "Objectives" nav link

### Documentation
- `README.md` — extensive goal/objective references
- `AGENTS.md` — goal references
- `docs/discovery-flow.md` — goal references
- `.turbo/specs/*.md` — goal specs (note: tracked specs kept as-is)
- `.turbo/plans/*.md` — goal plans (note: tracked plans kept as-is)

## Objectives → Questions

### Go files (high-signal)
- `internal/objective/objective.go` — core Objective type
- `internal/objective/objective_test.go` — objective tests
- `internal/domain/domain.go` — PropObjectiveLabel, Objective types
- `internal/domain/score.go` — objective scores
- `internal/sandbox/compile.go` — objective compilation
- `internal/sandbox/server.go` — objective_label fields
- `internal/orchestrator/submit.go` — objective validation
- `internal/orchestrator/hypothesis_test.go` — PinObjective
- `internal/sleepcycle/search.go` — objective pinning
- `internal/sleepcycle/publish.go` — objective.Improves
- `internal/verifier/*.go` — objective verification
- `internal/llm/*.go` — objective in prompts/schema

### Web UI files
- `web/components/ObjectivesList.tsx` — main objective UI
- `web/components/EffectReadout.tsx` — objective direction
- `web/components/LiveRun.tsx` — objectives list
- `web/components/HeuristicBrowser.tsx` — objective heuristics
- `web/lib/orchestrator.ts` — objective_label type
- `web/app/goals/page.tsx` — ObjectivesPage

### Documentation
- `README.md` — objectives throughout
- `AGENTS.md` — objective references
- `context/CONCEPT.md` — objective concept
- `.turbo/specs/objective-intake-and-navigation.md` — dedicated spec
- `.turbo/plans/objective-expression-prompt-grounding.md` — dedicated plan

## Heuristics/MetaHeuristics → Insights

### Go files (high-signal)
- `internal/heuristics/service.go` — heuristic service
- `internal/mcpserver/tools.go` — get_optimized_heuristics tool
- `internal/orchestrator/heuristics.go` — heuristic handler
- `internal/graph/neo4j.go` — MetaHeuristic nodes
- `internal/domain/domain.go` — MetaHeuristic struct
- `internal/domain/canonical.go` — RoleMetaHeuristic
- `internal/sleepcycle/abstract.go` — AbstractMetaHeuristic
- `internal/sleepcycle/grounding.go` — GetMetaHeuristics
- `internal/sleepcycle/worker.go` — MetaHeuristic operations
- `internal/llm/client.go` — AbstractMetaHeuristic, RepairMetaHeuristic
- `internal/llm/schema.go` — metaHeuristicSchema
- `internal/graph/repository.go` — MetaHeuristic methods

### SQL migrations
- `0004_meta_heuristic_embeddings.up.sql` — table rename needed
- `0004_meta_heuristic_embeddings.down.sql` — table rename needed
- `0005_grants.up.sql` — grant references
- `0005_grants.down.sql` — grant references
- `0008_embedding_delete_grant.up.sql` — grant references
- `0008_embedding_delete_grant.down.sql` — grant references
- `0011_embedding_goal_scope.up.sql` — goal reference
- `0011_embedding_goal_scope.down.sql` — goal reference

### Web UI files
- `web/components/HeuristicBrowser.tsx` — main heuristic UI (component rename)
- `web/app/heuristics/page.tsx` — heuristics page
- `web/components/AppNav.tsx` — "Heuristics" nav link
- `web/components/AgentChat.tsx` — meta-heuristics ref
- `web/components/CausalVerifications.tsx` — heuristic browser ref
- `web/app/page.tsx` — "Browse the heuristics"
- `web/app/layout.tsx` — "browse learned heuristics"
- `web/app/api/orchestrator/heuristics/search/route.ts` — API route
- `web/app/api/orchestrator/heuristics/[id]/trace/route.ts` — API route
- `web/lib/orchestrator.ts` — HeuristicMatch, searchHeuristics
- `web/lib/orchestrator.test.ts` — test data
- `web/lib/sse.test.ts` — get_optimized_heuristics
- `web/lib/chatTurn.test.ts` — tool call tests

### Documentation
- `AGENTS.md` — meta_heuristic_embeddings, MetaHeuristic
- `README.md` — Meta-Heuristics, heuristics
- `.turbo/improvements.md` — extensive references
- `.turbo/specs/arborette.md` — extensive references
- `.turbo/specs/arborette-v2-dual-engine.md` — v2 spec references
- `.turbo/plans/*.md` — plan references
- `docs/discovery-flow.md` — Meta-Heuristic references
