package store_test

import (
	"context"
	"testing"

	"github.com/arborette/arborette/internal/store"
	"github.com/arborette/arborette/internal/testutil"
)

// TestAdvisoryLockSemantics verifies the cross-process single-flight against a live
// Postgres over two pools: while one pool holds the (goal, data-source) lock a second
// pool cannot acquire it; after the holder's release func runs, the second pool
// acquires it, and — the unlock-before-Release leak check — a fresh acquire from the
// first pool succeeds too, proving the released connection did not return to the pool
// with the lock still held.
func TestAdvisoryLockSemantics(t *testing.T) {
	ctx := context.Background()
	cfg := setup(t, ctx)

	p1 := pool(t, ctx, cfg.Postgres.ServiceDSN())
	p2 := pool(t, ctx, cfg.Postgres.ServiceDSN())
	lock1 := store.NewAdvisoryLock(p1)
	lock2 := store.NewAdvisoryLock(p2)

	goalID := testutil.NewID(t)
	ref := "lock/" + testutil.NewID(t)

	release, ok, err := lock1.TryAcquireDiscoveryLock(ctx, goalID, ref)
	if err != nil || !ok {
		t.Fatalf("first acquire: ok=%v err=%v", ok, err)
	}

	// A second pool must not acquire the same key while the first holds it.
	if _, ok, err := lock2.TryAcquireDiscoveryLock(ctx, goalID, ref); err != nil {
		t.Fatalf("contended acquire errored: %v", err)
	} else if ok {
		t.Fatalf("second pool acquired a held lock")
	}

	release()

	// After release the second pool acquires it.
	release2, ok, err := lock2.TryAcquireDiscoveryLock(ctx, goalID, ref)
	if err != nil || !ok {
		t.Fatalf("acquire after release: ok=%v err=%v", ok, err)
	}
	release2()

	// Leak check: the first pool acquires again — its released connection was
	// unlocked before being returned, not leaked with the lock held.
	release3, ok, err := lock1.TryAcquireDiscoveryLock(ctx, goalID, ref)
	if err != nil || !ok {
		t.Fatalf("first pool re-acquire (leak check): ok=%v err=%v", ok, err)
	}
	release3()
}
