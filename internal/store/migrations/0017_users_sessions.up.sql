-- Users (the person's identity on the console) and sessions (the signed-in
-- cookie ledger). username is unique case-insensitively, enforced by a function
-- index on lower(username) -- the datasets.name precedent (0015) -- so "Sales"
-- and "sales" cannot both register. Sessions key the opaque cookie by a
-- SHA-256 of its value, so the raw token is never stored (the auth.go digest
-- rule); the middleware re-reads users.active every request, which is what
-- makes deactivation effective without deleting rows.
--
-- roles is a single extensible value: a later "groups" feature adds values
-- here, not a new table. sessions has no expires_at: a session lasts until
-- sign-out (row delete, ON DELETE CASCADE) or deauthorization (middleware
-- active re-check).
CREATE TABLE users (
    id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    username text NOT NULL,
    password_hash text NOT NULL,
    role text NOT NULL CHECK (role IN ('admin', 'member')),
    active boolean NOT NULL DEFAULT true,
    admin_notice_acknowledged boolean NOT NULL DEFAULT false,
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now()
);

CREATE UNIQUE INDEX users_username_lower_idx ON users (lower(username));

CREATE TABLE sessions (
    id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id uuid NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    token_hash text NOT NULL UNIQUE,
    created_at timestamptz NOT NULL DEFAULT now()
);

-- Orchestrator owns accounts and sessions end to end. No service-role grants:
-- Sleep-Cycle/Verifier reach audit only and never touch a user row (deliberate,
-- see 0005_grants). DELETE on users is not exposed by any v1 handler (deactivate
-- ships instead), but the grant stays symmetric with the sessions CASCADE so an
-- operator-level cleanup can run.
GRANT SELECT, INSERT, UPDATE, DELETE ON users TO arborette_orchestrator;
GRANT SELECT, INSERT, DELETE ON sessions TO arborette_orchestrator;