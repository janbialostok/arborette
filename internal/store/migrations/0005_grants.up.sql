-- Least-privilege matrix enforcing the audit boundary. Runs after every table
-- exists, granting against the two runtime LOGIN roles that db-bootstrap/IaC
-- created before migrations ran. No CREATE ROLE here (see the bootstrap
-- invariant): roles are cluster-global while migration history is per-database.

GRANT USAGE ON SCHEMA public TO arborette_orchestrator;
GRANT USAGE ON SCHEMA public TO arborette_service;

-- Orchestrator: owns the goal registry; append-only audit writer (identity PK
-- means table-level INSERT is sufficient -- no sequence grant); reads
-- embeddings for its heuristics similarity query.
GRANT SELECT, INSERT, UPDATE ON goal_registry TO arborette_orchestrator;
GRANT INSERT ON audit_log TO arborette_orchestrator;
GRANT SELECT ON meta_heuristic_embeddings TO arborette_orchestrator;

-- Service role: Sleep-Cycle upsert + MCP read of embeddings; worker goal-context
-- reads. No privilege of any kind on audit_log.
GRANT SELECT, INSERT, UPDATE ON meta_heuristic_embeddings TO arborette_service;
GRANT SELECT ON goal_registry TO arborette_service;
