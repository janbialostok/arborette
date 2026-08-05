-- Reverses 0014, dropping the goal intent track and the extracted claim, and
-- returning causal_verifications to the Verifier-only grants 0013 established.
REVOKE UPDATE (stale, updated_at) ON causal_verifications FROM arborette_orchestrator;
REVOKE SELECT ON causal_verifications FROM arborette_orchestrator;

ALTER TABLE goal_registry DROP COLUMN IF EXISTS track;
ALTER TABLE goal_registry DROP COLUMN IF EXISTS claim;
ALTER TABLE goal_registry DROP COLUMN IF EXISTS claim_error;
