-- Per-run status rows for the Active Hypothesis Loop: one row per loop trigger.
-- Written only by the Orchestrator role (mirrors the audit-writer ownership
-- pattern); both runtime roles may read. Grants are co-located here rather than
-- in 0005 (which runs before this table exists); no CREATE ROLE lives here --
-- roles are cluster-global, provisioned by db-bootstrap/IaC before migrations
-- run (see 0005_grants).
CREATE TABLE runs (
    run_id uuid PRIMARY KEY,
    optimization_function_id uuid NOT NULL REFERENCES goal_registry(optimization_function_id),
    status text NOT NULL,
    started_at timestamptz NOT NULL DEFAULT now(),
    ended_at timestamptz,
    failure_reason text
);

CREATE INDEX runs_goal_started_idx ON runs (optimization_function_id, started_at DESC);

GRANT SELECT, INSERT, UPDATE ON runs TO arborette_orchestrator;
GRANT SELECT ON runs TO arborette_service;
