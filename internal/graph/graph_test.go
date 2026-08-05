package graph_test

import (
	"context"
	"testing"

	"github.com/arborette/arborette/internal/domain"
	"github.com/arborette/arborette/internal/graph"
	"github.com/arborette/arborette/internal/testutil"
)

func newRepo(t *testing.T, ctx context.Context) *graph.Neo4jRepository {
	t.Helper()
	cfg := testutil.RequireIntegration(t)
	repo, err := graph.NewNeo4jRepository(ctx, cfg.Neo4j.URI, cfg.Neo4j.User, cfg.Neo4j.Password)
	if err != nil {
		t.Fatalf("connect neo4j: %v", err)
	}
	t.Cleanup(func() { repo.Close(ctx) })
	if err := repo.InitSchema(ctx); err != nil {
		t.Fatalf("init schema: %v", err)
	}
	return repo
}

// seedTriplet creates a State→Intervention→Outcome chain with its edges and
// returns the three ids.
func seedTriplet(t *testing.T, ctx context.Context, repo *graph.Neo4jRepository) (string, string, string) {
	t.Helper()
	stateID, interventionID, outcomeID := testutil.NewID(t), testutil.NewID(t), testutil.NewID(t)

	if err := repo.CreateState(ctx, domain.State{ID: stateID, Properties: map[string]any{"latency_ms": 210.0}}); err != nil {
		t.Fatalf("create state: %v", err)
	}
	if err := repo.CreateIntervention(ctx, domain.Intervention{ID: interventionID, Type: domain.InterventionQuery, Properties: map[string]any{"threshold": 0.5}}); err != nil {
		t.Fatalf("create intervention: %v", err)
	}
	if err := repo.CreateOutcome(ctx, domain.Outcome{ID: outcomeID, VerificationStatus: domain.VerificationVerified, Value: map[string]any{"delta": -12.0}}); err != nil {
		t.Fatalf("create outcome: %v", err)
	}
	if err := repo.CreatePreConditionFor(ctx, stateID, interventionID); err != nil {
		t.Fatalf("create pre_condition_for: %v", err)
	}
	if err := repo.CreateProduced(ctx, interventionID, outcomeID, domain.ProducedEdge{EffectSize: -12.0, Confidence: 1.0}); err != nil {
		t.Fatalf("create produced: %v", err)
	}
	return stateID, interventionID, outcomeID
}

func TestNodeAndEdgeRoundTrip(t *testing.T) {
	ctx := context.Background()
	repo := newRepo(t, ctx)
	stateID, interventionID, outcomeID := seedTriplet(t, ctx, repo)

	state, err := repo.GetState(ctx, stateID)
	if err != nil {
		t.Fatalf("get state: %v", err)
	}
	if state.ID != stateID || state.Properties["latency_ms"] != 210.0 {
		t.Fatalf("state round-trip mismatch: %+v", state)
	}

	intervention, err := repo.GetIntervention(ctx, interventionID)
	if err != nil {
		t.Fatalf("get intervention: %v", err)
	}
	if intervention.Type != domain.InterventionQuery {
		t.Fatalf("intervention type mismatch: %+v", intervention)
	}

	outcome, err := repo.GetOutcome(ctx, outcomeID)
	if err != nil {
		t.Fatalf("get outcome: %v", err)
	}
	if outcome.VerificationStatus != domain.VerificationVerified {
		t.Fatalf("outcome status mismatch: %+v", outcome)
	}
}

func TestCreateMetaHeuristicPendingLifecycle(t *testing.T) {
	ctx := context.Background()
	repo := newRepo(t, ctx)
	_, interventionID, outcomeID := seedTriplet(t, ctx, repo)

	mhID := testutil.NewID(t)
	if err := repo.CreateMetaHeuristic(ctx,
		domain.MetaHeuristic{ID: mhID, Definition: "reducing threshold restores latency"},
		[]string{interventionID, outcomeID},
	); err != nil {
		t.Fatalf("create meta-heuristic: %v", err)
	}

	created, err := repo.GetMetaHeuristic(ctx, mhID)
	if err != nil {
		t.Fatalf("get meta-heuristic: %v", err)
	}
	if !created.EmbeddingPending {
		t.Fatalf("a freshly created meta-heuristic must be embedding-pending: %+v", created)
	}

	if err := repo.ClearEmbeddingPending(ctx, mhID); err != nil {
		t.Fatalf("clear embedding pending: %v", err)
	}
	cleared, err := repo.GetMetaHeuristic(ctx, mhID)
	if err != nil {
		t.Fatalf("get meta-heuristic after clear: %v", err)
	}
	if cleared.EmbeddingPending {
		t.Fatalf("clearing the flag must leave the meta-heuristic not pending: %+v", cleared)
	}
}

