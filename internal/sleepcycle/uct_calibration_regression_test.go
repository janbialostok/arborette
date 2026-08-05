package sleepcycle

import (
	"context"
	"math"
	"sort"
	"strconv"
	"testing"

	"github.com/arborette/arborette/internal/domain"
	"github.com/arborette/arborette/internal/objective"
	"github.com/arborette/arborette/internal/sandboxclient"
	"github.com/arborette/arborette/internal/verifier/groundtruth"
)

// The calibration gate runs both policies over the planted ground-truth dataset —
// the same fixture the discovery regression measures against — with an in-process
// sandbox, so it exercises the search itself with no infra and no env gate.
//
// The dataset's load-bearing property here is the planted B×C interaction: Y is
// raised only in the single joint cell, so the conjunction is worth far more than
// either predicate alone. That is the signal a conjunction search exists to find,
// and it is what separates a search that descends where the evidence points from
// one that measures a whole level before looking deeper.

// calibrationConfig is the shipped tuning both policies run at, so the gate
// measures the defaults rather than a setting chosen to pass it. It is duplicated
// from the env defaults rather than loaded, because internal/config is a cmd-only
// dependency; the values must move together.
func calibrationConfig() Config {
	return Config{
		MaxMeasurements:       200,
		BeamWidth:             10,
		MaxOrder:              3,
		MinSupport:            30,
		MinLift:               0.05,
		MaxPublications:       20,
		Policy:                policyUCT,
		UCTExploration:        math.Sqrt2,
		CausalMultiplierScale: 1.0,
		GroundingFraction:     0.3,
		RetrievalK:            8,
		QuantileBins:          3,
		CrossGoalGrounding:    true,
	}
}

// TestUCTCalibrationRegression is the gate the default traversal is chosen by: at
// one budget, over one vocabulary, does the knowledge-guided search match or beat
// the level-wise beam?
//
// Three things are compared, and each is a distinct claim. The peak — neither
// policy may leave the strongest segment on the table. The yield — how many
// materially-backed segments the same budget produced, which is what a consumer
// actually reads. And the planted interaction, the effect that exists only in a
// conjunction, which is the whole reason to search conjunctions at all.
//
// What is deliberately NOT asserted is that one policy's winners contain the
// other's. At equal budget that would require the tree search to do the beam's work
// and more, which is not achievable and not the goal: the two cover different
// regions, and the comparison that matters is what each returns, not whether one
// retraced the other's steps.
func TestUCTCalibrationRegression(t *testing.T) {
	ds := groundtruth.Generate(groundtruth.DefaultConfig())
	sandbox := newGroundTruthSandbox(ds)
	cfg := calibrationConfig()
	obj := groundTruthObjective(t)
	baseline := sandbox.aggregate(nil)
	atoms := groundTruthAtoms(t, ds, cfg.QuantileBins)

	beam := runCalibration(t, cfg, sandbox, obj, baseline, newBeamPolicy(atoms, cfg, baseline, obj.Direction))
	uct := runCalibration(t, cfg, sandbox, obj, baseline, newUCTPolicy(atoms, nil, nil, nil, cfg, baseline, obj.Direction))

	t.Logf("atoms=%d beam: %d measurements, %d winners, best delta %.4f",
		len(atoms), beam.measurements, len(beam.winners), beam.bestDelta)
	t.Logf("atoms=%d uct:  %d measurements, %d winners, best delta %.4f",
		len(atoms), uct.measurements, len(uct.winners), uct.bestDelta)

	// The interaction cell is the planted conjunction-only effect: neither predicate
	// alone comes close to it, and it is a mid-ranked segment rather than the
	// strongest, so reaching it takes a search that spends its budget across columns
	// instead of driving into the one branch that measured best first.
	interaction := interactionCanonical(t)
	if _, found := uct.winners[interaction]; !found {
		t.Errorf("the knowledge-guided search did not find the planted interaction segment %s", interaction)
	}

	// The peak: the strongest segment either policy can establish at this budget.
	if uct.bestDelta < beam.bestDelta-deltaTolerance {
		t.Errorf("best delta: uct %.4f is worse than beam %.4f", uct.bestDelta, beam.bestDelta)
	}

	// The yield: how many materially-backed segments the same budget produced. This
	// is the comparison the default rests on, so it is asserted as a relation rather
	// than a pinned count — a change that made the tree search return fewer segments
	// than a level-wise walk would be the signal to revisit the default, whatever the
	// absolute numbers had drifted to.
	if len(uct.winners) < len(beam.winners) {
		t.Errorf("winners: uct found %d, fewer than beam's %d at the same budget",
			len(uct.winners), len(beam.winners))
	}
}

