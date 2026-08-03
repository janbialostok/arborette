package verifier

import (
	"context"
	"math/rand"
	"strconv"
	"testing"
	"time"

	"github.com/arborette/arborette/internal/domain"
	"github.com/arborette/arborette/internal/sandboxclient"
	"github.com/arborette/arborette/internal/store"
	"github.com/arborette/arborette/internal/verifier/groundtruth"
)

// effectAnalyzer computes stratified_effect / sampled_effect responses in-process
// from the planted ground-truth rows, matching the sandbox's semantics (complete-case
// grouping over the adjustment columns, per-arm objective aggregate, reservoir
// subsampling, synthetic random stratifier). It supports the avg objective over a
// bare column reference — the ground-truth objective — and threshold/equality
// segments. Introspect returns the schema so the placebo refutation can draw a
// pseudo-segment. Discovery is skipped in the verification tests (a committed graph is
// seeded), so contingency/moments are not implemented.
type effectAnalyzer struct {
	rows []groundtruth.Row
	rng  *rand.Rand
}

func newEffectAnalyzer(ds *groundtruth.Dataset) *effectAnalyzer {
	return &effectAnalyzer{rows: ds.Rows, rng: rand.New(rand.NewSource(7))}
}

func (e *effectAnalyzer) Introspect(_ context.Context, _ sandboxclient.IntrospectRequest) (sandboxclient.IntrospectResponse, error) {
	return sandboxclient.IntrospectResponse{Schema: sandboxclient.Schema{Kind: "tabular", Columns: []sandboxclient.Column{
		{Name: groundtruth.ColZ, Type: "DOUBLE"},
		{Name: groundtruth.ColX, Type: "DOUBLE"},
		{Name: groundtruth.ColA, Type: "DOUBLE"},
		{Name: groundtruth.ColB, Type: "VARCHAR", DistinctValues: []string{"b0", "b1", "b2"}},
		{Name: groundtruth.ColC, Type: "BOOLEAN", DistinctValues: []string{"true", "false"}},
		{Name: groundtruth.ColY, Type: "DOUBLE"},
		{Name: groundtruth.ColRegion, Type: "VARCHAR", DistinctValues: []string{"north", "south", "east", "west"}},
	}}}, nil
}

func (e *effectAnalyzer) Analyze(_ context.Context, req sandboxclient.AnalyzeRequest) (sandboxclient.AnalyzeResponse, error) {
	if req.Kind != sandboxclient.AnalyzeStratifiedEffect && req.Kind != sandboxclient.AnalyzeSampledEffect {
		return sandboxclient.AnalyzeResponse{}, nil
	}
	rows := e.rows
	if req.SampleFraction > 0 {
		rows = e.subsample(rows, req.SampleFraction)
	}

	cuts := make([][]float64, len(req.Adjust))
	for i, c := range req.Adjust {
		if c.Bins > 0 {
			cuts[i] = quantileCuts(colValues(rows, c.Name), c.Bins)
		}
	}
	valueCol := req.ValueExpression.Column

	type acc struct {
		n, segN, baseN  int64
		segSum, baseSum float64
		values          []string
	}
	strata := map[string]*acc{}
	var order []string
	for _, r := range rows {
		vals := make([]string, 0, len(req.Adjust)+1)
		for i, c := range req.Adjust {
			if c.Bins > 0 {
				vals = append(vals, bucketLabel(numVal(r, c.Name), cuts[i]))
			} else {
				vals = append(vals, catVal(r, c.Name))
			}
		}
		if req.RandomStratifierBins > 0 {
			vals = append(vals, strconv.Itoa(e.rng.Intn(req.RandomStratifierBins)))
		}
		key := joinKey(vals)
		a := strata[key]
		if a == nil {
			a = &acc{values: vals}
			strata[key] = a
			order = append(order, key)
		}
		a.n++
		v := numVal(r, valueCol)
		if matchConstraints(r, req.Segment) {
			a.segN++
			a.segSum += v
		} else {
			a.baseN++
			a.baseSum += v
		}
	}

	out := make([]sandboxclient.StratumRow, 0, len(order))
	for _, k := range order {
		a := strata[k]
		row := sandboxclient.StratumRow{Values: a.values, N: a.n, SegmentN: a.segN, BaselineN: a.baseN}
		if a.segN > 0 {
			avg := a.segSum / float64(a.segN)
			row.SegmentAgg = &avg
		}
		if a.baseN > 0 {
			avg := a.baseSum / float64(a.baseN)
			row.BaselineAgg = &avg
		}
		out = append(out, row)
	}
	return sandboxclient.AnalyzeResponse{Kind: req.Kind, Strata: out}, nil
}

