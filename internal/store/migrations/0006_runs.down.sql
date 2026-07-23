-- Roles are dropped by db-bootstrap/IaC teardown, never by a migration; this
-- only revokes the grants 0006 added, then drops the table (its index drops
-- with it).
REVOKE SELECT ON runs FROM arborette_service;
REVOKE SELECT, INSERT, UPDATE ON runs FROM arborette_orchestrator;

DROP TABLE IF EXISTS runs;
