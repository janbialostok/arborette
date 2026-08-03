package verifier

import (
	"context"
	"math"
	"sort"
	"strconv"
	"testing"
	"time"

	"github.com/arborette/arborette/internal/domain"
	"github.com/arborette/arborette/internal/sandboxclient"
	"github.com/arborette/arborette/internal/verifier/groundtruth"
)

// regressionConfig is the discovery tuning the regression runs under: the shipped
// defaults, with a generous per-call timeout (the fake analyzer is in-process).
func regressionConfig() Config {
	return Config{
		Alpha:       0.05,
		FDR:         "bh",
		MaxCondSet:  2,
		Bins:        4,
		ColumnCap:   50,
		MaxTests:    20000,
		CallTimeout: time.Minute,
	}
}

// TestDiscoveryRegression runs the statistical stages (sweep → colliders → Meek; LLM
// orientation skipped) against the planted ground-truth dataset and compares the
// recovered skeleton and oriented subset to the known graph. Thresholds are pinned
// from the first honest run with slack for boundary noise.
func TestDiscoveryRegression(t *testing.T) {
	ds := groundtruth.Generate(groundtruth.DefaultConfig())
	analyzer := newFakeAnalyzer(ds)
	cols := discoveryColumns()

	res, err := Discover(context.Background(), analyzer, "demo", regressionConfig(), cols, nil)
	if err != nil {
		t.Fatalf("discover: %v", err)
	}
	orientations := orientEdges(context.Background(), res, nil, "maximize Y", res.Columns, 0)

	truth := groundtruth.GroundTruth().Skeleton()
	precision, recall := skeletonScores(res.Edges, truth)
	t.Logf("skeleton precision=%.3f recall=%.3f edges=%d tests=%d", precision, recall, len(res.Edges), res.TestCount)
	if precision < 0.8 {
		t.Errorf("skeleton precision = %.3f, want >= 0.8", precision)
	}
	if recall < 0.8 {
		t.Errorf("skeleton recall = %.3f, want >= 0.8", recall)
	}

	// The confounder-induced spurious X–Y edge must be removed, separated by Z.
	xy := pairOf(groundtruth.ColX, groundtruth.ColY)
	if res.Edges[xy] {
		t.Errorf("spurious X–Y edge was not removed")
	}
	if !contains(res.SepSets[xy], groundtruth.ColZ) {
		t.Errorf("X–Y separating set = %v, want it to contain Z", res.SepSets[xy])
	}

	// The true A→Y effect must survive adjustment and orient A→Y (A < Y canonically,
	// so a_to_b encodes A causing Y).
	ay := pairOf(groundtruth.ColA, groundtruth.ColY)
	if !res.Edges[ay] {
		t.Errorf("true A–Y edge did not survive")
	}
	if dir := directionOf(orientations, ay); dir != domain.DirectionAToB {
		t.Errorf("A–Y direction = %q, want a_to_b (A causes Y)", dir)
	}

	// Determinism: a second run over the same rows produces an identical graph.
	res2, err := Discover(context.Background(), newFakeAnalyzer(ds), "demo", regressionConfig(), discoveryColumns(), nil)
	if err != nil {
		t.Fatalf("second discover: %v", err)
	}
	orientations2 := orientEdges(context.Background(), res2, nil, "maximize Y", res2.Columns, 0)
	if !sameOrientations(orientations, orientations2) {
		t.Errorf("discovery is not deterministic across two runs")
	}
}

func skeletonScores(edges map[Pair]bool, truth map[groundtruth.SkeletonPair]bool) (precision, recall float64) {
	hits := 0
	for p := range edges {
		if truth[groundtruth.CanonicalPair(p.A, p.B)] {
			hits++
		}
	}
	if len(edges) > 0 {
		precision = float64(hits) / float64(len(edges))
	} else {
		precision = 1
	}
	if len(truth) > 0 {
		recall = float64(hits) / float64(len(truth))
	}
	return precision, recall
}

func directionOf(orientations []edgeOrientation, pair Pair) domain.EdgeDirection {
	for _, o := range orientations {
		if o.Pair == pair {
			return o.Direction
		}
	}
	return ""
}

