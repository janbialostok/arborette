package verifier

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/arborette/arborette/internal/domain"
	"github.com/arborette/arborette/internal/sandboxclient"
	"github.com/arborette/arborette/internal/store"
	"github.com/arborette/arborette/internal/verifier/groundtruth"
)

// claimGoal is a verify-track goal carrying the given claim, stored the way intake
// persists it.
func claimGoal(t *testing.T, filters []domain.Constraint) store.Goal {
	t.Helper()
	goal := maximizeYGoal()
	goal.OptimizationFunctionID = "goal"
	goal.Track = store.TrackVerify
	if filters == nil {
		return goal
	}
	encoded, err := json.Marshal(domain.ClaimSpec{Filters: filters, Direction: domain.Maximize})
	if err != nil {
		t.Fatalf("marshal claim: %v", err)
	}
	goal.Claim = encoded
	return goal
}

// newClaimWorker wires a Worker over the ground-truth dataset with a committed graph,
// so VerifyClaim measures real numbers and the verification it dispatches proceeds.
func newClaimWorker(t *testing.T, goal store.Goal, g *fakeGraph, v verificationStore, audit *fakeAudit) *Worker {
	t.Helper()
	ds := groundtruth.Generate(groundtruth.DefaultConfig())
	return newVerifyWorker(newEffectAnalyzer(ds), g, goal, v, audit)
}

// TestVerifyClaimReifiesAndDispatches: a constructed claim is persisted as a full
// observational triplet measured against the goal's global baseline, and the
// verification it dispatches is exempt from the autonomous budget — the analyst asked
// for this one, so automation's spend must not be able to refuse it.
func TestVerifyClaimReifiesAndDispatches(t *testing.T) {
	filters := thresholdSegment(groundtruth.ColA, domain.GreaterThanOrEqual, 0)
	goal := claimGoal(t, filters)
	g := &fakeGraph{found: true, graphValue: groundGraph()}
	v := &fakeVerifications{accepted: false}

	if err := newClaimWorker(t, goal, g, v, &fakeAudit{}).VerifyClaim(context.Background(), "goal", "ref"); err != nil {
		t.Fatalf("VerifyClaim: %v", err)
	}

	if len(g.states) != 1 || len(g.interventions) != 1 || len(g.outcomes) != 1 ||
		len(g.preConditions) != 1 || len(g.produced) != 1 {
		t.Fatalf("claim was not reified as a complete triplet: states=%d interventions=%d outcomes=%d pre=%d produced=%d",
			len(g.states), len(g.interventions), len(g.outcomes), len(g.preConditions), len(g.produced))
	}
	intervention := g.interventions[0]
	if intervention.SleepDerived {
		t.Fatalf("a claim triplet is not sleep-derived")
	}
	if claimDerived, _ := intervention.Properties[domain.PropClaimDerived].(bool); !claimDerived {
		t.Fatalf("intervention is missing the claim_derived flag: %+v", intervention.Properties)
	}
	if g.produced[0].edge.EpistemicSource != domain.EpistemicObservational {
		t.Fatalf("reified edge = %q, want observational", g.produced[0].edge.EpistemicSource)
	}
	// The edge must link the reified pair, not just carry the right properties.
	if g.produced[0].interventionID != intervention.ID || g.produced[0].outcomeID != g.outcomes[0].ID {
		t.Fatalf("produced edge links (%q, %q), want the reified intervention and outcome",
			g.produced[0].interventionID, g.produced[0].outcomeID)
	}
	if g.preConditions[0] != [2]string{g.states[0].ID, intervention.ID} {
		t.Fatalf("precondition links %v, want the baseline state to the reified intervention", g.preConditions[0])
	}
	if outcome := g.outcomes[0]; outcome.Support == 0 || outcome.VerificationStatus != domain.VerificationVerified {
		t.Fatalf("reified outcome wrong: %+v", outcome)
	}

	if len(v.dispatched) != 1 {
		t.Fatalf("dispatched %d verifications, want 1", len(v.dispatched))
	}
	if v.dispatched[0].Budgeted {
		t.Fatalf("the primary claim verification must be budget-exempt")
	}
	if v.dispatched[0].InterventionID != intervention.ID {
		t.Fatalf("dispatched %q, want the reified intervention %q", v.dispatched[0].InterventionID, intervention.ID)
	}
}