// deltaTolerance absorbs float re-association between two orderings of the same
// aggregate; it is far below any difference between distinct segments here.
const deltaTolerance = 1e-9

// calibrationResult is one policy's run reduced to what the gate compares: the
// winning segments by canonical key with their directional deltas, the strongest of
// them, and how much budget the run spent.
type calibrationResult struct {
	winners      map[string]float64
	bestDelta    float64
	measurements int
}

func runCalibration(t *testing.T, cfg Config, sandbox *groundTruthSandbox, obj objective.Objective, baseline float64, policy SearchPolicy) calibrationResult {
	t.Helper()
	worker, err := NewWorker(newFakeRepo(), sandbox, &fakeClaude{}, &fakeProvider{}, &fakeEmbeddings{},
		&fakeGoals{goal: tabularGoal()}, &fakeAudits{}, cfg)
	if err != nil {
		t.Fatalf("new worker: %v", err)
	}
	target := searchTarget{goalID: "gt", dataSourceRef: "groundtruth.csv", namespace: goalNamespace("gt")}
	outcome := worker.runSearch(context.Background(), target, obj, policy)

	result := calibrationResult{winners: map[string]float64{}, bestDelta: math.Inf(-1), measurements: len(outcome.measured)}
	for _, won := range outcome.winners(int64(cfg.MinSupport)) {
		delta := domain.DirectionalDelta(won.measurement.Value, baseline, obj.Direction)
		result.winners[won.node.canonical] = delta
		if delta > result.bestDelta {
			result.bestDelta = delta
		}
	}
	return result
}

// groundTruthObjective is maximize avg(Y), the outcome every planted edge points at.
func groundTruthObjective(t *testing.T) objective.Objective {
	t.Helper()
	obj, err := objective.Pin(domain.EvaluationMatrix{Targets: []domain.Target{
		{Field: groundtruth.ColY, Direction: domain.Maximize, Aggregation: "avg"},
	}})
	if err != nil {
		t.Fatalf("pin objective: %v", err)
	}
	return obj
}

// groundTruthAtoms is the vocabulary both policies search, derived from the
// dataset's own schema exactly as a run with the schema-derived vocabulary would:
// one equality per categorical value, two thresholds per quantile cut. The objective
// column is left out, which is what the vocabulary critic does with a column that
// restates the objective — leaving it in would let both policies "win" by segmenting
// on the answer.
func groundTruthAtoms(t *testing.T, ds *groundtruth.Dataset, bins int) []atom {
	t.Helper()
	atoms, err := buildSchemaAtoms(groundTruthSchema(ds, bins), bins)
	if err != nil {
		t.Fatalf("build schema atoms: %v", err)
	}
	return rankAtoms(atoms)
}

func groundTruthSchema(ds *groundtruth.Dataset, bins int) sandboxclient.Schema {
	numeric := []string{groundtruth.ColZ, groundtruth.ColX, groundtruth.ColA}
	columns := make([]sandboxclient.Column, 0, len(numeric)+3)
	for _, name := range numeric {
		columns = append(columns, sandboxclient.Column{
			Name:         name,
			Type:         "DOUBLE",
			QuantileCuts: quantileCutsOf(numericColumnValues(ds, name), bins),
		})
	}
	columns = append(columns,
		sandboxclient.Column{Name: groundtruth.ColB, Type: "VARCHAR", DistinctValues: []string{"b0", "b1", "b2"}},
		sandboxclient.Column{Name: groundtruth.ColC, Type: "BOOLEAN", DistinctValues: []string{"true", "false"}},
		sandboxclient.Column{Name: groundtruth.ColRegion, Type: "VARCHAR", DistinctValues: []string{"north", "south", "east", "west"}},
	)
	return sandboxclient.Schema{Columns: columns}
}

// interactionCanonical is the canonical key of the planted interaction cell, the
// segment the gate requires the knowledge-guided search to find.
func interactionCanonical(t *testing.T) string {
	t.Helper()
	b, c := "b1", true
	return canonicalKeyOf([]string{
		mustCanonical(t, []domain.Constraint{{Field: groundtruth.ColB, Op: domain.Equal, Operand: &domain.LiteralValue{String: &b}}}),
		mustCanonical(t, []domain.Constraint{{Field: groundtruth.ColC, Op: domain.Equal, Operand: &domain.LiteralValue{Bool: &c}}}),
	})
}

// groundTruthSandbox measures the objective over the generated rows in process,
// evaluating a candidate's filter conjunction the way the compiler would: a
// complete-case aggregate over the matched rows, with the matched count alongside.
type groundTruthSandbox struct {
	rows []groundtruth.Row
}

