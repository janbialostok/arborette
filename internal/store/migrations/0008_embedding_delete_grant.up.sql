-- The graph and pgvector are written separately and only the graph can be reset
-- independently, which leaves embeddings that still match a query and then
-- resolve to no node. The read path is where that drift is observed, so it is
-- the only place that can repair it -- both heuristics query seams (Orchestrator
-- REST, MCP Server) therefore need DELETE, widening 0005's read-only stance for
-- arborette_orchestrator on this table specifically. DELETE only: no TRUNCATE.
GRANT DELETE ON meta_heuristic_embeddings TO arborette_orchestrator;
GRANT DELETE ON meta_heuristic_embeddings TO arborette_service;
