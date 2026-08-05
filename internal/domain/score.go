package domain

// ShrunkScore ranks a measured segment by how far it moves the objective, discounted
// by how much evidence backs the move. The weight support/(support+k) is the
// empirical-Bayes shrinkage of the measured value toward the baseline, and
// multiplying the delta by it is algebraically identical to shrinking the value first
// and then taking the delta.
//
// Ranking on the raw delta instead puts a 54-row segment that hit 100% above an
// 800-row segment at 98.9% — promoting the thinner evidence as the stronger finding,
// which is precisely backwards for any consumer that can only act on what was ranked
// first. k is the support level at which the weight reaches one half, so a caller
// passes the row count it considers sufficient evidence.
//
// It is shared rather than per-caller because publication selection and verification
// auto-promotion must agree on which findings are the strongest: two rankings that
// drifted would publish one set and causally test another.
func ShrunkScore(value, baseline float64, support int64, k float64, direction TargetDirection) float64 {
	return DirectionalDelta(value, baseline, direction) * float64(support) / (float64(support) + k)
}

// DirectionalDelta is the value's movement in the objective's desired direction, so a
// plain descending sort ranks both directions. Every ranked stage shares it: a sign
// error here silently inverts a whole ranking, so it gets one definition rather than
// one per stage.
func DirectionalDelta(value, baseline float64, direction TargetDirection) float64 {
	d := value - baseline
	if direction == Minimize {
		return -d
	}
	return d
}
