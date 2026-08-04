package orchestrator

import (
	"cmp"
	"context"
	"encoding/json"
	"log"
	"slices"
	"strings"
	"time"

	"github.com/arborette/arborette/internal/domain"
	"github.com/arborette/arborette/internal/graph"
	"github.com/arborette/arborette/internal/objective"
	"github.com/arborette/arborette/internal/store"
)

// promotionTimeout bounds the whole auto-promotion pass. It runs detached from the
// finished run's context (which may already be dead) and issues one graph read plus
// a bounded number of launcher POSTs, each itself bounded by the launcher's own
// timeout, so this is a backstop against a wedged dependency rather than the pacing.
const promotionTimeout = 2 * time.Minute

// runOutcome is what auto-promotion needs to know about the Phase-1 run it follows:
// the pinned objective and the root baseline every finding is scored against, whether
// that baseline was ever measured, and whether the run completed.
//
// The two flags are carried rather than inferred because they gate different things.
// A verify-track goal's own claim is dispatched regardless of both: it is measured
// directly from the stored claim and is never gated on Phase 1 having succeeded, let
// alone on Phase 1 having rediscovered it.
type runOutcome struct {
	objective   objective.Objective
	baseline    float64
	baselineSet bool
	succeeded   bool
}

// autoPromote routes a finished Phase-1 run's output to Engine B: it dispatches a
// verify-track goal's own claim, then promotes the run's strongest findings for
// causal verification under the per-goal budget.
//
// It runs as its own goroutine off the run's completion, with its own recover, for
// two reasons. The run goroutine has no recover of its own -- the loop's is inside a
// deferred closure that has already returned by the time this is reached -- so a
// panic here would take the orchestrator down; and a degraded Verifier would
// otherwise stall the run's terminal event behind a series of dispatch timeouts. The
// cost is that its own completion event is best-effort: the hub may already have
// evicted the run, which is why the audit record is the durable signal.
func (s *Server) autoPromote(goal store.Goal, run runOutcome) {
	defer func() {
		if r := recover(); r != nil {
			log.Printf("orchestrator: auto-promotion for %q panicked: %v", goal.OptimizationFunctionID, r)
		}
	}()
	// Detached from the run: the loop's context is expired or cancelled by now, and
	// the promotion is about the run's results, not its lifetime.
	ctx, cancel := context.WithTimeout(context.Background(), promotionTimeout)
	defer cancel()

	claims := s.dispatchClaim(ctx, goal)
	promoted := s.promoteFindings(ctx, goal, run)

	if err := s.recordAudit(ctx, "auto_promotion_complete", "causal_verification", map[string]any{
		"optimization_function_id": goal.OptimizationFunctionID,
		"track":                    goal.Track,
		"claims_dispatched":        claims,
		"findings_promoted":        promoted,
	}); err != nil {
		log.Printf("orchestrator: append audit: %v", err)
	}
	s.hub.PublishLive(goal.OptimizationFunctionID, Event{Type: "auto_promotion_complete", Payload: map[string]any{
		"claims_dispatched": claims,
		"findings_promoted": promoted,
	}})
}

// dispatchClaim launches the verify-track goal's own constructed claim, and reports
// how many dispatches it made (at most one).
//
// This is the one dispatch that runs outside the kill switch and outside the run's
// outcome. The claim is what the analyst asked to have tested, measured directly from
// the data rather than found by search, so an operator disabling autonomous promotion
// must not silently stop verify-track goals from verifying anything, and a Phase-1
// run that failed must not either. A goal whose claim could not be constructed
// dispatches nothing: an empty claim is an empty filter conjunction, which would
// reify the global baseline as the analyst's claim.
func (s *Server) dispatchClaim(ctx context.Context, goal store.Goal) int {
	if !hasClaim(goal) {
		return 0
	}
	if err := s.verifierJobs.Launch(ctx, verifierJobName,
		claimArgs(goal.OptimizationFunctionID, goal.DataSourceRef)); err != nil {
		log.Printf("orchestrator: dispatch claim for %q: %v", goal.OptimizationFunctionID, err)
		if auditErr := s.recordAudit(ctx, "auto_promotion_failure", "causal_verification", map[string]any{
			"optimization_function_id": goal.OptimizationFunctionID,
			"kind":                     verifierKindClaim,
			"error":                    err.Error(),
		}); auditErr != nil {
			log.Printf("orchestrator: append audit: %v", auditErr)
		}
		return 0
	}
	return 1
}