func (e *effectAnalyzer) subsample(rows []groundtruth.Row, fraction float64) []groundtruth.Row {
	out := make([]groundtruth.Row, 0, int(float64(len(rows))*fraction)+1)
	for _, r := range rows {
		if e.rng.Float64() < fraction {
			out = append(out, r)
		}
	}
	return out
}

func colValues(rows []groundtruth.Row, col string) []float64 {
	out := make([]float64, len(rows))
	for i, r := range rows {
		out[i] = numVal(r, col)
	}
	return out
}

// matchConstraints reports whether a row satisfies every segment constraint —
// numeric thresholds against numeric columns, equality against categorical columns.
func matchConstraints(r groundtruth.Row, cs []domain.Constraint) bool {
	for _, c := range cs {
		if !matchOne(r, c) {
			return false
		}
	}
	return true
}

func matchOne(r groundtruth.Row, c domain.Constraint) bool {
	switch {
	case c.IsNumericThresholdOp():
		v := numVal(r, c.Field)
		switch c.Op {
		case domain.LessThan:
			return v < c.Value
		case domain.LessThanOrEqual:
			return v <= c.Value
		case domain.GreaterThan:
			return v > c.Value
		case domain.GreaterThanOrEqual:
			return v >= c.Value
		}
	case c.IsEqualityOp():
		s := catVal(r, c.Field)
		want := ""
		if c.Operand != nil && c.Operand.String != nil {
			want = *c.Operand.String
		}
		if c.Op == domain.NotEqual {
			return s != want
		}
		return s == want
	}
	return false
}

// fakeVerifications is the leased-record seam for the VerifyOne unit tests, returning
// programmed dispatch/complete outcomes and counting heartbeats.
type fakeVerifications struct {
	accepted       bool
	dispatchErr    error
	completeHeld   bool
	dispatchCalls  int
	heartbeats     int
	completeCalls  int
	completeStatus string
	naive          *float64
	adjusted       *float64
	adjustmentSet  []string
	refScore       *float64
	confidence     *float64
}

func (f *fakeVerifications) DispatchAccept(_ context.Context, rec store.CausalVerification) (store.CausalVerification, bool, error) {
	f.dispatchCalls++
	if f.dispatchErr != nil {
		return store.CausalVerification{}, false, f.dispatchErr
	}
	rec.Status = store.CausalStatusPending
	return rec, f.accepted, nil
}

func (f *fakeVerifications) Heartbeat(_ context.Context, _ string) error {
	f.heartbeats++
	return nil
}

func (f *fakeVerifications) Complete(_ context.Context, _, status string, naive, adjusted *float64, adjustmentSet []string, refScore, confidence *float64) (bool, error) {
	f.completeCalls++
	f.completeStatus = status
	f.naive, f.adjusted, f.adjustmentSet, f.refScore, f.confidence = naive, adjusted, adjustmentSet, refScore, confidence
	return f.completeHeld, nil
}

