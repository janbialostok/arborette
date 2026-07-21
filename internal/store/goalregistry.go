package store

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/arborette/arborette/internal/domain"
)

// Goal is a registered optimization goal as persisted in goal_registry.
type Goal struct {
	OptimizationFunctionID string
	GoalText               string
	EvaluationMatrix       domain.EvaluationMatrix
	DataSourceRef          string
	CreatedAt              time.Time
}

// GoalRegistry is the goal-registry access package, backed by a runtime pool.
type GoalRegistry struct {
	pool *Pool
}

// NewGoalRegistry wires the registry to a pool.
func NewGoalRegistry(pool *Pool) *GoalRegistry {
	return &GoalRegistry{pool: pool}
}

// Insert persists a registered goal, marshalling its Evaluation Matrix to jsonb.
func (g *GoalRegistry) Insert(ctx context.Context, goal Goal) error {
	matrix, err := json.Marshal(goal.EvaluationMatrix)
	if err != nil {
		return fmt.Errorf("marshal evaluation matrix: %w", err)
	}
	_, err = g.pool.Exec(ctx,
		"INSERT INTO goal_registry (optimization_function_id, goal_text, evaluation_matrix, datasource_ref) "+
			"VALUES ($1, $2, $3, $4)",
		goal.OptimizationFunctionID, goal.GoalText, matrix, goal.DataSourceRef,
	)
	if err != nil {
		return fmt.Errorf("insert goal: %w", err)
	}
	return nil
}

// Get looks up a registered goal by its optimization_function_id.
func (g *GoalRegistry) Get(ctx context.Context, optimizationFunctionID string) (Goal, error) {
	var goal Goal
	var matrix []byte
	err := g.pool.QueryRow(ctx,
		"SELECT optimization_function_id, goal_text, evaluation_matrix, datasource_ref, created_at "+
			"FROM goal_registry WHERE optimization_function_id = $1",
		optimizationFunctionID,
	).Scan(&goal.OptimizationFunctionID, &goal.GoalText, &matrix, &goal.DataSourceRef, &goal.CreatedAt)
	if err != nil {
		return Goal{}, fmt.Errorf("get goal %q: %w", optimizationFunctionID, err)
	}
	if err := json.Unmarshal(matrix, &goal.EvaluationMatrix); err != nil {
		return Goal{}, fmt.Errorf("unmarshal evaluation matrix: %w", err)
	}
	return goal, nil
}
