package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/arborette/arborette/internal/domain"
)

// ErrAlreadyResolved reports that a resolution lost the race for a queue entry:
// either the guarded claim matched no pending row, or the on-demand insert hit
// an existing row for the same outcome. Callers match it with errors.Is to tell
// a lost claim from a failed write -- the first is a 409 (or a repair), the
// second a 5xx.
var ErrAlreadyResolved = errors.New("verification entry is already resolved")

// QueueStatus is a verification entry's lifecycle state as persisted in
// verification_queue.status.
type QueueStatus string

const (
	QueuePending  QueueStatus = "pending"
	QueueResolved QueueStatus = "resolved"
)

// QueueResolution is the analyst's verdict on an extracted value. It is empty
// while an entry is pending.
type QueueResolution string

const (
	ResolutionConfirmed QueueResolution = "confirmed"
	ResolutionCorrected QueueResolution = "corrected"
	ResolutionRejected  QueueResolution = "rejected"
)

// VerificationEntry is one queued extract-type outcome as persisted in
// verification_queue. Provenance, Confidence, and ExtractedValue are a snapshot
// taken when the entry was written; the graph holds the live values, which a
// correction updates in place. Resolution and CorrectedValue are empty and
// ResolvedAt nil while the entry is pending.
type VerificationEntry struct {
	QueueID                string
	OptimizationFunctionID string
	OutcomeID              string
	Field                  string
	ExtractedValue         string
	Provenance             *domain.ProvenanceLocator
	Confidence             float64
	Status                 QueueStatus
	Resolution             QueueResolution
	CorrectedValue         string
	CreatedAt              time.Time
	ResolvedAt             *time.Time
}

// VerificationQueue is the verification_queue access package, backed by a
// runtime pool. The Orchestrator is the sole writer (INSERT on queue routing,
// UPDATE on resolution); the boundary is enforced primarily by the
// least-privilege arborette_orchestrator role, with this write-restricted type
// as the second layer -- it exposes no delete.
type VerificationQueue struct {
	pool *Pool
}

// NewVerificationQueue wires the queue store to a pool.
func NewVerificationQueue(pool *Pool) *VerificationQueue {
	return &VerificationQueue{pool: pool}
}

// queueColumns is the read projection every row scan below expects, kept in one
// place so the column order and scanEntry cannot drift apart.
const queueColumns = "queue_id, optimization_function_id, outcome_id, field, extracted_value, " +
	"provenance, confidence, status, resolution, corrected_value, created_at, resolved_at"

// Enqueue records a below-threshold outcome as pending review. The caller
// assigns QueueID, mirroring how the runs store takes its id from the trigger.
func (q *VerificationQueue) Enqueue(ctx context.Context, e VerificationEntry) error {
	provenance, err := marshalLocator(e.Provenance)
	if err != nil {
		return err
	}
	_, err = q.pool.Exec(ctx,
		"INSERT INTO verification_queue "+
			"(queue_id, optimization_function_id, outcome_id, field, extracted_value, provenance, confidence, status) "+
			"VALUES ($1, $2, $3, $4, $5, $6, $7, $8)",
		e.QueueID, e.OptimizationFunctionID, e.OutcomeID, e.Field, e.ExtractedValue,
		provenance, e.Confidence, QueuePending,
	)
	if err != nil {
		return fmt.Errorf("enqueue verification for outcome %q: %w", e.OutcomeID, err)
	}
	return nil
}

// EnqueueResolved records an already-resolved entry for an outcome that was
// never queued -- the on-demand path, where an analyst reviews an
// at-or-above-threshold outcome -- so the table stays the complete resolution
// history rather than only the below-threshold slice. ON CONFLICT DO NOTHING
// makes it a claim: a row already existing for this outcome (queued by the loop,
// or inserted by a concurrent resolution) yields ErrAlreadyResolved instead of a
// unique violation, so the caller can repair or answer 409.
func (q *VerificationQueue) EnqueueResolved(ctx context.Context, e VerificationEntry, resolution QueueResolution, correctedValue string) error {
	provenance, err := marshalLocator(e.Provenance)
	if err != nil {
		return err
	}
	tag, err := q.pool.Exec(ctx,
		"INSERT INTO verification_queue "+
			"(queue_id, optimization_function_id, outcome_id, field, extracted_value, provenance, confidence, "+
			"status, resolution, corrected_value, resolved_at) "+
			"VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, now()) "+
			"ON CONFLICT (outcome_id) DO NOTHING",
		e.QueueID, e.OptimizationFunctionID, e.OutcomeID, e.Field, e.ExtractedValue,
		provenance, e.Confidence, QueueResolved, resolution, nullableText(correctedValue),
	)
	if err != nil {
		return fmt.Errorf("enqueue resolved verification for outcome %q: %w", e.OutcomeID, err)
	}
	if tag.RowsAffected() == 0 {
		return ErrAlreadyResolved
	}
	return nil
}

