package groundtruth

import (
	"math"
	"testing"

	"github.com/arborette/arborette/internal/verifier/stats"
)

// defaultMinSupport mirrors SLEEPCYCLE_SEARCH_MIN_SUPPORT: the absolute matched-row
// floor below which the sleep cycle prunes a segment and every superset of it. The
// bundled orders.csv could not demo Phase 2 because its default floor emptied the
// candidate union; these assertions pin the frozen fixture so a reseed can't
// silently reintroduce that failure.
const defaultMinSupport = 30

// TestSupportDensityFloors pins the planted segments to clear default floors: the
// interaction cell holds ≥3× MinSupport rows with a material Y delta, and each
// categorical level clears the floor. These are counted from the emitted rows, so
// a reseed that weakens the fixture fails here rather than in a later calibration.
func TestSupportDensityFloors(t *testing.T) {
	ds := Generate(DefaultConfig())

	var cell, notCell []float64
	bCounts := map[string]int{}
	regionCounts := map[string]int{}
	for _, r := range ds.Rows {
		bCounts[r.B]++
		regionCounts[r.Region]++
		if r.B == interactionB && r.C == interactionC {
			cell = append(cell, r.Y)
		} else {
			notCell = append(notCell, r.Y)
		}
	}

	if len(cell) < 3*defaultMinSupport {
		t.Fatalf("interaction cell holds %d rows, want >= %d (3x MinSupport)", len(cell), 3*defaultMinSupport)
	}
	delta := math.Abs(mean(cell) - mean(notCell))
	sd := stdev(collectY(ds))
	if delta < 0.5*sd {
		t.Fatalf("interaction Y delta = %.3f, want >= %.3f (0.5 sigma)", delta, 0.5*sd)
	}
	for level, n := range bCounts {
		if n < defaultMinSupport {
			t.Fatalf("B=%s holds %d rows, want >= %d", level, n, defaultMinSupport)
		}
	}
	for level, n := range regionCounts {
		if n < defaultMinSupport {
			t.Fatalf("region=%s holds %d rows, want >= %d", level, n, defaultMinSupport)
		}
	}
}

// TestCIPowerInvariants pins the associations the discovery regression's headline
// assertions depend on, computed directly from the emitted rows: the confounded
// X–Y edge must exist at level 0 (strong marginal association) and vanish at
// level 1 given Z (near-zero partial), while the true A→Y edge must survive
// adjustment (strong partial given Z). A reseed that preserves counts but flips
// one of these fails at the generator, not in a later shell.
func TestCIPowerInvariants(t *testing.T) {
	ds := Generate(DefaultConfig())
	x := column(ds, func(r Row) float64 { return r.X })
	y := column(ds, func(r Row) float64 { return r.Y })
	z := column(ds, func(r Row) float64 { return r.Z })
	a := column(ds, func(r Row) float64 { return r.A })

	marginalXY := math.Abs(pearson(x, y))
	if marginalXY < 0.35 {
		t.Fatalf("marginal corr(X,Y) = %.3f, want >= 0.35 (spurious edge must exist at level 0)", marginalXY)
	}

	partialXYgivenZ, ok := partial(x, y, z)
	if !ok {
		t.Fatal("partial corr(X,Y|Z) not computable")
	}
	if math.Abs(partialXYgivenZ) > 0.12 {
		t.Fatalf("partial corr(X,Y|Z) = %.3f, want <= 0.12 (level 1 must remove the spurious edge)", partialXYgivenZ)
	}

	partialAYgivenZ, ok := partial(a, y, z)
	if !ok {
		t.Fatal("partial corr(A,Y|Z) not computable")
	}
	if math.Abs(partialAYgivenZ) < 0.4 {
		t.Fatalf("partial corr(A,Y|Z) = %.3f, want >= 0.4 (true A->Y edge must survive)", partialAYgivenZ)
	}
}

func TestDeterministic(t *testing.T) {
	a := Generate(DefaultConfig())
	b := Generate(DefaultConfig())
	if len(a.Rows) != len(b.Rows) {
		t.Fatalf("row counts differ: %d vs %d", len(a.Rows), len(b.Rows))
	}
	for i := range a.Rows {
		if a.Rows[i] != b.Rows[i] {
			t.Fatalf("row %d differs between two generations", i)
		}
	}
}

func collectY(ds *Dataset) []float64 { return column(ds, func(r Row) float64 { return r.Y }) }

func column(ds *Dataset, get func(Row) float64) []float64 {
	out := make([]float64, len(ds.Rows))
	for i, r := range ds.Rows {
		out[i] = get(r)
	}
	return out
}

func mean(xs []float64) float64 {
	var s float64
	for _, x := range xs {
		s += x
	}
	return s / float64(len(xs))
}

func stdev(xs []float64) float64 {
	m := mean(xs)
	var s float64
	for _, x := range xs {
		s += (x - m) * (x - m)
	}
	return math.Sqrt(s / float64(len(xs)-1))
}

func pearson(x, y []float64) float64 {
	n := len(x)
	var sx, sy, sxx, syy, sxy float64
	for i := 0; i < n; i++ {
		sx += x[i]
		sy += y[i]
		sxx += x[i] * x[i]
		syy += y[i] * y[i]
		sxy += x[i] * y[i]
	}
	r, _ := stats.PearsonFromMoments(n, sx, sy, sxx, syy, sxy)
	return r
}

// partial computes the partial correlation of x and y given a single conditioner
// by assembling the 3×3 correlation matrix (order x, y, cond) and inverting it.
func partial(x, y, cond []float64) (float64, bool) {
	corr := [][]float64{
		{1, pearson(x, y), pearson(x, cond)},
		{pearson(y, x), 1, pearson(y, cond)},
		{pearson(cond, x), pearson(cond, y), 1},
	}
	return stats.PartialCorrelation(corr)
}
