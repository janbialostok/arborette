package store

import (
	"context"
	"fmt"
	"time"
)

// RunStatus is a hypothesis-run lifecycle state as persisted in runs.status.
type RunStatus string

const (
	RunRunning   RunStatus = "running"
	RunCompleted RunStatus = "completed"
	RunFailed    RunStatus = "failed"
)

// Run is one hypothesis-loop run as persisted in the runs table. EndedAt and
// FailureReason are nil until the run settles.
type Run struct {
	RunID                  string
	OptimizationFunctionID string
	Status                 RunStatus
	StartedAt              time.Time
	EndedAt                *time.Time
	FailureReason          *string
}

// Runs is the runs-table access package, backed by a runtime pool. The
// Orchestrator is the sole writer (INSERT on trigger, UPDATE on status
// transitions); the boundary is enforced primarily by the least-privilege
// arborette_orchestrator role, with this write-restricted type as the second
// layer -- it exposes no delete.
type Runs struct {
	pool *Pool
}

// NewRuns wires the runs store to a pool.
func NewRuns(pool *Pool) *Runs {
	return &Runs{pool: pool}
}

// Create records a new run in the running state; started_at defaults to now().
func (r *Runs) Create(ctx context.Context, runID, optimizationFunctionID string) error {
	_, err := r.pool.Exec(ctx,
		"INSERT INTO runs (run_id, optimization_function_id, status) VALUES ($1, $2, $3)",
		runID, optimizationFunctionID, RunRunning,
	)
	if err != nil {
		return fmt.Errorf("create run %q: %w", runID, err)
	}
	return nil
}

// SetStatus settles a run: it stamps the terminal status, ended_at, and the
// failure reason -- NULL when empty, so a completed run carries no reason.
func (r *Runs) SetStatus(ctx context.Context, runID string, status RunStatus, failureReason string) error {
	var reason *string
	if failureReason != "" {
		reason = &failureReason
	}
	_, err := r.pool.Exec(ctx,
		"UPDATE runs SET status = $2, ended_at = now(), failure_reason = $3 WHERE run_id = $1",
		runID, status, reason,
	)
	if err != nil {
		return fmt.Errorf("set run %q status: %w", runID, err)
	}
	return nil
}

// LatestByGoal returns the most recent run per optimization_function_id in
// goalIDs, keyed by that id. Goals with no run are absent from the map (the
// caller synthesizes a no-run state); an empty goalIDs matches no rows.
func (r *Runs) LatestByGoal(ctx context.Context, goalIDs []string) (map[string]Run, error) {
	rows, err := r.pool.Query(ctx,
		"SELECT DISTINCT ON (optimization_function_id) "+
			"run_id, optimization_function_id, status, started_at, ended_at, failure_reason "+
			"FROM runs WHERE optimization_function_id = ANY($1) "+
			"ORDER BY optimization_function_id, started_at DESC",
		goalIDs,
	)
	if err != nil {
		return nil, fmt.Errorf("latest runs by goal: %w", err)
	}
	defer rows.Close()

	latest := make(map[string]Run, len(goalIDs))
	for rows.Next() {
		var run Run
		if err := rows.Scan(&run.RunID, &run.OptimizationFunctionID, &run.Status,
			&run.StartedAt, &run.EndedAt, &run.FailureReason); err != nil {
			return nil, fmt.Errorf("scan run row: %w", err)
		}
		latest[run.OptimizationFunctionID] = run
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate run rows: %w", err)
	}
	return latest, nil
}

// FailOrphaned settles every still-running run as failed, for runs abandoned by
// a prior process crash or shutdown that never ran their terminal write. Called
// once at Orchestrator boot; returns the number of rows reconciled. Correct only
// while a single Orchestrator instance runs -- with multiple instances it would
// wrongly fail a peer's live run.
func (r *Runs) FailOrphaned(ctx context.Context, reason string) (int64, error) {
	tag, err := r.pool.Exec(ctx,
		"UPDATE runs SET status = $1, ended_at = now(), failure_reason = $2 WHERE status = $3",
		RunFailed, reason, RunRunning,
	)
	if err != nil {
		return 0, fmt.Errorf("reconcile orphaned runs: %w", err)
	}
	return tag.RowsAffected(), nil
}