// TestMetaHeuristicGoalScope covers the graph side of goal scoping: a goal set at
// create round-trips through GetMetaHeuristic and ListMetaHeuristics, a legacy
// node created without a goal heals to a real goal on re-abstraction, and a
// goal-less re-issue never re-blanks a node that already carries a goal.
func TestMetaHeuristicGoalScope(t *testing.T) {
	ctx := context.Background()
	repo := newRepo(t, ctx)
	_, interventionID, _ := seedTriplet(t, ctx, repo)

	// A node created with a goal carries it through both read paths.
	scopedID, goalA := testutil.NewID(t), testutil.NewID(t)
	if err := repo.CreateMetaHeuristic(ctx,
		domain.MetaHeuristic{ID: scopedID, Definition: "scoped at creation", GoalID: goalA},
		[]string{interventionID},
	); err != nil {
		t.Fatalf("create scoped meta-heuristic: %v", err)
	}
	got, err := repo.GetMetaHeuristic(ctx, scopedID)
	if err != nil {
		t.Fatalf("get scoped: %v", err)
	}
	if got.GoalID != goalA {
		t.Fatalf("goal_id must round-trip through GetMetaHeuristic, got %q want %q", got.GoalID, goalA)
	}
	all, err := repo.ListMetaHeuristics(ctx)
	if err != nil {
		t.Fatalf("list meta-heuristics: %v", err)
	}
	if goalOfID(all, scopedID) != goalA {
		t.Fatalf("goal_id must round-trip through ListMetaHeuristics, got %q", goalOfID(all, scopedID))
	}

	// A legacy node created without a goal reads back empty, heals to a real goal on
	// re-abstraction, and is never re-blanked by a later goal-less re-issue.
	legacyID, goalB := testutil.NewID(t), testutil.NewID(t)
	if err := repo.CreateMetaHeuristic(ctx,
		domain.MetaHeuristic{ID: legacyID, Definition: "legacy, no goal"},
		[]string{interventionID},
	); err != nil {
		t.Fatalf("create legacy meta-heuristic: %v", err)
	}
	if got, _ := repo.GetMetaHeuristic(ctx, legacyID); got.GoalID != "" {
		t.Fatalf("a legacy node must read back with an empty goal, got %q", got.GoalID)
	}
	if err := repo.CreateMetaHeuristic(ctx,
		domain.MetaHeuristic{ID: legacyID, Definition: "legacy, now scoped", GoalID: goalB},
		[]string{interventionID},
	); err != nil {
		t.Fatalf("re-abstract legacy meta-heuristic: %v", err)
	}
	if got, _ := repo.GetMetaHeuristic(ctx, legacyID); got.GoalID != goalB {
		t.Fatalf("re-abstraction must heal the legacy node's goal, got %q want %q", got.GoalID, goalB)
	}
	if err := repo.CreateMetaHeuristic(ctx,
		domain.MetaHeuristic{ID: legacyID, Definition: "goal-less re-issue"},
		[]string{interventionID},
	); err != nil {
		t.Fatalf("goal-less re-issue: %v", err)
	}
	if got, _ := repo.GetMetaHeuristic(ctx, legacyID); got.GoalID != goalB {
		t.Fatalf("a goal-less re-issue must not re-blank a scoped node, got %q want %q", got.GoalID, goalB)
	}
}

func goalOfID(mhs []domain.MetaHeuristic, id string) string {
	for _, mh := range mhs {
		if mh.ID == id {
			return mh.GoalID
		}
	}
	return ""
}

