-- Roles are dropped by db-bootstrap/IaC teardown, never by a migration; this
-- only revokes the grants 0005 added.
REVOKE SELECT ON goal_registry FROM arborette_service;
REVOKE SELECT, INSERT, UPDATE ON meta_heuristic_embeddings FROM arborette_service;

REVOKE SELECT ON meta_heuristic_embeddings FROM arborette_orchestrator;
REVOKE INSERT ON audit_log FROM arborette_orchestrator;
REVOKE SELECT, INSERT, UPDATE ON goal_registry FROM arborette_orchestrator;

REVOKE USAGE ON SCHEMA public FROM arborette_service;
REVOKE USAGE ON SCHEMA public FROM arborette_orchestrator;
