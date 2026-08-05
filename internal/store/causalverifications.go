package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

// Causal-verification lifecycle statuses persisted in causal_verifications.status.
// pending and failed are the leased-lifecycle states this store manages; the four
// terminal outcomes are stamped by the Verifier via Complete. causally_verified is
// the sole confirming outcome — deliberately distinct from the V1 HITL confirmed
// Outcome status.
const (
	CausalStatusPending              = "pending"
	CausalStatusCausallyVerified     = "causally_verified"
	CausalStatusConfounded           = "confounded"
	CausalStatusNotIdentifiable      = "not_identifiable"
	CausalStatusUnsupportedObjective = "unsupported_objective"
	CausalStatusFailed               = "failed"
)

// ErrBudgetExhausted and ErrInflightCapReached are the typed dispatch refusals the
// caller maps to a first-class outcome rather than a fault: the per-goal budget is
// spent, or the per-goal in-flight cap is reached. Callers match them with errors.Is.
var (
	ErrBudgetExhausted    = errors.New("verification budget exhausted for goal")
	ErrInflightCapReached = errors.New("verification in-flight cap reached")
)

// CausalVerification is one causal-verification record. The effect/score/confidence
// fields are nullable — set only once the run reaches a terminal outcome — and
// AdjustmentSet is the JSON-encoded adjustment columns.
type CausalVerification struct {
	ID              string
	GoalID          string
	InterventionID  string
	GraphVersion    int
	Status          string
	NaiveEffect     *float64
	AdjustedEffect  *float64
	AdjustmentSet   []string
	RefutationScore *float64
	Confidence      *float64
	Budgeted        bool
	Stale           bool
	LeaseDeadline   *time.Time
	CreatedAt       time.Time
	UpdatedAt       time.Time
}

// CausalVerifications is the causal_verifications access package, backed by a runtime
// pool. leaseTTL sets a fresh lease's deadline; inflightCap bounds concurrent pending
// records per goal; defaultBudget is the per-goal budget used when goal_registry
// carries no override.
type CausalVerifications struct {
	pool          *Pool
	leaseTTL      time.Duration
	inflightCap   int
	defaultBudget int
}

// NewCausalVerifications wires the store to a pool and the accounting knobs.
func NewCausalVerifications(pool *Pool, leaseTTL time.Duration, inflightCap, defaultBudget int) *CausalVerifications {
	return &CausalVerifications{pool: pool, leaseTTL: leaseTTL, inflightCap: inflightCap, defaultBudget: defaultBudget}
}

// causalColumns is the read projection every scan below expects, kept in one place so
// column order and scanVerification cannot drift apart.
const causalColumns = "id, goal_id, intervention_id, graph_version, status, naive_effect, adjusted_effect, " +
	"adjustment_set, refutation_score, confidence, budgeted, stale, lease_deadline, created_at, updated_at"

