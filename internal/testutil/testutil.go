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
	"github.com/arborette/arborette/internal/graph"
	"github.com/arborette/arborette/internal/objectstore"
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

// The Register*Cleanup helpers below are the test-hygiene harness: every
// integration test that writes a persistent fixture registers the matching
// helper with the fixture's id immediately after creating it, so t.Cleanup
// removes exactly what the test created whether it passes, fails, or panics.
// Each helper is idempotent (a test that already exercised a delete path still
// cleans up quietly) and runs over the owner role, the only role with DELETE on
// every table, so no runtime grant is widened. A cleanup failure is reported
// through t.Errorf and then caught by the post-suite cleanliness gate.
//
// Connection opening is deferred to cleanup time (nothing is held during the
// test), and a fresh non-cancellable context is used so a cancelled test
// context cannot silently skip the teardown that keeps the shared database
// clean.

func cleanupConn(t *testing.T, ctx context.Context, cfg config.Config) (*pgx.Conn, bool) {
	t.Helper()
	conn, err := pgx.Connect(ctx, cfg.Postgres.OwnerDSN())
	if err != nil {
		t.Errorf("cleanup: connect owner: %v", err)
		return nil, false
	}
	return conn, true
}

// RegisterDatasetCleanup removes the dataset row and, once no dataset or goal
// references its ref anymore, the data_source_registry entry it pointed at --
// the orchestration semantics for retiring a ref (see DatasetStore's usage
// check). The delete is made FK-safe regardless of teardown ordering: a goal can
// be bound to the dataset (goal_registry.dataset_id is NOT NULL), so any goal
// still pointing at it is removed first, along with that goal's runs,
// verifications, and causal verifications -- the same retirement set
// RegisterGoalCleanup would have taken if it had run earlier.
func RegisterDatasetCleanup(t *testing.T, ctx context.Context, cfg config.Config, datasetID string) {
	t.Helper()
	t.Cleanup(func() {
		conn, ok := cleanupConn(t, ctx, cfg)
		if !ok {
			return
		}
		defer conn.Close(context.WithoutCancel(ctx))
		cleanCtx := context.WithoutCancel(ctx)
		var ref string
		err := conn.QueryRow(cleanCtx, "SELECT datasource_ref FROM datasets WHERE id = $1", datasetID).Scan(&ref)
		if err != nil {
			return // already gone: nothing to do
		}
		goalRows, err := conn.Query(cleanCtx, "SELECT optimization_function_id FROM goal_registry WHERE dataset_id = $1", datasetID)
		if err != nil {
			t.Errorf("cleanup dataset %q: list goals: %v", datasetID, err)
			return
		}
		var goalIDs []string
		for goalRows.Next() {
			var gid string
			if err := goalRows.Scan(&gid); err != nil {
				goalRows.Close()
				t.Errorf("cleanup dataset %q: read goal id: %v", datasetID, err)
				return
			}
			goalIDs = append(goalIDs, gid)
		}
		goalRows.Close()
		if goalRows.Err() != nil {
			t.Errorf("cleanup dataset %q: scan goals: %v", datasetID, goalRows.Err())
			return
		}
		for _, gid := range goalIDs {
			for _, stmt := range []string{
				"DELETE FROM runs WHERE optimization_function_id = $1",
				"DELETE FROM verification_queue WHERE optimization_function_id = $1",
				"DELETE FROM causal_verifications WHERE goal_id = $1",
				"DELETE FROM goal_registry WHERE optimization_function_id = $1",
			} {
				if _, err := conn.Exec(cleanCtx, stmt, gid); err != nil {
					t.Errorf("cleanup dataset %q via goal %q: %v", datasetID, gid, err)
					return
				}
			}
		}
		if _, err := conn.Exec(cleanCtx, "DELETE FROM datasets WHERE id = $1", datasetID); err != nil {
			t.Errorf("cleanup dataset %q: %v", datasetID, err)
			return
		}
		if ref == "" {
			return
		}
		var n int
		if err := conn.QueryRow(cleanCtx,
			"SELECT (SELECT count(*) FROM datasets WHERE datasource_ref = $1) + "+
				"(SELECT count(*) FROM goal_registry WHERE datasource_ref = $1)", ref).Scan(&n); err != nil {
			return
		}
		if n == 0 {
			if _, err := conn.Exec(cleanCtx, "DELETE FROM data_source_registry WHERE ref = $1", ref); err != nil {
				t.Errorf("cleanup data source ref %q: %v", ref, err)
			}
		}
	})
}

