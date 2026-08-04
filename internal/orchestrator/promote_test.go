package orchestrator

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/arborette/arborette/internal/llm"

	"github.com/arborette/arborette/internal/domain"
	"github.com/arborette/arborette/internal/graph"
	"github.com/arborette/arborette/internal/objective"
	"github.com/arborette/arborette/internal/store"
)

// errLaunch is a launcher fault, and panicLauncher is a collaborator that panics --
// the two failure modes promotion has to contain on its own, since it runs after the
// loop's recover has already returned.
var errLaunch = errors.New("verifier unreachable")

type panicLauncher struct{}

func (panicLauncher) Launch(context.Context, string, map[string]string) error {
	panic("launcher exploded")
}

// promoteObjective is the pinned objective the promotion tests rank against: maximize
// the average of a "rate" column.
func promoteObjective() objective.Objective {
	return objective.Objective{Aggregation: "avg", Label: "avg(rate)", Direction: domain.Maximize}
}

// finding builds one eligible Phase-1 finding with the given segment, measured value,
// and support.
func finding(id, field string, value float64, support int64) graph.CausalTriplet {
	return graph.CausalTriplet{
		Intervention: domain.Intervention{ID: id, Properties: map[string]any{
			domain.PropEffectiveFilters: []domain.Constraint{{Field: field, Op: domain.Equal, Operand: strLiteral("yes")}},
		}},
		Outcome: domain.Outcome{ID: "o-" + id, Value: map[string]any{"avg(rate)": value}, Support: support},
	}
}

func strLiteral(s string) *domain.LiteralValue { return &domain.LiteralValue{String: &s} }

// verifyTrackGoal is a verify-track goal carrying the claim intake validated.
func verifyTrackGoal(t *testing.T, field string) store.Goal {
	t.Helper()
	goal := verifyGoal()
	goal.Track = store.TrackVerify
	claim, err := json.Marshal(domain.ClaimSpec{
		Filters:   []domain.Constraint{{Field: field, Op: domain.Equal, Operand: strLiteral("yes")}},
		Direction: domain.Maximize,
	})
	if err != nil {
		t.Fatalf("marshal claim: %v", err)
	}
	goal.Claim = claim
	return goal
}

// promoteServer wires a server whose auto-promotion runs against the given findings.
func promoteServer(findings []graph.CausalTriplet, launcher *fakeLauncher, router *RouterConfig) *Server {
	return testServer{
		repo:         &fakeRepo{findings: findings},
		goals:        &fakeGoals{},
		verifierJobs: launcher,
		router:       router,
	}.build()
}

func succeededRun() runOutcome {
	return runOutcome{objective: promoteObjective(), baseline: 0.5, baselineSet: true, succeeded: true}
}

// kinds counts the dispatches by kind so a test can assert what promotion sent
// without depending on ordering.
func kinds(launches []map[string]string) map[string]int {
	out := map[string]int{}
	for _, args := range launches {
		out[args["kind"]]++
	}
	return out
}

// TestAutoPromoteRanksByShrunkScore: promotion takes the top-N by support-shrunk
// score, so a thin high-delta segment ranks below a well-supported one. Ranking on the
// raw delta would promote the thinnest evidence first, which is backwards for a
// budget that can only verify a few findings.
func TestAutoPromoteRanksByShrunkScore(t *testing.T) {
	launcher := &fakeLauncher{}
	router := defaultRouter()
	router.AutoPromoteTopN = 1
	srv := promoteServer([]graph.CausalTriplet{
		finding("thin", "a", 1.0, 5),    // delta 0.50, shrunk to ~0.07
		finding("solid", "b", 0.8, 600), // delta 0.30, shrunk to ~0.29
	}, launcher, &router)

	srv.autoPromote(verifyGoal(), succeededRun())

	dispatches := launcher.dispatches()
	if len(dispatches) != 1 {
		t.Fatalf("dispatched %d, want the single top-N slot", len(dispatches))
	}
	if got := dispatches[0]["intervention_id"]; got != "solid" {
		t.Fatalf("promoted %q, want the better-evidenced finding", got)
	}
	if dispatches[0]["budgeted"] != "true" {
		t.Fatalf("autonomous promotion must be charged to the budget: %v", dispatches[0])
	}
}

