package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/arborette/arborette/internal/store/migrations"
	"github.com/golang-migrate/migrate/v4"
	pgxmigrate "github.com/golang-migrate/migrate/v4/database/pgx/v5"
	"github.com/golang-migrate/migrate/v4/source/iofs"
	_ "github.com/jackc/pgx/v5/stdlib"
)

// Migrate applies all up migrations using the owner DSN. It must run with the
// table owner: 0005_grants grants on tables this connection owns. Runtime pools
// must be built only after this has applied (the vector extension and tables
// must exist first). Idempotent -- a no-op when already current.
func Migrate(ctx context.Context, ownerDSN string) error {
	return runMigrate(ownerDSN, migrateUp)
}

// MigrateDown rolls every migration back using the owner DSN. Roles themselves
// are dropped by db-bootstrap/IaC teardown, never here.
func MigrateDown(ctx context.Context, ownerDSN string) error {
	return runMigrate(ownerDSN, migrateDown)
}

type migrateDirection int

const (
	migrateUp migrateDirection = iota
	migrateDown
)

func runMigrate(ownerDSN string, dir migrateDirection) error {
	src, err := iofs.New(migrations.FS, ".")
	if err != nil {
		return fmt.Errorf("open migration source: %w", err)
	}
	defer src.Close()

	db, err := sql.Open("pgx", ownerDSN)
	if err != nil {
		return fmt.Errorf("open owner db: %w", err)
	}
	defer db.Close()

	driver, err := pgxmigrate.WithInstance(db, &pgxmigrate.Config{})
	if err != nil {
		return fmt.Errorf("build migrate driver: %w", err)
	}

	m, err := migrate.NewWithInstance("iofs", src, "pgx5", driver)
	if err != nil {
		return fmt.Errorf("build migrator: %w", err)
	}

	switch dir {
	case migrateUp:
		err = m.Up()
	case migrateDown:
		err = m.Down()
	}
	if err != nil && !errors.Is(err, migrate.ErrNoChange) {
		return fmt.Errorf("apply migrations: %w", err)
	}
	return nil
}