// groundGraph is the planted causal structure seeded as the committed graph the
// verification tests derive adjustment sets from: Z confounds X and Y, A causes Y
// directly, and B and C cause Y (the interaction).
func groundGraph() domain.CausalGraph {
	cols := []domain.DataColumn{
		{Name: groundtruth.ColZ, Kind: "numeric"},
		{Name: groundtruth.ColX, Kind: "numeric"},
		{Name: groundtruth.ColA, Kind: "numeric"},
		{Name: groundtruth.ColY, Kind: "numeric"},
		{Name: groundtruth.ColB, Kind: "categorical"},
		{Name: groundtruth.ColC, Kind: "categorical"},
		{Name: groundtruth.ColRegion, Kind: "categorical"},
	}
	edge := func(cause, effect string) domain.CausalEdge {
		a, b := domain.CanonicalColumnPair(cause, effect)
		return domain.CausalEdge{
			ColA: a, ColB: b, Version: 1,
			Direction:  domain.DirectionForCause(cause, effect),
			Provenance: domain.ProvenanceStatistical,
			Confidence: 1,
			Status:     domain.EdgeTested,
		}
	}
	edges := []domain.CausalEdge{
		edge(groundtruth.ColZ, groundtruth.ColX),
		edge(groundtruth.ColZ, groundtruth.ColY),
		edge(groundtruth.ColA, groundtruth.ColY),
		edge(groundtruth.ColB, groundtruth.ColY),
		edge(groundtruth.ColC, groundtruth.ColY),
	}
	return domain.CausalGraph{Columns: cols, Edges: edges, Meta: domain.CausalGraphMeta{Version: 1}}
}

// finding builds an Intervention carrying a segment as its effective-filters property,
// the shape GetIntervention returns.
func finding(id string, filters []domain.Constraint) domain.Intervention {
	return domain.Intervention{
		ID:         id,
		Type:       domain.InterventionQuery,
		Properties: map[string]any{domain.PropEffectiveFilters: filters},
	}
}

// maximizeYGoal is the ground-truth goal: maximize the average of Y.
func maximizeYGoal() store.Goal {
	return store.Goal{
		GoalText: "maximize Y",
		EvaluationMatrix: domain.EvaluationMatrix{
			Targets: []domain.Target{{Field: groundtruth.ColY, Direction: domain.Maximize, Aggregation: "avg"}},
		},
	}
}

// verifyConfig is the verification tuning the tests run under — the shipped defaults.
func verifyConfig() Config {
	c := regressionConfig()
	c.SupportFloor = 20
	c.CollapseRatio = 0.5
	c.RefutationK = 20
	c.RefutationTau = 0.8
	c.SampleFraction = 0.7
	c.StabilityBand = 0.5
	c.RandomStratifierBins = 4
	return c
}

// newVerifyWorker wires a Worker for the verification tests with a seeded committed
// graph, so RunDiscovery short-circuits and the analyzer only serves effect calls.
func newVerifyWorker(a analyzer, g *fakeGraph, goal store.Goal, v verificationStore, audit *fakeAudit) *Worker {
	w := NewWorker(a, g, nil, &fakeLocker{acquired: true}, fakeGoalReader{goal: goal}, audit, v, verifyConfig(), 0, time.Millisecond)
	w.pollMin = time.Millisecond
	w.pollMax = 2 * time.Millisecond
	return w
}

func thresholdSegment(field string, op domain.ConstraintOp, value float64) []domain.Constraint {
	return []domain.Constraint{{Field: field, Op: op, Value: value}}
}

