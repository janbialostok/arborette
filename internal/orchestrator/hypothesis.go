package orchestrator

import (
	"context"
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"time"

	"github.com/google/uuid"

	"github.com/arborette/arborette/internal/domain"
	"github.com/arborette/arborette/internal/llm"
	"github.com/arborette/arborette/internal/store"
)

var errNonNumericValue = errors.New("sandbox returned a non-numeric objective value")

// loopTimeout bounds a whole hypothesis run. The loop runs on a background
// context (decoupled from the SSE stream), so without a deadline a wedged
// dependency would leak the goroutine indefinitely.
const loopTimeout = 10 * time.Minute

// objective is the run's fixed measurement, held constant across the whole tree:
// one aggregation over one target field, with the direction that decides what
// "improvement" means. Pinning it once is what makes the stop-on-no-improvement
// comparison meaningful — every value compared is the same measurement.
type objective struct {
	aggregation string
	field       string
	direction   domain.TargetDirection
}

// handleTriggerLoop starts the Phase-1 hypothesis loop for a goal. It looks the
// goal up (404 on miss), launches the loop in a goroutine on a timeout-bounded
// background context so an SSE disconnect never aborts it, and returns 202.
func (s *Server) handleTriggerLoop(w http.ResponseWriter, r *http.Request) {
	goal, ok := s.lookupGoal(r.Context(), w, r.PathValue("id"))
	if !ok {
		return
	}
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), loopTimeout)
		defer cancel()
		s.runLoop(ctx, goal)
	}()
	writeJSON(w, http.StatusAccepted, map[string]any{"optimization_function_id": goal.OptimizationFunctionID})
}

// runLoop drives the hypothesis tree: introspect the data source, pin the
// objective and measure the root baseline, then expand each root candidate.
func (s *Server) runLoop(ctx context.Context, goal store.Goal) {
	id := goal.OptimizationFunctionID
	defer func() {
		s.hub.Publish(id, Event{Type: "loop_complete"})
		s.hub.Complete(id)
	}()

	introspect, err := s.sandbox.Introspect(ctx, IntrospectRequest{
		DataSourceRef: goal.DataSourceRef,
		Targets:       goal.EvaluationMatrix.Targets,
	})
	if err != nil {
		log.Printf("orchestrator: hypothesis loop %q: introspect: %v", id, err)
		s.branchFailure(ctx, id, nil, err)
		return
	}
	schema := toSandboxSchema(introspect.Schema)

	root, err := s.claude.ProposeInterventionTree(ctx, goal.GoalText, goal.EvaluationMatrix, schema,
		llm.TreeContext{IsRoot: true, Breadth: defaultBreadth})
	if err != nil {
		log.Printf("orchestrator: hypothesis loop %q: root proposal: %v", id, err)
		s.branchFailure(ctx, id, nil, err)
		return
	}
	obj := pinObjective(root, goal.EvaluationMatrix)

	// Root baseline: the objective measured with no filters. Deeper baselines
	// reuse the parent's outcome value (cumulative nesting makes that valid).
	baseResp, err := s.sandbox.Execute(ctx, ExecuteRequest{
		DataSourceRef: goal.DataSourceRef,
		Type:          domain.InterventionQuery,
		Aggregation:   obj.aggregation,
		Target:        domain.Target{Field: obj.field, Direction: obj.direction},
		Filters:       nil,
	})
	if err != nil {
		log.Printf("orchestrator: hypothesis loop %q: root baseline: %v", id, err)
		s.branchFailure(ctx, id, nil, err)
		return
	}
	baseline, ok := numericValue(baseResp.Value, obj.field)
	if !ok {
		s.branchFailure(ctx, id, nil, errNonNumericValue)
		return
	}

	for _, cand := range root.Candidates {
		s.processCandidate(ctx, goal, obj, schema, nil, baseline, cand, 1)
	}
}

