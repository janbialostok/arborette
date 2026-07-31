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
	embeddings := store.NewEmbeddingStore(pool)
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

	srv := orchestrator.NewServer(
		repo, goals, runs, queue, audits, objects, heur, claude, chat, sandbox,
		orchestrator.NewHub(), orchestrator.StubLauncher{},
		orchestrator.StubIdentity{ID: cfg.Orchestrator.AnalystID},
		cfg.Orchestrator.LocalImportDir, cfg.Orchestrator.SleepCycleJobName,
		cfg.Orchestrator.InternalAuthToken,
		cfg.Orchestrator.HITLConfidenceThreshold, cfg.Orchestrator.BlockingLoopTimeout,
	)

	log.Printf("orchestrator: wired neo4j, postgres, object store, embeddings (dim=%d), serving HTTP on :%s", provider.Dimensions(), cfg.Orchestrator.Port)
	if err := service.RunHTTPServer("orchestrator", ":"+cfg.Orchestrator.Port, srv.Routes()); err != nil {
		log.Fatalf("orchestrator: http server: %v", err)
	}
}
