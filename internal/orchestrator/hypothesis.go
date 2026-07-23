package orchestrator

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"time"

	"github.com/google/uuid"

	"github.com/arborette/arborette/internal/domain"
	"github.com/arborette/arborette/internal/llm"
	"github.com/arborette/arborette/internal/store"
)

var (
	errNonNumericValue    = errors.New("sandbox returned a non-numeric objective value")
	errNoObjective        = errors.New("evaluation matrix has no target to pin as the objective")
	errMissingAggregation = errors.New("evaluation matrix target carries no aggregation; re-register the goal to fit a measurable objective")
)

// loopTimeout bounds a whole hypothesis run. The loop runs on a background
// context (decoupled from the SSE stream), so without a deadline a wedged
// dependency would leak the goroutine indefinitely.
const loopTimeout = 10 * time.Minute

// statusWriteTimeout bounds the run's terminal status write. It runs on a
// context detached from the loop's own deadline/cancellation, so a run whose
// loopTimeout expired still records its terminal status instead of stranding at
// running.
const statusWriteTimeout = 5 * time.Second

// objective is the run's fixed measurement, held constant across the whole tree:
// one aggregation over one value expression, with the direction that decides what
// "improvement" means. label is the rendered key the measured value is carried
// under end to end. Pinning it once is what makes the stop-on-no-improvement
// comparison meaningful — every value compared is the same measurement.
type objective struct {
	aggregation string
	expr        domain.Expression
	label       string
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
	// Create the run row synchronously, before launching the loop, so a client
	// that immediately subscribes and lists always sees a running row rather than
	// racing the goroutine, and a create failure surfaces as a 5xx here.
	runID := uuid.NewString()
	if err := s.runs.Create(r.Context(), runID, goal.OptimizationFunctionID); err != nil {
		log.Printf("orchestrator: create run for %q: %v", goal.OptimizationFunctionID, err)
		writeErr(w, http.StatusInternalServerError, "internal error")
		return
	}
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), loopTimeout)
		defer cancel()
		s.runLoop(ctx, goal, runID)
	}()
	writeJSON(w, http.StatusAccepted, map[string]any{"optimization_function_id": goal.OptimizationFunctionID})
}

