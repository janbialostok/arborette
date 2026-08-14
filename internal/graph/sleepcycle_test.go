package graph_test

import (
	"context"
	"errors"
	"testing"

	"github.com/arborette/arborette/internal/domain"
	"github.com/arborette/arborette/internal/graph"
	"github.com/arborette/arborette/internal/testutil"
)

// seedGoalTriplet writes a complete goal-scoped triplet and returns its ids, so
// the eligible-finding query has something shaped like a real Phase-1 finding to
// match (or deliberately not match).
func seedGoalTriplet(t *testing.T, ctx context.Context, repo *graph.Neo4jRepository,
	goalID string, status domain.VerificationStatus, sleepDerived bool) (string, string, string) {
	t.Helper()
	stateID, interventionID, outcomeID := testutil.NewID(t), testutil.NewID(t), testutil.NewID(t)
	cfg := testutil.RequireIntegration(t)
	testutil.RegisterGraphNodeCleanup(t, ctx, cfg, stateID)
	testutil.RegisterGraphNodeCleanup(t, ctx, cfg, interventionID)
	testutil.RegisterGraphNodeCleanup(t, ctx, cfg, outcomeID)

	mustCreate(t, repo.CreateState(ctx, domain.State{ID: stateID, GoalID: goalID}))
	mustCreate(t, repo.CreateIntervention(ctx, domain.Intervention{
		ID: interventionID, GoalID: goalID, Type: domain.InterventionQuery, SleepDerived: sleepDerived,
		Properties: map[string]any{"new_filters": []domain.Constraint{{Field: "qty", Op: domain.GreaterThan, Value: 1}}},
	}))
	mustCreate(t, repo.CreateOutcome(ctx, domain.Outcome{
		ID: outcomeID, GoalID: goalID, VerificationStatus: status, Value: map[string]any{"avg(qty)": 5.0},
	}))
	mustCreate(t, repo.CreatePreConditionFor(ctx, stateID, interventionID))
	mustCreate(t, repo.CreateProduced(ctx, interventionID, outcomeID,
		domain.ProducedEdge{EffectSize: 1, Confidence: 1.0, EpistemicSource: domain.EpistemicObservational}))
	return stateID, interventionID, outcomeID
}

func mustCreate(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatalf("seed: %v", err)
	}
}

func TestListEligibleFindings(t *testing.T) {
	ctx := context.Background()
	repo := newRepo(t, ctx)
	goalID := testutil.NewID(t)
	otherGoal := testutil.NewID(t)

	// One eligible finding per search-eligible status.
	eligible := map[string]bool{}
	for _, status := range domain.SearchEligibleStatuses {
		_, interventionID, _ := seedGoalTriplet(t, ctx, repo, goalID, status, false)
		eligible[interventionID] = true
	}
	// Excluded: wrong status, sleep-derived, and another goal entirely.
	_, unverified, _ := seedGoalTriplet(t, ctx, repo, goalID, domain.VerificationUnverified, false)
	_, rejected, _ := seedGoalTriplet(t, ctx, repo, goalID, domain.VerificationRejected, false)
	_, derived, _ := seedGoalTriplet(t, ctx, repo, goalID, domain.VerificationVerified, true)
	_, foreign, _ := seedGoalTriplet(t, ctx, repo, otherGoal, domain.VerificationVerified, false)

	found, err := repo.ListEligibleFindings(ctx, goalID)
	if err != nil {
		t.Fatalf("list eligible findings: %v", err)
	}

	got := map[string]bool{}
	for _, f := range found {
		got[f.Intervention.ID] = true
		if f.State.ID == "" || f.Outcome.ID == "" {
			t.Fatalf("only complete State→Intervention→Outcome paths may be returned: %+v", f)
		}
	}
	for id := range eligible {
		if !got[id] {
			t.Fatalf("a search-eligible finding was not returned: %s", id)
		}
	}
	for name, id := range map[string]string{
		"unverified":    unverified,
		"rejected":      rejected,
		"sleep-derived": derived,
		"another goal":  foreign,
	} {
		if got[id] {
			t.Fatalf("a %s finding must be excluded from the search space", name)
		}
	}
}

// TestListEligibleFindingsSkipsUngoaledNodes documents the known non-behavior:
// findings written before goal scoping carry no goal_id and are invisible here.
func TestListEligibleFindingsSkipsUngoaledNodes(t *testing.T) {
	ctx := context.Background()
	repo := newRepo(t, ctx)
	goalID := testutil.NewID(t)

	seedGoalTriplet(t, ctx, repo, "", domain.VerificationVerified, false)

	found, err := repo.ListEligibleFindings(ctx, goalID)
	if err != nil {
		t.Fatalf("list eligible findings: %v", err)
	}
	if len(found) != 0 {
		t.Fatalf("a node written without a goal_id must not be returned, got %d", len(found))
	}
}

