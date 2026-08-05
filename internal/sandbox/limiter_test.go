package sandbox

import (
	"context"
	"errors"
	"testing"
	"time"
)

// TestClassLimiterBlocksAtCapacity proves a cap-1 class serializes: the second
// Acquire blocks until the first releases, then proceeds.
func TestClassLimiterBlocksAtCapacity(t *testing.T) {
	l := NewClassLimiter(map[string]int{ClassDefault: 1})
	ctx := context.Background()

	release, err := l.Acquire(ctx, ClassDefault)
	if err != nil {
		t.Fatalf("first acquire: %v", err)
	}

	acquired := make(chan struct{})
	go func() {
		rel, err := l.Acquire(ctx, ClassDefault)
		if err == nil {
			rel()
		}
		close(acquired)
	}()

	select {
	case <-acquired:
		t.Fatalf("second acquire proceeded while the only slot was held")
	case <-time.After(50 * time.Millisecond):
	}

	release()
	select {
	case <-acquired:
	case <-time.After(time.Second):
		t.Fatalf("second acquire never proceeded after release")
	}
}

// TestClassLimiterCancelUnblocks proves a caller waiting on a full class unblocks
// with its own context error rather than hanging.
func TestClassLimiterCancelUnblocks(t *testing.T) {
	l := NewClassLimiter(map[string]int{ClassDefault: 1})

	release, err := l.Acquire(context.Background(), ClassDefault)
	if err != nil {
		t.Fatalf("first acquire: %v", err)
	}
	defer release()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := l.Acquire(ctx, ClassDefault); !errors.Is(err, context.Canceled) {
		t.Fatalf("acquire err=%v, want context.Canceled", err)
	}
}

// TestClassLimiterReleaseIsIdempotent pins the sync.Once guard: a release invoked
// twice must not block on a second drain of the slot channel, which would hang the
// caller (and, absent the guard, steal a slot the next acquirer expects to be free).
func TestClassLimiterReleaseIsIdempotent(t *testing.T) {
	l := NewClassLimiter(map[string]int{ClassDefault: 1})
	rel, err := l.Acquire(context.Background(), ClassDefault)
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}

	done := make(chan struct{})
	go func() {
		rel()
		rel() // must not block on an already-drained slot
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatalf("double release blocked; the idempotency guard is missing")
	}

	// The slot was freed exactly once, so it is acquirable again.
	rel2, err := l.Acquire(context.Background(), ClassDefault)
	if err != nil {
		t.Fatalf("re-acquire after release: %v", err)
	}
	rel2()
}

// TestClassLimiterUnknownClassUnbounded proves a class with no registered semaphore
// is unbounded, so a future route without its own class runs rather than stalls.
func TestClassLimiterUnknownClassUnbounded(t *testing.T) {
	l := NewClassLimiter(map[string]int{ClassDefault: 1})
	for i := 0; i < 100; i++ {
		rel, err := l.Acquire(context.Background(), "unregistered")
		if err != nil {
			t.Fatalf("unregistered acquire %d: %v", i, err)
		}
		rel()
	}
}