// TestAutoPromoteRequiresACompletedRun: findings are only comparable when the run
// completed and measured its baseline — without the baseline there is no delta to rank
// on, and a failed run's findings are an arbitrary prefix of the search.
func TestAutoPromoteRequiresACompletedRun(t *testing.T) {
	cases := map[string]runOutcome{
		"failed run":   {objective: promoteObjective(), baseline: 0.5, baselineSet: true, succeeded: false},
		"no baseline":  {objective: promoteObjective(), succeeded: true},
		"document run": {succeeded: true},
	}
	for name, run := range cases {
		t.Run(name, func(t *testing.T) {
			launcher := &fakeLauncher{}
			promoteServer([]graph.CausalTriplet{finding("f", "a", 0.9, 100)}, launcher, nil).
				autoPromote(verifyGoal(), run)
			if n := len(launcher.dispatches()); n != 0 {
				t.Fatalf("dispatched %d despite %s", n, name)
			}
		})
	}
}

// TestAutoPromoteKillSwitchSparesTheClaim is the kill switch's exact scope: it
// suppresses autonomous promotion, but a verify-track goal's own claim still
// dispatches. Gating the claim too would let an operator disabling automation
// silently stop verify-track goals from verifying anything.
func TestAutoPromoteKillSwitchSparesTheClaim(t *testing.T) {
	launcher := &fakeLauncher{}
	router := defaultRouter()
	router.AutoPromoteEnabled = false
	srv := promoteServer([]graph.CausalTriplet{finding("f", "a", 0.9, 100)}, launcher, &router)

	srv.autoPromote(verifyTrackGoal(t, "a"), succeededRun())

	got := kinds(launcher.dispatches())
	if got[verifierKindClaim] != 1 {
		t.Fatalf("the primary claim must dispatch under the kill switch: %v", launcher.dispatches())
	}
	if got[verifierKindVerify] != 0 {
		t.Fatalf("the kill switch must suppress autonomous promotion: %v", launcher.dispatches())
	}
}

// TestAutoPromoteClaimIsNeverGatedOnPhaseOne: a verify-track goal whose Phase-1 run
// failed still gets its claim tested. The claim is measured directly from the stored
// filters, so it never depended on the search finding it.
func TestAutoPromoteClaimIsNeverGatedOnPhaseOne(t *testing.T) {
	launcher := &fakeLauncher{}
	srv := promoteServer(nil, launcher, nil)

	srv.autoPromote(verifyTrackGoal(t, "a"), runOutcome{objective: promoteObjective(), succeeded: false})

	dispatches := launcher.dispatches()
	if len(dispatches) != 1 || dispatches[0]["kind"] != verifierKindClaim {
		t.Fatalf("dispatches = %v, want the claim alone", dispatches)
	}
	if _, charged := dispatches[0]["budgeted"]; charged {
		t.Fatalf("the primary claim carries no budget flag: %v", dispatches[0])
	}
}

// TestAutoPromoteSkipsUnconstructedClaim: a verify-track goal whose claim could not be
// constructed dispatches nothing claim-related. An empty claim is an empty filter
// conjunction, which would reify the global baseline as the analyst's tested claim.
func TestAutoPromoteSkipsUnconstructedClaim(t *testing.T) {
	launcher := &fakeLauncher{}
	goal := verifyGoal()
	goal.Track = store.TrackVerify
	goal.ClaimError = "the claim names filter columns that are not in the data source: region"

	promoteServer(nil, launcher, nil).autoPromote(goal, succeededRun())

	if n := kinds(launcher.dispatches())[verifierKindClaim]; n != 0 {
		t.Fatalf("dispatched %d claims for a goal that has none", n)
	}
}

// TestAutoPromoteVerifyTrackPromotesClaimMatches: on the verify track the budget goes
// to the findings that restate the claim, not to the strongest segments generally --
// plus the claim itself, exactly once.
func TestAutoPromoteVerifyTrackPromotesClaimMatches(t *testing.T) {
	launcher := &fakeLauncher{}
	srv := promoteServer([]graph.CausalTriplet{
		finding("matches", "a", 0.9, 100),
		finding("unrelated", "b", 0.95, 500),
	}, launcher, nil)

	srv.autoPromote(verifyTrackGoal(t, "a"), succeededRun())

	dispatches := launcher.dispatches()
	if got := kinds(dispatches); got[verifierKindClaim] != 1 {
		t.Fatalf("the claim must dispatch exactly once: %v", dispatches)
	}
	var promoted []string
	for _, args := range dispatches {
		if args["kind"] == verifierKindVerify {
			promoted = append(promoted, args["intervention_id"])
		}
	}
	if len(promoted) != 1 || promoted[0] != "matches" {
		t.Fatalf("promoted %v, want only the claim-matching finding", promoted)
	}
}

