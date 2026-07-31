// Command sleepcycle is the Sleep-Cycle Worker. It is a one-shot batch job, not
// a service: it runs one Sleep Cycle for the goal named on the command line and
// exits, so the job scheduler can mark it complete. It authenticates to Postgres as the service runtime role
// (embedding upsert/read, goal-registry read) and reaches the audit table only
// through the Orchestrator's internal API, which holds the sole credentials.
package main

import (
	"context"
	"flag"
	"log"
	"os"

	"github.com/arborette/arborette/internal/config"
	"github.com/arborette/arborette/internal/embedding"
	"github.com/arborette/arborette/internal/graph"
	"github.com/arborette/arborette/internal/llm"
	"github.com/arborette/arborette/internal/orchestratorclient"
	"github.com/arborette/arborette/internal/sandboxclient"
	"github.com/arborette/arborette/internal/sleepcycle"
	"github.com/arborette/arborette/internal/store"
)

func main() {
	ctx := context.Background()
	goalID := flag.String("goal", os.Getenv("SLEEPCYCLE_GOAL_ID"),
		"optimization_function_id to run the Sleep Cycle for (defaults to SLEEPCYCLE_GOAL_ID)")
	flag.Parse()
	if *goalID == "" {
		log.Fatalf("sleepcycle: a goal is required: pass -goal <optimization_function_id> or set SLEEPCYCLE_GOAL_ID")
	}

	cfg, err := config.Load()
	if err != nil {
		log.Fatalf("sleepcycle: load config: %v", err)
	}

	repo, err := graph.NewNeo4jRepository(ctx, cfg.Neo4j.URI, cfg.Neo4j.User, cfg.Neo4j.Password)
	if err != nil {
		log.Fatalf("sleepcycle: connect neo4j: %v", err)
	}
	defer repo.Close(ctx)
	if err := repo.InitSchema(ctx); err != nil {
		log.Fatalf("sleepcycle: init schema: %v", err)
	}

	pool, err := store.NewPool(ctx, cfg.Postgres.ServiceDSN())
	if err != nil {
		log.Fatalf("sleepcycle: connect postgres: %v", err)
	}
	defer pool.Close()
	if err := store.ValidateEmbeddingDimension(ctx, pool, cfg.Embedding.Dimension); err != nil {
		log.Fatalf("sleepcycle: %v", err)
	}

	provider := embedding.NewOllamaProvider(cfg.Ollama.URL, cfg.Ollama.Model, cfg.Embedding.Dimension)
	claude, err := llm.NewClient(cfg.LLM)
	if err != nil {
		log.Fatalf("sleepcycle: llm client: %v", err)
	}
	worker, err := sleepcycle.NewWorker(
		repo,
		sandboxclient.NewClient(cfg.SleepCycle.SandboxURL, cfg.SleepCycle.InternalAuthToken, nil),
		claude,
		provider,
		store.NewEmbeddingStore(pool),
		store.NewGoalRegistry(pool),
		orchestratorclient.NewClient(cfg.SleepCycle.OrchestratorURL,
			cfg.SleepCycle.InternalAuthToken, nil),
		sleepcycle.Config{
			MaxMeasurements: cfg.SleepCycle.MaxMeasurements,
			BeamWidth:       cfg.SleepCycle.BeamWidth,
			MaxOrder:        cfg.SleepCycle.MaxOrder,
			MinSupport:      cfg.SleepCycle.MinSupport,
			MinLift:         cfg.SleepCycle.MinLift,
			MaxPublications: cfg.SleepCycle.MaxPublications,
		},
	)
	if err != nil {
		log.Fatalf("sleepcycle: config: %v", err)
	}

	log.Printf("sleepcycle: wired neo4j, postgres, sandbox, embeddings (dim=%d); running goal %q",
		provider.Dimensions(), *goalID)
	if err := worker.Run(ctx, *goalID); err != nil {
		log.Fatalf("sleepcycle: run: %v", err)
	}
	log.Printf("sleepcycle: goal %q complete", *goalID)
}
