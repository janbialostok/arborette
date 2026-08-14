-- Roles are dropped by db-bootstrap/IaC teardown, never by a migration; this only
-- revokes the grants 0018 added, then reverses what it created.
REVOKE UPDATE (created_by) ON goal_registry FROM arborette_orchestrator;
REVOKE UPDATE (owner_id) ON datasets FROM arborette_orchestrator;
REVOKE SELECT, INSERT, DELETE ON dataset_shares FROM arborette_orchestrator;

ALTER TABLE goal_registry
    DROP COLUMN IF EXISTS created_by;

ALTER TABLE datasets
    DROP COLUMN IF EXISTS owner_id;

DROP TABLE IF EXISTS dataset_shares;