// TestVerifyClaimIDsAreDeterministic: re-dispatching one goal's claim mints the same
// ids, so the reification MERGEs over the same nodes instead of appending a second
// copy — and the dispatch coalesces on the same key rather than re-running.
func TestVerifyClaimIDsAreDeterministic(t *testing.T) {
	filters := thresholdSegment(groundtruth.ColA, domain.GreaterThanOrEqual, 0)
	goal := claimGoal(t, filters)
	g := &fakeGraph{found: true, graphValue: groundGraph()}
	w := newClaimWorker(t, goal, g, &fakeVerifications{accepted: false}, &fakeAudit{})

	for i := 0; i < 2; i++ {
		if err := w.VerifyClaim(context.Background(), "goal", "ref"); err != nil {
			t.Fatalf("VerifyClaim %d: %v", i, err)
		}
	}
	if g.interventions[0].ID != g.interventions[1].ID ||
		g.outcomes[0].ID != g.outcomes[1].ID ||
		g.states[0].ID != g.states[1].ID {
		t.Fatalf("claim ids are not deterministic across runs: %q/%q", g.interventions[0].ID, g.interventions[1].ID)
	}
	// The claim's Outcome must not collide with the Sleep Cycle's outcome for the same
	// segment: both are derived from one canonical filter under one goal namespace.
	ns := domain.GoalNamespace("goal")
	canonical, err := domain.CanonicalFilters(filters)
	if err != nil {
		t.Fatalf("canonical filters: %v", err)
	}
	if g.outcomes[0].ID == domain.DerivedID(ns, domain.RoleOutcome, canonical) {
		t.Fatalf("the claim outcome collides with the sleep-derived outcome for the same segment")
	}
}

// TestVerifyClaimCannotConstruct: a claim that no longer grounds in the schema, or a
// goal carrying none at all, reports the cannot-construct outcome and writes nothing.
// Reifying either would present something the analyst never claimed as their tested
// claim.
func TestVerifyClaimCannotConstruct(t *testing.T) {
	cases := map[string]struct {
		filters    []domain.Constraint
		wantReason string
	}{
		"unknown column": {
			[]domain.Constraint{{Field: "no_such_column", Op: domain.GreaterThan, Value: 1}},
			"not in the data source",
		},
		"unknown value": {
			[]domain.Constraint{{Field: groundtruth.ColB, Op: domain.Equal, Operand: literal("b9")}},
			"not present in their column",
		},
	}
	for name, tc := range cases {
		filters := tc.filters
		t.Run(name, func(t *testing.T) {
			g := &fakeGraph{found: true, graphValue: groundGraph()}
			v := &fakeVerifications{accepted: true}
			audit := &fakeAudit{}

			if err := newClaimWorker(t, claimGoal(t, filters), g, v, audit).VerifyClaim(context.Background(), "goal", "ref"); err != nil {
				t.Fatalf("a claim that cannot be constructed is not a fault: %v", err)
			}
			if len(g.interventions) != 0 || v.dispatchCalls != 0 {
				t.Fatalf("an unconstructable claim must not reify or dispatch")
			}
			if len(audit.events) != 1 || audit.events[0]["type"] != "claim_construction_failed" {
				t.Fatalf("events = %v, want one claim_construction_failed", audit.events)
			}
			// The reason discriminates this from a segment that simply matched no rows,
			// which the stand-in analyzer produces for an ungrounded filter too -- and it
			// is the only assertion on the shared analyst-facing wording.
			reason, _ := audit.events[0]["reason"].(string)
			if !strings.Contains(reason, tc.wantReason) {
				t.Fatalf("reason = %q, want it to name %q", reason, tc.wantReason)
			}
		})
	}

	t.Run("no claim", func(t *testing.T) {
		g := &fakeGraph{found: true, graphValue: groundGraph()}
		v := &fakeVerifications{accepted: true}
		audit := &fakeAudit{}

		if err := newClaimWorker(t, claimGoal(t, nil), g, v, audit).VerifyClaim(context.Background(), "goal", "ref"); err != nil {
			t.Fatalf("a goal with no claim is not a fault: %v", err)
		}
		if len(g.interventions) != 0 || v.dispatchCalls != 0 {
			t.Fatalf("a goal with no claim must not reify the global baseline as one")
		}
		if len(audit.events) != 1 || audit.events[0]["type"] != "claim_skipped" {
			t.Fatalf("a mis-dispatch is a skip, not a construction failure: %v", audit.events)
		}
	})
}

