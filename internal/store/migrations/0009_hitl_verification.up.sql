-- Human-in-the-loop verification queue: one row per extract-type outcome routed
-- for analyst review, plus the resolution an analyst submitted. Written only by
-- the Orchestrator role (mirrors runs/audit ownership); both runtime roles may
-- read. Grants are co-located here rather than in 0005 (which runs before this
-- table exists); no CREATE ROLE lives here -- roles are cluster-global,
-- provisioned by db-bootstrap/IaC before migrations run (see 0005_grants).
--
-- outcome_id references a Neo4j Outcome node's application-assigned UUID, so it
-- carries no foreign key. Its UNIQUE constraint is load-bearing beyond
-- deduplication: it is the ON CONFLICT target that makes the on-demand
-- resolution insert a race-safe claim rather than a unique-violation 500.
CREATE TABLE verification_queue (
    queue_id uuid PRIMARY KEY,
    optimization_function_id uuid NOT NULL REFERENCES goal_registry(optimization_function_id),
    outcome_id uuid NOT NULL UNIQUE,
    field text NOT NULL,
    extracted_value text NOT NULL,
    provenance jsonb,
    confidence double precision NOT NULL,
    status text NOT NULL DEFAULT 'pending',
    resolution text,
    corrected_value text,
    created_at timestamptz NOT NULL DEFAULT now(),
    resolved_at timestamptz
);

CREATE INDEX verification_queue_goal_status_idx
    ON verification_queue (optimization_function_id, status, created_at DESC);

GRANT SELECT, INSERT, UPDATE ON verification_queue TO arborette_orchestrator;
GRANT SELECT ON verification_queue TO arborette_service;

-- Per-goal HITL settings. A NULL confidence_threshold means "use the service
-- default"; epoch_mode defaults to speculative so an existing goal keeps the
-- non-blocking loop behavior. No new grants: the table-level GRANTs in 0005
-- cover new columns.
ALTER TABLE goal_registry ADD COLUMN confidence_threshold double precision;
ALTER TABLE goal_registry ADD COLUMN epoch_mode text NOT NULL DEFAULT 'speculative';