// TestVerifyOneCoalescedReturnsEarly: a DispatchAccept that coalesces (accepted=false)
// makes VerifyOne return with no analyze calls and no SSE/audit frames.
func TestVerifyOneCoalescedReturnsEarly(t *testing.T) {
	ds := groundtruth.Generate(groundtruth.DefaultConfig())
	a := newEffectAnalyzer(ds)
	g := &fakeGraph{found: true, graphValue: groundGraph(), intervention: finding("i", thresholdSegment(groundtruth.ColA, domain.GreaterThanOrEqual, 0))}
	audit := &fakeAudit{}
	v := &fakeVerifications{accepted: false, completeHeld: true}
	w := newVerifyWorker(a, g, maximizeYGoal(), v, audit)

	if err := w.VerifyOne(context.Background(), "goal", "i", "ref", true); err != nil {
		t.Fatalf("VerifyOne: %v", err)
	}
	if v.completeCalls != 0 {
		t.Fatalf("a coalesced dispatch must not complete, got %d completes", v.completeCalls)
	}
	if len(audit.events) != 0 {
		t.Fatalf("a coalesced dispatch must publish nothing, got %d events", len(audit.events))
	}
	if g.causalWrites != 0 || g.supersedes != 0 {
		t.Fatalf("a coalesced dispatch must not touch the graph")
	}
}

// TestVerifyOneUnsupportedObjective: a windowed objective short-circuits to
// unsupported_objective, and the non-confirming outcome supersedes rather than writes.
func TestVerifyOneUnsupportedObjective(t *testing.T) {
	ds := groundtruth.Generate(groundtruth.DefaultConfig())
	a := newEffectAnalyzer(ds)
	g := &fakeGraph{found: true, graphValue: groundGraph(), intervention: finding("i", thresholdSegment(groundtruth.ColA, domain.GreaterThanOrEqual, 0))}
	goal := maximizeYGoal()
	// A trailing-aggregate objective is windowed → unsupported.
	inner := domain.Expression{Kind: domain.ColumnRefKind, Column: groundtruth.ColY}
	goal.EvaluationMatrix.Targets[0].Value = &domain.Expression{Kind: domain.TrailingAggregateKind, WindowAgg: "avg", WindowSize: 3, Inner: &inner}
	goal.EntityKeyColumn, goal.TimeColumn = groundtruth.ColRegion, groundtruth.ColY
	v := &fakeVerifications{accepted: true, completeHeld: true}
	w := newVerifyWorker(a, g, goal, v, &fakeAudit{})

	if err := w.VerifyOne(context.Background(), "goal", "i", "ref", true); err != nil {
		t.Fatalf("VerifyOne: %v", err)
	}
	if v.completeStatus != string(outcomeUnsupportedObjective) {
		t.Fatalf("status = %q, want unsupported_objective", v.completeStatus)
	}
	if g.causalWrites != 0 || g.supersedes != 1 {
		t.Fatalf("unsupported must supersede (writes=%d supersedes=%d)", g.causalWrites, g.supersedes)
	}
}

// TestVerifyOneNotIdentifiableUnorientedEdge: an unoriented edge incident to the
// treatment column makes the effect not identifiable before any analyze call.
func TestVerifyOneNotIdentifiableUnorientedEdge(t *testing.T) {
	ds := groundtruth.Generate(groundtruth.DefaultConfig())
	a := newEffectAnalyzer(ds)
	gr := groundGraph()
	// Leave the A–Y edge undirected: A is the treatment, so its parents are unknown.
	for i := range gr.Edges {
		if gr.Edges[i].ColA == groundtruth.ColA || gr.Edges[i].ColB == groundtruth.ColA {
			gr.Edges[i].Direction = domain.DirectionUndirected
		}
	}
	g := &fakeGraph{found: true, graphValue: gr, intervention: finding("i", thresholdSegment(groundtruth.ColA, domain.GreaterThanOrEqual, 0))}
	v := &fakeVerifications{accepted: true, completeHeld: true}
	w := newVerifyWorker(a, g, maximizeYGoal(), v, &fakeAudit{})

	if err := w.VerifyOne(context.Background(), "goal", "i", "ref", true); err != nil {
		t.Fatalf("VerifyOne: %v", err)
	}
	if v.completeStatus != string(outcomeNotIdentifiable) {
		t.Fatalf("status = %q, want not_identifiable", v.completeStatus)
	}
	if g.causalWrites != 0 {
		t.Fatalf("not_identifiable must not write a causal edge")
	}
}

