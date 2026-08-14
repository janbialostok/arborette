package store

import (
	"context"
	"fmt"
)

// ResetTables lists every table the operator reset and the cleanliness gate
// cover, in FK-safe dependency order (children before parents, per the NO
// ACTION references in the migrations): embeddings carry no FK and go first so
// goal-scoped rows retire with their goals; runs/verification_queue/
// causal_verifications reference goal_registry and must go before it;
// goal_registry references datasets; the data_source_registry ledger is
// logically paired with datasets; sessions reference users (CASCADE) and are
// deleted explicitly before it; audit_log carries only JSONB references and is
// wiped last.
//
// The same order backs both the wipe and the gate's emptiness report, so the
// two can never drift apart on what "clean" means.
var ResetTables = []string{
	"meta_heuristic_embeddings",
	"runs",
	"verification_queue",
	"causal_verifications",
	"goal_registry",
	"datasets",
	"data_source_registry",
	"sessions",
	"users",
	"audit_log",
}

// ResetAll wipes every row from every table in ResetTables inside one
// transaction, using the owner role (the only role with DELETE on every table,
// including audit_log, which no runtime role may delete). It is the operator
// reset behind cmd/cleanup clean. Deleting an already-empty table is a no-op,
// so running it on a clean database succeeds and changes nothing (idempotent).
// Returns the rows removed per table so the caller can log a summary. A
// mid-wipe failure rolls the whole transaction back rather than leaving a
// half-wiped database.
func ResetAll(ctx context.Context, pool *Pool) (map[string]int64, error) {
	tx, err := pool.Begin(ctx)
	if err != nil {
		return nil, fmt.Errorf("begin reset: %w", err)
	}
	defer tx.Rollback(ctx)

	removed := make(map[string]int64, len(ResetTables))
	for _, table := range ResetTables {
		tag, err := tx.Exec(ctx, "DELETE FROM "+table)
		if err != nil {
			return nil, fmt.Errorf("reset %s: %w", table, err)
		}
		removed[table] = tag.RowsAffected()
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, fmt.Errorf("commit reset: %w", err)
	}
	return removed, nil
}

// EmptyCounts reports the row count of every table in ResetTables, for the
// cleanliness gate (cmd/cleanup check). A post-suite environment that was clean
// when the suite started is clean when every count is zero; a non-zero count
// names the table the gate flags. Reads run as the caller's role — the gate
// uses the owner role so audit_log and users/sessions, which runtime roles
// cannot read, are covered too.
func EmptyCounts(ctx context.Context, pool *Pool) (map[string]int64, error) {
	counts := make(map[string]int64, len(ResetTables))
	for _, table := range ResetTables {
		var n int64
		if err := pool.QueryRow(ctx, "SELECT count(*) FROM "+table).Scan(&n); err != nil {
			return nil, fmt.Errorf("count %s: %w", table, err)
		}
		counts[table] = n
	}
	return counts, nil
}
