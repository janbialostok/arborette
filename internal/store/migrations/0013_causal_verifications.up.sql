-- Causal-verification records: one row per (goal, finding intervention, causal-graph
-- version) verification attempt, carrying the leased lifecycle the Verifier's crash
-- reaping and budget/cap accounting rely on. Deliberately distinct from the V1 HITL
-- verification_queue: success here is 'causally_verified', not the HITL 'confirmed'
-- Outcome status. Written by the Verifier as arborette_service (mirrors the sleep-cycle
-- upsert grants); no CREATE ROLE here -- roles are cluster-global, provisioned by
-- db-bootstrap/IaC before migrations run (see 0005_grants).
--
-- intervention_id references a Neo4j Intervention node's application-assigned UUID, so
-- it carries no foreign key. The UNIQUE (goal, intervention, graph version) constraint
-- is load-bearing beyond deduplication: it is the coalesce key that makes a duplicate
-- dispatch return the live/completed record rather than spawning a second run.
CREATE TABLE causal_verifications (
    id uuid PRIMARY KEY,
    goal_id uuid NOT NULL REFERENCES goal_registry(optimization_function_id),
    intervention_id uuid NOT NULL,
    graph_version integer NOT NULL,
    status text NOT NULL DEFAULT 'pending',
    naive_effect double precision,
    adjusted_effect double precision,
    adjustment_set jsonb,
    refutation_score double precision,
    confidence double precision,
    budgeted boolean NOT NULL DEFAULT false,
    stale boolean NOT NULL DEFAULT false,
    lease_deadline timestamptz,
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now(),
    UNIQUE (goal_id, intervention_id, graph_version)
);

CREATE INDEX causal_verifications_goal_status_idx
    ON causal_verifications (goal_id, status, created_at DESC);

GRANT SELECT, INSERT, UPDATE ON causal_verifications TO arborette_service;

-- Per-goal verification budget (config-defaulted; NULL means "use the service
-- default"). The orchestrator sets it at registration; the Verifier enforces it by
-- row-counting, never a column decrement, so no goal_registry UPDATE grant is needed --
-- arborette_service's existing SELECT on goal_registry is sufficient, and the
-- table-level GRANTs in 0005 cover the new column for the orchestrator writer.
ALTER TABLE goal_registry ADD COLUMN verification_budget integer;
