// Command mcpserver is the MCP Server. It wires its connections and blocks until
// shutdown, authenticating to Postgres as the service runtime role (embedding
// read).
package main

import (
	"context"
	"log"
	"net/http"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/arborette/arborette/internal/config"
	"github.com/arborette/arborette/internal/embedding"
	"github.com/arborette/arborette/internal/graph"
	"github.com/arborette/arborette/internal/heuristics"
	"github.com/arborette/arborette/internal/mcpserver"
	"github.com/arborette/arborette/internal/service"
	"github.com/arborette/arborette/internal/store"
)

func main() {
	ctx := context.Background()
	cfg, err := config.Load()
	if err != nil {
		log.Fatalf("mcpserver: load config: %v", err)
	}

	repo, err := graph.NewNeo4jRepository(ctx, cfg.Neo4j.URI, cfg.Neo4j.User, cfg.Neo4j.Password)
	if err != nil {
		log.Fatalf("mcpserver: connect neo4j: %v", err)
	}
	defer repo.Close(ctx)
	if err := repo.InitSchema(ctx); err != nil {
		log.Fatalf("mcpserver: init schema: %v", err)
	}

	pool, err := store.NewPool(ctx, cfg.Postgres.ServiceDSN())
	if err != nil {
		log.Fatalf("mcpserver: connect postgres: %v", err)
	}
	defer pool.Close()
	if err := store.ValidateEmbeddingDimension(ctx, pool, cfg.Embedding.Dimension); err != nil {
		log.Fatalf("mcpserver: %v", err)
	}

	provider := embedding.NewOllamaProvider(cfg.Ollama.URL, cfg.Ollama.Model, cfg.Embedding.Dimension)
	queries := heuristics.NewService(provider, store.NewEmbeddingStore(pool), repo)
	log.Printf("mcpserver: wired neo4j, postgres, heuristics query service (dim=%d), serving HTTP on :%s", provider.Dimensions(), cfg.MCP.Port)

	orchClient := mcpserver.NewOrchestratorClient(cfg.MCP.OrchestratorURL, nil)
	srv := mcp.NewServer(&mcp.Implementation{Name: "arborette-mcp", Version: "0.1.0"}, nil)
	mcpserver.RegisterTools(srv, queries, orchClient)

	// A single http.Handler value: arborette's downstream Agent Chat Backend
	// wraps this with bearer-token middleware to expose it publicly, without
	// touching the tool handlers or transport.
	handler := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return srv }, nil)

	if err := service.RunHTTPServer("mcpserver", ":"+cfg.MCP.Port, handler); err != nil {
		log.Fatalf("mcpserver: http server: %v", err)
	}
}
