// Command orchestrator is the REST/API service. It wires its connections and
// serves the goal-registry, hypothesis-loop, streaming, and heuristic-browsing
// HTTP API until shutdown, authenticating to Postgres as the orchestrator
// runtime role.
package main

import (
	"context"
	"log"

	"github.com/arborette/arborette/internal/config"
	"github.com/arborette/arborette/internal/embedding"
	"github.com/arborette/arborette/internal/graph"
	"github.com/arborette/arborette/internal/heuristics"
	"github.com/arborette/arborette/internal/llm"
	"github.com/arborette/arborette/internal/objectstore"
	"github.com/arborette/arborette/internal/orchestrator"
	"github.com/arborette/arborette/internal/sandboxclient"
	"github.com/arborette/arborette/internal/service"
	"github.com/arborette/arborette/internal/store"
)

func main() {
	ctx := context.Background()
	cfg, err := config.Load()
	if err != nil {
		log.Fatalf("orchestrator: load config: %v", err)
	}

	repo, err := graph.NewNeo4jRepository(ctx, cfg.Neo4j.URI, cfg.Neo4j.User, cfg.Neo4j.Password)
	if err != nil {
		log.Fatalf("orchestrator: connect neo4j: %v", err)
	}
	defer repo.Close(ctx)
	if err := repo.InitSchema(ctx); err != nil {
		log.Fatalf("orchestrator: init schema: %v", err)
	}

	pool, err := store.NewPool(ctx, cfg.Postgres.OrchestratorDSN())
	if err != nil {
		log.Fatalf("orchestrator: connect postgres: %v", err)
	}
	defer pool.Close()
	if err := store.ValidateEmbeddingDimension(ctx, pool, cfg.Embedding.Dimension); err != nil {
		log.Fatalf("orchestrator: %v", err)
	}
	if err := store.ValidateDistanceFloor(cfg.Embedding.DistanceFloor); err != nil {
		log.Fatalf("orchestrator: %v", err)
	}
	if err := store.ValidateVectorExtensionVersion(ctx, pool); err != nil {
		log.Fatalf("orchestrator: %v", err)
	}

	objects, err := objectstore.NewClient(ctx, cfg.S3.Endpoint, cfg.S3.Region, cfg.S3.Bucket, cfg.S3.AccessKey, cfg.S3.SecretKey, cfg.S3.PathStyle)
	if err != nil {
		log.Fatalf("orchestrator: connect object store: %v", err)
	}
	if err := objects.EnsureBucket(ctx); err != nil {
		log.Fatalf("orchestrator: ensure bucket: %v", err)
	}

	provider := embedding.NewOllamaProvider(cfg.Ollama.URL, cfg.Ollama.Model, cfg.Embedding.Dimension)
	goals := store.NewGoalRegistry(pool)
	runs := store.NewRuns(pool)
	queue := store.NewVerificationQueue(pool)
	audits := store.NewAuditLog(pool)
	datasets := store.NewDatasetStore(pool)
	embeddings := store.NewEmbeddingStore(pool, cfg.Embedding.DistanceFloor)
	heur := heuristics.NewService(provider, embeddings, repo)
	claude, err := llm.NewClient(cfg.LLM)
	if err != nil {
		log.Fatalf("orchestrator: llm client: %v", err)
	}
	chat := llm.NewChatClient(cfg.LLM.Anthropic.APIKey, cfg.LLM.Anthropic.ChatModel, cfg.MCP.PublicURL, cfg.MCP.AuthorizationToken)
	sandbox := sandboxclient.NewClient(cfg.Orchestrator.SandboxURL, cfg.Orchestrator.InternalAuthToken, nil)

	// Runs whose loop was abandoned by a prior crash or shutdown never ran their
	// terminal write; settle them to failed at boot so they don't strand at
	// running. Non-fatal -- a stale row is a display nuisance, not a reason to
	// refuse to serve. Correct only for a single orchestrator instance.
	if n, err := runs.FailOrphaned(ctx, "orchestrator restarted"); err != nil {
		log.Printf("orchestrator: reconcile orphaned runs: %v", err)
	} else if n > 0 {
		log.Printf("orchestrator: reconciled %d orphaned run(s) to failed", n)
	}

	// Goals that reached the registry without a dataset parent (rows written
	// before the 0015 migration on a partially migrated stack) are bound to the
	// dataset for their ref and audited as dataset_reconcile. Normally dormant:
	// the migration backfills and locks NOT NULL, so this is a safety net, not
	// the primary path. Non-fatal like the run reconciliation.
	orchestrator.ReconcileDatasets(ctx, goals, datasets, audits,
		orchestrator.StubIdentity{ID: cfg.Orchestrator.AnalystID})

	// A configured worker URL selects the HTTP launcher (a worker in serve mode);
	// empty keeps the logging stub, so the AWS Batch seam stays the production path.
	var launcher orchestrator.JobLauncher
	if url := cfg.Orchestrator.SleepCycleWorkerURL; url != "" {
		launcher = orchestrator.NewHTTPLauncher(url, cfg.Orchestrator.InternalAuthToken, nil)
		log.Printf("orchestrator: wired HTTP sleep-cycle launcher -> %s", url)
	} else {
		launcher = orchestrator.StubLauncher{}
		log.Printf("orchestrator: wired stub sleep-cycle launcher (no worker URL configured)")
	}

	// The verifier launcher is selected the same way (its differently-pathed endpoint
	// reuses the HTTPLauncher unchanged), and carries every router dispatch: the
	// explicit verify affordance, auto-promotion, and correction-triggered
	// re-verification.
	var verifierLauncher orchestrator.JobLauncher
	if url := cfg.Orchestrator.VerifierWorkerURL; url != "" {
		verifierLauncher = orchestrator.NewHTTPLauncher(url, cfg.Orchestrator.InternalAuthToken, nil)
		log.Printf("orchestrator: wired HTTP verifier launcher -> %s", url)
	} else {
		verifierLauncher = orchestrator.StubLauncher{}
		log.Printf("orchestrator: wired stub verifier launcher (no verifier URL configured)")
	}

	// The verification records are read here and written by the Verifier, so the
	// accounting knobs it enforces are its own; this store only lists and invalidates.
	causalVerifications := store.NewCausalVerifications(pool, cfg.Verifier.LeaseTTL,
		cfg.Verifier.InflightCap, cfg.Verifier.VerificationBudget)

	srv := orchestrator.NewServer(
		repo, goals, runs, queue, causalVerifications, store.NewAdvisoryLock(pool),
		audits, objects, datasets, embeddings, heur, claude, chat, sandbox,
		orchestrator.NewHub(), launcher, verifierLauncher,
		orchestrator.StubIdentity{ID: cfg.Orchestrator.AnalystID},
		orchestrator.RouterConfig{
			AutoPromoteEnabled:    cfg.Orchestrator.AutoPromoteEnabled,
			AutoPromoteTopN:       cfg.Orchestrator.AutoPromoteTopN,
			AutoPromoteShrinkageK: cfg.Orchestrator.AutoPromoteShrinkageK,
			StaleReverifyCap:      cfg.Orchestrator.StaleReverifyCap,
		},
		cfg.Orchestrator.LocalImportDir, cfg.Orchestrator.SleepCycleJobName,
		cfg.Orchestrator.InternalAuthToken,
		cfg.Orchestrator.HITLConfidenceThreshold, cfg.Orchestrator.BlockingLoopTimeout,
	)

	log.Printf("orchestrator: wired neo4j, postgres, object store, embeddings (dim=%d), serving HTTP on :%s", provider.Dimensions(), cfg.Orchestrator.Port)
	if err := service.RunHTTPServer("orchestrator", ":"+cfg.Orchestrator.Port, srv.Routes()); err != nil {
		log.Fatalf("orchestrator: http server: %v", err)
	}
}