func sameOrientations(a, b []edgeOrientation) bool {
	if len(a) != len(b) {
		return false
	}
	key := func(o edgeOrientation) string {
		return o.Pair.A + "|" + o.Pair.B + "|" + string(o.Direction) + "|" + string(o.Status)
	}
	sortByPair := func(s []edgeOrientation) {
		sort.Slice(s, func(i, j int) bool { return key(s[i]) < key(s[j]) })
	}
	sortByPair(a)
	sortByPair(b)
	for i := range a {
		if key(a[i]) != key(b[i]) {
			return false
		}
	}
	return true
}

func contains(s []string, v string) bool {
	for _, x := range s {
		if x == v {
			return true
		}
	}
	return false
}

// discoveryColumns is the ground-truth schema as the discovery variable model: the
// numeric columns are numeric, the categoricals carry their sample values (the fake
// analyzer's Introspect supplies the same).
func discoveryColumns() []Column {
	return []Column{
		{Name: groundtruth.ColZ, Numeric: true},
		{Name: groundtruth.ColX, Numeric: true},
		{Name: groundtruth.ColA, Numeric: true},
		{Name: groundtruth.ColB, Numeric: false, Samples: []string{"b0", "b1", "b2"}},
		{Name: groundtruth.ColC, Numeric: false, Samples: []string{"true", "false"}},
		{Name: groundtruth.ColY, Numeric: true},
		{Name: groundtruth.ColRegion, Numeric: false, Samples: []string{"north", "south", "east", "west"}},
	}
}

// fakeAnalyzer computes contingency and moments aggregates in-process from the
// generated rows, matching the sandbox's quantile binning and complete-case
// semantics, so the regression measures the statistical stages without the real
// sandbox. The integration-gated variant drives the same dataset through the real
// sandbox to catch compile/binding drift.
type fakeAnalyzer struct {
	rows []groundtruth.Row
}

func newFakeAnalyzer(ds *groundtruth.Dataset) *fakeAnalyzer {
	return &fakeAnalyzer{rows: ds.Rows}
}

var numericColumns = map[string]bool{
	groundtruth.ColZ: true, groundtruth.ColX: true, groundtruth.ColA: true, groundtruth.ColY: true,
}

func (f *fakeAnalyzer) Introspect(_ context.Context, _ sandboxclient.IntrospectRequest) (sandboxclient.IntrospectResponse, error) {
	cols := []sandboxclient.Column{
		{Name: groundtruth.ColZ, Type: "DOUBLE"},
		{Name: groundtruth.ColX, Type: "DOUBLE"},
		{Name: groundtruth.ColA, Type: "DOUBLE"},
		{Name: groundtruth.ColB, Type: "VARCHAR", DistinctValues: []string{"b0", "b1", "b2"}},
		{Name: groundtruth.ColC, Type: "BOOLEAN", DistinctValues: []string{"true", "false"}},
		{Name: groundtruth.ColY, Type: "DOUBLE"},
		{Name: groundtruth.ColRegion, Type: "VARCHAR", DistinctValues: []string{"north", "south", "east", "west"}},
	}
	return sandboxclient.IntrospectResponse{Schema: sandboxclient.Schema{Kind: "tabular", Columns: cols}}, nil
}

func (f *fakeAnalyzer) Analyze(_ context.Context, req sandboxclient.AnalyzeRequest) (sandboxclient.AnalyzeResponse, error) {
	switch req.Kind {
	case sandboxclient.AnalyzeContingency:
		return f.contingency(req), nil
	case sandboxclient.AnalyzeMoments:
		return f.moments(req), nil
	default:
		return sandboxclient.AnalyzeResponse{}, nil
	}
}