// TestAutoPromoteIsolatesFailures: a launcher fault and a graph read failure are
// audited and swallowed. Promotion runs after the run's terminal write, so it cannot
// change how the run ended -- and a panic in it must not reach the run goroutine,
// which has no recover of its own.
func TestAutoPromoteIsolatesFailures(t *testing.T) {
	t.Run("launcher error", func(t *testing.T) {
		launcher := &fakeLauncher{err: errLaunch}
		audits := &fakeAudits{}
		srv := testServer{
			repo:         &fakeRepo{findings: []graph.CausalTriplet{finding("f", "a", 0.9, 100)}},
			goals:        &fakeGoals{},
			audits:       audits,
			verifierJobs: launcher,
		}.build()

		srv.autoPromote(verifyGoal(), succeededRun())

		if !hasAudit(audits, "auto_promotion_failure") {
			t.Fatalf("a dispatch failure must leave a trail: %+v", audits.records())
		}
		if !hasAudit(audits, "auto_promotion_complete") {
			t.Fatalf("promotion must still report completion: %+v", audits.records())
		}
	})

	t.Run("panicking collaborator", func(t *testing.T) {
		srv := testServer{
			repo:         &fakeRepo{findings: []graph.CausalTriplet{finding("f", "a", 0.9, 100)}},
			goals:        &fakeGoals{},
			verifierJobs: panicLauncher{},
		}.build()
		// Contained by autoPromote's own recover; without it this panic would unwind
		// into the run goroutine and take the process down.
		srv.autoPromote(verifyGoal(), succeededRun())
	})
}

// waitForDispatches polls the launcher until it has recorded at least n dispatches,
// or gives up. Promotion runs on its own goroutine so a degraded Verifier cannot hold
// the run's terminal event, which means the dispatch lands shortly after runLoop
// returns rather than before it.
func waitForDispatches(t *testing.T, launcher *fakeLauncher, n int) []map[string]string {
	t.Helper()
	var dispatches []map[string]string
	for i := 0; i < 200; i++ {
		if dispatches = launcher.dispatches(); len(dispatches) >= n {
			return dispatches
		}
		time.Sleep(5 * time.Millisecond)
	}
	return dispatches
}

func hasAudit(audits *fakeAudits, action string) bool {
	for _, r := range audits.records() {
		if r.Action == action {
			return true
		}
	}
	return false
}

// TestRunLoopRoutesToPromotion pins the seam between the two engines: a completed
// Phase-1 run must hand its objective and baseline to promotion and dispatch.
func TestRunLoopRoutesToPromotion(t *testing.T) {
	launcher := &fakeLauncher{}
	repo := &fakeRepo{findings: []graph.CausalTriplet{finding("f-1", "a", 9.0, 100)}}
	// One root candidate that does not improve, so the loop writes one triplet and
	// completes without expanding.
	claude := &fakeClaude{proposal: llm.Proposal{Candidates: []llm.CandidateIntervention{{Filters: nil}}}}
	sandbox := &fakeSandbox{
		introspect: IntrospectResponse{Schema: schemaDTO{Columns: []columnDTO{{Name: "rate", Type: "DOUBLE"}}}},
		execResps: []ExecuteResponse{
			{Value: map[string]any{"avg(rate)": 0.5}}, // root baseline
			{Value: map[string]any{"avg(rate)": 0.4}}, // candidate (no improvement → stop)
		},
	}
	srv := testServer{
		repo: repo, goals: &fakeGoals{}, claude: claude, sandbox: sandbox, verifierJobs: launcher,
	}.build()

	goal := store.Goal{OptimizationFunctionID: "g1", DataSourceRef: "ref.csv", Track: store.TrackExplore,
		EvaluationMatrix: domain.EvaluationMatrix{Targets: []domain.Target{{Field: "rate", Direction: domain.Maximize, Aggregation: "avg"}}}}
	srv.runLoop(context.Background(), goal, "run-1")

	dispatches := waitForDispatches(t, launcher, 1)
	if len(dispatches) != 1 {
		t.Fatalf("a completed run dispatched %d verifications, want the eligible finding promoted", len(dispatches))
	}
	if dispatches[0]["intervention_id"] != "f-1" || dispatches[0]["datasource_ref"] != "ref.csv" {
		t.Fatalf("dispatch = %v, want the goal's finding and data source", dispatches[0])
	}
	if dispatches[0]["budgeted"] != "true" {
		t.Fatalf("auto-promotion must charge the budget: %v", dispatches[0])
	}
}