func literal(s string) *domain.LiteralValue {
	return &domain.LiteralValue{String: &s}
}

// TestVerifyClaimCannotConstructMeasurementCases covers the two cannot-construct
// outcomes the measurement stage produces. Both must stay distinct from "we tested
// your claim and the data does not support it": a windowed objective's value is a
// function of an entity's history rather than a cross-section, and a segment matching
// no rows supports no claim at all.
func TestVerifyClaimCannotConstructMeasurementCases(t *testing.T) {
	t.Run("windowed objective", func(t *testing.T) {
		goal := claimGoal(t, thresholdSegment(groundtruth.ColA, domain.GreaterThanOrEqual, 0))
		inner := domain.Expression{Kind: domain.ColumnRefKind, Column: groundtruth.ColY}
		goal.EvaluationMatrix.Targets[0].Value = &domain.Expression{
			Kind: domain.TrailingAggregateKind, WindowAgg: "avg", WindowSize: 3, Inner: &inner,
		}
		goal.EntityKeyColumn, goal.TimeColumn = groundtruth.ColRegion, groundtruth.ColY

		g := &fakeGraph{found: true, graphValue: groundGraph()}
		v := &fakeVerifications{accepted: true}
		audit := &fakeAudit{}
		if err := newClaimWorker(t, goal, g, v, audit).VerifyClaim(context.Background(), "goal", "ref"); err != nil {
			t.Fatalf("a windowed objective is not a fault: %v", err)
		}
		if len(g.interventions) != 0 || v.dispatchCalls != 0 {
			t.Fatalf("a windowed objective must not reify a cross-sectional measurement")
		}
		if len(audit.events) != 1 || audit.events[0]["type"] != "claim_construction_failed" {
			t.Fatalf("events = %v, want one claim_construction_failed", audit.events)
		}
	})

	t.Run("zero-row segment", func(t *testing.T) {
		// A threshold no row clears: the segment is schema-valid but empty.
		goal := claimGoal(t, thresholdSegment(groundtruth.ColA, domain.GreaterThan, 1e9))
		g := &fakeGraph{found: true, graphValue: groundGraph()}
		v := &fakeVerifications{accepted: true}
		audit := &fakeAudit{}
		if err := newClaimWorker(t, goal, g, v, audit).VerifyClaim(context.Background(), "goal", "ref"); err != nil {
			t.Fatalf("an empty segment is not a fault: %v", err)
		}
		if len(g.outcomes) != 0 || v.dispatchCalls != 0 {
			t.Fatalf("an empty segment must not reify an outcome with no support")
		}
		if len(audit.events) != 1 || audit.events[0]["type"] != "claim_construction_failed" {
			t.Fatalf("events = %v, want one claim_construction_failed", audit.events)
		}
	})
}

