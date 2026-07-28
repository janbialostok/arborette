-- Roles are dropped by db-bootstrap/IaC teardown, never by a migration; this
-- only revokes the grants 0008 added, leaving 0005's matrix intact.
REVOKE DELETE ON meta_heuristic_embeddings FROM arborette_service;
REVOKE DELETE ON meta_heuristic_embeddings FROM arborette_orchestrator;