// TestVerifyOneNotIdentifiablePositivity: a support floor above the whole sample
// forces a positivity failure — every stratum falls below the floor — reported as
// not_identifiable.
func TestVerifyOneNotIdentifiablePositivity(t *testing.T) {
	ds := groundtruth.Generate(groundtruth.DefaultConfig())
	a := newEffectAnalyzer(ds)
	g := &fakeGraph{found: true, graphValue: groundGraph(), intervention: finding("i", thresholdSegment(groundtruth.ColA, domain.GreaterThanOrEqual, 0))}
	v := &fakeVerifications{accepted: true, completeHeld: true}
	w := newVerifyWorker(a, g, maximizeYGoal(), v, &fakeAudit{})
	w.cfg.SupportFloor = 1 << 30 // no stratum can clear this

	if err := w.VerifyOne(context.Background(), "goal", "i", "ref", true); err != nil {
		t.Fatalf("VerifyOne: %v", err)
	}
	if v.completeStatus != string(outcomeNotIdentifiable) {
		t.Fatalf("status = %q, want not_identifiable", v.completeStatus)
	}
}

// slowAnalyzer delays each effect call so a run outlives several heartbeat ticks.
type slowAnalyzer struct {
	inner analyzer
	delay time.Duration
}

func (s slowAnalyzer) Introspect(ctx context.Context, req sandboxclient.IntrospectRequest) (sandboxclient.IntrospectResponse, error) {
	return s.inner.Introspect(ctx, req)
}
func (s slowAnalyzer) Analyze(ctx context.Context, req sandboxclient.AnalyzeRequest) (sandboxclient.AnalyzeResponse, error) {
	time.Sleep(s.delay)
	return s.inner.Analyze(ctx, req)
}

// TestVerifyOneHeartbeatRenewsLease: a run whose analyze calls outlast the heartbeat
// interval renews the lease at least once before completing.
func TestVerifyOneHeartbeatRenewsLease(t *testing.T) {
	ds := groundtruth.Generate(groundtruth.DefaultConfig())
	a := slowAnalyzer{inner: newEffectAnalyzer(ds), delay: 5 * time.Millisecond}
	g := &fakeGraph{found: true, graphValue: groundGraph(), intervention: finding("i", thresholdSegment(groundtruth.ColA, domain.GreaterThanOrEqual, 0))}
	v := &fakeVerifications{accepted: true, completeHeld: true}
	w := newVerifyWorker(a, g, maximizeYGoal(), v, &fakeAudit{})

	if err := w.VerifyOne(context.Background(), "goal", "i", "ref", true); err != nil {
		t.Fatalf("VerifyOne: %v", err)
	}
	if v.heartbeats == 0 {
		t.Fatal("a slow run must renew the lease at least once")
	}
}

// TestVerifyOneRefutationBelowTau: an effect that survives positivity and the
// collapse check but fails the refutation gate (score below tau) is downgraded to
// confounded — no confidence, supersede rather than write. Forcing tau just above the
// achievable score exercises the failing side of the gate that the regression's
// confounded case (a collapse short-circuit) never reaches.
func TestVerifyOneRefutationBelowTau(t *testing.T) {
	ds := groundtruth.Generate(groundtruth.DefaultConfig())
	a := newEffectAnalyzer(ds)
	g := &fakeGraph{found: true, graphValue: groundGraph(), intervention: finding("i", thresholdSegment(groundtruth.ColA, domain.GreaterThanOrEqual, 0))}
	v := &fakeVerifications{accepted: true, completeHeld: true}
	w := newVerifyWorker(a, g, maximizeYGoal(), v, &fakeAudit{})
	w.cfg.RefutationTau = 1.01 // unreachable: even a perfect score fails the gate

	if err := w.VerifyOne(context.Background(), "goal", "i", "ref", true); err != nil {
		t.Fatalf("VerifyOne: %v", err)
	}
	if v.completeStatus != string(outcomeConfounded) {
		t.Fatalf("status = %q, want confounded (score below tau)", v.completeStatus)
	}
	if v.confidence != nil {
		t.Fatalf("a below-tau outcome must carry no confidence, got %v", *v.confidence)
	}
	if g.causalWrites != 0 || g.supersedes != 1 {
		t.Fatalf("a below-tau outcome must supersede, not write (writes=%d supersedes=%d)", g.causalWrites, g.supersedes)
	}
}

