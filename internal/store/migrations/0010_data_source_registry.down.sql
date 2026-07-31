-- Roles are dropped by db-bootstrap/IaC teardown, never by a migration; this only
-- revokes the grants 0010 added, then drops what it created.
REVOKE SELECT ON data_source_registry FROM arborette_service;
REVOKE SELECT, INSERT ON data_source_registry FROM arborette_orchestrator;

DROP TABLE IF EXISTS data_source_registry;
