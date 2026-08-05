// Package objective pins an Evaluation Matrix down to the single measurement a
// run is judged against, and shapes the sandbox requests that measure it. It is
// shared by the Orchestrator's hypothesis loop and the Sleep-Cycle Worker so the
// two cannot disagree on the objective label — the key every measured value is
// carried under end to end.
package objective

import (
	"encoding/json"
	"errors"

	"github.com/arborette/arborette/internal/domain"
	"github.com/arborette/arborette/internal/sandboxclient"
)

// Sentinel errors for a matrix that cannot be pinned. Both direct the caller to
// re-register the goal rather than substituting a default: a silently-chosen
// aggregation would mis-measure.
var (
	ErrNoObjective        = errors.New("evaluation matrix has no target to pin as the objective")
	ErrMissingAggregation = errors.New("evaluation matrix target carries no aggregation; re-register the goal to fit a measurable objective")

	// ErrNonNumericValue is what a NumericValue miss means to every caller: the
	// sandbox answered, but not with a value this objective can be measured by.
	// It lives here so the loop, the worker, and their audit trails cannot drift
	// on the wording.
	ErrNonNumericValue = errors.New("sandbox returned a non-numeric objective value")
)

// Objective is a run's fixed measurement, held constant across every candidate:
// one aggregation over one value expression, with the direction that decides what
// "improvement" means. Label is the rendered key the measured value is carried
// under end to end. Pinning it once is what makes value comparisons meaningful —
// every value compared is the same measurement. EntityKeyColumn and TimeColumn are
// the window bindings a windowed value expression compiles against; both empty for
// a plain aggregate objective. They live on the goal, not the matrix, so Pin cannot
// set them — the caller populates them after pinning.
type Objective struct {
	Aggregation     string
	Expr            domain.Expression
	Label           string
	Direction       domain.TargetDirection
	EntityKeyColumn string
	TimeColumn      string
}

// Pin fixes the objective from the Evaluation Matrix's first target: the
// aggregation, value expression, direction, and rendered label. A matrix with no
// target, or a legacy target with no aggregation, is a terminal failure.
func Pin(matrix domain.EvaluationMatrix) (Objective, error) {
	if len(matrix.Targets) == 0 {
		return Objective{}, ErrNoObjective
	}
	t := matrix.Targets[0]
	if t.Aggregation == "" {
		return Objective{}, ErrMissingAggregation
	}
	expr := t.ValueExpression()
	return Objective{
		Aggregation: t.Aggregation,
		Expr:        expr,
		Label:       domain.RenderObjectiveLabel(t.Aggregation, expr),
		Direction:   t.Direction,
	}, nil
}

// ExecuteRequestFor builds the execute request measuring the pinned objective
// under filters, shared so every call site pins the objective identically.
func ExecuteRequestFor(dataSourceRef string, obj Objective, filters []domain.Constraint) sandboxclient.ExecuteRequest {
	return sandboxclient.ExecuteRequest{
		DataSourceRef:   dataSourceRef,
		Type:            domain.InterventionQuery,
		Aggregation:     obj.Aggregation,
		Target:          domain.Target{Direction: obj.Direction},
		ValueExpression: &obj.Expr,
		ObjectiveLabel:  obj.Label,
		EntityKeyColumn: obj.EntityKeyColumn,
		TimeColumn:      obj.TimeColumn,
		Filters:         filters,
	}
}

// NumericValue extracts the objective value keyed by the objective label. A nil
// value (an empty aggregate) or a non-number is not usable.
func NumericValue(value map[string]any, label string) (float64, bool) {
	raw, ok := value[label]
	if !ok || raw == nil {
		return 0, false
	}
	switch n := raw.(type) {
	case float64:
		return n, true
	case json.Number:
		f, err := n.Float64()
		return f, err == nil
	default:
		return 0, false
	}
}

// Improves reports whether value moves the objective in the desired direction
// versus the baseline.
func Improves(baseline, value float64, direction domain.TargetDirection) bool {
	if direction == domain.Minimize {
		return value < baseline
	}
	return value > baseline
}
