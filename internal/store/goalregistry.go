package store

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/arborette/arborette/internal/domain"
)

// EpochMode is how a goal's hypothesis loop treats pending human verifications.
// speculative (the default) never waits: the loop prunes on unverified
// confidence while verifications resolve in the background. blocking trades loop
// speed for pruning decisions taken only on verified values.
type EpochMode string

const (
	EpochSpeculative EpochMode = "speculative"
	EpochBlocking    EpochMode = "blocking"
)

// Goal is a registered optimization goal as persisted in goal_registry. A goal
// carries exactly one objective form: a tabular goal has an EvaluationMatrix
// (TargetFields empty); a document goal has TargetFields (a zero-value matrix).
// ConfidenceThreshold is nil when the goal takes the service-wide HITL default.
type Goal struct {
	OptimizationFunctionID string
	GoalText               string
	EvaluationMatrix       domain.EvaluationMatrix
	TargetFields           []domain.TargetField
	DataSourceRef          string
	ConfidenceThreshold    *float64
	EpochMode              EpochMode
	CreatedAt              time.Time
}

// IsDocument reports whether this is a document goal (its objective is a set of
// fields to extract) rather than a tabular one (an EvaluationMatrix to optimize).
func (g Goal) IsDocument() bool { return len(g.TargetFields) > 0 }

// GoalRegistry is the goal-registry access package, backed by a runtime pool.
type GoalRegistry struct {
	pool *Pool
}

// NewGoalRegistry wires the registry to a pool.
func NewGoalRegistry(pool *Pool) *GoalRegistry {
	return &GoalRegistry{pool: pool}
}

// goalColumns is the read projection Get and List share, so their column order
// and scan order cannot drift apart.
const goalColumns = "optimization_function_id, goal_text, evaluation_matrix, datasource_ref, " +
	"target_fields, confidence_threshold, epoch_mode, created_at"

// Insert persists a registered goal. A document goal writes a NULL
// evaluation_matrix and populated target_fields; a tabular goal does the reverse.
// A nil []byte binds as SQL NULL, so exactly one jsonb column is populated.
func (g *GoalRegistry) Insert(ctx context.Context, goal Goal) error {
	var matrix, targetFields []byte
	var err error
	if goal.IsDocument() {
		targetFields, err = json.Marshal(goal.TargetFields)
		if err != nil {
			return fmt.Errorf("marshal target fields: %w", err)
		}
	} else {
		matrix, err = json.Marshal(goal.EvaluationMatrix)
		if err != nil {
			return fmt.Errorf("marshal evaluation matrix: %w", err)
		}
	}
	epochMode := goal.EpochMode
	if epochMode == "" {
		epochMode = EpochSpeculative
	}
	_, err = g.pool.Exec(ctx,
		"INSERT INTO goal_registry "+
			"(optimization_function_id, goal_text, evaluation_matrix, datasource_ref, target_fields, confidence_threshold, epoch_mode) "+
			"VALUES ($1, $2, $3, $4, $5, $6, $7)",
		goal.OptimizationFunctionID, goal.GoalText, matrix, goal.DataSourceRef, targetFields,
		goal.ConfidenceThreshold, epochMode,
	)
	if err != nil {
		return fmt.Errorf("insert goal: %w", err)
	}
	return nil
}

// Get looks up a registered goal by its optimization_function_id. Both jsonb
// columns are null-guarded: a document goal has a NULL evaluation_matrix and a
// tabular goal a NULL target_fields, and a NULL scans as a nil []byte that must
// not be unmarshalled (an empty input is not valid JSON) -- decoding both
// unconditionally would break every read once one document goal exists.
func (g *GoalRegistry) Get(ctx context.Context, optimizationFunctionID string) (Goal, error) {
	var goal Goal
	var matrix, targetFields []byte
	err := g.pool.QueryRow(ctx,
		"SELECT "+goalColumns+" FROM goal_registry WHERE optimization_function_id = $1",
		optimizationFunctionID,
	).Scan(&goal.OptimizationFunctionID, &goal.GoalText, &matrix, &goal.DataSourceRef, &targetFields,
		&goal.ConfidenceThreshold, &goal.EpochMode, &goal.CreatedAt)
	if err != nil {
		return Goal{}, fmt.Errorf("get goal %q: %w", optimizationFunctionID, err)
	}
	if err := decodeGoalObjective(&goal, matrix, targetFields); err != nil {
		return Goal{}, err
	}
	return goal, nil
}

// List returns every registered goal, newest first. Each goal's objective is
// null-guarded the same way Get null-guards it, so a mix of tabular and document
// goals decodes without a NULL-column error.
func (g *GoalRegistry) List(ctx context.Context) ([]Goal, error) {
	rows, err := g.pool.Query(ctx,
		"SELECT "+goalColumns+" FROM goal_registry ORDER BY created_at DESC",
	)
	if err != nil {
		return nil, fmt.Errorf("list goals: %w", err)
	}
	defer rows.Close()

	var goals []Goal
	for rows.Next() {
		var goal Goal
		var matrix, targetFields []byte
		if err := rows.Scan(&goal.OptimizationFunctionID, &goal.GoalText, &matrix,
			&goal.DataSourceRef, &targetFields, &goal.ConfidenceThreshold, &goal.EpochMode,
			&goal.CreatedAt); err != nil {
			return nil, fmt.Errorf("scan goal row: %w", err)
		}
		if err := decodeGoalObjective(&goal, matrix, targetFields); err != nil {
			return nil, err
		}
		goals = append(goals, goal)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate goal rows: %w", err)
	}
	return goals, nil
}

// decodeGoalObjective decodes a goal's objective from its two jsonb columns,
// skipping a NULL (nil []byte) column so a document goal's NULL matrix and a
// tabular goal's NULL target_fields each decode to a zero value rather than a
// json.Unmarshal error on empty input.
func decodeGoalObjective(goal *Goal, matrix, targetFields []byte) error {
	if len(matrix) > 0 {
		if err := json.Unmarshal(matrix, &goal.EvaluationMatrix); err != nil {
			return fmt.Errorf("unmarshal evaluation matrix: %w", err)
		}
	}
	if len(targetFields) > 0 {
		if err := json.Unmarshal(targetFields, &goal.TargetFields); err != nil {
			return fmt.Errorf("unmarshal target fields: %w", err)
		}
	}
	return nil
}