// TestRankFindingsDropsUnscorable: a finding whose value, support, or filters cannot
// be read is dropped rather than ranked at zero, so a malformed record can never
// occupy a promotion slot ahead of a real one.
func TestRankFindingsDropsUnscorable(t *testing.T) {
	noValue := finding("no-value", "a", 0.9, 100)
	noValue.Outcome.Value = map[string]any{"avg(other)": 0.9}
	noSupport := finding("no-support", "b", 0.9, 0)
	noFilters := finding("no-filters", "c", 0.9, 100)
	noFilters.Intervention.Properties = map[string]any{}

	ranked := rankFindings([]graph.CausalTriplet{
		noValue, noSupport, noFilters, finding("good", "d", 0.9, 100),
	}, succeededRun(), 30)

	if len(ranked) != 1 || ranked[0].interventionID != "good" {
		t.Fatalf("ranked = %+v, want only the scorable finding", ranked)
	}
}

// TestRankFindingsBreaksTiesDeterministically: slices.SortFunc is not stable, so
// equal-scoring findings need an explicit tiebreak or a capped selection would verify
// a different subset on each run.
func TestRankFindingsBreaksTiesDeterministically(t *testing.T) {
	tied := []graph.CausalTriplet{
		finding("c", "x", 0.9, 100),
		finding("a", "y", 0.9, 100),
		finding("b", "z", 0.9, 100),
	}
	first := rankFindings(tied, succeededRun(), 30)
	for i := 0; i < 5; i++ {
		again := rankFindings(tied, succeededRun(), 30)
		for j := range first {
			if again[j].interventionID != first[j].interventionID {
				t.Fatalf("tied findings ranked differently across runs: %v vs %v", first, again)
			}
		}
	}
	if first[0].interventionID != "a" {
		t.Fatalf("tiebreak = %q, want the lowest intervention id", first[0].interventionID)
	}
}

// TestClaimMatchesOrdersByOverlap: with a multi-predicate claim, the closest matches
// lead, and findings matching equally well keep the shrunk-score order the ranking
// already imposed. This is what decides how a verify-track goal spends its budget.
func TestClaimMatchesOrdersByOverlap(t *testing.T) {
	claim, err := json.Marshal(domain.ClaimSpec{
		Filters: []domain.Constraint{
			{Field: "a", Op: domain.Equal, Operand: strLiteral("yes")},
			{Field: "b", Op: domain.Equal, Operand: strLiteral("yes")},
		},
		Direction: domain.Maximize,
	})
	if err != nil {
		t.Fatalf("marshal claim: %v", err)
	}
	both := finding("both", "a", 0.7, 100)
	both.Intervention.Properties[domain.PropEffectiveFilters] = []domain.Constraint{
		{Field: "a", Op: domain.Equal, Operand: strLiteral("yes")},
		{Field: "b", Op: domain.Equal, Operand: strLiteral("yes")},
	}
	ranked := rankFindings([]graph.CausalTriplet{
		finding("half-strong", "a", 0.95, 500),
		finding("half-weak", "b", 0.6, 100),
		both,
		finding("unrelated", "z", 0.99, 900),
	}, succeededRun(), 30)

	matches := claimMatches(ranked, claim)

	var ids []string
	for _, m := range matches {
		ids = append(ids, m.interventionID)
	}
	// Full overlap first; then the two half-overlap findings in shrunk-score order.
	want := []string{"both", "half-strong", "half-weak"}
	if len(ids) != len(want) {
		t.Fatalf("matches = %v, want %v", ids, want)
	}
	for i := range want {
		if ids[i] != want[i] {
			t.Fatalf("matches = %v, want %v", ids, want)
		}
	}
}

// TestRunLoopRoutesClaimOnFailedRun: a verify-track goal whose Phase-1 run FAILS must
// still get its claim dispatched. The claim is measured directly from the stored
// filters, so gating the promotion hand-off on the run's outcome would silently
// eliminate exactly the case the design calls out.
func TestRunLoopRoutesClaimOnFailedRun(t *testing.T) {
	launcher := &fakeLauncher{}
	// A root proposal failure ends the run terminally, before any baseline.
	claude := &fakeClaude{proposalErr: errors.New("upstream down")}
	sandbox := &fakeSandbox{
		introspect: IntrospectResponse{Schema: schemaDTO{Columns: []columnDTO{{Name: "rate", Type: "DOUBLE"}}}},
	}
	srv := testServer{
		repo: &fakeRepo{}, goals: &fakeGoals{}, claude: claude, sandbox: sandbox, verifierJobs: launcher,
	}.build()

	goal := verifyTrackGoal(t, "a")
	goal.EvaluationMatrix = domain.EvaluationMatrix{
		Targets: []domain.Target{{Field: "rate", Direction: domain.Maximize, Aggregation: "avg"}},
	}
	srv.runLoop(context.Background(), goal, "run-1")

	dispatches := waitForDispatches(t, launcher, 1)
	if len(dispatches) != 1 || dispatches[0]["kind"] != verifierKindClaim {
		t.Fatalf("dispatches = %v, want the claim dispatched despite the failed run", dispatches)
	}
}

