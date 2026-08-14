-- Dataset ownership & sharing: the dataset becomes the ownership root. Every
-- dataset records the signed-in user who created it (datasets.owner_id); every
-- goal records who registered it (goal_registry.created_by); and dataset_shares
-- is the explicit per-account grant that lets a non-owner see a dataset and all
-- its children. The access predicate the whole feature enforces is:
--   accessible(d, u) := d.owner_id = u.id
--                   OR EXISTS (SELECT 1 FROM dataset_shares sh
--                              WHERE sh.dataset_id = d.id AND sh.user_id = u.id)
--
-- owner_id / created_by are NULLable: pre-existing rows (created before
-- ownership existed) and rows written by background processes have no recorded
-- signed-in creator. NULL-owner datasets are visible to nobody until they are
-- attributed to the admin caretaker (the earliest active admin, backfilled here
-- and self-healed at runtime); legacy goals (created_by NULL) are visible to
-- whoever can access their dataset and deletable by the dataset owner.
CREATE TABLE dataset_shares (
    dataset_id uuid NOT NULL REFERENCES datasets(id) ON DELETE CASCADE,
    user_id    uuid NOT NULL REFERENCES users(id)    ON DELETE CASCADE,
    created_at timestamptz NOT NULL DEFAULT now(),
    created_by uuid REFERENCES users(id),
    PRIMARY KEY (dataset_id, user_id)
);

-- One grant per dataset/account (the PK), created/revoked only by the dataset
-- owner. The double ON DELETE CASCADE is the FR-012 edge: a removed account's
-- grants die with the account, and deleting a dataset removes its shares.

ALTER TABLE datasets
    ADD COLUMN owner_id uuid REFERENCES users(id);

ALTER TABLE goal_registry
    ADD COLUMN created_by uuid REFERENCES users(id);

-- Backfill: pre-existing datasets have no recorded creator, so they are
-- attributed to the admin caretaker -- the earliest active admin, by the same
-- order the runtime self-heal uses (ORDER BY created_at, id LIMIT 1). A fresh
-- database that has no admin yet leaves them NULL (visible to nobody) until the
-- runtime self-heal attributes them when the first admin registers. Legacy
-- goals deliberately keep NULL created_by: they are visible/deletable through
-- their dataset (FR-014), and a backfilled caretaker tag would wrongly let one
-- account manage another account's shared goals.
UPDATE datasets SET owner_id = (
    SELECT id FROM users
    WHERE role = 'admin' AND active
    ORDER BY created_at ASC, id ASC
    LIMIT 1
) WHERE owner_id IS NULL;

-- Deletion grants: the orchestrator deletes share rows when an owner revokes a
-- grant, and writes the ownership columns on create/update. No TRUNCATE
-- anywhere. The service role never reads a user row (0005), and needs no new
-- grants: the Sleep-Cycle grounding scopes its corpus read to the goal's own
-- dataset via goal_registry's existing SELECT (a self-join on dataset_id, plus
-- the legacy NULL-goal corpus), never through dataset_shares.
GRANT SELECT, INSERT, DELETE ON dataset_shares TO arborette_orchestrator;
GRANT UPDATE (owner_id) ON datasets TO arborette_orchestrator;
GRANT UPDATE (created_by) ON goal_registry TO arborette_orchestrator;
