package verifier

import (
	"context"
	"math"
	"sort"
	"strings"

	"github.com/arborette/arborette/internal/sandboxclient"
	"github.com/arborette/arborette/internal/verifier/stats"
)

// Column is one node of the discovery variable set: its name, whether discovery
// treats it as a numeric endpoint/conditioner (partialled) or a categorical one
// (stratified/binned), and its low-cardinality sample values (nil for numeric or
// high-cardinality columns), which feed both categorical routing and the
// orientation prompt.
type Column struct {
	Name    string
	Numeric bool
	Samples []string
}

// testResult is one conditional-independence test outcome. ok is false when the
// test could not be computed (a sandbox error, a singular conditioning matrix, a
// degenerate table, or every stratum dropped) — the caller maps not-ok to a
// retained fail-closed edge rather than an independence conclusion.
type testResult struct {
	p  float64
	ok bool
}

// sweep holds the collaborators and knobs the conditional-independence tests share:
// the analyzer seam, the data-source ref every Analyze call scopes to, and the
// discovery configuration. It also counts the sandbox round-trips it makes, the
// figure the budget guard bounds.
type sweep struct {
	analyzer      analyzer
	datasourceRef string
	cfg           Config
	tests         int
}

// test routes one conditional-independence test of a canonical endpoint pair given
// a conditioning set to exactly one statistic, deterministically, by endpoint type
// first then conditioner composition (the totality-and-determinism contract). It
// returns the two-sided p-value under the null of independence; a small p is
// dependence (keep the edge), a large p is independence (remove it). Every route's
// request fits the /analyze caps at the configured bounds.
func (s *sweep) test(ctx context.Context, endpoints [2]Column, cond []Column) testResult {
	numEndpoints := endpoints[0].Numeric && endpoints[1].Numeric
	catEndpoints := !endpoints[0].Numeric && !endpoints[1].Numeric

	switch {
	case numEndpoints:
		if !hasCategorical(cond) {
			return s.routePartial(ctx, endpoints, cond) // route (a)
		}
		return s.routeStratifiedPartial(ctx, endpoints, cond) // route (c)
	case catEndpoints:
		return s.routeStratifiedG(ctx, [2]Column{endpoints[0], endpoints[1]}, cond, [2]bool{false, false}) // route (b), raw endpoints
	default:
		// Mixed endpoints: bin the numeric endpoint and treat both as categorical, the
		// same reduction the binned hybrid uses at level 0, so every pair type has one
		// route at every level.
		bin := [2]bool{endpoints[0].Numeric, endpoints[1].Numeric}
		return s.routeStratifiedG(ctx, endpoints, cond, bin) // route (b), one endpoint binned
	}
}

// routePartial is route (a): both endpoints numeric, all conditioners numeric.
// One ungrouped moments call over {X, Y, numeric conditioners} yields the
// correlation matrix; partial correlation between the endpoints, then Fisher-z at
// |S| conditioning df.
func (s *sweep) routePartial(ctx context.Context, endpoints [2]Column, cond []Column) testResult {
	vars := append([]string{endpoints[0].Name, endpoints[1].Name}, columnNames(cond)...)
	resp, err := s.analyze(ctx, sandboxclient.AnalyzeRequest{
		DataSourceRef: s.datasourceRef,
		Kind:          sandboxclient.AnalyzeMoments,
		Variables:     vars,
	})
	if err != nil || len(resp.Moments) != 1 {
		return testResult{ok: false}
	}
	row := resp.Moments[0]
	corr, ok := correlationMatrix(row.N, row.Sum, row.SumSq, row.Cross)
	if !ok {
		return testResult{ok: false}
	}
	r, ok := stats.PartialCorrelation(corr)
	if !ok {
		return testResult{ok: false}
	}
	p, ok := stats.FisherZP(r, int(row.N), len(cond))
	return testResult{p: p, ok: ok}
}