// TestCreateMetaHeuristicIsIdempotent is the crash-safety guard: a re-run must
// re-write the same node rather than fail the uniqueness constraint, and must
// never resurrect a cleared embedding_pending — that would make a finished
// abstraction look unprocessed and re-embed it forever.
func TestCreateMetaHeuristicIsIdempotent(t *testing.T) {
	ctx := context.Background()
	repo := newRepo(t, ctx)
	goalID := testutil.NewID(t)
	_, interventionID, outcomeID := seedGoalTriplet(t, ctx, repo, goalID, domain.VerificationVerified, false)

	mhID := testutil.NewID(t)
	cfg := testutil.RequireIntegration(t)
	testutil.RegisterGraphNodeCleanup(t, ctx, cfg, mhID)
	refs := []string{interventionID, outcomeID}
	mh := domain.MetaHeuristic{ID: mhID, Definition: "[Primary Population Center] raises [System Output]"}

	if err := repo.CreateMetaHeuristic(ctx, mh, refs); err != nil {
		t.Fatalf("first create: %v", err)
	}
	if err := repo.ClearEmbeddingPending(ctx, mhID); err != nil {
		t.Fatalf("clear embedding pending: %v", err)
	}
	if err := repo.CreateMetaHeuristic(ctx, mh, refs); err != nil {
		t.Fatalf("re-run must MERGE rather than fail: %v", err)
	}

	after, err := repo.GetMetaHeuristic(ctx, mhID)
	if err != nil {
		t.Fatalf("get meta-heuristic: %v", err)
	}
	if after.EmbeddingPending {
		t.Fatal("a re-run must not resurrect a cleared embedding_pending")
	}

	// Exactly one node, and the trace still resolves through it.
	triplets, err := repo.TraceCausalChain(ctx, mhID)
	if err != nil {
		t.Fatalf("trace: %v", err)
	}
	if len(triplets) != 1 {
		t.Fatalf("expected one traced triplet after two creates, got %d", len(triplets))
	}
}

// TestMarkStaleMetaHeuristics pins the deliberate complement with
// SearchEligibleStatuses: rejected marks stale, corrected does not. A
// corrected-triggering sweep would flag the worker's own fresh output on the very
// next run, since corrected outcomes are search-eligible.
func TestMarkStaleMetaHeuristics(t *testing.T) {
	ctx := context.Background()
	repo := newRepo(t, ctx)
	goalID := testutil.NewID(t)

	seed := func(status domain.VerificationStatus) string {
		_, interventionID, outcomeID := seedGoalTriplet(t, ctx, repo, goalID, status, false)
		mhID := testutil.NewID(t)
		cfg := testutil.RequireIntegration(t)
		testutil.RegisterGraphNodeCleanup(t, ctx, cfg, mhID)
		mustCreate(t, repo.CreateMetaHeuristic(ctx,
			domain.MetaHeuristic{ID: mhID, Definition: "abstraction"}, []string{interventionID, outcomeID}))
		return mhID
	}

	rejected := seed(domain.VerificationRejected)
	verified := seed(domain.VerificationVerified)
	corrected := seed(domain.VerificationCorrected)

	if _, err := repo.MarkStaleMetaHeuristics(ctx, goalID); err != nil {
		t.Fatalf("mark stale: %v", err)
	}

	assertStale(t, ctx, repo, rejected, true, "a heuristic abstracted from a rejected outcome must be flagged")
	assertStale(t, ctx, repo, verified, false, "a heuristic with only verified components must stay unflagged")
	assertStale(t, ctx, repo, corrected, false,
		"corrected is search-eligible, so flagging on it would invalidate the worker's own fresh output every run")

	// The sweep recomputes rather than only ever setting true, so an analyst who
	// corrects the rejected evidence un-suppresses the heuristic on the next run.
	// A set-only sweep would leave it hidden forever, with no writer to clear it.
	rejectedOutcome := staleOutcomeID(t, ctx, repo, rejected)
	if err := repo.UpdateOutcomeVerification(ctx, rejectedOutcome, domain.VerificationCorrected, 1.0); err != nil {
		t.Fatalf("correct the rejected outcome: %v", err)
	}
	if _, err := repo.MarkStaleMetaHeuristics(ctx, goalID); err != nil {
		t.Fatalf("re-sweep: %v", err)
	}
	assertStale(t, ctx, repo, rejected, false,
		"once its rejected component is corrected, the heuristic must stop being stale")
}

