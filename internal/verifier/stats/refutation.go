package stats

import "math"

// RefutationScore combines the three normalized refutations into one scalar in
// [0, 1] by equal-weight mean: the placebo (a random pseudo-segment shows ~zero
// effect), subsample stability, and random-confounder insertion. The gate compares
// this against the configured threshold. Kept here so the verifier's refutation
// scoring stays pure-Go and CGO-free alongside the discovery statistics.
func RefutationScore(placebo, stability, randconf float64) float64 {
	return (placebo + stability + randconf) / 3
}

// Attenuation scores how well a perturbed effect agrees with the reference effect on
// a 0–1 scale: 1 when the numerator (the placebo effect itself, or the shift a
// random confounder induced) is zero relative to the reference, falling to 0 as it
// reaches or exceeds the reference magnitude. A zero reference is unscorable and
// returns 0 (no agreement can be established).
func Attenuation(numerator, reference float64) float64 {
	if reference == 0 {
		return 0
	}
	ratio := math.Abs(numerator) / math.Abs(reference)
	if ratio > 1 {
		ratio = 1
	}
	return 1 - ratio
}
