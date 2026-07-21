// Command sandbox is the Sandbox Execution service. It wires its connections
// (object store) and serves the introspection/execution HTTP API until shutdown.
// It is stateless and holds no graph or Postgres connection: the Orchestrator
// owns triplet writes, so the sandbox only needs the object store the data
// sources live in.
package main

import (
	"context"
	"log"

	"github.com/arborette/arborette/internal/config"
	"github.com/arborette/arborette/internal/objectstore"
	"github.com/arborette/arborette/internal/sandbox"
	"github.com/arborette/arborette/internal/service"
)

func main() {
	ctx := context.Background()
	cfg, err := config.Load()
	if err != nil {
		log.Fatalf("sandbox: load config: %v", err)
	}

	objects, err := objectstore.NewClient(ctx, cfg.S3.Endpoint, cfg.S3.Region, cfg.S3.Bucket, cfg.S3.AccessKey, cfg.S3.SecretKey, cfg.S3.PathStyle)
	if err != nil {
		log.Fatalf("sandbox: connect object store: %v", err)
	}
	if err := objects.EnsureBucket(ctx); err != nil {
		log.Fatalf("sandbox: ensure bucket: %v", err)
	}

	srv := sandbox.NewServer(objects, cfg.Sandbox.MaxObjectBytes, cfg.Sandbox.MaxTempDirSize)
	log.Printf("sandbox: wired object store, serving HTTP on :%s", cfg.Sandbox.Port)
	if err := service.RunHTTPServer("sandbox", ":"+cfg.Sandbox.Port, srv.Routes()); err != nil {
		log.Fatalf("sandbox: http server: %v", err)
	}
}
