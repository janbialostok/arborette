// Package stats holds the pure-Go statistical primitives the causal-discovery
// sweep needs: chi-square / G-test p-values, partial correlation by matrix
// inversion, the conditioned Fisher-z test, stratified pooling, and the
// Benjamini–Hochberg FDR correction. Adding gonum for two special functions is
// not warranted, so the regularized incomplete gamma and the normal tail are
// implemented here and pinned against scipy-computed reference values in the
// tests. Everything is CGO-free so the verifier stays CGO-free.
package stats

import (
	"math"
	"sort"
)

// detFloor is the determinant/pivot magnitude below which a correlation matrix is
// treated as singular. At ≤4×4 a near-collinear conditioning set drives the
// determinant toward zero; inverting it would leak NaN/±Inf into Fisher-z and the
// BH pool, so the partial-correlation routine reports not-ok (fail-closed) instead.
const detFloor = 1e-12

// ChiSquareP returns the upper-tail probability P(X > stat) for a chi-square
// distribution with dof degrees of freedom — the survival function used to turn a
// chi-square or G statistic into a p-value. A non-positive statistic is p=1
// (perfectly consistent with independence); a non-positive dof is an untestable
// table, also p=1 so the edge is retained.
func ChiSquareP(stat float64, dof int) float64 {
	if dof <= 0 || stat <= 0 {
		return 1
	}
	return gammaQ(float64(dof)/2, stat/2)
}

// GStatistic computes the G (likelihood-ratio) statistic and its degrees of
// freedom from a contingency table of observed counts. Rows and columns whose
// marginal is zero are dropped from the dof accounting (they carry no
// information), so dof = (nonzeroRows-1)*(nonzeroCols-1). A table that collapses
// to a single effective row or column returns dof 0 — untestable, which the
// caller maps to retained-unknown. Only cells with a positive observed count
// contribute to G (0·ln0 = 0).
func GStatistic(table [][]float64) (g float64, dof int) {
	rows := len(table)
	if rows == 0 {
		return 0, 0
	}
	cols := len(table[0])
	rowSums := make([]float64, rows)
	colSums := make([]float64, cols)
	var total float64
	for i := 0; i < rows; i++ {
		for j := 0; j < cols; j++ {
			v := table[i][j]
			rowSums[i] += v
			colSums[j] += v
			total += v
		}
	}
	if total <= 0 {
		return 0, 0
	}
	nonzeroRows, nonzeroCols := 0, 0
	for _, s := range rowSums {
		if s > 0 {
			nonzeroRows++
		}
	}
	for _, s := range colSums {
		if s > 0 {
			nonzeroCols++
		}
	}
	dof = (nonzeroRows - 1) * (nonzeroCols - 1)
	if dof <= 0 {
		return 0, 0
	}
	for i := 0; i < rows; i++ {
		if rowSums[i] == 0 {
			continue
		}
		for j := 0; j < cols; j++ {
			o := table[i][j]
			if o <= 0 || colSums[j] == 0 {
				continue
			}
			e := rowSums[i] * colSums[j] / total
			g += o * math.Log(o/e)
		}
	}
	return 2 * g, dof
}

// PearsonFromMoments computes the Pearson correlation of two variables from their
// joint moments over the same n complete-case rows: n, the per-variable sums and
// sums of squares, and the cross-product sum. A non-positive variance (a constant
// column within the stratum) yields (0, false) so the caller drops the pair
// rather than dividing by zero.
func PearsonFromMoments(n int, sumX, sumY, sumXX, sumYY, sumXY float64) (float64, bool) {
	if n <= 1 {
		return 0, false
	}
	fn := float64(n)
	covXY := sumXY - sumX*sumY/fn
	varX := sumXX - sumX*sumX/fn
	varY := sumYY - sumY*sumY/fn
	if varX <= 0 || varY <= 0 {
		return 0, false
	}
	r := covXY / math.Sqrt(varX*varY)
	return clampCorrelation(r), true
}

