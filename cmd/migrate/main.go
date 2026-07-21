// Command migrate applies (or rolls back) the PostgreSQL migrations using the
// owner DSN. It runs to completion and exits; the compose migrate init service
// and the migrate-up/migrate-down Make targets both invoke it. Usage:
//
//	migrate [up|down]
//
// with "up" the default.
package main

import (
	"context"
	"log"
	"os"

	"github.com/arborette/arborette/internal/config"
	"github.com/arborette/arborette/internal/store"
)

func main() {
	ctx := context.Background()
	cfg, err := config.Load()
	if err != nil {
		log.Fatalf("migrate: load config: %v", err)
	}

	direction := "up"
	if len(os.Args) > 1 {
		direction = os.Args[1]
	}

	ownerDSN := cfg.Postgres.OwnerDSN()
	switch direction {
	case "up":
		if err := store.Migrate(ctx, ownerDSN); err != nil {
			log.Fatalf("migrate: up: %v", err)
		}
		log.Printf("migrate: applied migrations")
	case "down":
		if err := store.MigrateDown(ctx, ownerDSN); err != nil {
			log.Fatalf("migrate: down: %v", err)
		}
		log.Printf("migrate: rolled back migrations")
	default:
		log.Fatalf("migrate: unknown direction %q (want up or down)", direction)
	}
}
