-- Roles are dropped by db-bootstrap/IaC teardown, never by a migration; this only
-- revokes the grants 0015 added, then reverses what it created.
REVOKE DELETE ON data_source_registry FROM arborette_orchestrator;
REVOKE DELETE ON causal_verifications FROM arborette_orchestrator;
REVOKE DELETE ON verification_queue FROM arborette_orchestrator;
REVOKE DELETE ON runs FROM arborette_orchestrator;
REVOKE DELETE ON goal_registry FROM arborette_orchestrator;
REVOKE SELECT ON datasets FROM arborette_service;
REVOKE SELECT, INSERT, UPDATE, DELETE ON datasets FROM arborette_orchestrator;

ALTER TABLE goal_registry
    ALTER COLUMN dataset_id DROP NOT NULL,
    DROP COLUMN IF EXISTS dataset_id;

DROP TABLE IF EXISTS datasets;