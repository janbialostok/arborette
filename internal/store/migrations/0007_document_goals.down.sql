-- Reverses 0007. Restoring the NOT NULL constraint requires no document goals to
-- exist (their evaluation_matrix is NULL); the down path assumes the up path is
-- being unwound before any document goal was registered.
ALTER TABLE goal_registry DROP COLUMN target_fields;
ALTER TABLE goal_registry ALTER COLUMN evaluation_matrix SET NOT NULL;