// DispatchAccept is the transactional charge-at-accept: it coalesces a duplicate
// dispatch, re-leases a crashed (failed) record, or inserts a fresh one, enforcing the
// in-flight cap and per-goal budget under a per-goal transaction advisory lock. A live
// (pending) or completed record returns accepted=false (the caller must not re-run
// it); a fresh insert or a failed→pending re-lease returns accepted=true. The budget
// is read inside the same transaction from goal_registry so a concurrent dispatch
// cannot race a stale value. Charge is uniform at accept; ErrBudgetExhausted /
// ErrInflightCapReached are returned when a fresh charge would exceed a bound.
func (c *CausalVerifications) DispatchAccept(ctx context.Context, rec CausalVerification) (CausalVerification, bool, error) {
	tx, err := c.pool.Begin(ctx)
	if err != nil {
		return CausalVerification{}, false, fmt.Errorf("begin dispatch tx: %w", err)
	}
	defer tx.Rollback(ctx)

	// A per-goal transaction advisory lock serializes concurrent dispatches for one
	// goal so two budgeted accepts cannot both pass the count guard and overspend. It
	// auto-releases at commit/rollback, sidestepping the session-lock connection-
	// pinning gotcha.
	if _, err := tx.Exec(ctx, "SELECT pg_advisory_xact_lock($1)", fnvInt64Key(rec.GoalID)); err != nil {
		return CausalVerification{}, false, fmt.Errorf("acquire goal dispatch lock: %w", err)
	}

	// Lazily reap this goal's expired leases before counting, so a crashed run's slot
	// and budget are freed within the same accept that might need them.
	if _, err := tx.Exec(ctx,
		"UPDATE causal_verifications SET status = $1, updated_at = now() "+
			"WHERE goal_id = $2 AND status = $3 AND lease_deadline < now()",
		CausalStatusFailed, rec.GoalID, CausalStatusPending,
	); err != nil {
		return CausalVerification{}, false, fmt.Errorf("reap expired leases: %w", err)
	}

	existing, found, err := scanOne(ctx, tx,
		"SELECT "+causalColumns+" FROM causal_verifications "+
			"WHERE goal_id = $1 AND intervention_id = $2 AND graph_version = $3 FOR UPDATE",
		rec.GoalID, rec.InterventionID, rec.GraphVersion)
	if err != nil {
		return CausalVerification{}, false, err
	}
	// A live or completed record coalesces: the caller surfaces it without re-running.
	// Only a crashed (failed) record is re-leasable.
	if found && existing.Status != CausalStatusFailed {
		return existing, false, nil
	}

	if err := c.enforceLimits(ctx, tx, rec); err != nil {
		return CausalVerification{}, false, err
	}

	deadline := time.Now().Add(c.leaseTTL)
	if found {
		released, ok, err := scanOne(ctx, tx,
			"UPDATE causal_verifications SET status = $1, lease_deadline = $2, budgeted = $3, updated_at = now() "+
				"WHERE id = $4 RETURNING "+causalColumns,
			CausalStatusPending, deadline, rec.Budgeted, existing.ID)
		if err != nil || !ok {
			return CausalVerification{}, false, fmt.Errorf("re-lease failed record: %w", err)
		}
		if err := tx.Commit(ctx); err != nil {
			return CausalVerification{}, false, fmt.Errorf("commit re-lease: %w", err)
		}
		return released, true, nil
	}

	inserted, ok, err := scanOne(ctx, tx,
		"INSERT INTO causal_verifications (id, goal_id, intervention_id, graph_version, status, budgeted, lease_deadline) "+
			"VALUES ($1, $2, $3, $4, $5, $6, $7) RETURNING "+causalColumns,
		rec.ID, rec.GoalID, rec.InterventionID, rec.GraphVersion, CausalStatusPending, rec.Budgeted, deadline)
	if err != nil || !ok {
		return CausalVerification{}, false, fmt.Errorf("insert verification: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return CausalVerification{}, false, fmt.Errorf("commit insert: %w", err)
	}
	return inserted, true, nil
}

// enforceLimits checks the in-flight cap (all dispatches) and, for a budgeted
// dispatch, the per-goal budget, both counted in the dispatch transaction with the
// budget read from goal_registry so it cannot be raced by a stale value.
//
// A budgeted dispatch is held one slot below the cap so autonomous work can never
// fill it completely. Without that reservation the exemption is only half real: an
// analyst-initiated dispatch is spared the budget but still loses the slot race, and
// it loses it systematically rather than occasionally -- a verify-track goal's own
// claim has to be introspected, measured and reified before it asks for a slot, while
// the promotions launched alongside it ask immediately, so the one verification the
// analyst actually requested is the one most likely to be refused.
func (c *CausalVerifications) enforceLimits(ctx context.Context, tx pgx.Tx, rec CausalVerification) error {
	var pending int
	if err := tx.QueryRow(ctx,
		"SELECT count(*) FROM causal_verifications WHERE goal_id = $1 AND status = $2",
		rec.GoalID, CausalStatusPending).Scan(&pending); err != nil {
		return fmt.Errorf("count in-flight verifications: %w", err)
	}
	if pending >= c.inflightSlots(rec.Budgeted) {
		return ErrInflightCapReached
	}
	if !rec.Budgeted {
		return nil
	}
	var budget int
	if err := tx.QueryRow(ctx,
		"SELECT coalesce(verification_budget, $1) FROM goal_registry WHERE optimization_function_id = $2",
		c.defaultBudget, rec.GoalID).Scan(&budget); err != nil {
		return fmt.Errorf("read verification budget: %w", err)
	}
	var charged int
	if err := tx.QueryRow(ctx,
		"SELECT count(*) FROM causal_verifications WHERE goal_id = $1 AND budgeted AND status <> $2",
		rec.GoalID, CausalStatusFailed).Scan(&charged); err != nil {
		return fmt.Errorf("count budgeted verifications: %w", err)
	}
	if charged >= budget {
		return ErrBudgetExhausted
	}
	return nil
}

// inflightSlots is how many concurrent verifications a dispatch may find already in
// flight. An exempt dispatch gets the whole cap; a budgeted one gets one less, which
// is the reservation. A cap of 1 leaves budgeted dispatches no slots at all, which is
// the honest reading of "one at a time, reserved for the analyst" rather than a
// special case worth smoothing over.
func (c *CausalVerifications) inflightSlots(budgeted bool) int {
	if !budgeted {
		return c.inflightCap
	}
	return c.inflightCap - 1
}

// Heartbeat renews a leased record's deadline mid-run. The status='pending' guard
// scopes it to a still-live lease: a record already reaped to failed is not revived.
func (c *CausalVerifications) Heartbeat(ctx context.Context, id string) error {
	_, err := c.pool.Exec(ctx,
		"UPDATE causal_verifications SET lease_deadline = $1, updated_at = now() WHERE id = $2 AND status = $3",
		time.Now().Add(c.leaseTTL), id, CausalStatusPending,
	)
	if err != nil {
		return fmt.Errorf("heartbeat verification %q: %w", id, err)
	}
	return nil
}

// Complete stamps a terminal status and the measured effects on a still-leased
// record. The status='pending' guard doubles as an ownership re-check: RowsAffected 0
// means the lease was reaped to failed mid-run, so the caller must skip the graph
// write rather than overwrite a failed record. It reports whether it held the lease.
func (c *CausalVerifications) Complete(ctx context.Context, id, status string, naive, adjusted *float64, adjustmentSet []string, refutationScore, confidence *float64) (bool, error) {
	set, err := marshalAdjustmentSet(adjustmentSet)
	if err != nil {
		return false, err
	}
	tag, err := c.pool.Exec(ctx,
		"UPDATE causal_verifications SET status = $1, naive_effect = $2, adjusted_effect = $3, "+
			"adjustment_set = $4, refutation_score = $5, confidence = $6, lease_deadline = NULL, updated_at = now() "+
			"WHERE id = $7 AND status = $8",
		status, naive, adjusted, set, refutationScore, confidence, id, CausalStatusPending,
	)
	if err != nil {
		return false, fmt.Errorf("complete verification %q: %w", id, err)
	}
	return tag.RowsAffected() == 1, nil
}

// ReapExpired flips every expired pending lease to failed, refunding its budget and
// in-flight slot implicitly (the count guards exclude failed). It runs on a ticker in
// serve mode and lazily inside DispatchAccept. It returns how many it reaped.
func (c *CausalVerifications) ReapExpired(ctx context.Context) (int, error) {
	tag, err := c.pool.Exec(ctx,
		"UPDATE causal_verifications SET status = $1, updated_at = now() WHERE status = $2 AND lease_deadline < now()",
		CausalStatusFailed, CausalStatusPending,
	)
	if err != nil {
		return 0, fmt.Errorf("reap expired verifications: %w", err)
	}
	return int(tag.RowsAffected()), nil
}

// MarkStale flags a goal's completed verifications whose adjustment set touches a
// column, so a graph correction can trigger their re-verification, and returns the
// interventions it flagged. A bumped graph version makes each re-verification a new
// key rather than a duplicate.
//
// Returning the ids it actually flagged is what scopes a correction to its own
// invalidations. Nothing ever clears the flag, so re-reading the goal's stale records
// instead would hand every later correction the whole history to re-dispatch —
// burning the staleness cap on work already done and overstating what this correction
// invalidated.
//
// The columns must carry the graph's own spelling: adjustment_set holds the column
// names discovery wrote, and ?| is a byte-exact jsonb overlap test.
func (c *CausalVerifications) MarkStale(ctx context.Context, goalID string, columns []string) ([]string, error) {
	if len(columns) == 0 {
		return nil, nil
	}
	// The adjustment_set jsonb is an array of column names; ?| tests array/string
	// overlap. Only completed (non-pending, non-failed) records are marked.
	// DISTINCT matters: the table is unique on (goal, intervention, graph version), so
	// one intervention holds a completed record per version it was verified at, and a
	// bare RETURNING would hand back the same finding once per version -- each copy
	// consuming a slot of the caller's re-verification cap and inflating the count it
	// reports. The ORDER BY keeps which findings survive that cap reproducible.
	rows, err := c.pool.Query(ctx,
		"WITH flagged AS ("+
			"UPDATE causal_verifications SET stale = true, updated_at = now() "+
			"WHERE goal_id = $1 AND status NOT IN ($2, $3) AND adjustment_set ?| $4 "+
			"RETURNING intervention_id) "+
			"SELECT DISTINCT intervention_id FROM flagged ORDER BY intervention_id",
		goalID, CausalStatusPending, CausalStatusFailed, columns,
	)
	if err != nil {
		return nil, fmt.Errorf("mark stale verifications for %q: %w", goalID, err)
	}
	defer rows.Close()

	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("scan stale verification: %w", err)
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate stale verifications: %w", err)
	}
	return ids, nil
}

