package verifier

import (
	"context"
	"sort"
	"time"

	"github.com/arborette/arborette/internal/domain"
	"github.com/arborette/arborette/internal/verifier/stats"
)

// Config is the discovery sweep's tuning. The service maps its env-loaded
// VerifierConfig into this. FDR "bh" applies Benjamini–Hochberg per level; "none"
// thresholds each p at Alpha with no correction. MaxCondSet bounds the
// conditioning-set size; Bins is the quantile bin count for binned columns;
// ColumnCap caps the sweep's variable count; MaxTests bounds the total sandbox
// round-trips; CallTimeout bounds one analyze call.
type Config struct {
	Alpha       float64
	FDR         string
	MaxCondSet  int
	Bins        int
	ColumnCap   int
	MaxTests    int
	CallTimeout time.Duration
}

// Pair is a canonical undirected column pair (A < B lexicographically), the key
// the skeleton, separating sets, and edge statuses are all keyed on.
type Pair struct {
	A string
	B string
}

// pairOf orders two column names into a canonical Pair.
func pairOf(a, b string) Pair {
	colA, colB := domain.CanonicalColumnPair(a, b)
	return Pair{A: colA, B: colB}
}

// DiscoveryResult is the skeleton the sweep recovered plus the bookkeeping later
// stages need: the selected node columns, the columns excluded by the cap, the
// surviving undirected edges and their fail-closed statuses, the separating set
// recorded for each removed pair (needed for collider orientation), and the test
// count / truncation the budget guard produced.
type DiscoveryResult struct {
	Columns   []Column
	Excluded  []string
	Edges     map[Pair]bool
	Status    map[Pair]domain.EdgeStatus
	SepSets   map[Pair][]string
	TestCount int
	Truncated bool
}

// Discover runs the PC-style conditional-independence sweep over cols, selecting a
// deterministic subset when the count exceeds the column cap (priority columns —
// objective and finding-referenced — first, then by association strength with the
// objective columns, ties by name). It returns the recovered skeleton with per-pair
// fail-closed statuses and separating sets. A sandbox error on an individual test
// fails closed (the pair is retained with unknown status); only a selection-time
// failure returns an error.
func Discover(ctx context.Context, a analyzer, ref string, cfg Config, cols []Column, priority []string) (*DiscoveryResult, error) {
	s := &sweep{analyzer: a, datasourceRef: ref, cfg: cfg}

	selected, excluded, err := s.selectColumns(ctx, cols, priority)
	if err != nil {
		return nil, err
	}

	res := &DiscoveryResult{
		Columns:  selected,
		Excluded: excluded,
		Edges:    map[Pair]bool{},
		Status:   map[Pair]domain.EdgeStatus{},
		SepSets:  map[Pair][]string{},
	}
	byName := map[string]Column{}
	for _, c := range selected {
		byName[c.Name] = c
	}
	for i := 0; i < len(selected); i++ {
		for j := i + 1; j < len(selected); j++ {
			p := pairOf(selected[i].Name, selected[j].Name)
			res.Edges[p] = true
			res.Status[p] = domain.EdgeTested
		}
	}

	for level := 0; level <= cfg.MaxCondSet; level++ {
		if s.budgetExceeded(res, level) {
			s.markBudgetCapped(res, level)
			res.Truncated = true
			break
		}
		s.runLevel(ctx, res, byName, level)
	}

	res.TestCount = s.tests
	return res, nil
}

// levelTest is one scheduled conditional-independence test at a level: the pair
// under test and the conditioning set (empty at level 0).
type levelTest struct {
	pair Pair
	cond []string
}

