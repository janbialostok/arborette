package mcpserver

import (
	"context"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/arborette/arborette/internal/domain"
	"github.com/arborette/arborette/internal/embedding"
	"github.com/arborette/arborette/internal/graph"
	"github.com/arborette/arborette/internal/heuristics"
	"github.com/arborette/arborette/internal/store"
	"github.com/arborette/arborette/internal/testutil"
)

// TestToolsIntegration exercises the two read tools end-to-end against live
// Neo4j + pgvector via an in-memory MCP client. It is runtime-skip gated (no
// build tag) so `go test -run Integration` selects it.
func TestToolsIntegration(t *testing.T) {
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
	embeddings := store.NewEmbeddingStore(p, cfg.Embedding.DistanceFloor)
	provider := embedding.NewOllamaProvider(cfg.Ollama.URL, cfg.Ollama.Model, cfg.Embedding.Dimension)

	stateID, interventionID, outcomeID := testutil.NewID(t), testutil.NewID(t), testutil.NewID(t)
	mustSeed(t, repo.CreateState(ctx, domain.State{ID: stateID, Properties: map[string]any{"latency_ms": 210.0}}))
	mustSeed(t, repo.CreateIntervention(ctx, domain.Intervention{ID: interventionID, Type: domain.InterventionQuery}))
	mustSeed(t, repo.CreateOutcome(ctx, domain.Outcome{ID: outcomeID, VerificationStatus: domain.VerificationVerified}))
	mustSeed(t, repo.CreatePreConditionFor(ctx, stateID, interventionID))
	mustSeed(t, repo.CreateProduced(ctx, interventionID, outcomeID, domain.ProducedEdge{EffectSize: -12, Confidence: 1.0}))

	mhID := testutil.NewID(t)
	definition := "reducing the alert threshold restores latency without degrading recall"
	mustSeed(t, repo.CreateMetaHeuristic(ctx, domain.MetaHeuristic{ID: mhID, Definition: definition}, []string{interventionID}))

	docVec, err := provider.EmbedDocument(ctx, definition)
	if err != nil {
		t.Fatalf("embed document: %v", err)
	}
	if err := embeddings.Upsert(ctx, mhID, "", docVec); err != nil {
		t.Fatalf("upsert embedding: %v", err)
	}
	if err := repo.ClearEmbeddingPending(ctx, mhID); err != nil {
		t.Fatalf("clear embedding pending: %v", err)
	}

	queries := heuristics.NewService(provider, embeddings, repo)
	cs := connectTools(t, queries, &fakeSubmitter{})

	searchRes, err := cs.CallTool(ctx, &mcp.CallToolParams{
		Name:      "get_optimized_heuristics",
		Arguments: map[string]any{"operational_state": "latency climbing above the threshold", "k": 1},
	})
	if err != nil {
		t.Fatalf("get_optimized_heuristics: %v", err)
	}
	if searchRes.IsError {
		t.Fatalf("get_optimized_heuristics error: %+v", searchRes.Content)
	}
	var search getOptimizedHeuristicsOutput
	decodeOutput(t, searchRes, &search)
	if len(search.Heuristics) != 1 || search.Heuristics[0].ID != mhID {
		t.Fatalf("expected the seeded meta-heuristic, got %+v", search.Heuristics)
	}

	traceRes, err := cs.CallTool(ctx, &mcp.CallToolParams{
		Name:      "trace_causal_chain",
		Arguments: map[string]any{"meta_heuristic_id": mhID},
	})
	if err != nil {
		t.Fatalf("trace_causal_chain: %v", err)
	}
	if traceRes.IsError {
		t.Fatalf("trace_causal_chain error: %+v", traceRes.Content)
	}
	var trace traceCausalChainOutput
	decodeOutput(t, traceRes, &trace)
	if len(trace.Triplets) != 1 || trace.Triplets[0].Intervention.ID != interventionID {
		t.Fatalf("expected one triplet for the intervention, got %+v", trace.Triplets)
	}
}

func mustSeed(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatalf("seed: %v", err)
	}
}
