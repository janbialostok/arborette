// Package testutil provides shared setup for the integration tests. The tests
// require a live compose stack (make up) and are gated on ARBORETTE_INTEGRATION
// so `go test ./...` stays green without infrastructure. Connection details
// come from the same env vars the services use; run tests from the host against
// the published ports (see the Makefile test target).
package testutil

import (
	"context"
	"crypto/rand"
	"fmt"
	"os"
	"testing"

	"github.com/arborette/arborette/internal/config"
	"github.com/arborette/arborette/internal/store"
	"github.com/jackc/pgx/v5"
)

// RequireIntegration skips the test unless ARBORETTE_INTEGRATION is set, then
// returns the env-loaded config.
func RequireIntegration(t *testing.T) config.Config {
	t.Helper()
	if os.Getenv("ARBORETTE_INTEGRATION") == "" {
		t.Skip("integration test: set ARBORETTE_INTEGRATION=1 and run `make up` first")
	}
	cfg, err := config.Load()
	if err != nil {
		t.Fatalf("load config: %v", err)
	}
	return cfg
}

// SetupPostgres provisions the runtime roles then applies migrations, mirroring
// the compose db-bootstrap → migrate init chain. It is the setup that lets a
// bare Postgres survive 0005_grants (which grants against pre-existing roles).
// Idempotent, so multiple test files may call it.
func SetupPostgres(t *testing.T, ctx context.Context, cfg config.Config) {
	t.Helper()
	roles := []store.RoleSpec{
		{Name: cfg.Postgres.OrchestratorUser, Password: cfg.Postgres.OrchestratorPassword},
		{Name: cfg.Postgres.ServiceUser, Password: cfg.Postgres.ServicePassword},
	}
	if err := store.EnsureRoles(ctx, cfg.Postgres.OwnerDSN(), roles); err != nil {
		t.Fatalf("ensure roles: %v", err)
	}
	if err := store.Migrate(ctx, cfg.Postgres.OwnerDSN()); err != nil {
		t.Fatalf("migrate: %v", err)
	}
}

// TruncateEmbeddings empties the embeddings table via the owner DSN so a test
// that asserts on similarity results is not contaminated by rows other tests
// left in the shared database. Run integration tests with -p 1 so packages do
// not race on this shared state (see the Makefile test target).
func TruncateEmbeddings(t *testing.T, ctx context.Context, cfg config.Config) {
	t.Helper()
	conn, err := pgx.Connect(ctx, cfg.Postgres.OwnerDSN())
	if err != nil {
		t.Fatalf("connect as owner: %v", err)
	}
	defer conn.Close(ctx)
	if _, err := conn.Exec(ctx, "TRUNCATE meta_heuristic_embeddings"); err != nil {
		t.Fatalf("truncate embeddings: %v", err)
	}
}

// NewID returns a random v4 UUID string for use as an application-assigned node
// id or goal id, without pulling in a UUID dependency.
func NewID(t *testing.T) string {
	t.Helper()
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		t.Fatalf("generate uuid: %v", err)
	}
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}