// RegisterDataSourceRefCleanup removes a data_source_registry row once nothing
// references the ref anymore (the sibling of the dataset helper's retirement
// arm, for fixtures registered without a dataset).
func RegisterDataSourceRefCleanup(t *testing.T, ctx context.Context, cfg config.Config, ref string) {
	t.Helper()
	t.Cleanup(func() {
		conn, ok := cleanupConn(t, ctx, cfg)
		if !ok {
			return
		}
		defer conn.Close(context.WithoutCancel(ctx))
		cleanCtx := context.WithoutCancel(ctx)
		var n int
		if err := conn.QueryRow(cleanCtx,
			"SELECT (SELECT count(*) FROM datasets WHERE datasource_ref = $1) + "+
				"(SELECT count(*) FROM goal_registry WHERE datasource_ref = $1)", ref).Scan(&n); err != nil {
			return
		}
		if n == 0 {
			if _, err := conn.Exec(cleanCtx, "DELETE FROM data_source_registry WHERE ref = $1", ref); err != nil {
				t.Errorf("cleanup data source ref %q: %v", ref, err)
			}
		}
	})
}

// RegisterGoalCleanup removes a goal's dependent rows (runs, HITL verifications,
// causal verifications) and then the goal row itself. Unconditional deletes are
// correct here: the reset semantics a teardown needs remove a fixture in any
// lifecycle state, unlike the guarded HTTP delete.
func RegisterGoalCleanup(t *testing.T, ctx context.Context, cfg config.Config, goalID string) {
	t.Helper()
	t.Cleanup(func() {
		conn, ok := cleanupConn(t, ctx, cfg)
		if !ok {
			return
		}
		defer conn.Close(context.WithoutCancel(ctx))
		cleanCtx := context.WithoutCancel(ctx)
		for _, stmt := range []string{
			"DELETE FROM runs WHERE optimization_function_id = $1",
			"DELETE FROM verification_queue WHERE optimization_function_id = $1",
			"DELETE FROM causal_verifications WHERE goal_id = $1",
			"DELETE FROM goal_registry WHERE optimization_function_id = $1",
		} {
			if _, err := conn.Exec(cleanCtx, stmt, goalID); err != nil {
				t.Errorf("cleanup goal %q: %v", goalID, err)
				return
			}
		}
	})
}

// RegisterEmbeddingCleanup removes one meta_heuristic_embeddings row by node id.
// Every integration test now deletes only its own rows this way; the harness no
// longer truncates the whole table (which once destroyed real Meta-Heuristic
// embeddings on the shared stack).
func RegisterEmbeddingCleanup(t *testing.T, ctx context.Context, cfg config.Config, nodeID string) {
	t.Helper()
	t.Cleanup(func() {
		conn, ok := cleanupConn(t, ctx, cfg)
		if !ok {
			return
		}
		defer conn.Close(context.WithoutCancel(ctx))
		if _, err := conn.Exec(context.WithoutCancel(ctx),
			"DELETE FROM meta_heuristic_embeddings WHERE node_id = $1", nodeID); err != nil {
			t.Errorf("cleanup embedding %q: %v", nodeID, err)
		}
	})
}

// RegisterUserCleanup removes one users row at cleanup time; its sessions go
// with it via the ON DELETE CASCADE.
func RegisterUserCleanup(t *testing.T, ctx context.Context, cfg config.Config, userID string) {
	t.Helper()
	t.Cleanup(func() {
		conn, ok := cleanupConn(t, ctx, cfg)
		if !ok {
			return
		}
		defer conn.Close(context.WithoutCancel(ctx))
		if _, err := conn.Exec(context.WithoutCancel(ctx),
			"DELETE FROM users WHERE id = $1", userID); err != nil {
			t.Errorf("cleanup user %q: %v", userID, err)
		}
	})
}

