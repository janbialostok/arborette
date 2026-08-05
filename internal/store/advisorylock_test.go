package store

import "testing"

// TestLockKeyStability pins the advisory-lock key derivation: the same (goal,
// data-source) pair must hash to the same int64 across processes (so the Verifier's
// serve and job modes contend on one lock), and distinct pairs must not collide for
// the fixtures used. The key is load-bearing — a drift in the hash would silently
// stop coordinating discoveries.
func TestLockKeyStability(t *testing.T) {
	a := lockKey("goal-1", "ref-1")
	if b := lockKey("goal-1", "ref-1"); a != b {
		t.Fatalf("lockKey not stable: %d != %d", a, b)
	}
	// The delimiter prevents ("goal1","") and ("goal","1") from colliding.
	if lockKey("goal1", "") == lockKey("goal", "1") {
		t.Fatalf("lockKey collides across a shifted delimiter boundary")
	}
	if lockKey("goal-1", "ref-1") == lockKey("goal-2", "ref-1") {
		t.Fatalf("distinct goals hashed to the same key")
	}
	if lockKey("goal-1", "ref-1") == lockKey("goal-1", "ref-2") {
		t.Fatalf("distinct data sources hashed to the same key")
	}
}