// TestGetMetaHeuristicsBatch pins the hydration read behind a similarity search:
// ids with no node come back absent rather than erroring, and no ids is an empty
// answer rather than a failure. (Whether no ids also skips the round trip is not
// observable from here — both variants return the same empty result.)
func TestGetMetaHeuristicsBatch(t *testing.T) {
	ctx := context.Background()
	repo := newRepo(t, ctx)
	_, interventionID, _ := seedTriplet(t, ctx, repo)

	presentIDs := []string{testutil.NewID(t), testutil.NewID(t)}
	for _, id := range presentIDs {
		if err := repo.CreateMetaHeuristic(ctx,
			domain.MetaHeuristic{ID: id, Definition: "definition " + id},
			[]string{interventionID},
		); err != nil {
			t.Fatalf("create meta-heuristic: %v", err)
		}
	}
	absentID := testutil.NewID(t)

	got, err := repo.GetMetaHeuristics(ctx, append(append([]string{}, presentIDs...), absentID))
	if err != nil {
		t.Fatalf("get meta-heuristics: %v", err)
	}
	if len(got) != len(presentIDs) {
		t.Fatalf("fetched %d meta-heuristic(s), want only the %d seeded: %+v", len(got), len(presentIDs), got)
	}
	for _, id := range presentIDs {
		if !containsID(got, id) {
			t.Fatalf("expected %s in the batch result, got %+v", id, got)
		}
	}
	if containsID(got, absentID) {
		t.Fatalf("an id with no node must be absent, not fabricated: %+v", got)
	}

	empty, err := repo.GetMetaHeuristics(ctx, nil)
	if err != nil {
		t.Fatalf("get meta-heuristics for no ids: %v", err)
	}
	if len(empty) != 0 {
		t.Fatalf("expected an empty result for no ids, got %+v", empty)
	}
}

func TestCreateMetaHeuristicRejectsBadReferences(t *testing.T) {
	ctx := context.Background()
	repo := newRepo(t, ctx)
	_, interventionID, _ := seedTriplet(t, ctx, repo)

	cases := map[string][]string{
		"nonexistent": {interventionID, testutil.NewID(t)},
		"duplicate":   {interventionID, interventionID},
	}
	for name, refs := range cases {
		t.Run(name, func(t *testing.T) {
			mhID := testutil.NewID(t)
			err := repo.CreateMetaHeuristic(ctx,
				domain.MetaHeuristic{ID: mhID, Definition: "bad refs"}, refs,
			)
			if err == nil {
				t.Fatalf("expected error for %s references", name)
			}
			if _, err := repo.GetMetaHeuristic(ctx, mhID); err == nil {
				t.Fatalf("meta-heuristic node was left behind after rejected create")
			}
		})
	}
}

func TestTraceCausalChain(t *testing.T) {
	ctx := context.Background()
	repo := newRepo(t, ctx)
	stateID, interventionID, outcomeID := seedTriplet(t, ctx, repo)

	mhID := testutil.NewID(t)
	if err := repo.CreateMetaHeuristic(ctx,
		domain.MetaHeuristic{ID: mhID, Definition: "abstraction"},
		[]string{interventionID},
	); err != nil {
		t.Fatalf("create meta-heuristic: %v", err)
	}

	triplets, err := repo.TraceCausalChain(ctx, mhID)
	if err != nil {
		t.Fatalf("trace causal chain: %v", err)
	}
	if len(triplets) != 1 {
		t.Fatalf("expected 1 triplet, got %d", len(triplets))
	}
	tr := triplets[0]
	if tr.State.ID != stateID || tr.Intervention.ID != interventionID || tr.Outcome.ID != outcomeID {
		t.Fatalf("triplet ids mismatch: %+v", tr)
	}
}

func TestUpdateOutcomeVerification(t *testing.T) {
	ctx := context.Background()
	repo := newRepo(t, ctx)
	_, _, outcomeID := seedTriplet(t, ctx, repo)

	if err := repo.UpdateOutcomeVerification(ctx, outcomeID, domain.VerificationConfirmed, 1.0); err != nil {
		t.Fatalf("update verification: %v", err)
	}
	outcome, err := repo.GetOutcome(ctx, outcomeID)
	if err != nil {
		t.Fatalf("get outcome: %v", err)
	}
	if outcome.VerificationStatus != domain.VerificationConfirmed {
		t.Fatalf("expected confirmed status, got %s", outcome.VerificationStatus)
	}
}

func containsID(list []domain.MetaHeuristic, id string) bool {
	for _, mh := range list {
		if mh.ID == id {
			return true
		}
	}
	return false
}
