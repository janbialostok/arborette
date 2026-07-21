-- Append-only audit trail. Uses GENERATED ALWAYS AS IDENTITY (not bigserial) so
-- the insert-only runtime role needs only table-level INSERT, avoiding a
-- separate sequence USAGE grant. No CREATE ROLE lives here: roles are
-- provisioned by db-bootstrap/IaC before migrations run (see 0005_grants).
CREATE TABLE audit_log (
    id bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    actor text NOT NULL,
    action text NOT NULL,
    event_type text NOT NULL,
    detail jsonb,
    created_at timestamptz NOT NULL DEFAULT now()
);