// routeStratifiedPartial is route (c): both endpoints numeric with at least one
// categorical conditioner. Grouped moments with the group key = the categorical
// conditioners only, requesting cross-products over {X, Y, numeric conditioners};
// per-stratum partial correlation partials out the numeric conditioners, pooled by
// weighted Fisher-z at |S_num| df.
func (s *sweep) routeStratifiedPartial(ctx context.Context, endpoints [2]Column, cond []Column) testResult {
	numCond, catCond := splitByType(cond)
	vars := append([]string{endpoints[0].Name, endpoints[1].Name}, columnNames(numCond)...)
	resp, err := s.analyze(ctx, sandboxclient.AnalyzeRequest{
		DataSourceRef: s.datasourceRef,
		Kind:          sandboxclient.AnalyzeMoments,
		Variables:     vars,
		GroupBy:       columnNames(catCond),
	})
	if err != nil {
		return testResult{ok: false}
	}
	var rs []float64
	var ns []int
	for _, row := range resp.Moments {
		corr, ok := correlationMatrix(row.N, row.Sum, row.SumSq, row.Cross)
		if !ok {
			continue
		}
		r, ok := stats.PartialCorrelation(corr)
		if !ok {
			continue
		}
		rs = append(rs, r)
		ns = append(ns, int(row.N))
	}
	p, ok := stats.PooledFisherZP(rs, ns, len(numCond))
	return testResult{p: p, ok: ok}
}

// routeStratifiedG is route (b): categorical (or binned-mixed) endpoints, any
// conditioners. A joint contingency grouped by the endpoints and all conditioners
// (categoricals raw, numerics binned into the group key), then a G-test within each
// conditioning stratum summing G and dof across strata (the standard conditional
// G-test). binEndpoint marks which endpoint is a binned numeric.
func (s *sweep) routeStratifiedG(ctx context.Context, endpoints [2]Column, cond []Column, binEndpoint [2]bool) testResult {
	cols := []sandboxclient.AnalyzeColumn{
		{Name: endpoints[0].Name, Bins: s.binsFor(binEndpoint[0])},
		{Name: endpoints[1].Name, Bins: s.binsFor(binEndpoint[1])},
	}
	for _, c := range cond {
		cols = append(cols, sandboxclient.AnalyzeColumn{Name: c.Name, Bins: s.binsFor(c.Numeric)})
	}
	resp, err := s.analyze(ctx, sandboxclient.AnalyzeRequest{
		DataSourceRef: s.datasourceRef,
		Kind:          sandboxclient.AnalyzeContingency,
		Columns:       cols,
	})
	if err != nil {
		return testResult{ok: false}
	}
	return stratifiedGTest(resp.Cells)
}

// binsFor returns the quantile bin count for a column position: the configured bins
// for a numeric column that must be binned, 0 (raw) for a categorical column.
func (s *sweep) binsFor(numeric bool) int {
	if numeric {
		return s.cfg.Bins
	}
	return 0
}

// stratifiedGTest pools a conditional G-test from contingency cells whose first two
// values are the endpoint pair and whose remaining values are the conditioning
// stratum key. It builds one 2-D table per stratum over the endpoint values, sums G
// and dof across strata, and converts to a chi-square p-value. A pooled dof of 0
// (every stratum degenerate) is untestable → not ok.
func stratifiedGTest(cells []sandboxclient.ContingencyCell) testResult {
	type table struct {
		rows map[string]map[string]float64
	}
	strata := map[string]*table{}
	for _, cell := range cells {
		if len(cell.Values) < 2 {
			return testResult{ok: false}
		}
		key := strings.Join(cell.Values[2:], "\x00")
		t := strata[key]
		if t == nil {
			t = &table{rows: map[string]map[string]float64{}}
			strata[key] = t
		}
		row := t.rows[cell.Values[0]]
		if row == nil {
			row = map[string]float64{}
			t.rows[cell.Values[0]] = row
		}
		row[cell.Values[1]] += float64(cell.Count)
	}

	var sumG float64
	var sumDof int
	for _, key := range sortedKeys(strata) {
		g, dof := stats.GStatistic(denseTable(strata[key].rows))
		sumG += g
		sumDof += dof
	}
	if sumDof <= 0 {
		return testResult{ok: false}
	}
	return testResult{p: stats.ChiSquareP(sumG, sumDof), ok: true}
}

