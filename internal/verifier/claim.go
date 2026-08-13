package verifier

import (
	"context"
	"encoding/json"
	"fmt"
	"log"

	"github.com/arborette/arborette/internal/domain"
	"github.com/arborette/arborette/internal/objective"
	"github.com/arborette/arborette/internal/sandboxclient"
)

// VerifyClaim constructs, measures, and dispatches a verify-track goal's claim. It is
// the third dispatch kind beside RunDiscovery and VerifyOne, and the only one that
// starts from a goal rather than from a finding the graph already holds: it reifies
// the claimed segment as an ordinary observational triplet, then verifies that
// triplet like any other.
//
// The claim rides on the goal row rather than the dispatch wire. The launcher seam
// carries only string pairs and a filter conjunction has no encoding there, so the
// claim is loaded here — which also guarantees the claim verified is byte-identical
// to the one intake validated and persisted.
//
// Nothing in this path fails the dispatch. A claim that cannot be constructed —
// absent, no longer grounded in the schema, over a windowed objective, or naming a
// segment with no rows — is reported as that first-class outcome and returns nil,
// because "we could not build your claim" and "we tested your claim and the data does
// not support it" are different answers and the second must never be implied.
func (w *Worker) VerifyClaim(ctx context.Context, goalID, datasourceRef string) error {
	goal, err := w.goals.Get(ctx, goalID)
	if err != nil {
		return fmt.Errorf("get goal %q: %w", goalID, err)
	}
	if len(goal.Claim) == 0 {
		// A cannot-construct goal, or a mis-dispatch. An empty claim is an empty filter
		// conjunction, which measures the global baseline -- reifying that would present
		// the baseline as the analyst's tested claim.
		w.publishVerification(ctx, goalID, map[string]any{
			"type": "claim_skipped", "reason": "goal carries no constructed claim",
		})
		return nil
	}
	var claim domain.ClaimSpec
	if err := json.Unmarshal(goal.Claim, &claim); err != nil {
		return fmt.Errorf("decode stored claim for %q: %w", goalID, err)
	}
	// The goal's data source binds at registration and never churns, so the goal row
	// is the authoritative ref and the dispatched one can only agree or lie. Taking it
	// from the row for the same reason the claim is taken from the row: measuring a
	// goal's claim against another goal's data would persist a finding on this goal's
	// graph that describes neither.
	datasourceRef = goal.DataSourceRef

	introspect, err := w.analyzer.Introspect(ctx, sandboxclient.IntrospectRequest{DataSourceRef: datasourceRef})
	if err != nil {
		return fmt.Errorf("introspect %q: %w", datasourceRef, err)
	}
	if reason := groundingFailure(claim.Filters, introspect.Schema.Columns); reason != "" {
		w.reportClaimFailure(ctx, goalID, reason)
		return nil
	}

	obj, err := objective.Pin(goal.EvaluationMatrix)
	if err != nil {
		return fmt.Errorf("pin objective for %q: %w", goalID, err)
	}
	obj.EntityKeyColumn = goal.EntityKeyColumn
	obj.TimeColumn = goal.TimeColumn
	// A windowed objective's causal effect is out of scope, and so is the naive effect
	// this reification would have to measure: the value is a function of an entity's
	// history, not a cross-section. Report it as a claim that cannot be constructed
	// rather than reifying a number that does not mean what it appears to.
	if domain.HasWindowKind(obj.Expr) {
		w.reportClaimFailure(ctx, goalID, "the goal's objective is windowed, which causal verification does not support")
		return nil
	}

	measured, err := w.measureClaim(ctx, datasourceRef, obj, claim.Filters)
	if err != nil {
		return err
	}
	if measured.segmentN == 0 {
		w.reportClaimFailure(ctx, goalID, "the claimed segment matched no rows in the data source")
		return nil
	}

	interventionID, err := w.reifyClaim(ctx, goal.OptimizationFunctionID, datasourceRef, obj, claim.Filters, measured)
	if err != nil {
		return fmt.Errorf("reify claim for %q: %w", goalID, err)
	}
	// Whether the segment moved the objective the way the analyst said it would is the
	// half of the claim verification cannot answer: the verdict classifies the effect
	// as causal or confounded, never as agreeing or disagreeing with what was
	// asserted. Reporting it here is what makes the claimed direction mean something.
	agrees := domain.DirectionalDelta(measured.segment, measured.baseline, claim.Direction) > 0
	w.publishVerification(ctx, goalID, map[string]any{
		"type": "claim_reified", "intervention_id": interventionID,
		"support":           measured.segmentN,
		"claimed_direction": string(claim.Direction),
		"effect_size":       measured.segment - measured.baseline,
		"agrees_with_claim": agrees,
	})

	// The primary constructed-claim verification is analyst-initiated, so it is exempt
	// from the autonomous budget and bounded by the per-goal in-flight cap alone.
	return w.VerifyOne(ctx, goalID, interventionID, datasourceRef, false)
}

