# Improvements

Out-of-scope improvement opportunities captured during work sessions. Review periodically and pull items into active work when appropriate.

### Batch the Meta-Heuristic graph fetch in heuristics.Query to avoid an N+1

- **Type**: plan
- **Category**: performance
- **Where**: `internal/heuristics/service.go` (Query), interface in `internal/graph/repository.go`
- **Why**: Query calls `repo.GetMetaHeuristic(id)` once per similarity hit, each opening a separate Neo4j session/read tx — k round-trips on the `get_optimized_heuristics` read hot path. Add a batched `GetMetaHeuristics(ctx, ids)` (`MATCH (m:MetaHeuristic) WHERE m.id IN $ids`) and use it in Query. Deferred: no read handlers consume this seam yet and k is small; batch when the MCP/Orchestrator handler shells land.
- **Noted**: 2026-07-21
