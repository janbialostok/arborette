-- goal_id scopes an embedding row to the optimization function whose abstraction
-- run wrote it, so retrieval can answer a goal-scoped query without dragging in
-- every other goal's corpus. Nullable on purpose: goal provenance is
-- unrecoverable for rows written before this column existed (the graph's
-- derivedID namespaces are one-way hashes), so legacy rows stay NULL and are
-- matched only by cross-goal queries until the reconcile pass heals them. The
-- type is uuid to match goal_registry.optimization_function_id and node_id. No
-- new grants: 0005/0008 already cover this table for both runtime roles.
ALTER TABLE meta_heuristic_embeddings ADD COLUMN goal_id uuid;

CREATE INDEX meta_heuristic_embeddings_goal_id ON meta_heuristic_embeddings (goal_id);
