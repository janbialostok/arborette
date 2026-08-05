package store

import (
	"context"
	"fmt"
	"hash/fnv"
	"log"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// AdvisoryLock is the repo's first Postgres advisory-lock use: a cross-process
// single-flight over a (goal, data-source) key, since the Verifier's serve mode and
// job mode are separate processes an in-process mutex cannot coordinate. Session
// advisory locks bind to a physical backend connection, so the lock holder pins one
// pool connection for the entire multi-round-trip discovery — pool sizing must
// account for concurrent per-goal discoveries.
type AdvisoryLock struct {
	pool *Pool
}

// NewAdvisoryLock wires the lock to a runtime pool.
func NewAdvisoryLock(pool *Pool) *AdvisoryLock {
	return &AdvisoryLock{pool: pool}
}

// unlockTimeout bounds the unlock that runs in the release func, so a release deferred
// under a cancelled discovery context still completes on a fresh context.
const unlockTimeout = 5 * time.Second

// TryAcquireDiscoveryLock attempts a non-blocking session advisory lock on the
// (goal, data-source) key, hashed to an int64 via FNV-1a. It acquires a dedicated
// pool connection (the lock binds to it) and calls pg_try_advisory_lock; on success
// it returns a release func and acquired=true, on contention it releases the
// connection immediately and returns acquired=false. The caller must never hold a
// pooled connection while waiting, which is why a failed try releases at once rather
// than blocking.
//
// Release semantics (pgx v5): (*pgxpool.Conn).Release() returns the connection to the
// pool with the lock still held, leaking it to the next borrower — so the release
// func runs pg_advisory_unlock on the same connection, verifies it returned true, and
// only then Releases; on an unlock failure it destroys the connection (Hijack +
// Close) rather than pooling a still-locked connection.
func (l *AdvisoryLock) TryAcquireDiscoveryLock(ctx context.Context, goalID, datasourceRef string) (func(), bool, error) {
	key := lockKey(goalID, datasourceRef)

	conn, err := l.pool.Acquire(ctx)
	if err != nil {
		return nil, false, fmt.Errorf("acquire lock connection: %w", err)
	}

	var acquired bool
	if err := conn.QueryRow(ctx, "SELECT pg_try_advisory_lock($1)", key).Scan(&acquired); err != nil {
		conn.Release()
		return nil, false, fmt.Errorf("pg_try_advisory_lock: %w", err)
	}
	if !acquired {
		conn.Release()
		return nil, false, nil
	}

	release := func() {
		unlockCtx, cancel := context.WithTimeout(context.Background(), unlockTimeout)
		defer cancel()
		var unlocked bool
		if err := conn.QueryRow(unlockCtx, "SELECT pg_advisory_unlock($1)", key).Scan(&unlocked); err != nil || !unlocked {
			// The lock is still held on this connection; do not return it to the pool
			// where the next borrower would inherit the lock. Destroy it instead.
			log.Printf("store: advisory unlock failed for key %d (unlocked=%v, err=%v); destroying connection", key, unlocked, err)
			destroyConn(unlockCtx, conn)
			return
		}
		conn.Release()
	}
	return release, true, nil
}

// destroyConn removes a connection from the pool and closes it, so a connection that
// could not be unlocked is never reused with the lock still held.
func destroyConn(ctx context.Context, conn *pgxpool.Conn) {
	hijacked := conn.Hijack()
	if hijacked != nil {
		_ = hijacked.Close(ctx)
	}
}

// lockKey hashes the (goal, data-source) pair to the int64 advisory-lock key via
// FNV-1a, the same key both processes derive so they contend on one lock.
func lockKey(goalID, datasourceRef string) int64 {
	return fnvInt64Key(goalID, datasourceRef)
}

// fnvInt64Key hashes its NUL-joined parts to the int64 Postgres advisory-lock key via
// FNV-1a. Shared by the discovery (goal, data-source) session lock and the per-goal
// verification transaction lock so the two derivations cannot drift.
func fnvInt64Key(parts ...string) int64 {
	h := fnv.New64a()
	_, _ = h.Write([]byte(strings.Join(parts, "\x00")))
	return int64(h.Sum64())
}
