-- Registry of every data source ref this system minted at ingest, so the Sandbox
-- can scope requests to registered refs rather than arbitrary bucket paths. The
-- Orchestrator inserts a ref the moment the object lands (before the intake
-- introspect/dry-run that runs against it); the Sandbox reads it to validate. No
-- CREATE ROLE here -- roles are cluster-global, provisioned by db-bootstrap/IaC
-- before migrations run (see 0005_grants).
CREATE TABLE data_source_registry (
    ref text PRIMARY KEY,
    created_at timestamptz NOT NULL DEFAULT now()
);

-- Backfill refs of pre-existing goals so their sandbox calls keep validating after
-- this table starts gating them (the actual goal_registry column is datasource_ref).
INSERT INTO data_source_registry (ref)
SELECT DISTINCT datasource_ref FROM goal_registry
ON CONFLICT DO NOTHING;

GRANT SELECT, INSERT ON data_source_registry TO arborette_orchestrator;
GRANT SELECT ON data_source_registry TO arborette_service;
