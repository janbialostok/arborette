-- Document goals have no Evaluation Matrix to optimize -- their objective is a
-- set of fields to extract accurately -- so relax evaluation_matrix to nullable
-- and add target_fields for the document intake path. A goal populates exactly
-- one: a tabular goal keeps evaluation_matrix (target_fields NULL), a document
-- goal keeps target_fields (evaluation_matrix NULL). No new grants: the
-- table-level GRANTs in 0005 cover new columns.
ALTER TABLE goal_registry ALTER COLUMN evaluation_matrix DROP NOT NULL;
ALTER TABLE goal_registry ADD COLUMN target_fields jsonb;