// ListForGoal returns a goal's verification records, newest first, powering the
// orchestrator's verification listing.
func (c *CausalVerifications) ListForGoal(ctx context.Context, goalID string) ([]CausalVerification, error) {
	rows, err := c.pool.Query(ctx,
		"SELECT "+causalColumns+" FROM causal_verifications WHERE goal_id = $1 ORDER BY created_at DESC",
		goalID,
	)
	if err != nil {
		return nil, fmt.Errorf("list verifications for %q: %w", goalID, err)
	}
	defer rows.Close()

	var out []CausalVerification
	for rows.Next() {
		v, err := scanVerification(rows)
		if err != nil {
			return nil, fmt.Errorf("scan verification row: %w", err)
		}
		out = append(out, v)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate verification rows: %w", err)
	}
	return out, nil
}

// scanOne runs a query expected to return at most one row and scans it, reporting
// whether a row was present. It reads through QueryRow semantics so a permission error
// surfaces eagerly (pgx defers it past Query, and an unread Rows deadlocks Close).
func scanOne(ctx context.Context, q pgxQuerier, sql string, args ...any) (CausalVerification, bool, error) {
	rows, err := q.Query(ctx, sql, args...)
	if err != nil {
		return CausalVerification{}, false, fmt.Errorf("query verification: %w", err)
	}
	defer rows.Close()
	if !rows.Next() {
		if err := rows.Err(); err != nil {
			return CausalVerification{}, false, fmt.Errorf("query verification: %w", err)
		}
		return CausalVerification{}, false, nil
	}
	v, err := scanVerification(rows)
	if err != nil {
		return CausalVerification{}, false, fmt.Errorf("scan verification: %w", err)
	}
	return v, true, nil
}

