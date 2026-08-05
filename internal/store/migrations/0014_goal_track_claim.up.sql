-- Goal intent: the routing track the intake classifier assigned, and the claim it
-- extracted on the verify track.
--
-- track defaults to 'explore' so a goal registered before this column existed (or by
-- a path that never runs classification) routes as it always did. claim holds the
-- canonical JSON of the validated claim and is NULL on the explore track and on a
-- verify-track goal whose claim could not be constructed. claim_error carries that
-- cannot-construct reason: a claim that fails schema validation has no run to fail,
-- so the goal row is the only surface that can report why -- and reporting the
-- validation reason is what keeps it distinct from "the claim was tested and not
-- supported", which it never was.
--
-- No new grants for these columns: the table-level GRANTs in 0005 cover them.
ALTER TABLE goal_registry
    ADD COLUMN track text NOT NULL DEFAULT 'explore',
    ADD COLUMN claim jsonb,
    ADD COLUMN claim_error text;

-- The orchestrator now reads causal_verifications (the analyst-facing listing) and
-- flags them stale when an analyst corrects the causal graph. 0013 granted the table
-- to arborette_service alone, because only the Verifier touched it then.
--
-- The write grant is column-scoped to what marking stale touches. No INSERT, and no
-- UPDATE on the accounting columns: record creation and the charge-at-accept status,
-- budget, and lease columns stay the Verifier's, so a single writer owns them --
-- table-wide UPDATE would let this role flip a record to failed, which is exactly the
-- state the budget and in-flight counts exclude.
GRANT SELECT ON causal_verifications TO arborette_orchestrator;
GRANT UPDATE (stale, updated_at) ON causal_verifications TO arborette_orchestrator;
