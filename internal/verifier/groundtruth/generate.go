// Package groundtruth generates the bundled synthetic dataset with a planted
// causal structure. The generator is deterministic (seeded) so the emitted CSV
// is a frozen regression fixture: discovery accuracy is measurable only against
// a known graph, and later shells calibrate their defaults against these exact
// rows. The planted structure is deliberately minimal and load-bearing:
//
//   - a confounder Z that causes both X and Y, making X and Y spuriously
//     correlated marginally but independent given Z (the edge Engine B must kill);
//   - a true cause A of Y that survives adjustment for every other parent;
//   - an interaction B×C on Y that raises Y only in one joint cell, so both B and
//     C are true causes of Y while the effect concentrates where conjunction
//     search finds it.
//
// The mixed-type schema (numeric Z/X/A/Y, categorical B/region, boolean C) forces
// all three conditional-independence test kinds — chi-square/G-test, partial
// correlation, and the binned hybrid — to be exercised by the discovery sweep.
package groundtruth

import (
	"math/rand"
	"strconv"
)

// Column names of the emitted dataset. They are exported so the regression test
// and the ground-truth graph reference one spelling.
const (
	ColZ      = "Z"
	ColX      = "X"
	ColA      = "A"
	ColB      = "B"
	ColC      = "C"
	ColY      = "Y"
	ColRegion = "region"
)

// Planted structural coefficients. They are named so the generator-test floors
// document the design intent rather than re-deriving it from magic numbers, and
// so a reseed that preserves row counts but weakens an association fails at the
// generator rather than in a later shell.
const (
	// coefZX is Z's loading on X. corr(X,Z) ≈ coefZX/sqrt(coefZX²+1); at 1.5 that
	// is ≈0.83, strong enough that Z induces a marginal X–Y association a level-0
	// test detects, while X's only path to Y is through Z (so X⊥Y | Z holds).
	coefZX = 1.5
	// coefZY and coefAY are Z's and A's direct loadings on Y. A is independent of
	// every other column, so partial corr(A,Y | any conditioning set) stays high
	// and the A→Y edge survives adjustment.
	coefZY = 1.2
	coefAY = 1.0
	// interactionDelta is the additive shift on Y in the single cell B=b1 ∧ C=true.
	// At 2.0 over σ_Y≈1.9 the cell's Y mean moves ≈1.05σ (well past the ~0.5σ
	// material-effect floor), and each of B and C carries a detectable marginal
	// association with Y (B: δ·P(C)=1.0; C: δ·P(B=b1)≈0.67).
	interactionDelta = 2.0
)

// interactionB and interactionC name the joint cell the interaction fires in.
const (
	interactionB = "b1"
	interactionC = true
)

// bLevels and regionLevels are the categorical value sets. B is low-cardinality
// (3 levels) so the interaction cell B=b1∧C=true clears the support floor at
// ~1/6 of the rows; region is a pure-noise categorical with no planted edge, so
// discovery must leave it unconnected.
var (
	bLevels      = []string{"b0", "b1", "b2"}
	regionLevels = []string{"north", "south", "east", "west"}
)

// Config parameterizes generation. DefaultConfig fixes the frozen fixture.
type Config struct {
	Seed int64
	Rows int
}

// DefaultConfig is the settings the committed CSV is generated from. Rows is
// ~2,000 so every quantile bin and the interaction cell clear the support floor.
func DefaultConfig() Config {
	return Config{Seed: 20260803, Rows: 2000}
}

// Row is one generated observation with typed fields, so the fake analyzer can
// compute aggregates directly and the CSV writer can format each column to the
// type DuckDB's read_csv_auto infers (DOUBLE, VARCHAR, BOOLEAN).
type Row struct {
	Z, X, A, Y float64
	B          string
	C          bool
	Region     string
}

// Dataset is the generated rows plus the header, the single source both the CSV
// renderer and the in-process regression analyzer read from.
type Dataset struct {
	Rows []Row
}

// Header is the CSV column order, shared by the renderer and the introspection
// the fake analyzer reports, so the two cannot drift.
func Header() []string {
	return []string{ColZ, ColX, ColA, ColB, ColC, ColY, ColRegion}
}

// Generate produces the deterministic dataset for cfg. The same seed and row
// count always yield byte-identical rows, which is what makes the committed CSV
// a stable fixture.
func Generate(cfg Config) *Dataset {
	rng := rand.New(rand.NewSource(cfg.Seed))
	rows := make([]Row, 0, cfg.Rows)
	for i := 0; i < cfg.Rows; i++ {
		z := rng.NormFloat64()
		a := rng.NormFloat64()
		b := bLevels[rng.Intn(len(bLevels))]
		c := rng.Intn(2) == 1
		x := coefZX*z + rng.NormFloat64()
		y := coefZY*z + coefAY*a + rng.NormFloat64()
		if b == interactionB && c == interactionC {
			y += interactionDelta
		}
		rows = append(rows, Row{
			Z:      z,
			X:      x,
			A:      a,
			B:      b,
			C:      c,
			Y:      y,
			Region: regionLevels[rng.Intn(len(regionLevels))],
		})
	}
	return &Dataset{Rows: rows}
}

// Records renders the dataset as CSV records (header first). Floats use a fixed
// precision so the committed file is stable across runs; booleans render as
// true/false (DuckDB infers BOOLEAN) and categoricals verbatim (VARCHAR).
func (d *Dataset) Records() [][]string {
	records := make([][]string, 0, len(d.Rows)+1)
	records = append(records, Header())
	for _, r := range d.Rows {
		records = append(records, []string{
			formatFloat(r.Z),
			formatFloat(r.X),
			formatFloat(r.A),
			r.B,
			strconv.FormatBool(r.C),
			formatFloat(r.Y),
			r.Region,
		})
	}
	return records
}

func formatFloat(v float64) string {
	return strconv.FormatFloat(v, 'f', 6, 64)
}