// pgxQuerier is the shared Query surface of *Pool and pgx.Tx, so scanOne serves both a
// pooled read and a read inside the dispatch transaction.
type pgxQuerier interface {
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
}

func scanVerification(rows pgx.Rows) (CausalVerification, error) {
	var v CausalVerification
	var set []byte
	if err := rows.Scan(&v.ID, &v.GoalID, &v.InterventionID, &v.GraphVersion, &v.Status,
		&v.NaiveEffect, &v.AdjustedEffect, &set, &v.RefutationScore, &v.Confidence,
		&v.Budgeted, &v.Stale, &v.LeaseDeadline, &v.CreatedAt, &v.UpdatedAt); err != nil {
		return CausalVerification{}, err
	}
	if len(set) > 0 {
		if err := json.Unmarshal(set, &v.AdjustmentSet); err != nil {
			return CausalVerification{}, fmt.Errorf("unmarshal adjustment set: %w", err)
		}
	}
	return v, nil
}

// marshalAdjustmentSet serializes the adjustment columns for the jsonb column; a nil
// set binds as SQL NULL rather than the JSON literal "null".
func marshalAdjustmentSet(cols []string) ([]byte, error) {
	if cols == nil {
		return nil, nil
	}
	b, err := json.Marshal(cols)
	if err != nil {
		return nil, fmt.Errorf("marshal adjustment set: %w", err)
	}
	return b, nil
}