// claimMeasurement is the claimed segment measured against the goal's global
// baseline: both values and the segment's row count.
type claimMeasurement struct {
	baseline float64
	segment  float64
	segmentN int64
}

// measureClaim measures the claimed segment and the goal's global baseline.
//
// The two come from different calls on purpose. The segment's value and support come
// from the same Z-less stratified call VerifyOne's naive-effect path uses, so the
// reified Outcome carries a number computed exactly like every other measurement. The
// baseline cannot come from that call's other arm: that arm is the segment's
// *complement*, and the triplet's baseline State is the goal-wide node the Sleep
// Cycle writes the true unfiltered aggregate into — writing a complement there would
// silently republish the goal's baseline as an out-of-segment number wherever it is
// rendered, and would make this finding's effect size incomparable with every other
// finding's, which are all relative to the global baseline.
func (w *Worker) measureClaim(ctx context.Context, datasourceRef string, obj objective.Objective, filters []domain.Constraint) (claimMeasurement, error) {
	adj := &adjuster{analyzer: w.analyzer, datasourceRef: datasourceRef, cfg: w.cfg}
	resp, err := adj.effectCall(ctx, adjustmentPlan{objective: obj, segment: filters}, nil, 0, 0)
	if err != nil {
		return claimMeasurement{}, fmt.Errorf("measure claim: %w", err)
	}
	// A Z-less effect call groups on nothing, so it answers with exactly one stratum
	// carrying both arms.
	if len(resp.Strata) != 1 {
		return claimMeasurement{}, fmt.Errorf("measure claim: expected one stratum, got %d", len(resp.Strata))
	}
	row := resp.Strata[0]
	if row.SegmentN == 0 {
		return claimMeasurement{segmentN: 0}, nil
	}
	// A segment with rows but an all-NULL objective column aggregates to NULL. Guard
	// before the dereference: the claim dispatch runs in a bare goroutine, so a nil
	// dereference here would take the Verifier process down rather than fail one run.
	if row.SegmentAgg == nil {
		return claimMeasurement{}, fmt.Errorf("measure claim: sandbox returned a null objective aggregate")
	}

	baseline, err := w.measureGlobalBaseline(ctx, datasourceRef, obj)
	if err != nil {
		return claimMeasurement{}, err
	}
	return claimMeasurement{baseline: baseline, segment: *row.SegmentAgg, segmentN: row.SegmentN}, nil
}

// measureGlobalBaseline measures the objective with no filters, the reference every
// derived triplet's effect size is relative to. It is the same call the Sleep Cycle's
// own baseline measurement makes, so the two writers of the shared baseline State
// agree on what the number means.
func (w *Worker) measureGlobalBaseline(ctx context.Context, datasourceRef string, obj objective.Objective) (float64, error) {
	callCtx, cancel := context.WithTimeout(ctx, w.cfg.CallTimeout)
	defer cancel()
	resp, err := w.analyzer.Execute(callCtx, objective.ExecuteRequestFor(datasourceRef, obj, nil))
	if err != nil {
		return 0, fmt.Errorf("measure global baseline: %w", err)
	}
	value, ok := objective.NumericValue(resp.Value, obj.Label)
	if !ok {
		return 0, fmt.Errorf("measure global baseline: %w", objective.ErrNonNumericValue)
	}
	return value, nil
}