// PartialCorrelation returns the partial correlation between variables 0 and 1 of
// a correlation matrix, conditioning on variables 2..n-1, computed by inverting
// the matrix (the precision-matrix identity r_{01·rest} = -P01/sqrt(P00·P11)).
// The matrix must be square and at least 2×2. It reports not-ok when the matrix is
// singular or near-singular (|det| < detFloor) — a near-collinear conditioning set
// — so the caller routes the test to unknown/fail-closed instead of propagating a
// NaN.
func PartialCorrelation(corr [][]float64) (float64, bool) {
	n := len(corr)
	if n < 2 {
		return 0, false
	}
	inv, det, ok := invert(corr)
	if !ok || math.Abs(det) < detFloor {
		return 0, false
	}
	denom := inv[0][0] * inv[1][1]
	if denom <= 0 {
		return 0, false
	}
	r := -inv[0][1] / math.Sqrt(denom)
	if math.IsNaN(r) || math.IsInf(r, 0) {
		return 0, false
	}
	return clampCorrelation(r), true
}

// FisherZP applies the conditioned Fisher z-transform to a (partial) correlation
// and returns its two-sided normal p-value. condSize is the conditioning-set size
// |S|: the test statistic is z = sqrt(n − |S| − 3) · atanh(r), so the marginal
// sqrt(n−3) is the |S|=0 case. Too few effective rows (n ≤ |S| + 3) is untestable
// and returns (1, false).
func FisherZP(r float64, n, condSize int) (float64, bool) {
	dfN := n - condSize - 3
	if dfN <= 0 {
		return 1, false
	}
	z := math.Sqrt(float64(dfN)) * math.Atanh(clampCorrelation(r))
	return normalTwoSidedP(z), true
}

// PooledFisherZP combines per-stratum partial correlations by weighted Fisher-z,
// the conditional test route (c) uses when the endpoints are numeric and at least
// one conditioner is categorical. Each stratum contributes z_i = atanh(r_i)
// weighted by w_i = n_i − condSize − 3, over strata with n_i ≥ condSize + 3 + 5
// (the minimum-stratum-size rule); Z = Σ w_i z_i / sqrt(Σ w_i). condSize here is
// the count of numeric conditioners partialled out within each stratum. If every
// stratum is below the minimum it returns (1, false) so the test is unknown.
func PooledFisherZP(rs []float64, ns []int, condSize int) (float64, bool) {
	const minStratumSlack = 5
	minN := condSize + 3 + minStratumSlack
	var sumWeightedZ, sumWeight float64
	used := 0
	for i, n := range ns {
		if n < minN {
			continue
		}
		w := float64(n - condSize - 3)
		if w <= 0 {
			continue
		}
		sumWeightedZ += w * math.Atanh(clampCorrelation(rs[i]))
		sumWeight += w
		used++
	}
	if used == 0 || sumWeight <= 0 {
		return 1, false
	}
	z := sumWeightedZ / math.Sqrt(sumWeight)
	return normalTwoSidedP(z), true
}

// BenjaminiHochberg returns, for each p-value, whether it is rejected (declared
// significant) at false-discovery-rate alpha under the BH step-up procedure over
// the whole slice as one family. The result is index-aligned with pvals.
func BenjaminiHochberg(pvals []float64, alpha float64) []bool {
	m := len(pvals)
	reject := make([]bool, m)
	if m == 0 {
		return reject
	}
	order := make([]int, m)
	for i := range order {
		order[i] = i
	}
	sort.Slice(order, func(a, b int) bool { return pvals[order[a]] < pvals[order[b]] })
	// Largest rank k (1-based) with p_(k) <= (k/m)*alpha; reject every p at rank <= k.
	maxK := 0
	for rank := 1; rank <= m; rank++ {
		if pvals[order[rank-1]] <= float64(rank)/float64(m)*alpha {
			maxK = rank
		}
	}
	for rank := 1; rank <= maxK; rank++ {
		reject[order[rank-1]] = true
	}
	return reject
}

// normalTwoSidedP is the two-sided tail probability of a standard-normal z via the
// complementary error function: P(|Z| > |z|) = erfc(|z|/sqrt2).
func normalTwoSidedP(z float64) float64 {
	return math.Erfc(math.Abs(z) / math.Sqrt2)
}

