// Command dbbootstrap idempotently provisions the runtime LOGIN roles before
// migrations run. It is a thin wrapper over store.EnsureRoles (the same path
// integration tests use), not a golang-migrate migration: static migration SQL
// cannot interpolate env passwords, and roles are cluster-global while
// migration history is per-database. It runs to completion and exits. In
// production these roles are provisioned by IaC/secret management instead.
package main

import (
	"context"
	"log"

	"github.com/arborette/arborette/internal/config"
	"github.com/arborette/arborette/internal/store"
)

func main() {
	ctx := context.Background()
	cfg, err := config.Load()
	if err != nil {
		log.Fatalf("dbbootstrap: load config: %v", err)
	}

	roles := []store.RoleSpec{
		{Name: cfg.Postgres.OrchestratorUser, Password: cfg.Postgres.OrchestratorPassword},
		{Name: cfg.Postgres.ServiceUser, Password: cfg.Postgres.ServicePassword},
	}
	if err := store.EnsureRoles(ctx, cfg.Postgres.OwnerDSN(), roles); err != nil {
		log.Fatalf("dbbootstrap: ensure roles: %v", err)
	}
	log.Printf("dbbootstrap: provisioned %d runtime roles", len(roles))
}