// runLoop drives the hypothesis tree: introspect the data source, pin the
// objective and measure the root baseline, then expand each root candidate.
func (s *Server) runLoop(ctx context.Context, goal store.Goal, runID string) {
	id := goal.OptimizationFunctionID
	// termErr holds a terminal (root) failure; nil means the run completed. Only
	// the root-failure sites below set it, so a per-candidate branch failure (a
	// separate function with no access to it) never flips the run to failed.
	var termErr error
	defer func() {
		// A panic unwinds through this defer with termErr still nil; recover so a
		// crashed run is marked failed rather than mislabeled completed, and one
		// run's panic cannot bring down the orchestrator. Otherwise, a
		// timeout/cancellation that fired anywhere in the run -- including
		// mid-expansion, where per-candidate failures are non-terminal -- also
		// means the run did not complete.
		if r := recover(); r != nil {
			termErr = fmt.Errorf("hypothesis loop panicked: %v", r)
		} else if termErr == nil && ctx.Err() != nil {
			termErr = ctx.Err()
		}
		// Detached from the loop's own deadline/cancellation so the write lands
		// even when the loop terminated because that context expired.
		writeCtx, cancel := context.WithTimeout(context.Background(), statusWriteTimeout)
		defer cancel()
		status, reason := store.RunCompleted, ""
		if termErr != nil {
			status, reason = store.RunFailed, termErr.Error()
		}
		if err := s.runs.SetStatus(writeCtx, runID, status, reason); err != nil {
			log.Printf("orchestrator: hypothesis loop %q: set run status: %v", id, err)
		}
		s.hub.Publish(id, Event{Type: "loop_complete"})
		s.hub.Complete(id)
	}()

	obj, err := pinObjective(goal.EvaluationMatrix)
	if err != nil {
		log.Printf("orchestrator: hypothesis loop %q: pin objective: %v", id, err)
		termErr = err
		s.branchFailure(ctx, id, nil, err)
		return
	}

	introspect, err := s.sandbox.Introspect(ctx, IntrospectRequest{DataSourceRef: goal.DataSourceRef})
	if err != nil {
		log.Printf("orchestrator: hypothesis loop %q: introspect: %v", id, err)
		termErr = err
		s.branchFailure(ctx, id, nil, err)
		return
	}
	schema := toSandboxSchema(introspect.Schema)

	root, err := s.claude.ProposeInterventionTree(ctx, goal.GoalText, goal.EvaluationMatrix, schema,
		llm.TreeContext{IsRoot: true, Breadth: defaultBreadth})
	if err != nil {
		log.Printf("orchestrator: hypothesis loop %q: root proposal: %v", id, err)
		termErr = err
		s.branchFailure(ctx, id, nil, err)
		return
	}

	// Root baseline: the objective measured with no filters. Deeper baselines
	// reuse the parent's outcome value (cumulative nesting makes that valid).
	baseResp, err := s.sandbox.Execute(ctx, executeRequestFor(goal, obj, nil))
	if err != nil {
		log.Printf("orchestrator: hypothesis loop %q: root baseline: %v", id, err)
		termErr = err
		s.branchFailure(ctx, id, nil, err)
		return
	}
	baseline, ok := numericValue(baseResp.Value, obj.label)
	if !ok {
		termErr = errNonNumericValue
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

	resp, err := s.sandbox.Execute(ctx, executeRequestFor(goal, obj, effective))
	if err != nil {
		s.branchFailure(ctx, id, effective, err)
		return
	}
	value, ok := numericValue(resp.Value, obj.label)
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
	if !constraintsSatisfied(goal.EvaluationMatrix.Constraints, objectiveField(obj), value) {
		s.branchFailure(ctx, id, effective, errors.New("candidate violates a hard constraint on the objective field"))
		return
	}
	if depth >= defaultDepth {
		return
	}

	child, err := s.claude.ProposeInterventionTree(ctx, goal.GoalText, goal.EvaluationMatrix, schema, llm.TreeContext{
		Breadth:        defaultBreadth,
		ObjectiveLabel: obj.label,
		Direction:      obj.direction,
		ParentFilters:  effective,
		PriorValue:     &value,
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
		"objective_label":       obj.label,
		"effective_filters":     parentFilters,
		"value":                 baseline,
	}}
	if err := s.repo.CreateState(ctx, state); err != nil {
		return err
	}

	interventionID := uuid.NewString()
	intervention := domain.Intervention{ID: interventionID, Type: domain.InterventionQuery, Properties: map[string]any{
		"objective_aggregation": obj.aggregation,
		"objective_label":       obj.label,
		"new_filters":           cand.Filters,
		"effective_filters":     effective,
	}}
	if err := s.repo.CreateIntervention(ctx, intervention); err != nil {
		return err
	}

	outcomeID := uuid.NewString()
	outcome := domain.Outcome{ID: outcomeID, VerificationStatus: domain.VerificationVerified, Value: map[string]any{obj.label: value}}
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

// pinObjective fixes the run's objective from the Evaluation Matrix's first
// target: the aggregation, value expression, direction, and rendered label the
// whole tree measures against. A matrix with no target, or a legacy target with
// no aggregation, is a terminal failure directing re-registration — there is no
// count fallback, because a silently-substituted aggregation would mis-measure.
func pinObjective(matrix domain.EvaluationMatrix) (objective, error) {
	if len(matrix.Targets) == 0 {
		return objective{}, errNoObjective
	}
	t := matrix.Targets[0]
	if t.Aggregation == "" {
		return objective{}, errMissingAggregation
	}
	expr := t.ValueExpression()
	return objective{
		aggregation: t.Aggregation,
		expr:        expr,
		label:       domain.RenderObjectiveLabel(t.Aggregation, expr),
		direction:   t.Direction,
	}, nil
}

// executeRequestFor builds the execute request measuring the pinned objective
// under filters, shared so every call site pins the objective identically.
func executeRequestFor(goal store.Goal, obj objective, filters []domain.Constraint) ExecuteRequest {
	return ExecuteRequest{
		DataSourceRef:   goal.DataSourceRef,
		Type:            domain.InterventionQuery,
		Aggregation:     obj.aggregation,
		Target:          domain.Target{Direction: obj.direction},
		ValueExpression: &obj.expr,
		ObjectiveLabel:  obj.label,
		Filters:         filters,
	}
}

// dryRunObjective validates a fitted matrix by executing its pinned objective
// against the sandbox with no filters — the same request the root baseline runs,
// pinned identically. A nil return means the objective compiles and measures; a
// non-nil error is either a pin failure or the sandbox's execute error.
func (s *Server) dryRunObjective(ctx context.Context, ref string, matrix domain.EvaluationMatrix) error {
	obj, err := pinObjective(matrix)
	if err != nil {
		return err
	}
	_, err = s.sandbox.Execute(ctx, executeRequestFor(store.Goal{DataSourceRef: ref}, obj, nil))
	return err
}

// isObjectiveValidationFailure reports whether a dry-run error is a repairable
// objective-fit failure: a missing/absent aggregation the model can supply, or a
// sandbox 400 compile/type error. A sandbox 4xx≠400, 5xx, or transport error is a
// fault, not an unfixable objective.
func isObjectiveValidationFailure(err error) bool {
	if errors.Is(err, errNoObjective) || errors.Is(err, errMissingAggregation) {
		return true
	}
	var se *SandboxError
	return errors.As(err, &se) && se.Status == http.StatusBadRequest
}

// objectiveField is the constraint field a hard constraint can be checked
// against: the objective's column only when it is a bare ColumnRef. A compound
// expression has no single column, so it yields "" and matches no constraint
// (expression-based constraints are deferred).
func objectiveField(obj objective) string {
	if obj.expr.Kind == domain.ColumnRefKind {
		return obj.expr.Column
	}
	return ""
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

// numericValue extracts the objective value keyed by the objective label. A nil
// value (an empty aggregate) or a non-number is not usable.
func numericValue(value map[string]any, label string) (float64, bool) {
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