// TestVerifyClaimUsesTheGoalsDataSource: the goal's data source binds at registration
// and never churns, so it is authoritative and a dispatched ref can only agree or lie.
// Measuring against another dataset would persist a finding on this goal's graph that
// describes neither.
func TestVerifyClaimUsesTheGoalsDataSource(t *testing.T) {
	goal := claimGoal(t, thresholdSegment(groundtruth.ColA, domain.GreaterThanOrEqual, 0))
	goal.DataSourceRef = "datasources/the-goals-own.csv"
	g := &fakeGraph{found: true, graphValue: groundGraph()}

	if err := newClaimWorker(t, goal, g, &fakeVerifications{accepted: false}, &fakeAudit{}).
		VerifyClaim(context.Background(), "goal", "datasources/someone-elses.csv"); err != nil {
		t.Fatalf("VerifyClaim: %v", err)
	}
	if len(g.states) != 1 {
		t.Fatalf("claim was not reified")
	}
	if ref := g.states[0].Properties[domain.PropDataSourceRef]; ref != goal.DataSourceRef {
		t.Fatalf("reified against %q, want the goal's own %q", ref, goal.DataSourceRef)
	}
}

// TestVerifyClaimReportsDirectionAgreement: the verdict says whether an effect is
// causal, never whether it matches what the analyst asserted. Reporting agreement is
// what makes the claimed direction mean something.
func TestVerifyClaimReportsDirectionAgreement(t *testing.T) {
	filters := thresholdSegment(groundtruth.ColA, domain.GreaterThanOrEqual, 0)
	// A above its mean raises Y, so a maximize claim agrees and a minimize claim does not.
	for _, tc := range []struct {
		direction domain.TargetDirection
		want      bool
	}{{domain.Maximize, true}, {domain.Minimize, false}} {
		t.Run(string(tc.direction), func(t *testing.T) {
			goal := claimGoal(t, filters)
			encoded, err := json.Marshal(domain.ClaimSpec{Filters: filters, Direction: tc.direction})
			if err != nil {
				t.Fatalf("marshal claim: %v", err)
			}
			goal.Claim = encoded

			audit := &fakeAudit{}
			g := &fakeGraph{found: true, graphValue: groundGraph()}
			if err := newClaimWorker(t, goal, g, &fakeVerifications{accepted: false}, audit).
				VerifyClaim(context.Background(), "goal", "ref"); err != nil {
				t.Fatalf("VerifyClaim: %v", err)
			}
			var measured map[string]any
			for _, ev := range audit.events {
				if ev["type"] == "claim_reified" {
					measured = ev
				}
			}
			if measured == nil {
				t.Fatalf("no claim_reified event: %v", audit.events)
			}
			if measured["agrees_with_claim"] != tc.want {
				t.Fatalf("agrees_with_claim = %v, want %v (effect %v)", measured["agrees_with_claim"], tc.want, measured["effect_size"])
			}
		})
	}
}

// TestVerifyClaimBaselineIsGlobalNotTheComplement pins the number the reified
// triplet's baseline State carries. That node is the goal-wide baseline the Sleep
// Cycle also writes, and its properties say it is unfiltered, so it must hold the
// objective over the whole data source -- not the claimed segment's complement, which
// is what the effect call's other arm measures. Getting this wrong republishes the
// goal's baseline as an out-of-segment number and makes the claim's effect size
// incomparable with every other finding's.
func TestVerifyClaimBaselineIsGlobalNotTheComplement(t *testing.T) {
	filters := thresholdSegment(groundtruth.ColA, domain.GreaterThanOrEqual, 0)
	goal := claimGoal(t, filters)
	g := &fakeGraph{found: true, graphValue: groundGraph()}
	ds := groundtruth.Generate(groundtruth.DefaultConfig())
	a := newEffectAnalyzer(ds)

	if err := newVerifyWorker(a, g, goal, &fakeVerifications{accepted: false}, &fakeAudit{}).
		VerifyClaim(context.Background(), "goal", "ref"); err != nil {
		t.Fatalf("VerifyClaim: %v", err)
	}
	if len(g.states) != 1 {
		t.Fatalf("claim was not reified")
	}
	got, ok := g.states[0].Properties["value"].(float64)
	if !ok {
		t.Fatalf("baseline state carries no numeric value: %+v", g.states[0].Properties)
	}

	global := meanOver(ds.Rows, groundtruth.ColY, nil)
	complement := meanOver(ds.Rows, groundtruth.ColY, func(r groundtruth.Row) bool {
		return !matchConstraints(r, filters)
	})
	if !closeEnough(got, global) {
		t.Fatalf("baseline = %v, want the global %v", got, global)
	}
	// Guards the assertion itself: if the two ever coincided the test would pass
	// against the complement too, and pin nothing.
	if closeEnough(global, complement) {
		t.Fatalf("fixture is degenerate: global and complement baselines both %v", global)
	}
}

