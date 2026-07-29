-- Roles are dropped by db-bootstrap/IaC teardown, never by a migration; this
-- only revokes the grants 0009 added, then drops what it created.
REVOKE SELECT ON verification_queue FROM arborette_service;
REVOKE SELECT, INSERT, UPDATE ON verification_queue FROM arborette_orchestrator;

ALTER TABLE goal_registry DROP COLUMN IF EXISTS epoch_mode;
ALTER TABLE goal_registry DROP COLUMN IF EXISTS confidence_threshold;

DROP TABLE IF EXISTS verification_queue;