// processCandidate measures one candidate at its effective (cumulative) filter
// set, writes the causal triplet, and — if the candidate improves the objective
// within constraints and the depth cap is not reached — proposes and expands its
// refinement children.
func (s *Server) processCandidate(ctx context.Context, goal store.Goal, obj objective, schema llm.SandboxSchema, parentFilters []domain.Constraint, baseline float64, cand llm.CandidateIntervention, depth int) {
	id := goal.OptimizationFunctionID
	effective := concatFilters(parentFilters, cand.Filters)

	resp, err := s.sandbox.Execute(ctx, ExecuteRequest{
		DataSourceRef: goal.DataSourceRef,
		Type:          domain.InterventionQuery,
		Aggregation:   obj.aggregation,
		Target:        domain.Target{Field: obj.field, Direction: obj.direction},
		Filters:       effective,
	})
	if err != nil {
		s.branchFailure(ctx, id, effective, err)
		return
	}
	value, ok := numericValue(resp.Value, obj.field)
	if !ok {
		s.branchFailure(ctx, id, effective, errNonNumericValue)
		return
	}

	if err := s.writeTriplet(ctx, goal, obj, parentFilters, baseline, cand, effective, value); err != nil {
		log.Printf("orchestrator: hypothesis loop %q: write triplet: %v", id, err)
		s.branchFailure(ctx, id, effective, err)
		return
	}

	if !improves(baseline, value, obj.direction) {
		return
	}
	if !constraintsSatisfied(goal.EvaluationMatrix.Constraints, obj.field, value) {
		s.branchFailure(ctx, id, effective, errors.New("candidate violates a hard constraint on the objective field"))
		return
	}
	if depth >= defaultDepth {
		return
	}

	child, err := s.claude.ProposeInterventionTree(ctx, goal.GoalText, goal.EvaluationMatrix, schema, llm.TreeContext{
		Breadth:              defaultBreadth,
		ObjectiveField:       obj.field,
		ObjectiveAggregation: obj.aggregation,
		Direction:            obj.direction,
		ParentFilters:        effective,
		PriorValue:           &value,
	})
	if err != nil {
		s.branchFailure(ctx, id, effective, err)
		return
	}
	for _, c := range child.Candidates {
		s.processCandidate(ctx, goal, obj, schema, effective, value, c, depth+1)
	}
}

// writeTriplet persists one State→Intervention→Outcome triplet and audits the
// intervention and outcome. The start state is the objective measured at the
// parent's effective filters (the baseline the candidate is judged against);
// query outcomes are always verified with PRODUCED confidence fixed at 1.0.
func (s *Server) writeTriplet(ctx context.Context, goal store.Goal, obj objective, parentFilters []domain.Constraint, baseline float64, cand llm.CandidateIntervention, effective []domain.Constraint, value float64) error {
	stateID := uuid.NewString()
	state := domain.State{ID: stateID, Properties: map[string]any{
		"data_source_ref":       goal.DataSourceRef,
		"objective_aggregation": obj.aggregation,
		"objective_field":       obj.field,
		"effective_filters":     parentFilters,
		"value":                 baseline,
	}}
	if err := s.repo.CreateState(ctx, state); err != nil {
		return err
	}

	interventionID := uuid.NewString()
	intervention := domain.Intervention{ID: interventionID, Type: domain.InterventionQuery, Properties: map[string]any{
		"objective_aggregation": obj.aggregation,
		"objective_field":       obj.field,
		"new_filters":           cand.Filters,
		"effective_filters":     effective,
	}}
	if err := s.repo.CreateIntervention(ctx, intervention); err != nil {
		return err
	}

	outcomeID := uuid.NewString()
	outcome := domain.Outcome{ID: outcomeID, VerificationStatus: domain.VerificationVerified, Value: map[string]any{obj.field: value}}
	if err := s.repo.CreateOutcome(ctx, outcome); err != nil {
		return err
	}

	if err := s.repo.CreatePreConditionFor(ctx, stateID, interventionID); err != nil {
		return err
	}
	if err := s.repo.CreateProduced(ctx, interventionID, outcomeID, domain.ProducedEdge{EffectSize: value - baseline, Confidence: 1.0}); err != nil {
		return err
	}

	// Audit every intervention and outcome, not only failures.
	if err := s.recordAudit(ctx, "hypothesis_intervention", "intervention", map[string]any{
		"optimization_function_id": goal.OptimizationFunctionID,
		"intervention_id":          interventionID,
		"new_filters":              cand.Filters,
	}); err != nil {
		log.Printf("orchestrator: append audit: %v", err)
	}
	if err := s.recordAudit(ctx, "hypothesis_outcome", "outcome", map[string]any{
		"optimization_function_id": goal.OptimizationFunctionID,
		"outcome_id":               outcomeID,
		"value":                    value,
	}); err != nil {
		log.Printf("orchestrator: append audit: %v", err)
	}

	s.hub.Publish(goal.OptimizationFunctionID, Event{Type: "triplet", Payload: map[string]any{
		"state_id":        stateID,
		"intervention_id": interventionID,
		"outcome_id":      outcomeID,
		"baseline":        baseline,
		"value":           value,
		"effect_size":     value - baseline,
	}})
	return nil
}

