// Command sleepcycle is the Sleep-Cycle Worker. It runs in one of two modes. The
// default one-shot mode runs a single Sleep Cycle for the goal named on the
// command line and exits, so a job scheduler can mark it complete: this is the
// AWS Batch seam, the production path. The -serve mode instead wires the worker
// once and runs one cycle per POST /runs, the local/long-running alternative the
// Orchestrator's HTTPLauncher drives. Both authenticate to Postgres as the service
// runtime role (embedding upsert/read, goal-registry read) and reach the audit
// table only through the Orchestrator's internal API, which holds the sole
// credentials.
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"os"

	"github.com/arborette/arborette/internal/config"
	"github.com/arborette/arborette/internal/embedding"
	"github.com/arborette/arborette/internal/graph"
	"github.com/arborette/arborette/internal/llm"
	"github.com/arborette/arborette/internal/orchestratorclient"
	"github.com/arborette/arborette/internal/sandboxclient"
	"github.com/arborette/arborette/internal/service"
	"github.com/arborette/arborette/internal/sleepcycle"
	"github.com/arborette/arborette/internal/store"
)

func main() {
	ctx := context.Background()
	serve := flag.Bool("serve", false,
		"run as a long-running service (one cycle per POST /runs) instead of the one-shot Batch job")
	goalID := flag.String("goal", os.Getenv("SLEEPCYCLE_GOAL_ID"),
		"optimization_function_id to run the Sleep Cycle for (defaults to SLEEPCYCLE_GOAL_ID)")
	flag.Parse()

	// The modes are exclusive. Detect an explicitly-passed -goal via flag.Visit, not
	// the resolved value: -goal defaults to SLEEPCYCLE_GOAL_ID and the serve
	// container inherits env_file, so a value check would fatal serve boot whenever
	// that env var happens to be set. Serve mode ignores the SLEEPCYCLE_GOAL_ID
	// fallback entirely.
	goalPassed := false
	flag.Visit(func(f *flag.Flag) {
		if f.Name == "goal" {
			goalPassed = true
		}
	})
	if *serve && goalPassed {
		log.Fatalf("sleepcycle: -serve and -goal are mutually exclusive")
	}
	if !*serve && *goalID == "" {
		log.Fatalf("sleepcycle: a goal is required: pass -goal <optimization_function_id> or set SLEEPCYCLE_GOAL_ID")
	}

	cfg, err := config.Load()
	if err != nil {
		log.Fatalf("sleepcycle: load config: %v", err)
	}

	worker, cleanup, err := wireWorker(ctx, cfg)
	if err != nil {
		log.Fatalf("sleepcycle: %v", err)
	}
	defer cleanup()

	if *serve {
		// A launch body is one UUID, so the 1 MiB cap is generous with no config knob.
		srv := sleepcycle.NewServer(worker, 1<<20)
		log.Printf("sleepcycle: serving HTTP on :%s", cfg.SleepCycle.Port)
		if err := service.RunHTTPServer("sleepcycle", ":"+cfg.SleepCycle.Port,
			service.BearerAuth(cfg.SleepCycle.InternalAuthToken, srv.Routes())); err != nil {
			log.Fatalf("sleepcycle: http server: %v", err)
		}
		return
	}

	log.Printf("sleepcycle: running goal %q", *goalID)
	if err := worker.Run(ctx, *goalID); err != nil {
		log.Fatalf("sleepcycle: run: %v", err)
	}
	log.Printf("sleepcycle: goal %q complete", *goalID)
}

// wireWorker builds the worker and its collaborators, returning a cleanup that
// closes the graph and Postgres connections. The cleanup is returned rather than
// deferred inside so serve mode's connections live for the whole serve loop: a
// deferred close here would fire at this function's return, handing the server a
// worker with dead connections.
func wireWorker(ctx context.Context, cfg config.Config) (*sleepcycle.Worker, func(), error) {
	repo, err := graph.NewNeo4jRepository(ctx, cfg.Neo4j.URI, cfg.Neo4j.User, cfg.Neo4j.Password)
	if err != nil {
		return nil, nil, fmt.Errorf("connect neo4j: %w", err)
	}
	if err := repo.InitSchema(ctx); err != nil {
		repo.Close(ctx)
		return nil, nil, fmt.Errorf("init schema: %w", err)
	}

	pool, err := store.NewPool(ctx, cfg.Postgres.ServiceDSN())
	if err != nil {
		repo.Close(ctx)
		return nil, nil, fmt.Errorf("connect postgres: %w", err)
	}
	if err := store.ValidateEmbeddingDimension(ctx, pool, cfg.Embedding.Dimension); err != nil {
		pool.Close()
		repo.Close(ctx)
		return nil, nil, err
	}
	if err := store.ValidateDistanceFloor(cfg.Embedding.DistanceFloor); err != nil {
		pool.Close()
		repo.Close(ctx)
		return nil, nil, err
	}
	if err := store.ValidateVectorExtensionVersion(ctx, pool); err != nil {
		pool.Close()
		repo.Close(ctx)
		return nil, nil, err
	}
	cleanup := func() {
		pool.Close()
		repo.Close(ctx)
	}

	provider := embedding.NewOllamaProvider(cfg.Ollama.URL, cfg.Ollama.Model, cfg.Embedding.Dimension)
	claude, err := llm.NewClient(cfg.LLM)
	if err != nil {
		cleanup()
		return nil, nil, fmt.Errorf("llm client: %w", err)
	}
	worker, err := sleepcycle.NewWorker(
		repo,
		sandboxclient.NewClient(cfg.SleepCycle.SandboxURL, cfg.SleepCycle.InternalAuthToken, nil),
		claude,
		provider,
		store.NewEmbeddingStore(pool, cfg.Embedding.DistanceFloor),
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

			Policy:                cfg.SleepCycle.Policy,
			SchemaAtoms:           cfg.SleepCycle.SchemaAtoms,
			UCTExploration:        cfg.SleepCycle.UCTExploration,
			CausalMultiplierScale: cfg.SleepCycle.CausalMultiplierScale,
			GroundingFraction:     cfg.SleepCycle.GroundingFraction,
			RetrievalK:            cfg.SleepCycle.RetrievalK,
			QuantileBins:          cfg.SleepCycle.QuantileBins,
			CrossGoalGrounding:    cfg.SleepCycle.CrossGoalGrounding,
		},
	)
	if err != nil {
		cleanup()
		return nil, nil, fmt.Errorf("config: %w", err)
	}

	log.Printf("sleepcycle: wired neo4j, postgres, sandbox, embeddings (dim=%d)", provider.Dimensions())
	return worker, cleanup, nil
}