func newGroundTruthSandbox(ds *groundtruth.Dataset) *groundTruthSandbox {
	return &groundTruthSandbox{rows: ds.Rows}
}

func (s *groundTruthSandbox) Introspect(_ context.Context, _ sandboxclient.IntrospectRequest) (sandboxclient.IntrospectResponse, error) {
	return sandboxclient.IntrospectResponse{Schema: groundTruthSchema(&groundtruth.Dataset{Rows: s.rows}, 4)}, nil
}

func (s *groundTruthSandbox) Execute(_ context.Context, req sandboxclient.ExecuteRequest) (sandboxclient.ExecuteResponse, error) {
	value, matched := s.measure(req.Filters)
	out := map[string]any{req.ObjectiveLabel: value}
	if matched == 0 {
		out[req.ObjectiveLabel] = nil
	}
	if req.IncludeRowCount {
		out[sandboxclient.RowCountKey] = float64(matched)
	}
	return sandboxclient.ExecuteResponse{Value: out}, nil
}

func (s *groundTruthSandbox) aggregate(filters []domain.Constraint) float64 {
	value, _ := s.measure(filters)
	return value
}

func (s *groundTruthSandbox) measure(filters []domain.Constraint) (float64, int64) {
	sum, n := 0.0, int64(0)
	for _, r := range s.rows {
		if !matchesAll(r, filters) {
			continue
		}
		sum += r.Y
		n++
	}
	if n == 0 {
		return 0, 0
	}
	return sum / float64(n), n
}

func matchesAll(r groundtruth.Row, filters []domain.Constraint) bool {
	for _, f := range filters {
		if !matches(r, f) {
			return false
		}
	}
	return true
}

// matches evaluates one predicate against a row, by the same operator classes the
// compiler splits on: a numeric threshold against a numeric column, and an equality
// against the typed operand the vocabulary derived from the column's own type.
func matches(r groundtruth.Row, f domain.Constraint) bool {
	switch {
	case f.IsNumericThresholdOp():
		v := groundTruthNumeric(r, f.Field)
		switch f.Op {
		case domain.LessThan:
			return v < f.Value
		case domain.LessThanOrEqual:
			return v <= f.Value
		case domain.GreaterThan:
			return v > f.Value
		default:
			return v >= f.Value
		}
	case f.IsEqualityOp():
		equal := groundTruthCategorical(r, f.Field) == operandText(f.Operand)
		if f.Op == domain.NotEqual {
			return !equal
		}
		return equal
	default:
		return false
	}
}

func operandText(l *domain.LiteralValue) string {
	switch {
	case l == nil:
		return ""
	case l.String != nil:
		return *l.String
	case l.Bool != nil:
		return strconv.FormatBool(*l.Bool)
	case l.Number != nil:
		return strconv.FormatFloat(*l.Number, 'f', -1, 64)
	default:
		return ""
	}
}

func groundTruthNumeric(r groundtruth.Row, column string) float64 {
	switch column {
	case groundtruth.ColZ:
		return r.Z
	case groundtruth.ColX:
		return r.X
	case groundtruth.ColA:
		return r.A
	case groundtruth.ColY:
		return r.Y
	default:
		return 0
	}
}

func groundTruthCategorical(r groundtruth.Row, column string) string {
	switch column {
	case groundtruth.ColB:
		return r.B
	case groundtruth.ColC:
		return strconv.FormatBool(r.C)
	case groundtruth.ColRegion:
		return r.Region
	default:
		return ""
	}
}

func numericColumnValues(ds *groundtruth.Dataset, column string) []float64 {
	out := make([]float64, 0, len(ds.Rows))
	for _, r := range ds.Rows {
		out = append(out, groundTruthNumeric(r, column))
	}
	return out
}

// quantileCutsOf mirrors the sandbox's continuous quantile cuts (linear
// interpolation), so the vocabulary the gate searches is the one a real run would
// receive from introspection.
func quantileCutsOf(values []float64, bins int) []float64 {
	sorted := append([]float64(nil), values...)
	sort.Float64s(sorted)
	cuts := make([]float64, 0, bins-1)
	for i := 1; i < bins; i++ {
		cuts = append(cuts, quantileAt(sorted, float64(i)/float64(bins)))
	}
	return cuts
}

func quantileAt(sorted []float64, p float64) float64 {
	n := len(sorted)
	switch {
	case n == 0:
		return 0
	case n == 1:
		return sorted[0]
	}
	idx := p * float64(n-1)
	lo := int(math.Floor(idx))
	if lo+1 >= n {
		return sorted[n-1]
	}
	return sorted[lo] + (idx-float64(lo))*(sorted[lo+1]-sorted[lo])
}