// branchFailure records a branch-failure audit event and emits a failure SSE
// event; siblings continue.
func (s *Server) branchFailure(ctx context.Context, id string, filters []domain.Constraint, cause error) {
	if err := s.recordAudit(ctx, "hypothesis_branch_failure", "failure", map[string]any{
		"optimization_function_id": id,
		"filters":                  filters,
		"error":                    cause.Error(),
	}); err != nil {
		log.Printf("orchestrator: append audit: %v", err)
	}
	s.hub.Publish(id, Event{Type: "branch_failure", Payload: map[string]any{"error": cause.Error()}})
}

// pinObjective fixes the run's objective from the root proposal, falling back to
// the Evaluation Matrix's first target and a count aggregation when the model
// does not choose. Targets is unordered, so the run pins one field once.
func pinObjective(root llm.Proposal, matrix domain.EvaluationMatrix) objective {
	field := root.ObjectiveField
	if field == "" && len(matrix.Targets) > 0 {
		field = matrix.Targets[0].Field
	}
	agg := root.ObjectiveAggregation
	if !domain.IsAggregation(agg) {
		agg = "count"
	}
	direction := domain.Maximize
	for _, t := range matrix.Targets {
		if t.Field == field {
			direction = t.Direction
			break
		}
	}
	return objective{aggregation: agg, field: field, direction: direction}
}

// improves reports whether value moves the objective in the desired direction
// versus the baseline.
func improves(baseline, value float64, direction domain.TargetDirection) bool {
	if direction == domain.Minimize {
		return value < baseline
	}
	return value > baseline
}

// constraintsSatisfied checks the measured objective value against any hard
// constraint on the objective field. Constraints on other fields cannot be
// verified from a single objective aggregate and are treated as satisfied.
func constraintsSatisfied(constraints []domain.Constraint, field string, value float64) bool {
	for _, c := range constraints {
		if c.Field != field {
			continue
		}
		switch c.Op {
		case domain.LessThan:
			if !(value < c.Value) {
				return false
			}
		case domain.LessThanOrEqual:
			if !(value <= c.Value) {
				return false
			}
		case domain.GreaterThan:
			if !(value > c.Value) {
				return false
			}
		case domain.GreaterThanOrEqual:
			if !(value >= c.Value) {
				return false
			}
		}
	}
	return true
}

// numericValue extracts the objective value keyed by field. A nil value (an
// empty aggregate) or a non-number is not usable.
func numericValue(value map[string]any, field string) (float64, bool) {
	raw, ok := value[field]
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

// concatFilters returns a fresh slice of the parent's effective filters plus the
// candidate's newly proposed filters (cumulative nesting), never aliasing the
// parent slice.
func concatFilters(parent, added []domain.Constraint) []domain.Constraint {
	out := make([]domain.Constraint, 0, len(parent)+len(added))
	out = append(out, parent...)
	out = append(out, added...)
	return out
}

func toSandboxSchema(schema schemaDTO) llm.SandboxSchema {
	cols := make([]llm.SandboxColumn, 0, len(schema.Columns))
	for _, c := range schema.Columns {
		cols = append(cols, llm.SandboxColumn{Name: c.Name, Type: c.Type})
	}
	return llm.SandboxSchema{Columns: cols}
}