// RegisterAuditCleanup removes one append-only audit_log row by its identity id.
// The runtime roles cannot delete audit rows (that is the boundary the boundary
// tests prove), so teardown must run as the owner -- the only role allowed to.
func RegisterAuditCleanup(t *testing.T, ctx context.Context, cfg config.Config, auditID int64) {
	t.Helper()
	t.Cleanup(func() {
		conn, ok := cleanupConn(t, ctx, cfg)
		if !ok {
			return
		}
		defer conn.Close(context.WithoutCancel(ctx))
		if _, err := conn.Exec(context.WithoutCancel(ctx),
			"DELETE FROM audit_log WHERE id = $1", auditID); err != nil {
			t.Errorf("cleanup audit row %d: %v", auditID, err)
		}
	})
}

// RegisterGraphNodeCleanup removes one graph node and its edges by id through a
// cfg-wired repository (NewNeo4jRepository at cleanup time). Absent-id deletes
// are a no-op, so tests that already removed the node themselves stay clean.
func RegisterGraphNodeCleanup(t *testing.T, ctx context.Context, cfg config.Config, nodeID string) {
	t.Helper()
	t.Cleanup(func() {
		cleanCtx := context.WithoutCancel(ctx)
		repo, err := graph.NewNeo4jRepository(cleanCtx, cfg.Neo4j.URI, cfg.Neo4j.User, cfg.Neo4j.Password)
		if err != nil {
			t.Errorf("cleanup node %q: connect graph: %v", nodeID, err)
			return
		}
		defer repo.Close(cleanCtx)
		if _, err := repo.DeleteNode(cleanCtx, nodeID); err != nil {
			t.Errorf("cleanup node %q: %v", nodeID, err)
		}
	})
}

// RegisterGoalGraphCleanup removes every graph node a goal wrote (and the edges
// between them) through the repository's DeleteGoalGraph -- the whole goal-scoped
// corpus in one call. It is the harness for goal-scoped graph fixtures: a causal
// graph's DataColumn and CausalGraphMeta nodes, or a goal-scoped triplet and the
// Meta-Heuristics abstracted from it, are all keyed by goal_id and need no
// per-node bookkeeping. Idempotent: a test that already deleted its own graph
// still cleans up quietly.
func RegisterGoalGraphCleanup(t *testing.T, ctx context.Context, cfg config.Config, goalID string) {
	t.Helper()
	t.Cleanup(func() {
		cleanCtx := context.WithoutCancel(ctx)
		repo, err := graph.NewNeo4jRepository(cleanCtx, cfg.Neo4j.URI, cfg.Neo4j.User, cfg.Neo4j.Password)
		if err != nil {
			t.Errorf("cleanup goal graph %q: connect graph: %v", goalID, err)
			return
		}
		defer repo.Close(cleanCtx)
		if err := repo.DeleteGoalGraph(cleanCtx, goalID); err != nil {
			t.Errorf("cleanup goal graph %q: %v", goalID, err)
		}
	})
}

// RegisterObjectCleanup removes one object-store key at cleanup time through a
// cfg-wired client. Deleting an absent key is a no-op, so tests whose objects
// were already removed stay clean.
func RegisterObjectCleanup(t *testing.T, ctx context.Context, cfg config.Config, key string) {
	t.Helper()
	t.Cleanup(func() {
		cleanCtx := context.WithoutCancel(ctx)
		client, err := objectstore.NewClient(cleanCtx, cfg.S3.Endpoint, cfg.S3.Region, cfg.S3.Bucket,
			cfg.S3.AccessKey, cfg.S3.SecretKey, cfg.S3.PathStyle)
		if err != nil {
			t.Errorf("cleanup object %q: connect object store: %v", key, err)
			return
		}
		if err := client.Delete(cleanCtx, key); err != nil {
			t.Errorf("cleanup object %q: %v", key, err)
		}
	})
}
