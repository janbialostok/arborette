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

	pending, err := repo.ListEmbeddingPending(ctx)
	if err != nil {
		t.Fatalf("list embedding pending: %v", err)
	}
	if !containsID(pending, mhID) {
		t.Fatalf("expected %s in embedding-pending list", mhID)
	}

	if err := repo.ClearEmbeddingPending(ctx, mhID); err != nil {
		t.Fatalf("clear embedding pending: %v", err)
	}
	pending, err = repo.ListEmbeddingPending(ctx)
	if err != nil {
		t.Fatalf("list embedding pending after clear: %v", err)
	}
	if containsID(pending, mhID) {
		t.Fatalf("expected %s cleared from embedding-pending list", mhID)
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