// TestAutoPromoteSkipsTheReifiedClaim: the goal's own claim is reified as an ordinary
// observational finding, so a later run finds it in the eligible pool where it matches
// the claim perfectly and sorts first. Promoting it would charge the analyst's own
// verification to the autonomous budget, racing the exempt dispatch for the same
// coalesced key.
func TestAutoPromoteSkipsTheReifiedClaim(t *testing.T) {
	reified := finding("the-claim", "a", 0.9, 300)
	reified.Intervention.Properties[domain.PropClaimDerived] = true
	launcher := &fakeLauncher{}
	srv := promoteServer([]graph.CausalTriplet{reified, finding("ordinary", "a", 0.8, 300)}, launcher, nil)

	srv.autoPromote(verifyTrackGoal(t, "a"), succeededRun())

	for _, args := range launcher.dispatches() {
		if args["intervention_id"] == "the-claim" {
			t.Fatalf("the reified claim was promoted under the budget: %v", args)
		}
	}
	if kinds(launcher.dispatches())[verifierKindClaim] != 1 {
		t.Fatalf("the claim must still dispatch on its own exempt path: %v", launcher.dispatches())
	}
}

// TestDispatchVerificationsIsolatesEachFailure: a batch dispatch is best-effort by
// contract, so one launcher fault is audited and skipped rather than abandoning the
// findings behind it. A regression that returned on the first error would leave a
// correction's re-verification fan-out silently truncated at the first transient
// failure.
func TestDispatchVerificationsIsolatesEachFailure(t *testing.T) {
	launcher := &fakeLauncher{failFor: map[string]error{"f-1": errors.New("verifier down")}}
	audits := &fakeAudits{}
	srv := testServer{goals: &fakeGoals{}, audits: audits, verifierJobs: launcher}.build()

	dispatched := srv.dispatchVerifications(context.Background(), verifyGoal(),
		[]string{"f-1", "f-2", "f-3"}, true, "auto_promotion_failure")

	if dispatched != 2 {
		t.Fatalf("dispatched = %d, want the two findings behind the failing one", dispatched)
	}
	got := launcher.dispatches()
	if len(got) != 2 || got[0]["intervention_id"] != "f-2" || got[1]["intervention_id"] != "f-3" {
		t.Fatalf("dispatches = %v, want f-2 and f-3 launched", got)
	}
	failures := 0
	for _, r := range audits.records() {
		if r.Action == "auto_promotion_failure" {
			failures++
		}
	}
	if failures != 1 {
		t.Fatalf("failure audits = %d, want exactly one for the failed dispatch", failures)
	}
}

// TestPromoteFindingsFallsBackForAClaimlessVerifyGoal: a verify-track goal whose claim
// failed intake validation carries none, so there is no claim to narrow the ranking
// to. It must promote like an explore goal rather than promoting nothing — an intake
// failure the analyst is already being told about must not silently also cost them
// every autonomous verification.
func TestPromoteFindingsFallsBackForAClaimlessVerifyGoal(t *testing.T) {
	launcher := &fakeLauncher{}
	srv := testServer{
		repo:         &fakeRepo{findings: []graph.CausalTriplet{finding("f", "a", 0.9, 100)}},
		goals:        &fakeGoals{},
		verifierJobs: launcher,
	}.build()

	// A verify-track goal carrying no claim: the shape intake persists when the
	// classifier extracted a claim that failed schema validation.
	goal := verifyGoal()
	goal.Track = store.TrackVerify
	goal.Claim = nil
	goal.ClaimError = "the claim names filter columns that are not in the data source: region"

	if promoted := srv.promoteFindings(context.Background(), goal, succeededRun()); promoted != 1 {
		t.Fatalf("promoted = %d, want the run's finding promoted despite the missing claim", promoted)
	}
	if got := launcher.dispatches(); len(got) != 1 || got[0]["intervention_id"] != "f" {
		t.Fatalf("dispatches = %v, want the eligible finding", got)
	}
}