// Resolve claims a pending entry and stamps the analyst's verdict. The
// status='pending' guard makes the claim atomic: a second resolution of the same
// outcome (a double-click, or a retry racing the original) affects no rows and
// gets ErrAlreadyResolved, so exactly one caller proceeds to the graph write.
func (q *VerificationQueue) Resolve(ctx context.Context, outcomeID string, resolution QueueResolution, correctedValue string) error {
	tag, err := q.pool.Exec(ctx,
		"UPDATE verification_queue SET status = $2, resolution = $3, corrected_value = $4, resolved_at = now() "+
			"WHERE outcome_id = $1 AND status = $5",
		outcomeID, QueueResolved, resolution, nullableText(correctedValue), QueuePending,
	)
	if err != nil {
		return fmt.Errorf("resolve verification for outcome %q: %w", outcomeID, err)
	}
	if tag.RowsAffected() == 0 {
		return ErrAlreadyResolved
	}
	return nil
}

// Unclaim reverts a claim whose graph write-through failed, returning the entry
// to pending so the analyst can retry. The resolution guard scopes it to the
// claim this caller wrote: a request that never claimed (or whose claim was
// superseded) affects no rows, so a failing repair can never revert another
// request's resolution.
func (q *VerificationQueue) Unclaim(ctx context.Context, outcomeID string, resolution QueueResolution) error {
	_, err := q.pool.Exec(ctx,
		"UPDATE verification_queue SET status = $2, resolution = NULL, corrected_value = NULL, resolved_at = NULL "+
			"WHERE outcome_id = $1 AND status = $3 AND resolution = $4",
		outcomeID, QueuePending, QueueResolved, resolution,
	)
	if err != nil {
		return fmt.Errorf("unclaim verification for outcome %q: %w", outcomeID, err)
	}
	return nil
}

// GetByOutcome returns one entry by the outcome it verifies, or pgx.ErrNoRows
// when the outcome was never queued.
func (q *VerificationQueue) GetByOutcome(ctx context.Context, outcomeID string) (VerificationEntry, error) {
	row := q.pool.QueryRow(ctx,
		"SELECT "+queueColumns+" FROM verification_queue WHERE outcome_id = $1",
		outcomeID,
	)
	entry, err := scanEntry(row)
	if err != nil {
		return VerificationEntry{}, fmt.Errorf("get verification for outcome %q: %w", outcomeID, err)
	}
	return entry, nil
}

// ListForGoal returns a goal's queue entries, newest first. An empty status
// returns every entry; otherwise only those in that state.
func (q *VerificationQueue) ListForGoal(ctx context.Context, goalID string, status QueueStatus) ([]VerificationEntry, error) {
	sql := "SELECT " + queueColumns + " FROM verification_queue WHERE optimization_function_id = $1"
	args := []any{goalID}
	if status != "" {
		sql += " AND status = $2"
		args = append(args, status)
	}
	sql += " ORDER BY created_at DESC"

	rows, err := q.pool.Query(ctx, sql, args...)
	if err != nil {
		return nil, fmt.Errorf("list verifications for %q: %w", goalID, err)
	}
	defer rows.Close()

	var entries []VerificationEntry
	for rows.Next() {
		entry, err := scanEntry(rows)
		if err != nil {
			return nil, fmt.Errorf("scan verification row: %w", err)
		}
		entries = append(entries, entry)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate verification rows: %w", err)
	}
	return entries, nil
}

// scanner is the shared surface of pgx.Row and pgx.Rows, so one scan routine
// serves both the single-entry read and the list iteration.
type scanner interface {
	Scan(dest ...any) error
}

func scanEntry(s scanner) (VerificationEntry, error) {
	var e VerificationEntry
	var provenance []byte
	var resolution, correctedValue *string
	if err := s.Scan(&e.QueueID, &e.OptimizationFunctionID, &e.OutcomeID, &e.Field, &e.ExtractedValue,
		&provenance, &e.Confidence, &e.Status, &resolution, &correctedValue,
		&e.CreatedAt, &e.ResolvedAt); err != nil {
		return VerificationEntry{}, err
	}
	if len(provenance) > 0 {
		var locator domain.ProvenanceLocator
		if err := json.Unmarshal(provenance, &locator); err != nil {
			return VerificationEntry{}, fmt.Errorf("unmarshal provenance: %w", err)
		}
		e.Provenance = &locator
	}
	if resolution != nil {
		e.Resolution = QueueResolution(*resolution)
	}
	if correctedValue != nil {
		e.CorrectedValue = *correctedValue
	}
	return e, nil
}

// marshalLocator serializes a locator for the jsonb column; a nil locator binds
// as SQL NULL rather than the JSON literal "null", so a missing match stays
// absent.
func marshalLocator(p *domain.ProvenanceLocator) ([]byte, error) {
	if p == nil {
		return nil, nil
	}
	b, err := json.Marshal(p)
	if err != nil {
		return nil, fmt.Errorf("marshal provenance: %w", err)
	}
	return b, nil
}

// nullableText binds an empty string as SQL NULL, keeping "no corrected value"
// distinct from a corrected empty string.
func nullableText(v string) *string {
	if v == "" {
		return nil
	}
	return &v
}
