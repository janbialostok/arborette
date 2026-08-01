DROP INDEX IF EXISTS meta_heuristic_embeddings_goal_id;

ALTER TABLE meta_heuristic_embeddings DROP COLUMN IF EXISTS goal_id;