// runLevel runs every conditioning-set test of one level, applies the level's FDR
// correction once over the completed tests, and removes each edge that any test
// found independent — recording that edge's separating set by the pinned tiebreak.
// Removals are deferred to the end of the level so the adjacency conditioning sets
// are drawn from is stable within the level.
func (s *sweep) runLevel(ctx context.Context, res *DiscoveryResult, byName map[string]Column, level int) {
	scheduled := s.scheduleLevel(res, byName, level)

	pvals := make([]float64, 0, len(scheduled))
	completed := make([]levelTest, 0, len(scheduled))
	for _, lt := range scheduled {
		endpoints := [2]Column{byName[lt.pair.A], byName[lt.pair.B]}
		cond := columnsByName(byName, lt.cond)
		r := s.test(ctx, endpoints, cond)
		if !r.ok {
			// A test that errors or is degenerate fails closed: the pair keeps its edge
			// and is flagged unknown, never removed or oriented on this evidence.
			res.Status[lt.pair] = domain.EdgeUnknown
			continue
		}
		pvals = append(pvals, r.p)
		completed = append(completed, lt)
	}

	independent := s.independenceMask(pvals)

	// Choose each removed pair's separating set among its accepted-independent sets:
	// maximum p-value first, ties broken by smallest cardinality then lexicographic
	// column order. The choice feeds collider orientation, so it is pinned rather than
	// incidental to loop order.
	type candidate struct {
		p    float64
		cond []string
	}
	best := map[Pair]candidate{}
	for i, lt := range completed {
		if !independent[i] {
			continue
		}
		cur, seen := best[lt.pair]
		if !seen || betterSepSet(pvals[i], lt.cond, cur.p, cur.cond) {
			best[lt.pair] = candidate{p: pvals[i], cond: lt.cond}
		}
	}
	for pair, c := range best {
		delete(res.Edges, pair)
		delete(res.Status, pair)
		res.SepSets[pair] = c.cond
	}
}

// scheduleLevel enumerates the level's tests: one empty-conditioning-set test per
// present pair at level 0, and at deeper levels every size-`level` conditioning set
// drawn from the union of the pair's endpoints' current neighbors. Pairs with fewer
// than `level` available conditioners get no test and survive the level.
func (s *sweep) scheduleLevel(res *DiscoveryResult, byName map[string]Column, level int) []levelTest {
	var tests []levelTest
	for _, pair := range sortedPairs(res.Edges) {
		if level == 0 {
			tests = append(tests, levelTest{pair: pair})
			continue
		}
		for _, cond := range combinations(s.conditioners(res, pair), level) {
			tests = append(tests, levelTest{pair: pair, cond: cond})
		}
	}
	return tests
}

// conditioners returns the sorted candidate conditioning columns for a pair: the
// union of both endpoints' current neighbors, excluding the endpoints themselves.
func (s *sweep) conditioners(res *DiscoveryResult, pair Pair) []string {
	set := map[string]struct{}{}
	for other := range res.Edges {
		if other.A == pair.A && other.B == pair.B {
			continue
		}
		for _, end := range []string{pair.A, pair.B} {
			if other.A == end {
				set[other.B] = struct{}{}
			}
			if other.B == end {
				set[other.A] = struct{}{}
			}
		}
	}
	delete(set, pair.A)
	delete(set, pair.B)
	out := make([]string, 0, len(set))
	for c := range set {
		out = append(out, c)
	}
	sort.Strings(out)
	return out
}

// independenceMask maps each completed test's p-value to whether it concluded
// independence (the edge-removal signal). Under BH, an independence conclusion is a
// test the step-up procedure did NOT reject (large p); under no correction it is a
// p above Alpha. Rejection means dependence, which keeps the edge.
func (s *sweep) independenceMask(pvals []float64) []bool {
	independent := make([]bool, len(pvals))
	if s.cfg.FDR == "bh" {
		reject := stats.BenjaminiHochberg(pvals, s.cfg.Alpha)
		for i := range pvals {
			independent[i] = !reject[i]
		}
		return independent
	}
	for i, p := range pvals {
		independent[i] = p > s.cfg.Alpha
	}
	return independent
}

// budgetExceeded reports whether running the given level would push the sandbox
// round-trip count past MaxTests, estimated from each present pair's available
// conditioning sets.
func (s *sweep) budgetExceeded(res *DiscoveryResult, level int) bool {
	estimate := 0
	for _, pair := range sortedPairs(res.Edges) {
		if level == 0 {
			estimate++
			continue
		}
		estimate += choose(len(s.conditioners(res, pair)), level)
	}
	return s.tests+estimate > s.cfg.MaxTests
}

// markBudgetCapped flags every present pair that could still be tested at the
// stopped level (it has enough available conditioners) as budget_capped, retained
// unoriented. A pair already flagged unknown stays unknown — both are fail-closed.
func (s *sweep) markBudgetCapped(res *DiscoveryResult, level int) {
	for _, pair := range sortedPairs(res.Edges) {
		eligible := level == 0 || len(s.conditioners(res, pair)) >= level
		if eligible && res.Status[pair] != domain.EdgeUnknown {
			res.Status[pair] = domain.EdgeBudgetCapped
		}
	}
}

