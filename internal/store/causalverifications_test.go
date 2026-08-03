package store_test

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/arborette/arborette/internal/store"
	"github.com/arborette/arborette/internal/testutil"
)

// dispatchRec builds a dispatch record for a goal/intervention with a fresh id.
func dispatchRec(t *testing.T, goalID, interventionID string, version int, budgeted bool) store.CausalVerification {
	t.Helper()
	return store.CausalVerification{
		ID:             testutil.NewID(t),
		GoalID:         goalID,
		InterventionID: interventionID,
		GraphVersion:   version,
		Budgeted:       budgeted,
	}
}

// TestCausalVerificationsDispatchIdempotency: a fresh dispatch accepts; a duplicate
// on the same (goal, intervention, version) key coalesces to the live record,
// returning accepted=false and writing no second row.
func TestCausalVerificationsDispatchIdempotency(t *testing.T) {
	ctx := context.Background()
	cfg := setup(t, ctx)
	p := pool(t, ctx, cfg.Postgres.ServiceDSN())
	orch := pool(t, ctx, cfg.Postgres.OrchestratorDSN())
	goalID := seedGoal(t, ctx, orch)

	s := store.NewCausalVerifications(p, time.Minute, 4, 20)
	rec := dispatchRec(t, goalID, testutil.NewID(t), 1, false)

	first, accepted, err := s.DispatchAccept(ctx, rec)
	if err != nil || !accepted {
		t.Fatalf("first dispatch: accepted=%v err=%v, want true/nil", accepted, err)
	}

	dup := rec
	dup.ID = testutil.NewID(t) // a second dispatcher generates its own id
	coalesced, accepted, err := s.DispatchAccept(ctx, dup)
	if err != nil {
		t.Fatalf("duplicate dispatch: %v", err)
	}
	if accepted {
		t.Fatal("a duplicate dispatch must coalesce (accepted=false), not re-run")
	}
	if coalesced.ID != first.ID {
		t.Fatalf("coalesced record id = %q, want the live %q", coalesced.ID, first.ID)
	}

	all, err := s.ListForGoal(ctx, goalID)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(all) != 1 {
		t.Fatalf("expected exactly one row, got %d", len(all))
	}
}

// TestCausalVerificationsReapAndRelease: an expired lease reaps to failed (freeing
// the budget/cap counts), and a re-dispatch on the same key re-leases it — an UPDATE
// returning accepted=true that re-charges the budget.
func TestCausalVerificationsReapAndRelease(t *testing.T) {
	ctx := context.Background()
	cfg := setup(t, ctx)
	p := pool(t, ctx, cfg.Postgres.ServiceDSN())
	orch := pool(t, ctx, cfg.Postgres.OrchestratorDSN())
	goalID := seedGoal(t, ctx, orch)

	// A zero TTL makes the lease deadline now, so ReapExpired flips it immediately.
	s := store.NewCausalVerifications(p, 0, 4, 20)
	rec := dispatchRec(t, goalID, testutil.NewID(t), 1, true)

	first, accepted, err := s.DispatchAccept(ctx, rec)
	if err != nil || !accepted {
		t.Fatalf("first dispatch: accepted=%v err=%v", accepted, err)
	}

	reaped, err := s.ReapExpired(ctx)
	if err != nil || reaped < 1 {
		t.Fatalf("reap: n=%d err=%v, want >=1", reaped, err)
	}

	released, accepted, err := s.DispatchAccept(ctx, rec)
	if err != nil || !accepted {
		t.Fatalf("re-lease of a failed record: accepted=%v err=%v, want true/nil", accepted, err)
	}
	if released.ID != first.ID {
		t.Fatalf("re-lease must reuse the same row id, got %q want %q", released.ID, first.ID)
	}
	if released.Status != store.CausalStatusPending {
		t.Fatalf("re-leased status = %q, want pending", released.Status)
	}

	all, err := s.ListForGoal(ctx, goalID)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(all) != 1 {
		t.Fatalf("re-lease must not create a second row, got %d", len(all))
	}
}

// TestCausalVerificationsCompleteGuard: Complete stamps a terminal status on a live
// lease (RowsAffected 1), but a reaped record (failed) is not overwritten (0).
func TestCausalVerificationsCompleteGuard(t *testing.T) {
	ctx := context.Background()
	cfg := setup(t, ctx)
	p := pool(t, ctx, cfg.Postgres.ServiceDSN())
	orch := pool(t, ctx, cfg.Postgres.OrchestratorDSN())
	goalID := seedGoal(t, ctx, orch)

	s := store.NewCausalVerifications(p, time.Minute, 4, 20)
	rec := dispatchRec(t, goalID, testutil.NewID(t), 1, false)
	first, _, err := s.DispatchAccept(ctx, rec)
	if err != nil {
		t.Fatalf("dispatch: %v", err)
	}

	eff := 1.5
	held, err := s.Complete(ctx, first.ID, store.CausalStatusCausallyVerified, &eff, &eff, []string{"Z"}, &eff, &eff)
	if err != nil || !held {
		t.Fatalf("complete on a live lease: held=%v err=%v, want true/nil", held, err)
	}

	// A completed record is no longer pending, so a second Complete does not overwrite.
	held, err = s.Complete(ctx, first.ID, store.CausalStatusConfounded, nil, nil, nil, nil, nil)
	if err != nil {
		t.Fatalf("second complete: %v", err)
	}
	if held {
		t.Fatal("a non-pending record must not be re-completed (held=false)")
	}
}