// reifyClaim persists the claimed segment as a full observational triplet and returns
// its Intervention id. The ids are the same deterministic scheme the Sleep Cycle's
// write-back mints, so re-dispatching one goal's claim MERGEs over the same nodes
// instead of appending a second copy — which is also what makes the dispatch
// idempotent on the (goal, intervention, graph-version) key downstream.
//
// The triplet is observational, not causal: it records what the segment measures, and
// the verification it triggers is what may later attach causal evidence to it. The
// claim_derived flag marks where the Intervention came from without changing that —
// it stays an ordinary measured finding and participates in the lattice as one.
func (w *Worker) reifyClaim(ctx context.Context, goalID, datasourceRef string, obj objective.Objective, filters []domain.Constraint, measured claimMeasurement) (string, error) {
	canonical, err := domain.CanonicalFilters(filters)
	if err != nil {
		return "", err
	}
	ns := domain.GoalNamespace(goalID)
	stateID := domain.BaselineStateID(ns)
	interventionID := domain.DerivedID(ns, domain.RoleClaim, canonical)
	outcomeID := domain.DerivedID(ns, domain.RoleClaimOutcome, canonical)

	state := domain.State{ID: stateID, GoalID: goalID, Properties: map[string]any{
		domain.PropDataSourceRef:        datasourceRef,
		domain.PropObjectiveAggregation: obj.Aggregation,
		domain.PropObjectiveLabel:       obj.Label,
		domain.PropEffectiveFilters:     nil,
		"value":                         measured.baseline,
	}}
	if err := w.graph.CreateState(ctx, state); err != nil {
		return "", err
	}

	intervention := domain.Intervention{ID: interventionID, GoalID: goalID, Type: domain.InterventionQuery, Properties: map[string]any{
		domain.PropObjectiveAggregation: obj.Aggregation,
		domain.PropObjectiveLabel:       obj.Label,
		domain.PropNewFilters:           filters,
		domain.PropEffectiveFilters:     filters,
		domain.PropSupport:              measured.segmentN,
		"canonical_filter":              canonical,
		domain.PropClaimDerived:         true,
	}}
	if err := w.graph.CreateIntervention(ctx, intervention); err != nil {
		return "", err
	}

	outcome := domain.Outcome{
		ID:                 outcomeID,
		GoalID:             goalID,
		VerificationStatus: domain.VerificationVerified,
		Value:              map[string]any{obj.Label: measured.segment},
		Support:            measured.segmentN,
	}
	if err := w.graph.CreateOutcome(ctx, outcome); err != nil {
		return "", err
	}

	if err := w.graph.CreatePreConditionFor(ctx, stateID, interventionID); err != nil {
		return "", err
	}
	if err := w.graph.CreateProduced(ctx, interventionID, outcomeID, domain.ProducedEdge{
		EffectSize:      measured.segment - measured.baseline,
		Confidence:      1.0,
		EpistemicSource: domain.EpistemicObservational,
	}); err != nil {
		return "", err
	}
	return interventionID, nil
}

// groundingFailure reports why a claim's filters no longer ground in the data source's
// schema, or "" when they do. Intake validated the claim at registration; this is the
// re-check at dispatch time, worded through the same shared builder so the analyst
// reads one message wherever the failure was caught.
func groundingFailure(filters []domain.Constraint, cols []sandboxclient.Column) string {
	names := make([]string, 0, len(cols))
	values := map[string][]string{}
	for _, c := range cols {
		names = append(names, c.Name)
		if len(c.DistinctValues) > 0 {
			values[c.Name] = c.DistinctValues
		}
	}
	return domain.ClaimGroundingError(filters, names, values)
}

// reportClaimFailure delivers the cannot-construct outcome. It is a first-class
// answer, not a fault, so the caller returns nil after it.
//
// One call covers both sinks: the verification-events path streams the event on the
// goal's channel AND records it to the audit trail, so a second reportAudit here
// would file the same failure twice under two actions — and under the wrong category,
// since the discovery audit helper hardcodes its event type.
func (w *Worker) reportClaimFailure(ctx context.Context, goalID, reason string) {
	log.Printf("verifier: claim for goal %q cannot be constructed: %s", goalID, reason)
	w.publishVerification(ctx, goalID, map[string]any{
		"type": domain.ClaimConstructionFailed, "reason": reason,
	})
}