func meanOver(rows []groundtruth.Row, col string, keep func(groundtruth.Row) bool) float64 {
	var sum float64
	var n int
	for _, r := range rows {
		if keep != nil && !keep(r) {
			continue
		}
		sum += numVal(r, col)
		n++
	}
	if n == 0 {
		return 0
	}
	return sum / float64(n)
}

func closeEnough(a, b float64) bool {
	d := a - b
	return d < 1e-9 && d > -1e-9
}

// nullAggAnalyzer answers the effect call with rows in the segment but a NULL
// objective aggregate -- what an all-NULL objective column produces.
type nullAggAnalyzer struct{ analyzer }

func (n nullAggAnalyzer) Analyze(context.Context, sandboxclient.AnalyzeRequest) (sandboxclient.AnalyzeResponse, error) {
	return sandboxclient.AnalyzeResponse{Strata: []sandboxclient.StratumRow{{N: 10, SegmentN: 4, BaselineN: 6}}}, nil
}

// twoStrataAnalyzer answers a Z-less effect call with more than the one stratum it
// can produce.
type twoStrataAnalyzer struct{ analyzer }

func (t twoStrataAnalyzer) Analyze(context.Context, sandboxclient.AnalyzeRequest) (sandboxclient.AnalyzeResponse, error) {
	agg := 1.0
	return sandboxclient.AnalyzeResponse{Strata: []sandboxclient.StratumRow{
		{N: 5, SegmentN: 2, SegmentAgg: &agg, BaselineN: 3, BaselineAgg: &agg},
		{N: 5, SegmentN: 2, SegmentAgg: &agg, BaselineN: 3, BaselineAgg: &agg},
	}}, nil
}

// TestVerifyClaimMeasurementGuards: a measurement the claim path cannot read fails the
// dispatch rather than reifying a number or dereferencing a nil. The claim dispatch
// runs in a bare goroutine with no recover, so a nil aggregate reaching the
// dereference would take the Verifier process down instead of failing one run.
func TestVerifyClaimMeasurementGuards(t *testing.T) {
	filters := thresholdSegment(groundtruth.ColA, domain.GreaterThanOrEqual, 0)
	ds := groundtruth.Generate(groundtruth.DefaultConfig())
	cases := map[string]analyzer{
		"null aggregate":  nullAggAnalyzer{newEffectAnalyzer(ds)},
		"multiple strata": twoStrataAnalyzer{newEffectAnalyzer(ds)},
	}
	for name, a := range cases {
		t.Run(name, func(t *testing.T) {
			g := &fakeGraph{found: true, graphValue: groundGraph()}
			v := &fakeVerifications{accepted: true}
			err := newVerifyWorker(a, g, claimGoal(t, filters), v, &fakeAudit{}).
				VerifyClaim(context.Background(), "goal", "ref")
			if err == nil {
				t.Fatalf("an unreadable measurement must fail the dispatch")
			}
			if len(g.interventions) != 0 || v.dispatchCalls != 0 {
				t.Fatalf("nothing may be reified or dispatched from an unreadable measurement")
			}
		})
	}
}
