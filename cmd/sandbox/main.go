// Command sandbox is the Sandbox Execution service. It wires its connections
// (graph, object store) and blocks until shutdown.
package main

import (
	"context"
	"log"

	"github.com/arborette/arborette/internal/config"
	"github.com/arborette/arborette/internal/graph"
	"github.com/arborette/arborette/internal/objectstore"
	"github.com/arborette/arborette/internal/service"
)

func main() {
	ctx := context.Background()
	cfg, err := config.Load()
	if err != nil {
		log.Fatalf("sandbox: load config: %v", err)
	}

	repo, err := graph.NewNeo4jRepository(ctx, cfg.Neo4j.URI, cfg.Neo4j.User, cfg.Neo4j.Password)
	if err != nil {
		log.Fatalf("sandbox: connect neo4j: %v", err)
	}
	defer repo.Close(ctx)
	if err := repo.InitSchema(ctx); err != nil {
		log.Fatalf("sandbox: init schema: %v", err)
	}

	objects, err := objectstore.NewClient(ctx, cfg.S3.Endpoint, cfg.S3.Region, cfg.S3.Bucket, cfg.S3.AccessKey, cfg.S3.SecretKey, cfg.S3.PathStyle)
	if err != nil {
		log.Fatalf("sandbox: connect object store: %v", err)
	}
	if err := objects.EnsureBucket(ctx); err != nil {
		log.Fatalf("sandbox: ensure bucket: %v", err)
	}

	log.Printf("sandbox: wired neo4j, object store")
	service.WaitForShutdown("sandbox")
}