// hasClaim reports whether the goal is a verify-track goal carrying a constructed
// claim. The claim dispatch and the promotion narrowing must agree on this: a goal
// that dispatches no claim must not have its promotion narrowed to one either, and one
// predicate is what makes that agreement structural rather than a coincidence of two
// matching conditions.
func hasClaim(goal store.Goal) bool {
	return goal.Track == store.TrackVerify && len(goal.Claim) > 0
}

// promoteFindings dispatches the run's findings for verification under the per-goal
// budget: on the explore track the top-N by support-shrunk score, on the verify track
// the findings that match the claim. Both are autonomous spend, so both sit behind
// the kill switch and both are charged.
//
// Everything here needs the run to have produced comparable findings, so it needs a
// successful run and a measured baseline: without the baseline there is no delta to
// rank on, and a failed run's findings are an arbitrary prefix of the search.
func (s *Server) promoteFindings(ctx context.Context, goal store.Goal, run runOutcome) int {
	if !s.router.AutoPromoteEnabled || !run.succeeded || !run.baselineSet {
		return 0
	}
	findings, err := s.repo.ListEligibleFindings(ctx, goal.OptimizationFunctionID)
	if err != nil {
		log.Printf("orchestrator: list eligible findings for %q: %v", goal.OptimizationFunctionID, err)
		if auditErr := s.recordAudit(ctx, "auto_promotion_failure", "causal_verification", map[string]any{
			"optimization_function_id": goal.OptimizationFunctionID,
			"error":                    err.Error(),
		}); auditErr != nil {
			log.Printf("orchestrator: append audit: %v", auditErr)
		}
		return 0
	}

	ranked := rankFindings(findings, run, float64(s.router.AutoPromoteShrinkageK))
	// Narrowing to the claim's neighbourhood only makes sense when there is a claim.
	// A verify-track goal whose claim failed validation carries none, and matching
	// against it would select nothing -- leaving that goal with strictly less
	// verification than the same goal would have got on the explore track, as a silent
	// second cost of an intake failure it is already being told about.
	if hasClaim(goal) {
		ranked = claimMatches(ranked, goal.Claim)
	}
	// Both tracks stop at top-N. The per-goal budget would refuse the excess anyway,
	// as a non-fault rejection per dispatch, so launching past N buys nothing and
	// spends a request on each refusal.
	selected := topN(ranked, s.router.AutoPromoteTopN)
	return s.dispatchVerifications(ctx, goal, selected, true, "auto_promotion_failure")
}

// rankedFinding is one eligible finding reduced to what promotion selects on: its
// intervention id, its segment predicate, and its support-shrunk score.
type rankedFinding struct {
	interventionID string
	filters        []domain.Constraint
	score          float64
	// overlap is the share of the goal's claim this finding restates, set only on the
	// verify track where it decides ordering.
	overlap float64
}

