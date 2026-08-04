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

// TestCausalVerificationsMarkStale exercises the invalidation path a graph correction
// drives, through the orchestrator's own role. It covers the
// jsonb overlap predicate and the 0014 grant against real Postgres, both of which fail
// silently in production — the caller logs and reports "redispatched: 0" — so a
// missing grant or a binding mismatch would otherwise look like "nothing to
// invalidate".
func TestCausalVerificationsMarkStale(t *testing.T) {
	ctx := context.Background()
	cfg := setup(t, ctx)
	service := pool(t, ctx, cfg.Postgres.ServiceDSN())
	orch := pool(t, ctx, cfg.Postgres.OrchestratorDSN())
	goalID := seedGoal(t, ctx, orch)

	writer := store.NewCausalVerifications(service, time.Minute, 8, 20)
	// Two completed records adjusting on different columns, plus one still in flight.
	touched, other, inflight := testutil.NewID(t), testutil.NewID(t), testutil.NewID(t)
	for id, adjust := range map[string][]string{touched: {"Z", "X"}, other: {"unrelated"}} {
		rec, accepted, err := writer.DispatchAccept(ctx, dispatchRec(t, goalID, id, 1, false))
		if err != nil || !accepted {
			t.Fatalf("dispatch %q: accepted=%v err=%v", id, accepted, err)
		}
		if held, err := writer.Complete(ctx, rec.ID, store.CausalStatusCausallyVerified, nil, nil, adjust, nil, nil); err != nil || !held {
			t.Fatalf("complete %q: held=%v err=%v", id, held, err)
		}
	}
	if _, accepted, err := writer.DispatchAccept(ctx, dispatchRec(t, goalID, inflight, 1, false)); err != nil || !accepted {
		t.Fatalf("dispatch in-flight: accepted=%v err=%v", accepted, err)
	}

	// The orchestrator role is the one that marks stale, so run it through that pool:
	// this is what proves 0014's grant.
	reader := store.NewCausalVerifications(orch, time.Minute, 8, 20)
	flagged, err := reader.MarkStale(ctx, goalID, []string{"Z", "Y"})
	if err != nil {
		t.Fatalf("mark stale as the orchestrator role: %v", err)
	}
	if len(flagged) != 1 || flagged[0] != touched {
		t.Fatalf("flagged = %v, want only the record adjusting on Z", flagged)
	}

	records, err := reader.ListForGoal(ctx, goalID)
	if err != nil {
		t.Fatalf("list as the orchestrator role: %v", err)
	}
	stale := map[string]bool{}
	for _, rec := range records {
		stale[rec.InterventionID] = rec.Stale
	}
	if !stale[touched] {
		t.Fatalf("the record adjusting on a corrected column was not flagged")
	}
	if stale[other] || stale[inflight] {
		t.Fatalf("stale spread beyond the overlapping completed record: %v", stale)
	}

	// A second correction must return only what it flags, not the goal's accumulated
	// stale history -- otherwise every later correction re-dispatches earlier work.
	again, err := reader.MarkStale(ctx, goalID, []string{"unrelated"})
	if err != nil {
		t.Fatalf("second mark stale: %v", err)
	}
	if len(again) != 1 || again[0] != other {
		t.Fatalf("second correction flagged %v, want only its own record", again)
	}

	// One finding verified at several graph versions holds one completed record per
	// version, and the caller spends a slot of its re-dispatch cap per id it gets back.
	// Without the DISTINCT the same intervention would come back once per version,
	// truncating the fan-out and inflating the count the analyst is shown.
	multi := testutil.NewID(t)
	for _, version := range []int{1, 2} {
		rec, accepted, err := writer.DispatchAccept(ctx, dispatchRec(t, goalID, multi, version, false))
		if err != nil || !accepted {
			t.Fatalf("dispatch %q at version %d: accepted=%v err=%v", multi, version, accepted, err)
		}
		if held, err := writer.Complete(ctx, rec.ID, store.CausalStatusCausallyVerified, nil, nil, []string{"W"}, nil, nil); err != nil || !held {
			t.Fatalf("complete %q at version %d: held=%v err=%v", multi, version, held, err)
		}
	}
	deduped, err := reader.MarkStale(ctx, goalID, []string{"W"})
	if err != nil {
		t.Fatalf("mark stale across versions: %v", err)
	}
	if len(deduped) != 1 || deduped[0] != multi {
		t.Fatalf("flagged %v, want the intervention once despite two verified versions", deduped)
	}

	// The orchestrator reads and invalidates but never creates: record creation stays
	// the Verifier's, so the charge-at-accept accounting has exactly one writer.
	if _, _, err := reader.DispatchAccept(ctx, dispatchRec(t, goalID, testutil.NewID(t), 2, false)); err == nil {
		t.Fatalf("the orchestrator role must not be able to insert a verification record")
	}
}

// TestCausalVerificationsReservesASlotForExemptDispatches: autonomous promotion must
// not be able to fill the in-flight cap, or the budget exemption is only half real --
// an analyst-initiated verification would be spared the budget and then lose the slot
// race anyway. A verify goal's own claim loses that race systematically, because it is
// introspected, measured and reified before it asks for a slot while the promotions
// dispatched alongside it ask immediately.
func TestCausalVerificationsReservesASlotForExemptDispatches(t *testing.T) {
	ctx := context.Background()
	cfg := setup(t, ctx)
	p := pool(t, ctx, cfg.Postgres.ServiceDSN())
	orch := pool(t, ctx, cfg.Postgres.OrchestratorDSN())
	goalID := seedGoal(t, ctx, orch)

	const cap = 3
	writer := store.NewCausalVerifications(p, time.Minute, cap, 20)

	// Autonomous promotion fills what it is allowed to: one slot short of the cap.
	for i := 0; i < cap-1; i++ {
		if _, accepted, err := writer.DispatchAccept(ctx, dispatchRec(t, goalID, testutil.NewID(t), 1, true)); err != nil || !accepted {
			t.Fatalf("budgeted dispatch %d: accepted=%v err=%v", i, accepted, err)
		}
	}
	// The next autonomous one is refused even though the cap itself is not reached.
	_, _, err := writer.DispatchAccept(ctx, dispatchRec(t, goalID, testutil.NewID(t), 1, true))
	if !errors.Is(err, store.ErrInflightCapReached) {
		t.Fatalf("budgeted dispatch past the reservation: err=%v, want ErrInflightCapReached", err)
	}
	// The reserved slot is still there for the analyst's own.
	if _, accepted, err := writer.DispatchAccept(ctx, dispatchRec(t, goalID, testutil.NewID(t), 1, false)); err != nil || !accepted {
		t.Fatalf("exempt dispatch into the reserved slot: accepted=%v err=%v", accepted, err)
	}
	// And the cap still binds it.
	if _, _, err := writer.DispatchAccept(ctx, dispatchRec(t, goalID, testutil.NewID(t), 1, false)); !errors.Is(err, store.ErrInflightCapReached) {
		t.Fatalf("exempt dispatch past the cap: err=%v, want ErrInflightCapReached", err)
	}
}
