-- Windowed/entity-relative objectives bind an entity-key column and a time/order
-- column at goal registration; the sandbox compiles them into PARTITION BY entity
-- ORDER BY ts window SQL. Both are nullable: a plain aggregate goal binds neither,
-- and a goal registered before this column existed stays NULL. No new grants: the
-- table-level GRANTs in 0005 cover new columns.
ALTER TABLE goal_registry ADD COLUMN entity_key_column text, ADD COLUMN time_column text;
