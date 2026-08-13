-- First-class datasets: the user-managed container an objective's data source
-- lives in, giving analysts the full create/read/update/delete lifecycle the goal
-- submission side-effect never had. data_source_registry stays the raw ref ledger
-- the sandbox validator reads; a dataset handles lifecycle state (name,
-- description, status) on top of one ref.
--
-- name is unique case-insensitively ("Sales" and "sales" conflict), enforced by a
-- function index on lower(name) rather than a plain UNIQUE constraint.
--
-- goal_registry gains dataset_id as the objective -> dataset parent link. It is
-- backfilled before NOT NULL is applied: one dataset per distinct legacy
-- datasource_ref is created so every pre-existing goal ends up a child of a
-- dataset (no orphans), then the column is locked NOT NULL so new goals cannot be
-- written without a parent.
CREATE TABLE datasets (
    id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    name text NOT NULL,
    description text NOT NULL DEFAULT '',
    status text NOT NULL DEFAULT 'active' CHECK (status IN ('active', 'archived')),
    datasource_ref text NOT NULL,
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now()
);

CREATE UNIQUE INDEX datasets_name_lower_idx ON datasets (lower(name));

ALTER TABLE goal_registry
    ADD COLUMN dataset_id uuid REFERENCES datasets(id);

-- Backfill: one dataset per distinct ref among pre-existing goals, replacing the
-- extra-long path segments with the ref's base filename (or a fallback when the
-- ref has no filename-like tail) so the auto-created name stays recognizable.
INSERT INTO datasets (name, datasource_ref)
SELECT
    lower((regexp_replace(split_part(ref, '/', array_length(string_to_array(ref, '/'), 1)),
           '[^a-zA-Z0-9._-]', '_', 'g'))) || '-' || substring(md5(ref) for 6),
    ref
FROM data_source_registry;

UPDATE goal_registry g
SET dataset_id = d.id
FROM datasets d
WHERE g.datasource_ref = d.datasource_ref;

ALTER TABLE goal_registry
    ALTER COLUMN dataset_id SET NOT NULL;

-- Deletion grants: datasets are user-managed (any lifecycle op), and the
-- objective/dataset delete paths remove rows from the tables that carry a NO
-- ACTION FK to the goal (runs, verification_queue, causal_verifications) plus the
-- ref row itself, so each delete ships its own DELETE grant rather than widening
-- the base INSERT/UPDATE grants. DELETE only -- no TRUNCATE anywhere.
GRANT SELECT, INSERT, UPDATE, DELETE ON datasets TO arborette_orchestrator;
GRANT SELECT ON datasets TO arborette_service;
GRANT DELETE ON goal_registry TO arborette_orchestrator;
GRANT DELETE ON runs TO arborette_orchestrator;
GRANT DELETE ON verification_queue TO arborette_orchestrator;
GRANT DELETE ON causal_verifications TO arborette_orchestrator;
GRANT DELETE ON data_source_registry TO arborette_orchestrator;