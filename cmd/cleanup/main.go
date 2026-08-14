// Command cleanup wipes every test-created dataset, objective, and derived
// record/artifact from the shared dev stores, and verifies they are clean. It
// is the one-shot seam behind `make clean-data` and the post-suite gate behind
// `make test`, mirroring the cmd/migrate precedent: load config, run to
// completion, exit. Usage:
//
//	cleanup clean    # wipe every dataset, objective, and derived record/artifact
//	cleanup check    # verify the stores are clean; exit non-zero if not
//
// Both modes connect as the owner Postgres role (the only role with DELETE on
// every table, including audit_log) plus the configured Neo4j and object-store
// clients. A connection failure to any store is fatal and names the store, so a
// half-wiped state never reads as "cleaned".
package main

import (
	"context"
	"fmt"
	"log"
	"os"
	"sort"

	"github.com/arborette/arborette/internal/config"
	"github.com/arborette/arborette/internal/graph"
	"github.com/arborette/arborette/internal/objectstore"
	"github.com/arborette/arborette/internal/store"
)

func main() {
	ctx := context.Background()
	cfg, err := config.Load()
	if err != nil {
		log.Fatalf("cleanup: load config: %v", err)
	}

	mode := ""
	if len(os.Args) > 1 {
		mode = os.Args[1]
	}

	switch mode {
	case "clean":
		if err := clean(ctx, cfg); err != nil {
			log.Fatalf("cleanup: clean: %v", err)
		}
	case "check":
		if err := check(ctx, cfg); err != nil {
			log.Fatalf("cleanup: check: %v", err)
		}
	case "":
		fmt.Fprintln(os.Stderr, "usage: cleanup clean|check")
		os.Exit(2)
	default:
		fmt.Fprintf(os.Stderr, "cleanup: unknown mode %q (want clean or check)\n", mode)
		os.Exit(2)
	}
}

// clean wipes all three stores and logs the per-store summary.
func clean(ctx context.Context, cfg config.Config) error {
	pool, err := store.NewPool(ctx, cfg.Postgres.OwnerDSN())
	if err != nil {
		return fmt.Errorf("postgres: %w", err)
	}
	defer pool.Close()

	removed, err := store.ResetAll(ctx, pool)
	if err != nil {
		return fmt.Errorf("postgres: %w", err)
	}

	repo, err := graph.NewNeo4jRepository(ctx, cfg.Neo4j.URI, cfg.Neo4j.User, cfg.Neo4j.Password)
	if err != nil {
		return fmt.Errorf("neo4j: %w", err)
	}
	defer repo.Close(ctx)

	nodesRemoved, err := repo.Wipe(ctx)
	if err != nil {
		return fmt.Errorf("neo4j: %w", err)
	}

	objects, err := objectstore.NewClient(ctx, cfg.S3.Endpoint, cfg.S3.Region, cfg.S3.Bucket,
		cfg.S3.AccessKey, cfg.S3.SecretKey, cfg.S3.PathStyle)
	if err != nil {
		return fmt.Errorf("object store: %w", err)
	}

	keysRemoved, err := objects.Wipe(ctx)
	if err != nil {
		return fmt.Errorf("object store: %w", err)
	}

	for _, table := range store.ResetTables {
		log.Printf("cleanup: %s: %d rows removed", table, removed[table])
	}
	log.Printf("cleanup: neo4j: %d nodes removed", nodesRemoved)
	log.Printf("cleanup: object store: %d keys removed", keysRemoved)
	return nil
}

// check verifies every store is empty, that the environment the test suite
// depends on stayed clean. Any non-zero count is an offender and fails the
// gate. It returns an error naming every offender so the failure reads as the
// list of tables (plus graph/object counts) that still hold residue.
func check(ctx context.Context, cfg config.Config) error {
	pool, err := store.NewPool(ctx, cfg.Postgres.OwnerDSN())
	if err != nil {
		return fmt.Errorf("postgres: %w", err)
	}
	defer pool.Close()

	counts, err := store.EmptyCounts(ctx, pool)
	if err != nil {
		return fmt.Errorf("postgres: %w", err)
	}

	repo, err := graph.NewNeo4jRepository(ctx, cfg.Neo4j.URI, cfg.Neo4j.User, cfg.Neo4j.Password)
	if err != nil {
		return fmt.Errorf("neo4j: %w", err)
	}
	defer repo.Close(ctx)

	nodes, err := repo.CountNodes(ctx)
	if err != nil {
		return fmt.Errorf("neo4j: %w", err)
	}

	objects, err := objectstore.NewClient(ctx, cfg.S3.Endpoint, cfg.S3.Region, cfg.S3.Bucket,
		cfg.S3.AccessKey, cfg.S3.SecretKey, cfg.S3.PathStyle)
	if err != nil {
		return fmt.Errorf("object store: %w", err)
	}

	keys, err := objects.ListKeys(ctx)
	if err != nil {
		return fmt.Errorf("object store: %w", err)
	}

	var offenders []string
	for _, table := range store.ResetTables {
		if counts[table] != 0 {
			offenders = append(offenders, fmt.Sprintf("%s: %d rows", table, counts[table]))
		}
	}
	if nodes != 0 {
		offenders = append(offenders, fmt.Sprintf("neo4j: %d nodes", nodes))
	}
	if len(keys) != 0 {
		offenders = append(offenders, fmt.Sprintf("object store: %d keys", len(keys)))
	}

	if len(offenders) == 0 {
		log.Printf("cleanup: check: clean (all tables, graph, and object store empty)")
		return nil
	}

	sort.Strings(offenders)
	msg := "cleanup: check: residue found: "
	for _, offender := range offenders {
		msg += offender + "; "
	}
	return fmt.Errorf("%s", msg)
}