// TestCausalVerificationsBudgetConcurrency: with a budget of one, many concurrent
// budgeted dispatches of distinct interventions must let exactly one through — the
// per-goal transaction advisory lock serializes the count guard so two cannot both
// overspend.
func TestCausalVerificationsBudgetConcurrency(t *testing.T) {
	ctx := context.Background()
	cfg := setup(t, ctx)
	p := pool(t, ctx, cfg.Postgres.ServiceDSN())
	orch := pool(t, ctx, cfg.Postgres.OrchestratorDSN())
	goalID := seedGoal(t, ctx, orch)

	s := store.NewCausalVerifications(p, time.Minute, 100, 1) // budget 1, cap generous

	const n = 8
	var wg sync.WaitGroup
	results := make([]error, n)
	accepts := make([]bool, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, accepted, err := s.DispatchAccept(ctx, dispatchRec(t, goalID, testutil.NewID(t), 1, true))
			results[i], accepts[i] = err, accepted
		}(i)
	}
	wg.Wait()

	accepted, exhausted := 0, 0
	for i := 0; i < n; i++ {
		switch {
		case accepts[i] && results[i] == nil:
			accepted++
		case errors.Is(results[i], store.ErrBudgetExhausted):
			exhausted++
		default:
			t.Fatalf("dispatch %d: accepted=%v err=%v, want accept or ErrBudgetExhausted", i, accepts[i], results[i])
		}
	}
	if accepted != 1 {
		t.Fatalf("budget of one admitted %d dispatches, want exactly 1", accepted)
	}
	if exhausted != n-1 {
		t.Fatalf("budget exhausted count = %d, want %d", exhausted, n-1)
	}
}

// TestCausalVerificationsBudgetOverride: a per-goal verification_budget column value
// bounds admissions, overriding the store's default. Seeding an override distinct
// from the default proves DispatchAccept reads the goal row (not the constructor
// default) — the branch every other test masks by leaving the column NULL.
func TestCausalVerificationsBudgetOverride(t *testing.T) {
	ctx := context.Background()
	cfg := setup(t, ctx)
	p := pool(t, ctx, cfg.Postgres.ServiceDSN())
	orch := pool(t, ctx, cfg.Postgres.OrchestratorDSN())
	goalID := seedGoal(t, ctx, orch)
	// Set a per-goal budget of 2, distinct from the generous store default of 100.
	if _, err := orch.Exec(ctx, "UPDATE goal_registry SET verification_budget = 2 WHERE optimization_function_id = $1", goalID); err != nil {
		t.Fatalf("set goal budget: %v", err)
	}

	s := store.NewCausalVerifications(p, time.Minute, 100, 100)
	accepted := 0
	for i := 0; i < 4; i++ {
		_, ok, err := s.DispatchAccept(ctx, dispatchRec(t, goalID, testutil.NewID(t), 1, true))
		if ok && err == nil {
			accepted++
			continue
		}
		if !errors.Is(err, store.ErrBudgetExhausted) {
			t.Fatalf("dispatch %d: err=%v, want ErrBudgetExhausted", i, err)
		}
	}
	if accepted != 2 {
		t.Fatalf("per-goal budget of 2 admitted %d, want 2 (the override, not the default 100)", accepted)
	}
}

// TestCausalVerificationsInflightCap: the in-flight cap bounds concurrent pending
// records regardless of budgeting.
func TestCausalVerificationsInflightCap(t *testing.T) {
	ctx := context.Background()
	cfg := setup(t, ctx)
	p := pool(t, ctx, cfg.Postgres.ServiceDSN())
	orch := pool(t, ctx, cfg.Postgres.OrchestratorDSN())
	goalID := seedGoal(t, ctx, orch)

	s := store.NewCausalVerifications(p, time.Minute, 2, 100) // cap 2

	accepted := 0
	for i := 0; i < 4; i++ {
		_, ok, err := s.DispatchAccept(ctx, dispatchRec(t, goalID, testutil.NewID(t), 1, false))
		if ok && err == nil {
			accepted++
			continue
		}
		if !errors.Is(err, store.ErrInflightCapReached) {
			t.Fatalf("dispatch %d: err=%v, want ErrInflightCapReached", i, err)
		}
	}
	if accepted != 2 {
		t.Fatalf("in-flight cap of two admitted %d, want 2", accepted)
	}
}

// TestCausalVerificationsGrants pins the grant boundary: the verifier's service role
// can INSERT (dispatch) and UPDATE (complete) causal_verifications.
func TestCausalVerificationsGrants(t *testing.T) {
	ctx := context.Background()
	cfg := setup(t, ctx)
	service := pool(t, ctx, cfg.Postgres.ServiceDSN())
	orch := pool(t, ctx, cfg.Postgres.OrchestratorDSN())
	goalID := seedGoal(t, ctx, orch)

	s := store.NewCausalVerifications(service, time.Minute, 4, 20)
	rec := dispatchRec(t, goalID, testutil.NewID(t), 1, false)
	first, accepted, err := s.DispatchAccept(ctx, rec)
	if err != nil || !accepted {
		t.Fatalf("service INSERT via dispatch: accepted=%v err=%v", accepted, err)
	}
	if held, err := s.Complete(ctx, first.ID, store.CausalStatusNotIdentifiable, nil, nil, nil, nil, nil); err != nil || !held {
		t.Fatalf("service UPDATE via complete: held=%v err=%v", held, err)
	}
}
