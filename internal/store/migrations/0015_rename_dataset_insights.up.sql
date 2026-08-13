-- Renames tables for the dataset-insights terminology.
-- Column renames are NOT applied here: the application code reads the
-- existing wire column names (optimization_function_id, goal_id) directly
-- and the store layer already uses the new Go struct field names.
-- Only tables that carry the old naming are renamed.

ALTER TABLE goal_registry RENAME TO dataset_registry;

ALTER TABLE meta_heuristic_embeddings RENAME TO insight_embeddings;