// clampCorrelation pulls a correlation just inside (-1, 1) so atanh does not blow
// up to ±Inf on a degenerate perfect correlation.
func clampCorrelation(r float64) float64 {
	const eps = 1e-12
	if r >= 1 {
		return 1 - eps
	}
	if r <= -1 {
		return -1 + eps
	}
	return r
}

// invert returns the inverse of a square matrix and its determinant via
// Gauss–Jordan elimination with partial pivoting, reporting not-ok on a zero
// pivot. It is used only on correlation matrices of at most four variables (a pair
// plus a conditioning set of size ≤2), where this direct method is exact enough
// and the degeneracy guard in PartialCorrelation catches the ill-conditioned case.
func invert(a [][]float64) ([][]float64, float64, bool) {
	n := len(a)
	// Augmented [a | I]; m[i] holds row i of both halves.
	m := make([][]float64, n)
	for i := 0; i < n; i++ {
		m[i] = make([]float64, 2*n)
		copy(m[i], a[i])
		m[i][n+i] = 1
	}
	det := 1.0
	for col := 0; col < n; col++ {
		pivot := col
		for r := col + 1; r < n; r++ {
			if math.Abs(m[r][col]) > math.Abs(m[pivot][col]) {
				pivot = r
			}
		}
		if math.Abs(m[pivot][col]) < detFloor {
			return nil, 0, false
		}
		if pivot != col {
			m[col], m[pivot] = m[pivot], m[col]
			det = -det
		}
		det *= m[col][col]
		inv := 1 / m[col][col]
		for j := 0; j < 2*n; j++ {
			m[col][j] *= inv
		}
		for r := 0; r < n; r++ {
			if r == col {
				continue
			}
			factor := m[r][col]
			if factor == 0 {
				continue
			}
			for j := 0; j < 2*n; j++ {
				m[r][j] -= factor * m[col][j]
			}
		}
	}
	out := make([][]float64, n)
	for i := 0; i < n; i++ {
		out[i] = make([]float64, n)
		copy(out[i], m[i][n:])
	}
	return out, det, true
}

// gammaQ is the regularized upper incomplete gamma function Q(a,x) = 1 - P(a,x),
// selecting the series expansion for x < a+1 and the continued fraction otherwise
// (each converges fast in its regime). Q(a,x) is the chi-square survival function
// at a = dof/2, x = stat/2.
func gammaQ(a, x float64) float64 {
	if x <= 0 {
		return 1
	}
	if x < a+1 {
		return 1 - gammaSeries(a, x)
	}
	return gammaContinuedFraction(a, x)
}

// gammaSeries evaluates the lower regularized incomplete gamma P(a,x) by its power
// series, valid and fast for x < a+1.
func gammaSeries(a, x float64) float64 {
	const maxIter = 200
	const eps = 1e-14
	lg, _ := math.Lgamma(a)
	ap := a
	sum := 1 / a
	del := sum
	for n := 0; n < maxIter; n++ {
		ap++
		del *= x / ap
		sum += del
		if math.Abs(del) < math.Abs(sum)*eps {
			break
		}
	}
	return sum * math.Exp(-x+a*math.Log(x)-lg)
}

// gammaContinuedFraction evaluates the upper regularized incomplete gamma Q(a,x)
// by the modified Lentz continued fraction, valid and fast for x >= a+1.
func gammaContinuedFraction(a, x float64) float64 {
	const maxIter = 200
	const eps = 1e-14
	const tiny = 1e-300
	lg, _ := math.Lgamma(a)
	b := x + 1 - a
	c := 1 / tiny
	d := 1 / b
	h := d
	for i := 1; i < maxIter; i++ {
		fi := float64(i)
		an := -fi * (fi - a)
		b += 2
		d = an*d + b
		if math.Abs(d) < tiny {
			d = tiny
		}
		c = b + an/c
		if math.Abs(c) < tiny {
			c = tiny
		}
		d = 1 / d
		del := d * c
		h *= del
		if math.Abs(del-1) < eps {
			break
		}
	}
	return math.Exp(-x+a*math.Log(x)-lg) * h
}
