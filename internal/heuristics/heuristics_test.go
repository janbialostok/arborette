package heuristics_test

import (
	"context"
	"testing"

	"github.com/arborette/arborette/internal/domain"
	"github.com/arborette/arborette/internal/embedding"
	"github.com/arborette/arborette/internal/graph"
	"github.com/arborette/arborette/internal/heuristics"
	"github.com/arborette/arborette/internal/store"
	"github.com/arborette/arborette/internal/testutil"
)

func TestQueryAndTrace(t *testing.T) {
	ctx := context.Background()
	cfg := testutil.RequireIntegration(t)
	testutil.SetupPostgres(t, ctx, cfg)
	testutil.TruncateEmbeddings(t, ctx, cfg)

	repo, err := graph.NewNeo4jRepository(ctx, cfg.Neo4j.URI, cfg.Neo4j.User, cfg.Neo4j.Password)
	if err != nil {
		t.Fatalf("connect neo4j: %v", err)
	}
	t.Cleanup(func() { repo.Close(ctx) })
	if err := repo.InitSchema(ctx); err != nil {
		t.Fatalf("init schema: %v", err)
	}

	p, err := store.NewPool(ctx, cfg.Postgres.ServiceDSN())
	if err != nil {
		t.Fatalf("open pool: %v", err)
	}
	t.Cleanup(p.Close)
	embeddings := store.NewEmbeddingStore(p)
	provider := embedding.NewOllamaProvider(cfg.Ollama.URL, cfg.Ollama.Model, cfg.Embedding.Dimension)

	// Seed a full triplet and a Meta-Heuristic abstracted from the intervention.
	stateID, interventionID, outcomeID := testutil.NewID(t), testutil.NewID(t), testutil.NewID(t)
	mustCreate(t, repo.CreateState(ctx, domain.State{ID: stateID, Properties: map[string]any{"latency_ms": 210.0}}))
	mustCreate(t, repo.CreateIntervention(ctx, domain.Intervention{ID: interventionID, Type: domain.InterventionQuery}))
	mustCreate(t, repo.CreateOutcome(ctx, domain.Outcome{ID: outcomeID, VerificationStatus: domain.VerificationVerified}))
	mustCreate(t, repo.CreatePreConditionFor(ctx, stateID, interventionID))
	mustCreate(t, repo.CreateProduced(ctx, interventionID, outcomeID, domain.ProducedEdge{EffectSize: -12, Confidence: 1.0}))

	mhID := testutil.NewID(t)
	definition := "reducing the alert threshold restores latency without degrading recall"
	mustCreate(t, repo.CreateMetaHeuristic(ctx, domain.MetaHeuristic{ID: mhID, Definition: definition}, []string{interventionID}))

	// Embed the stored definition (document side) and clear the pending flag.
	docVec, err := provider.EmbedDocument(ctx, definition)
	if err != nil {
		t.Fatalf("embed document: %v", err)
	}
	if err := embeddings.Upsert(ctx, mhID, docVec); err != nil {
		t.Fatalf("upsert embedding: %v", err)
	}
	if err := repo.ClearEmbeddingPending(ctx, mhID); err != nil {
		t.Fatalf("clear embedding pending: %v", err)
	}

	svc := heuristics.NewService(provider, embeddings, repo)

	matches, err := svc.Query(ctx, "latency climbing above the threshold", 1)
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	if len(matches) != 1 || matches[0].MetaHeuristic.ID != mhID {
		t.Fatalf("expected seeded meta-heuristic, got %+v", matches)
	}

	triplets, err := svc.Trace(ctx, mhID)
	if err != nil {
		t.Fatalf("trace: %v", err)
	}
	if len(triplets) != 1 || triplets[0].Intervention.ID != interventionID {
		t.Fatalf("expected one triplet for the intervention, got %+v", triplets)
	}
}

func mustCreate(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatalf("seed: %v", err)
	}
}