func (f *fakeAnalyzer) contingency(req sandboxclient.AnalyzeRequest) sandboxclient.AnalyzeResponse {
	cuts := make([][]float64, len(req.Columns))
	for i, c := range req.Columns {
		if c.Bins > 0 {
			cuts[i] = quantileCuts(f.numericValues(c.Name), c.Bins)
		}
	}
	counts := map[string]int64{}
	var order []string
	keyToValues := map[string][]string{}
	for _, r := range f.rows {
		values := make([]string, len(req.Columns))
		for i, c := range req.Columns {
			if c.Bins > 0 {
				values[i] = bucketLabel(numVal(r, c.Name), cuts[i])
			} else {
				values[i] = catVal(r, c.Name)
			}
		}
		k := joinKey(values)
		if _, seen := counts[k]; !seen {
			order = append(order, k)
			keyToValues[k] = values
		}
		counts[k]++
	}
	sort.Strings(order)
	cells := make([]sandboxclient.ContingencyCell, 0, len(order))
	for _, k := range order {
		cells = append(cells, sandboxclient.ContingencyCell{Values: keyToValues[k], Count: counts[k]})
	}
	return sandboxclient.AnalyzeResponse{Kind: sandboxclient.AnalyzeContingency, Cells: cells}
}

func (f *fakeAnalyzer) moments(req sandboxclient.AnalyzeRequest) sandboxclient.AnalyzeResponse {
	type acc struct {
		n     int64
		sum   []float64
		sumSq []float64
		cross []float64
	}
	nVars := len(req.Variables)
	nCross := nVars * (nVars - 1) / 2
	strata := map[string]*acc{}
	var order []string
	keyToGroup := map[string][]string{}
	for _, r := range f.rows {
		group := make([]string, len(req.GroupBy))
		for i, g := range req.GroupBy {
			group[i] = catVal(r, g)
		}
		k := joinKey(group)
		a := strata[k]
		if a == nil {
			a = &acc{sum: make([]float64, nVars), sumSq: make([]float64, nVars), cross: make([]float64, nCross)}
			strata[k] = a
			order = append(order, k)
			keyToGroup[k] = group
		}
		a.n++
		vals := make([]float64, nVars)
		for i, v := range req.Variables {
			vals[i] = numVal(r, v)
			a.sum[i] += vals[i]
			a.sumSq[i] += vals[i] * vals[i]
		}
		idx := 0
		for i := 0; i < nVars; i++ {
			for j := i + 1; j < nVars; j++ {
				a.cross[idx] += vals[i] * vals[j]
				idx++
			}
		}
	}
	sort.Strings(order)
	rows := make([]sandboxclient.MomentsRow, 0, len(order))
	for _, k := range order {
		a := strata[k]
		row := sandboxclient.MomentsRow{N: a.n, Sum: a.sum, SumSq: a.sumSq, Cross: a.cross}
		if len(req.GroupBy) > 0 {
			row.Group = keyToGroup[k]
		}
		rows = append(rows, row)
	}
	return sandboxclient.AnalyzeResponse{Kind: sandboxclient.AnalyzeMoments, Moments: rows}
}

func (f *fakeAnalyzer) numericValues(col string) []float64 {
	out := make([]float64, len(f.rows))
	for i, r := range f.rows {
		out[i] = numVal(r, col)
	}
	return out
}

func numVal(r groundtruth.Row, col string) float64 {
	switch col {
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

func catVal(r groundtruth.Row, col string) string {
	switch col {
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

// quantileCuts computes the bins-1 interior continuous quantile cut points of a
// column, matching DuckDB's quantile_cont (linear interpolation).
func quantileCuts(values []float64, bins int) []float64 {
	sorted := append([]float64(nil), values...)
	sort.Float64s(sorted)
	cuts := make([]float64, 0, bins-1)
	for i := 1; i < bins; i++ {
		cuts = append(cuts, quantileCont(sorted, float64(i)/float64(bins)))
	}
	return cuts
}

func quantileCont(sorted []float64, p float64) float64 {
	n := len(sorted)
	if n == 0 {
		return 0
	}
	if n == 1 {
		return sorted[0]
	}
	idx := p * float64(n-1)
	lo := int(math.Floor(idx))
	if lo+1 >= n {
		return sorted[n-1]
	}
	return sorted[lo] + (idx-float64(lo))*(sorted[lo+1]-sorted[lo])
}

// bucketLabel matches the sandbox binExpr: the first cut the value is <= names its
// bucket, else the terminal bucket.
func bucketLabel(v float64, cuts []float64) string {
	for i, cut := range cuts {
		if v <= cut {
			return strconv.Itoa(i)
		}
	}
	return strconv.Itoa(len(cuts))
}

func joinKey(parts []string) string {
	out := ""
	for _, p := range parts {
		out += p + "\x00"
	}
	return out
}