// TestVerifyOneBudgetRejection: a typed budget/cap refusal from DispatchAccept is
// mapped to a first-class rejection event and a nil return — not a fault, no run, no
// complete, no graph write.
func TestVerifyOneBudgetRejection(t *testing.T) {
	ds := groundtruth.Generate(groundtruth.DefaultConfig())
	a := newEffectAnalyzer(ds)
	g := &fakeGraph{found: true, graphValue: groundGraph(), intervention: finding("i", thresholdSegment(groundtruth.ColA, domain.GreaterThanOrEqual, 0))}
	audit := &fakeAudit{}
	v := &fakeVerifications{dispatchErr: store.ErrBudgetExhausted, completeHeld: true}
	w := newVerifyWorker(a, g, maximizeYGoal(), v, audit)

	if err := w.VerifyOne(context.Background(), "goal", "i", "ref", true); err != nil {
		t.Fatalf("a budget refusal must return nil, got %v", err)
	}
	if v.completeCalls != 0 || g.causalWrites != 0 || g.supersedes != 0 {
		t.Fatalf("a rejected dispatch must not run (completes=%d writes=%d supersedes=%d)", v.completeCalls, g.causalWrites, g.supersedes)
	}
	rejected := false
	for _, ev := range audit.events {
		if ev["type"] == "verification_rejected" {
			rejected = true
		}
	}
	if !rejected {
		t.Fatalf("a budget refusal must publish a verification_rejected event, got %v", audit.events)
	}
}

// TestCollapsed pins the confounded trigger's decision boundaries: the naive-zero
// guard, the sign-flip branch, and the magnitude-retention ratio at its edge.
func TestCollapsed(t *testing.T) {
	cases := []struct {
		name            string
		naive, adjusted float64
		ratio           float64
		want            bool
	}{
		{"naive zero is never collapsed", 0, 5, 0.5, false},
		{"sign flip collapses", 1.0, -0.5, 0.5, true},
		{"magnitude below ratio collapses", 2.0, 0.5, 0.5, true},
		{"magnitude retained does not collapse", 2.0, 1.5, 0.5, false},
		{"exactly at the ratio does not collapse", 2.0, 1.0, 0.5, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := collapsed(c.naive, c.adjusted, c.ratio); got != c.want {
				t.Fatalf("collapsed(%v, %v, %v) = %v, want %v", c.naive, c.adjusted, c.ratio, got, c.want)
			}
		})
	}
}

// TestVerifyOneReapedLeaseSkipsWrite: when Complete reports the lease was reaped
// (held=false), VerifyOne skips the graph write entirely.
func TestVerifyOneReapedLeaseSkipsWrite(t *testing.T) {
	ds := groundtruth.Generate(groundtruth.DefaultConfig())
	a := newEffectAnalyzer(ds)
	g := &fakeGraph{found: true, graphValue: groundGraph(), intervention: finding("i", thresholdSegment(groundtruth.ColA, domain.GreaterThanOrEqual, 0.5))}
	v := &fakeVerifications{accepted: true, completeHeld: false} // lease reaped mid-run
	w := newVerifyWorker(a, g, maximizeYGoal(), v, &fakeAudit{})

	if err := w.VerifyOne(context.Background(), "goal", "i", "ref", true); err != nil {
		t.Fatalf("VerifyOne: %v", err)
	}
	if g.causalWrites != 0 || g.supersedes != 0 {
		t.Fatalf("a reaped lease must skip the graph write (writes=%d supersedes=%d)", g.causalWrites, g.supersedes)
	}
}
