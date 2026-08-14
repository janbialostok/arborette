-- Roles are dropped by db-bootstrap/IaC teardown, never by a migration; this only
-- revokes the grants 0017 added, then reverses what it created. sessions drops
-- before users (its FK-to-parent cascade order).
REVOKE SELECT, INSERT, DELETE ON sessions FROM arborette_orchestrator;
REVOKE SELECT, INSERT, UPDATE, DELETE ON users FROM arborette_orchestrator;

DROP TABLE IF EXISTS sessions;
DROP TABLE IF EXISTS users;