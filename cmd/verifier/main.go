// Command verifier is Engine B's Verifier service. It runs in one of two modes,
// mirroring the Sleep-Cycle Worker. The default one-shot mode runs a single causal
// discovery for the (goal, data-source) named on the command line and exits (the AWS
// Batch seam). The -serve mode wires the worker once and runs one discovery per POST
// /verifications, the local/long-running alternative the Orchestrator's HTTPLauncher
// drives. The binary is CGO-free: it consumes internal/sandboxclient, never
// internal/sandbox.
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"os"

	"github.com/arborette/arborette/internal/config"
	"github.com/arborette/arborette/internal/graph"
	"github.com/arborette/arborette/internal/llm"
	"github.com/arborette/arborette/internal/orchestratorclient"
	"github.com/arborette/arborette/internal/sandboxclient"
	"github.com/arborette/arborette/internal/service"
	"github.com/arborette/arborette/internal/store"
	"github.com/arborette/arborette/internal/verifier"
)

func main() {
	ctx := context.Background()
	serve := flag.Bool("serve", false,
		"run as a long-running service (one discovery per POST /verifications) instead of the one-shot Batch job")
	goalID := flag.String("goal", os.Getenv("VERIFIER_GOAL_ID"),
		"optimization_function_id to run discovery for (defaults to VERIFIER_GOAL_ID)")
	datasourceRef := flag.String("datasource", os.Getenv("VERIFIER_DATASOURCE_REF"),
		"data source ref to discover the graph over (defaults to VERIFIER_DATASOURCE_REF)")
	flag.Parse()

	// The modes are exclusive. Detect explicitly-passed one-shot flags via flag.Visit,
	// not the resolved values: -goal/-datasource default to their env vars and the
	// serve container inherits env_file, so a value check would fatal serve boot
	// whenever those happen to be set. Serve mode ignores the one-shot flags entirely.
	oneShotPassed := false
	flag.Visit(func(f *flag.Flag) {
		if f.Name == "goal" || f.Name == "datasource" {
			oneShotPassed = true
		}
	})
	if *serve && oneShotPassed {
		log.Fatalf("verifier: -serve and -goal/-datasource are mutually exclusive")
	}
	if !*serve && (*goalID == "" || *datasourceRef == "") {
		log.Fatalf("verifier: one-shot mode requires -goal and -datasource (or set VERIFIER_GOAL_ID / VERIFIER_DATASOURCE_REF)")
	}

	cfg, err := config.Load()
	if err != nil {
		log.Fatalf("verifier: load config: %v", err)
	}

	worker, cleanup, err := wireVerifier(ctx, cfg)
	if err != nil {
		log.Fatalf("verifier: %v", err)
	}
	defer cleanup()

	if *serve {
		// A dispatch body is small (two ids plus a kind), so the 1 MiB cap is generous.
		srv := verifier.NewServer(worker, 1<<20)
		log.Printf("verifier: serving HTTP on :%s", cfg.Verifier.Port)
		if err := service.RunHTTPServer("verifier", ":"+cfg.Verifier.Port,
			service.BearerAuth(cfg.Verifier.InternalAuthToken, srv.Routes())); err != nil {
			log.Fatalf("verifier: http server: %v", err)
		}
		return
	}

	log.Printf("verifier: running discovery for goal %q over %q", *goalID, *datasourceRef)
	if err := worker.RunDiscovery(ctx, *goalID, *datasourceRef); err != nil {
		log.Fatalf("verifier: discovery: %v", err)
	}
	log.Printf("verifier: discovery for goal %q complete", *goalID)
}

// wireVerifier builds the worker and its collaborators, returning a cleanup that
// closes the graph and Postgres connections. The cleanup is returned rather than
// deferred inside so serve mode's connections live for the whole serve loop.
func wireVerifier(ctx context.Context, cfg config.Config) (*verifier.Worker, func(), error) {
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
	cleanup := func() {
		pool.Close()
		repo.Close(ctx)
	}

	claude, err := llm.NewClient(cfg.LLM)
	if err != nil {
		cleanup()
		return nil, nil, fmt.Errorf("llm client: %w", err)
	}

	worker := verifier.NewWorker(
		sandboxclient.NewClient(cfg.Verifier.SandboxURL, cfg.Verifier.InternalAuthToken, nil),
		repo,
		claude,
		store.NewAdvisoryLock(pool),
		store.NewGoalRegistry(pool),
		orchestratorclient.NewClient(cfg.Verifier.OrchestratorURL, cfg.Verifier.InternalAuthToken, nil),
		verifier.Config{
			Alpha:       cfg.Verifier.DiscoveryAlpha,
			FDR:         cfg.Verifier.DiscoveryFDR,
			MaxCondSet:  cfg.Verifier.DiscoveryMaxCondSet,
			Bins:        cfg.Verifier.DiscoveryBins,
			ColumnCap:   cfg.Verifier.DiscoveryColumnCap,
			MaxTests:    cfg.Verifier.DiscoveryMaxTests,
			CallTimeout: cfg.Verifier.DiscoveryCallTimeout,
		},
		cfg.Verifier.OrientMaxRepairs,
	)

	log.Printf("verifier: wired neo4j, postgres, sandbox, llm")
	return worker, cleanup, nil
}
