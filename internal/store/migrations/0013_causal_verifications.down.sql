-- Roles are dropped by db-bootstrap/IaC teardown, never by a migration; this only
-- revokes the grants 0013 added, then drops what it created.
REVOKE SELECT, INSERT, UPDATE ON causal_verifications FROM arborette_service;

ALTER TABLE goal_registry DROP COLUMN IF EXISTS verification_budget;

DROP TABLE IF EXISTS causal_verifications;
