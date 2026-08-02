// Command sandbox is the Sandbox Execution service. It wires its connections
// (object store, plus a read-only Postgres pool for the data-source ref registry it
// validates against) and serves the introspection/execution HTTP API until
// shutdown. It authenticates to Postgres as the service runtime role and holds no
// graph connection: the Orchestrator owns triplet writes, so the sandbox needs only
// the object store the data sources live in and the registry it scopes requests to.
package main

import (
	"context"
	"log"
	"os"

	"github.com/arborette/arborette/internal/config"
	"github.com/arborette/arborette/internal/objectstore"
	"github.com/arborette/arborette/internal/sandbox"
	"github.com/arborette/arborette/internal/service"
	"github.com/arborette/arborette/internal/store"
)

// refRegistry adapts the goal registry's ref-existence check into the sandbox's
// narrow ref validator, so internal/sandbox need not import internal/store.
type refRegistry struct {
	goals *store.GoalRegistry
}

func (r refRegistry) Validate(ctx context.Context, ref string) (bool, error) {
	return r.goals.DataSourceRefExists(ctx, ref)
}

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

	// The sandbox hard-depends on Postgres for the ref registry it validates
	// against; a connect failure is fatal, matching every other service main.
	pool, err := store.NewPool(ctx, cfg.Postgres.ServiceDSN())
	if err != nil {
		log.Fatalf("sandbox: connect postgres: %v", err)
	}
	defer pool.Close()
	validator := refRegistry{goals: store.NewGoalRegistry(pool)}

	cache, err := buildStageCache(cfg.Sandbox)
	if err != nil {
		log.Fatalf("sandbox: init stage cache: %v", err)
	}

	limiter := sandbox.NewClassLimiter(map[string]int{sandbox.ClassDefault: cfg.Sandbox.ExecuteConcurrency})
	srv := sandbox.NewServer(objects, validator, cache, limiter,
		cfg.Sandbox.MaxObjectBytes, cfg.Sandbox.MaxBodyBytes, cfg.Sandbox.MaxTempDirSize, cfg.Sandbox.DistinctValueCap)

	log.Printf("sandbox: wired object store, postgres, stage cache; serving HTTP on :%s", cfg.Sandbox.Port)
	if err := service.RunHTTPServer("sandbox", ":"+cfg.Sandbox.Port,
		service.BearerAuth(cfg.Sandbox.InternalAuthToken, srv.Routes())); err != nil {
		log.Fatalf("sandbox: http server: %v", err)
	}
}

// buildStageCache resolves the boot-scoped staging cache. A zero budget disables it
// (nil cache ⇒ per-request staging). A configured directory is emptied at boot (not
// merely created) so no prior-boot file sits on disk uncounted by the byte
// accounting; an empty setting gets a fresh temp dir.
func buildStageCache(cfg config.SandboxConfig) (*sandbox.StageCache, error) {
	if cfg.StageCacheMaxBytes <= 0 {
		return nil, nil
	}
	dir := cfg.StageCacheDir
	if dir == "" {
		var err error
		if dir, err = os.MkdirTemp("", "arborette-stage-cache-"); err != nil {
			return nil, err
		}
	} else if err := os.RemoveAll(dir); err != nil {
		return nil, err
	}
	return sandbox.NewStageCache(dir, cfg.StageCacheMaxBytes)
}
