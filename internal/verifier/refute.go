package verifier

import (
	"context"
	"math"
	"math/rand"
	"strings"

	"github.com/arborette/arborette/internal/domain"
	"github.com/arborette/arborette/internal/sandboxclient"
	"github.com/arborette/arborette/internal/verifier/stats"
)

// refutationResult is the refutation battery's output: the three normalized
// refutations and their equal-weight mean score, on which the causally_verified gate
// and the served confidence both turn.
type refutationResult struct {
	placebo   float64
	stability float64
	randconf  float64
	score     float64
}

// refuter runs the refutation battery for one adjusted effect, reusing the adjuster's
// effect-call machinery so every perturbation measures the same objective through the
// same non-hallucinating SQL. rng seeds only the placebo-segment draw; the subsample
// randomness lives in DuckDB's reservoir sampler.
type refuter struct {
	adjuster *adjuster
	cfg      Config
	rng      *rand.Rand
}

// refute stress-tests the adjusted effect three ways and combines the results:
//
//   - placebo: a random pseudo-segment (drawn from an unrelated categorical column's
//     values) measured under the same adjustment must show ~zero effect;
//   - stability: K reservoir subsamples must keep the full-sample sign and stay within
//     the relative band;
//   - random-confounder: adding a synthetic random column to Z must not move the
//     effect.
//
// The score is their equal-weight mean; the caller gates on it. A perturbation that
// cannot be measured (no eligible placebo column, a fully-degenerate subsample)
// contributes 0 rather than being dropped, so an unmeasurable battery cannot inflate
// the score toward a false confirmation.
func (r *refuter) refute(ctx context.Context, plan adjustmentPlan, adjustedEffect float64) (refutationResult, error) {
	placebo, err := r.placebo(ctx, plan, adjustedEffect)
	if err != nil {
		return refutationResult{}, err
	}
	stability, err := r.stability(ctx, plan, adjustedEffect)
	if err != nil {
		return refutationResult{}, err
	}
	randconf, err := r.randConfounder(ctx, plan, adjustedEffect)
	if err != nil {
		return refutationResult{}, err
	}
	return refutationResult{
		placebo:   placebo,
		stability: stability,
		randconf:  randconf,
		score:     stats.RefutationScore(placebo, stability, randconf),
	}, nil
}

// placebo replaces the real segment with a random pseudo-segment of an unrelated
// categorical column and measures its adjusted effect: a sound adjustment shows ~zero
// effect for a segment with no causal relation to the objective. Support-matching is
// approximate (a random value partitions the rows), which the score tolerates because
// it compares magnitudes. When no eligible column exists the placebo is unmeasurable
// and scores 0.
func (r *refuter) placebo(ctx context.Context, plan adjustmentPlan, adjustedEffect float64) (float64, error) {
	segment, ok, err := r.placeboSegment(ctx, plan)
	if err != nil {
		return 0, err
	}
	if !ok {
		return 0, nil
	}
	placeboPlan := plan
	placeboPlan.segment = segment
	resp, err := r.adjuster.effectCall(ctx, placeboPlan, plan.adjustCols, 0, 0)
	if err != nil {
		return 0, err
	}
	eff, ok := reweightLenient(resp.Strata)
	if !ok {
		return 0, nil
	}
	return stats.Attenuation(eff, adjustedEffect), nil
}

// stability runs K reservoir subsamples of the adjusted effect and scores the share
// that keep the full-sample sign and land within the relative band. A subsample too
// sparse to estimate does not count as stable.
func (r *refuter) stability(ctx context.Context, plan adjustmentPlan, adjustedEffect float64) (float64, error) {
	k := r.cfg.RefutationK
	if k <= 0 {
		return 0, nil
	}
	stable := 0
	for i := 0; i < k; i++ {
		resp, err := r.adjuster.effectCall(ctx, plan, plan.adjustCols, 0, r.cfg.SampleFraction)
		if err != nil {
			return 0, err
		}
		eff, ok := reweightLenient(resp.Strata)
		if !ok {
			continue
		}
		if math.Signbit(eff) == math.Signbit(adjustedEffect) &&
			math.Abs(eff-adjustedEffect) <= r.cfg.StabilityBand*math.Abs(adjustedEffect) {
			stable++
		}
	}
	return float64(stable) / float64(k), nil
}

// randConfounder recomputes the adjusted effect with a synthetic random column added
// to the adjustment set: an independent random confounder must not move the effect,
// so a large shift is evidence of an unstable estimate.
func (r *refuter) randConfounder(ctx context.Context, plan adjustmentPlan, adjustedEffect float64) (float64, error) {
	bins := r.cfg.RandomStratifierBins
	if bins <= 0 {
		return 0, nil
	}
	resp, err := r.adjuster.effectCall(ctx, plan, plan.adjustCols, bins, 0)
	if err != nil {
		return 0, err
	}
	eff, ok := reweightLenient(resp.Strata)
	if !ok {
		return 0, nil
	}
	return stats.Attenuation(eff-adjustedEffect, adjustedEffect), nil
}

// placeboSegment draws a random single-value predicate from a categorical column
// unrelated to the finding: it introspects the data source's low-cardinality columns
// and picks one that is not a treatment, adjustment, or objective column, then a
// random one of its distinct values. ok is false when no such column exists.
func (r *refuter) placeboSegment(ctx context.Context, plan adjustmentPlan) ([]domain.Constraint, bool, error) {
	introspect, err := r.adjuster.analyzer.Introspect(ctx, sandboxclient.IntrospectRequest{DataSourceRef: r.adjuster.datasourceRef})
	if err != nil {
		return nil, false, err
	}
	excluded := lowerSet(plan.adjustNames)
	for _, c := range constraintColumns(plan.segment) {
		excluded[strings.ToLower(c)] = true
	}
	for _, c := range expressionColumns(plan.objective.Expr) {
		excluded[strings.ToLower(c)] = true
	}
	// Also exclude columns adjacent to the objective in the graph: a genuine cause of
	// the outcome is not a valid null placebo.
	for _, c := range plan.placeboExclude {
		excluded[strings.ToLower(c)] = true
	}
	// Restrict to text columns: a distinct value is probed as a VARCHAR string, so a
	// string-operand equality compiles only against a text column — a boolean or
	// integer column would need a typed operand and the sandbox would reject a
	// mismatched one.
	var eligible []sandboxclient.Column
	for _, col := range introspect.Schema.Columns {
		if len(col.DistinctValues) > 0 && isTextColumn(col.Type) && !excluded[strings.ToLower(col.Name)] {
			eligible = append(eligible, col)
		}
	}
	if len(eligible) == 0 {
		return nil, false, nil
	}
	col := eligible[r.rng.Intn(len(eligible))]
	value := col.DistinctValues[r.rng.Intn(len(col.DistinctValues))]
	return []domain.Constraint{{Field: col.Name, Op: domain.Equal, Operand: &domain.LiteralValue{String: &value}}}, true, nil
}

// isTextColumn reports whether a DuckDB column type is a string type — the columns a
// string-operand equality can filter. It mirrors the sandbox compiler's own textual
// classification across the CGO firewall (the sandbox package cannot be imported
// here), so a placebo predicate compiles rather than failing a coarse-type check.
func isTextColumn(t string) bool {
	u := strings.ToUpper(strings.TrimSpace(t))
	switch {
	case strings.HasPrefix(u, "VARCHAR"), strings.HasPrefix(u, "CHAR"),
		strings.HasPrefix(u, "TEXT"), u == "BPCHAR", u == "STRING":
		return true
	default:
		return false
	}
}
