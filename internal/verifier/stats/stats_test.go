package stats

import (
	"math"
	"testing"
)

func approx(t *testing.T, got, want, tol float64, label string) {
	t.Helper()
	if math.Abs(got-want) > tol {
		t.Fatalf("%s = %.10f, want %.10f (tol %g)", label, got, want, tol)
	}
}

// Reference values are scipy-computed: chi2.sf(stat, dof).
func TestChiSquareP(t *testing.T) {
	cases := []struct {
		stat float64
		dof  int
		want float64
	}{
		{0, 1, 1},
		{3.841459, 1, 0.05},
		{1.0, 1, 0.3173105},
		{6.634897, 1, 0.01},
		{2.0, 2, 0.3678794},
		{10.0, 4, 0.0404277},
		{-5, 3, 1},
		{5, 0, 1},
	}
	for _, c := range cases {
		approx(t, ChiSquareP(c.stat, c.dof), c.want, 1e-4, "ChiSquareP")
	}
}

// GStatistic against scipy.stats.chi2_contingency(table, lambda_="log-likelihood").
func TestGStatistic(t *testing.T) {
	g, dof := GStatistic([][]float64{{10, 20}, {30, 40}})
	if dof != 1 {
		t.Fatalf("dof = %d, want 1", dof)
	}
	approx(t, g, 0.8043040, 1e-4, "G")

	// A table with an all-zero column drops to a single effective column → untestable.
	g, dof = GStatistic([][]float64{{5, 0}, {7, 0}})
	if dof != 0 || g != 0 {
		t.Fatalf("degenerate table: got g=%v dof=%d, want 0,0", g, dof)
	}
}

func TestPearsonFromMoments(t *testing.T) {
	// x = 1..4, y = 2,4,6,8 → r = 1.
	n := 4
	sx, sy := 10.0, 20.0
	sxx, syy := 30.0, 120.0
	sxy := 60.0
	r, ok := PearsonFromMoments(n, sx, sy, sxx, syy, sxy)
	if !ok {
		t.Fatal("expected ok")
	}
	approx(t, r, 1, 1e-9, "pearson")

	// A constant column has zero variance → not ok.
	if _, ok := PearsonFromMoments(3, 6, 9, 12, 27, 18); ok {
		// y constant=3: sy=9,syy=27 => var=27-81/3=0
		t.Fatal("expected not-ok for constant column")
	}
}

func TestPartialCorrelation(t *testing.T) {
	// 2×2: partial corr with empty conditioning set is the raw correlation.
	r, ok := PartialCorrelation([][]float64{{1, 0.5}, {0.5, 1}})
	if !ok {
		t.Fatal("expected ok")
	}
	approx(t, r, 0.5, 1e-9, "partial 2x2")

	// 3×3: closed-form r_{01·2} = (r01 - r02 r12)/sqrt((1-r02²)(1-r12²)).
	corr := [][]float64{
		{1, 0.6, 0.5},
		{0.6, 1, 0.4},
		{0.5, 0.4, 1},
	}
	r, ok = PartialCorrelation(corr)
	if !ok {
		t.Fatal("expected ok")
	}
	approx(t, r, 0.5039526, 1e-6, "partial 3x3")

	// Collinear conditioners (variables 2 and 3 identical) → singular → not ok.
	collinear := [][]float64{
		{1, 0.5, 0.3, 0.3},
		{0.5, 1, 0.4, 0.4},
		{0.3, 0.4, 1, 1},
		{0.3, 0.4, 1, 1},
	}
	if _, ok := PartialCorrelation(collinear); ok {
		t.Fatal("expected not-ok for collinear conditioners")
	}
}

func TestFisherZP(t *testing.T) {
	p, ok := FisherZP(0.5, 100, 0)
	if !ok {
		t.Fatal("expected ok")
	}
	if p >= 1e-6 {
		t.Fatalf("strong correlation p = %g, want < 1e-6", p)
	}
	// Near-zero correlation is consistent with independence (large p).
	p, ok = FisherZP(0.01, 100, 0)
	if !ok {
		t.Fatal("expected ok")
	}
	if p < 0.5 {
		t.Fatalf("near-zero correlation p = %g, want >= 0.5", p)
	}
	// Too few effective rows is untestable.
	if _, ok := FisherZP(0.9, 3, 0); ok {
		t.Fatal("expected not-ok for n <= condSize+3")
	}
}

func TestNormalTwoSidedP(t *testing.T) {
	approx(t, normalTwoSidedP(1.959964), 0.05, 1e-4, "z=1.96")
	approx(t, normalTwoSidedP(1.0), 0.3173105, 1e-4, "z=1.0")
	approx(t, normalTwoSidedP(2.575829), 0.01, 1e-4, "z=2.576")
}

func TestPooledFisherZP(t *testing.T) {
	// Two strata, both strongly positively correlated → tiny pooled p.
	p, ok := PooledFisherZP([]float64{0.5, 0.55}, []int{100, 120}, 0)
	if !ok {
		t.Fatal("expected ok")
	}
	if p >= 1e-6 {
		t.Fatalf("pooled p = %g, want < 1e-6", p)
	}
	// All strata below the minimum size → dropped → not ok.
	if _, ok := PooledFisherZP([]float64{0.5}, []int{4}, 0); ok {
		t.Fatal("expected not-ok when every stratum is below minimum size")
	}
}

func TestBenjaminiHochberg(t *testing.T) {
	// Benjamini–Hochberg (1995) worked example: m=15, alpha=0.05 → 4 rejections.
	pvals := []float64{0.0001, 0.0004, 0.0019, 0.0095, 0.0201, 0.0278, 0.0298,
		0.0344, 0.0459, 0.3240, 0.4262, 0.5719, 0.6528, 0.7590, 1.000}
	reject := BenjaminiHochberg(pvals, 0.05)
	count := 0
	for _, r := range reject {
		if r {
			count++
		}
	}
	if count != 4 {
		t.Fatalf("rejections = %d, want 4", count)
	}
	for i := 0; i < 4; i++ {
		if !reject[i] {
			t.Fatalf("expected p[%d]=%g rejected", i, pvals[i])
		}
	}
	if reject[4] {
		t.Fatalf("did not expect p[4]=%g rejected", pvals[4])
	}
}