// selectColumns applies the column cap: priority columns (objective- and
// finding-referenced) first in first-seen order, then the remaining columns ranked
// by descending pairwise association with the objective columns (measured by
// level-0 tests, smallest p first), ties broken by name. Below the cap it is a
// no-op. Excluded columns are returned for persistence.
func (s *sweep) selectColumns(ctx context.Context, cols []Column, priority []string) (selected []Column, excluded []string, err error) {
	if len(cols) <= s.cfg.ColumnCap {
		return cols, nil, nil
	}
	byName := map[string]Column{}
	for _, c := range cols {
		byName[c.Name] = c
	}

	keep := map[string]bool{}
	order := make([]Column, 0, s.cfg.ColumnCap)
	add := func(name string) bool {
		if keep[name] || len(order) >= s.cfg.ColumnCap {
			return false
		}
		if c, ok := byName[name]; ok {
			keep[name] = true
			order = append(order, c)
			return true
		}
		return false
	}
	priorityCols := make([]Column, 0, len(priority))
	for _, name := range priority {
		if add(name) {
			priorityCols = append(priorityCols, byName[name])
		}
	}

	// Rank the remaining columns by their strongest level-0 association with any
	// priority column (smallest p wins); a column that never tests keeps p=1.
	type ranked struct {
		col Column
		p   float64
	}
	var rest []ranked
	for _, c := range cols {
		if keep[c.Name] {
			continue
		}
		best := 1.0
		for _, pc := range priorityCols {
			r := s.test(ctx, [2]Column{c, pc}, nil)
			if r.ok && r.p < best {
				best = r.p
			}
		}
		rest = append(rest, ranked{col: c, p: best})
	}
	sort.SliceStable(rest, func(i, j int) bool {
		if rest[i].p != rest[j].p {
			return rest[i].p < rest[j].p
		}
		return rest[i].col.Name < rest[j].col.Name
	})
	for _, r := range rest {
		if !add(r.col.Name) {
			excluded = append(excluded, r.col.Name)
		}
	}
	sort.Strings(excluded)
	return order, excluded, nil
}

// betterSepSet reports whether candidate (p, cond) beats the current best under the
// pinned tiebreak: larger p first, then smaller cardinality, then lexicographic.
func betterSepSet(p float64, cond []string, curP float64, curCond []string) bool {
	if p != curP {
		return p > curP
	}
	if len(cond) != len(curCond) {
		return len(cond) < len(curCond)
	}
	return joinCond(cond) < joinCond(curCond)
}

func joinCond(cond []string) string {
	c := append([]string(nil), cond...)
	sort.Strings(c)
	out := ""
	for _, s := range c {
		out += s + "\x00"
	}
	return out
}

// sortedPairs returns the present pairs in a stable order so scheduling and budget
// estimation are deterministic regardless of map iteration.
func sortedPairs(edges map[Pair]bool) []Pair {
	pairs := make([]Pair, 0, len(edges))
	for p := range edges {
		pairs = append(pairs, p)
	}
	sort.Slice(pairs, func(i, j int) bool {
		if pairs[i].A != pairs[j].A {
			return pairs[i].A < pairs[j].A
		}
		return pairs[i].B < pairs[j].B
	})
	return pairs
}

func columnsByName(byName map[string]Column, names []string) []Column {
	cols := make([]Column, 0, len(names))
	for _, n := range names {
		cols = append(cols, byName[n])
	}
	return cols
}

// combinations returns every size-k subset of a sorted slice in lexicographic index
// order, so conditioning-set enumeration is deterministic.
func combinations(items []string, k int) [][]string {
	if k <= 0 || k > len(items) {
		return nil
	}
	var out [][]string
	idx := make([]int, k)
	for i := range idx {
		idx[i] = i
	}
	for {
		combo := make([]string, k)
		for i, j := range idx {
			combo[i] = items[j]
		}
		out = append(out, combo)
		pos := k - 1
		for pos >= 0 && idx[pos] == len(items)-k+pos {
			pos--
		}
		if pos < 0 {
			break
		}
		idx[pos]++
		for i := pos + 1; i < k; i++ {
			idx[i] = idx[i-1] + 1
		}
	}
	return out
}

// choose is the binomial coefficient C(n, k) used to estimate a level's test count.
func choose(n, k int) int {
	if k < 0 || k > n {
		return 0
	}
	if k > n-k {
		k = n - k
	}
	result := 1
	for i := 0; i < k; i++ {
		result = result * (n - i) / (i + 1)
	}
	return result
}