// rankFindings scores every eligible finding against the run's baseline and sorts them
// strongest-first. A finding that cannot be scored -- an undecodable filter property,
// a value the objective cannot read, no measured support -- is dropped rather than
// ranked at zero, so a malformed record can never occupy a promotion slot.
//
// Ties break on the intervention id so a re-run promotes the same set: slices.SortFunc
// is not stable, and without a tiebreak a capped selection over tied findings would
// verify a different subset each time.
func rankFindings(findings []graph.CausalTriplet, run runOutcome, k float64) []rankedFinding {
	ranked := make([]rankedFinding, 0, len(findings))
	for _, f := range findings {
		// A goal's own claim is reified as an ordinary observational finding, so a later
		// run finds it here -- and being the claim, it matches the claim perfectly and
		// sorts first. Promoting it would charge the analyst's own verification to the
		// autonomous budget, racing the exempt dispatch for the same coalesced key.
		if claimDerived, _ := f.Intervention.Properties[domain.PropClaimDerived].(bool); claimDerived {
			continue
		}
		value, ok := objective.NumericValue(f.Outcome.Value, run.objective.Label)
		if !ok || f.Outcome.Support <= 0 {
			continue
		}
		filters, err := domain.DecodeConstraints(f.Intervention.Properties[domain.PropEffectiveFilters])
		if err != nil || len(filters) == 0 {
			continue
		}
		ranked = append(ranked, rankedFinding{
			interventionID: f.Intervention.ID,
			filters:        filters,
			score:          domain.ShrunkScore(value, run.baseline, f.Outcome.Support, k, run.objective.Direction),
		})
	}
	slices.SortFunc(ranked, func(a, b rankedFinding) int {
		if d := cmp.Compare(b.score, a.score); d != 0 {
			return d
		}
		return strings.Compare(a.interventionID, b.interventionID)
	})
	return ranked
}

func topN(ranked []rankedFinding, n int) []string {
	if n < 0 || n > len(ranked) {
		n = len(ranked)
	}
	ids := make([]string, 0, n)
	for _, f := range ranked[:n] {
		ids = append(ids, f.interventionID)
	}
	return ids
}

// claimMatches keeps the Phase-1 findings that restate the goal's claim, so a
// verify-track goal spends its budget on the findings that bear on what the analyst
// asserted rather than on the strongest segments generally.
//
// The match is deterministic filter overlap -- how much of the claim's conjunction a
// finding's own conjunction contains -- re-ordered so the closest matches lead. A
// stable sort keeps the shrunk-score order the caller already imposed within an
// overlap tier, so among findings that match the claim equally well the
// better-evidenced one goes first. A finding sharing none of the claim's predicates is
// not about the claim and is dropped entirely.
func claimMatches(ranked []rankedFinding, claim []byte) []rankedFinding {
	spec, ok := decodeClaim(claim)
	if !ok || len(spec.Filters) == 0 {
		return nil
	}
	claimed := predicateSet(spec.Filters)

	matches := make([]rankedFinding, 0, len(ranked))
	for _, f := range ranked {
		f.overlap = overlapFraction(predicateSet(f.filters), claimed)
		if f.overlap == 0 {
			continue
		}
		matches = append(matches, f)
	}
	slices.SortStableFunc(matches, func(a, b rankedFinding) int { return cmp.Compare(b.overlap, a.overlap) })
	return matches
}

// predicateSet renders a filter conjunction to the canonical per-predicate encodings
// it is a set of, so two conjunctions are compared the same way the derived-id scheme
// compares them -- by predicate identity, not by rendered text.
func predicateSet(filters []domain.Constraint) map[string]bool {
	canonical, err := domain.CanonicalFilters(filters)
	if err != nil {
		return nil
	}
	set := map[string]bool{}
	for _, p := range domain.CanonicalComponents(canonical) {
		set[p] = true
	}
	return set
}

// overlapFraction is the share of the claim's predicates a finding restates.
func overlapFraction(finding, claimed map[string]bool) float64 {
	if len(claimed) == 0 {
		return 0
	}
	shared := 0
	for p := range claimed {
		if finding[p] {
			shared++
		}
	}
	return float64(shared) / float64(len(claimed))
}

// decodeClaim reads a goal's stored claim, reporting false when the goal carries none.
// A stored claim that will not decode is logged and treated as absent: the claim is
// validated before it is written, so this can only mean a hand-edited row, and no
// dispatch is a safer answer than a dispatch against a claim nothing can read.
func decodeClaim(claim []byte) (domain.ClaimSpec, bool) {
	if len(claim) == 0 {
		return domain.ClaimSpec{}, false
	}
	var spec domain.ClaimSpec
	if err := json.Unmarshal(claim, &spec); err != nil {
		log.Printf("orchestrator: decode stored claim: %v", err)
		return domain.ClaimSpec{}, false
	}
	return spec, true
}
