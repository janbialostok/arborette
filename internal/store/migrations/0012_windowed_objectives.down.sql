-- Reverses 0012, dropping the window-objective bindings.
ALTER TABLE goal_registry DROP COLUMN IF EXISTS entity_key_column;
ALTER TABLE goal_registry DROP COLUMN IF EXISTS time_column;