// staleOutcomeID returns the Outcome a Meta-Heuristic was abstracted from.
func staleOutcomeID(t *testing.T, ctx context.Context, repo *graph.Neo4jRepository, mhID string) string {
	t.Helper()
	triplets, err := repo.TraceCausalChain(ctx, mhID)
	if err != nil {
		t.Fatalf("trace %q: %v", mhID, err)
	}
	if len(triplets) != 1 {
		t.Fatalf("expected one traced triplet for %q, got %d", mhID, len(triplets))
	}
	return triplets[0].Outcome.ID
}

func assertStale(t *testing.T, ctx context.Context, repo *graph.Neo4jRepository, id string, want bool, why string) {
	t.Helper()
	mh, err := repo.GetMetaHeuristic(ctx, id)
	if err != nil {
		t.Fatalf("get meta-heuristic %q: %v", id, err)
	}
	if mh.Stale != want {
		t.Fatalf("%s (stale = %v, want %v)", why, mh.Stale, want)
	}
}

// TestGetNodeNotFound is the guard against the driver reporting zero-versus-many
// records as the same error type: the Sleep-Cycle Worker's crash-safe skip needs
// an absent node to be distinguishable from an unreachable database.
func TestGetNodeNotFound(t *testing.T) {
	ctx := context.Background()
	repo := newRepo(t, ctx)

	_, err := repo.GetMetaHeuristic(ctx, testutil.NewID(t))
	if !errors.Is(err, graph.ErrNotFound) {
		t.Fatalf("error = %v, want one satisfying errors.Is(err, graph.ErrNotFound)", err)
	}

	goalID := testutil.NewID(t)
	_, interventionID, _ := seedGoalTriplet(t, ctx, repo, goalID, domain.VerificationVerified, false)
	intervention, err := repo.GetIntervention(ctx, interventionID)
	if err != nil {
		t.Fatalf("a present node must not report not-found: %v", err)
	}
	if intervention.GoalID != goalID {
		t.Fatalf("goal_id did not round-trip: %+v", intervention)
	}
}

// TestOutcomeSupportRoundTrips pins the S* floor's persistence contract: a
// recorded support round-trips through ListEligibleFindings (the read path that
// feeds bestSingleSegment), and a legacy outcome written without one hydrates
// as 0 — self-excluding from any positive floor.
func TestOutcomeSupportRoundTrips(t *testing.T) {
	ctx := context.Background()
	repo := newRepo(t, ctx)
	goalID := testutil.NewID(t)

	_, _, supportedID := seedGoalTriplet(t, ctx, repo, goalID, domain.VerificationVerified, false)
	_, _, legacyID := seedGoalTriplet(t, ctx, repo, goalID, domain.VerificationVerified, false)
	// CreateOutcome MERGEs on id, so this re-write records support on the first
	// outcome in place.
	mustCreate(t, repo.CreateOutcome(ctx, domain.Outcome{
		ID: supportedID, GoalID: goalID, VerificationStatus: domain.VerificationVerified,
		Value: map[string]any{"avg(qty)": 5.0}, Support: 45,
	}))

	found, err := repo.ListEligibleFindings(ctx, goalID)
	if err != nil {
		t.Fatalf("list eligible findings: %v", err)
	}
	support := map[string]int64{}
	for _, f := range found {
		support[f.Outcome.ID] = f.Outcome.Support
	}
	if support[supportedID] != 45 {
		t.Fatalf("support = %d, want 45 to round-trip", support[supportedID])
	}
	if support[legacyID] != 0 {
		t.Fatalf("a legacy outcome without a recorded support must hydrate as 0, got %d", support[legacyID])
	}
}

// TestSleepDerivedRoundTrips pins the discriminator the search excludes on.
func TestSleepDerivedRoundTrips(t *testing.T) {
	ctx := context.Background()
	repo := newRepo(t, ctx)
	goalID := testutil.NewID(t)

	_, derivedID, _ := seedGoalTriplet(t, ctx, repo, goalID, domain.VerificationVerified, true)
	_, phase1ID, _ := seedGoalTriplet(t, ctx, repo, goalID, domain.VerificationVerified, false)

	derived, err := repo.GetIntervention(ctx, derivedID)
	if err != nil {
		t.Fatalf("get derived intervention: %v", err)
	}
	if !derived.SleepDerived {
		t.Fatal("sleep_derived must round-trip as true")
	}
	phase1, err := repo.GetIntervention(ctx, phase1ID)
	if err != nil {
		t.Fatalf("get phase-1 intervention: %v", err)
	}
	if phase1.SleepDerived {
		t.Fatal("a Phase-1 finding must not read back as sleep-derived")
	}
}