// denseTable materializes a sparse row→col→count map into the dense matrix
// GStatistic consumes, with a stable row/column ordering so the statistic is
// deterministic regardless of map iteration.
func denseTable(rows map[string]map[string]float64) [][]float64 {
	rowKeys := make([]string, 0, len(rows))
	colSet := map[string]struct{}{}
	for rk, cols := range rows {
		rowKeys = append(rowKeys, rk)
		for ck := range cols {
			colSet[ck] = struct{}{}
		}
	}
	sort.Strings(rowKeys)
	colKeys := make([]string, 0, len(colSet))
	for ck := range colSet {
		colKeys = append(colKeys, ck)
	}
	sort.Strings(colKeys)

	table := make([][]float64, len(rowKeys))
	for i, rk := range rowKeys {
		table[i] = make([]float64, len(colKeys))
		for j, ck := range colKeys {
			table[i][j] = rows[rk][ck]
		}
	}
	return table
}

// correlationMatrix builds the correlation matrix over k variables from one
// stratum's moment sums: the k×k scatter matrix (SumSq on the diagonal, Cross —
// flattened over pairs i<j — off-diagonal), normalized to correlations. It reports
// not-ok when n is too small or any variable is constant within the stratum
// (non-positive scatter), so the caller drops the stratum or the test rather than
// dividing by zero.
func correlationMatrix(n int64, sum, sumSq, cross []float64) ([][]float64, bool) {
	k := len(sum)
	if n <= 1 || k < 2 || len(sumSq) != k || len(cross) != k*(k-1)/2 {
		return nil, false
	}
	fn := float64(n)
	scatter := make([][]float64, k)
	for i := range scatter {
		scatter[i] = make([]float64, k)
		scatter[i][i] = sumSq[i] - sum[i]*sum[i]/fn
		if scatter[i][i] <= 0 {
			return nil, false
		}
	}
	idx := 0
	for i := 0; i < k; i++ {
		for j := i + 1; j < k; j++ {
			s := cross[idx] - sum[i]*sum[j]/fn
			scatter[i][j] = s
			scatter[j][i] = s
			idx++
		}
	}
	corr := make([][]float64, k)
	for i := 0; i < k; i++ {
		corr[i] = make([]float64, k)
		for j := 0; j < k; j++ {
			corr[i][j] = scatter[i][j] / math.Sqrt(scatter[i][i]*scatter[j][j])
		}
	}
	return corr, true
}

// analyze wraps one sandbox analyze call with a per-call timeout and counts it
// toward the sweep's test budget.
func (s *sweep) analyze(ctx context.Context, req sandboxclient.AnalyzeRequest) (sandboxclient.AnalyzeResponse, error) {
	s.tests++
	callCtx, cancel := context.WithTimeout(ctx, s.cfg.CallTimeout)
	defer cancel()
	return s.analyzer.Analyze(callCtx, req)
}

func hasCategorical(cols []Column) bool {
	for _, c := range cols {
		if !c.Numeric {
			return true
		}
	}
	return false
}

func splitByType(cols []Column) (numeric, categorical []Column) {
	for _, c := range cols {
		if c.Numeric {
			numeric = append(numeric, c)
		} else {
			categorical = append(categorical, c)
		}
	}
	return numeric, categorical
}

func columnNames(cols []Column) []string {
	names := make([]string, len(cols))
	for i, c := range cols {
		names[i] = c.Name
	}
	return names
}

func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
