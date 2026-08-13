# Migration Map: Old Names → New Names

This document maps the old terminology to the new terminology for the 
dataset-insights refactoring (branch 001-dataset-insights).

## Entity Renames

| Old Name | New Name | Rationale |
|----------|----------|-----------|
| Goal / Optimization Function | Dataset | Top-level entity: the data being analyzed |
| Objective | Question | Users ask questions of datasets |
| Heuristic / MetaHeuristic | Insight | Questions generate insights |
| optimization_function_id | dataset_id | Consistent foreign key naming |
| meta_heuristic_embeddings | insight_embeddings | Embedding table rename |

## Go Package Renames

| Old Path | New Path |
|----------|----------|
| internal/objective/* | internal/question/* |
| internal/heuristics/* | internal/insight/* |
| internal/domain (Objective types) | internal/question/models |
| internal/domain (MetaHeuristic types) | internal/insight/models |
| internal/orchestrator/heuristics.go | internal/orchestrator/insights.go |

## API Route Changes (Web UI)

| Old Route | New Route |
|-----------|-----------|
| /goals | /datasets |
| /heuristics | /insights |
| /api/orchestrator/heuristics/search | /api/orchestrator/insights/search |
| /api/orchestrator/heuristics/[id]/trace | /api/orchestrator/insights/[id]/trace |

## Graph Labels (Neo4j)

| Old Label | New Label |
|-----------|-----------|
| Goal | Dataset |
| MetaHeuristic | Insight |

## Function/Method Renames

| Old Name | New Name |
|----------|----------|
| GetMetaHeuristic | GetInsight |
| CreateMetaHeuristic | CreateInsight |
| ListMetaHeuristics | ListInsights |
| MarkStaleMetaHeuristics | MarkStaleInsights |
| searchHeuristics | searchInsights |
| traceHeuristic | traceInsight |
| get_optimized_heuristics | get_insights |

## MCP Tool Renames

| Old Tool | New Tool |
|----------|----------|
| get_optimized_heuristics | get_insights |
| trace_causal_chain (was Trace) | trace_causal_chain (unchanged) |

## User-Facing Terminology

| Old Term (UI/Docs) | New Term (UI/Docs) |
|-------------------|-------------------|
| Objective | Question |
| Goal | Dataset |
| Heuristic | Insight |
| Meta-Heuristic | Insight |
| Optimization function | Dataset |
| Registered optimization objective | Question about a dataset |
| Browse learned heuristics | Browse generated insights |
