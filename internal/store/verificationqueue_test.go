package store_test

import (
	"context"
	"errors"
	"testing"

	"github.com/arborette/arborette/internal/domain"
	"github.com/arborette/arborette/internal/store"
	"github.com/arborette/arborette/internal/testutil"
	"github.com/jackc/pgx/v5"
)

// queuedEntry is one below-threshold extraction awaiting review, with a locator
// so the jsonb round-trip is exercised.
func queuedEntry(t *testing.T, goalID string) store.VerificationEntry {
	t.Helper()
	return store.VerificationEntry{
		QueueID:                testutil.NewID(t),
		OptimizationFunctionID: goalID,
		OutcomeID:              testutil.NewID(t),
		Field:                  "effective_date",
		ExtractedValue:         "2026-01-01",
		Provenance:             &domain.ProvenanceLocator{Page: 2, CharStart: 10, CharEnd: 20},
		Confidence:             0.42,
	}
}

func TestVerificationQueueLifecycle(t *testing.T) {
	ctx := context.Background()
	cfg := setup(t, ctx)
	p := pool(t, ctx, cfg.Postgres.OrchestratorDSN())
	queue := store.NewVerificationQueue(p)
	goalID := seedGoal(t, ctx, p)

	entry := queuedEntry(t, goalID)
	if err := queue.Enqueue(ctx, entry); err != nil {
		t.Fatalf("enqueue: %v", err)
	}

	got, err := queue.GetByOutcome(ctx, entry.OutcomeID)
	if err != nil {
		t.Fatalf("get by outcome: %v", err)
	}
	if got.Status != store.QueuePending || got.Resolution != "" || got.ResolvedAt != nil {
		t.Fatalf("a fresh entry must be pending and unresolved: %+v", got)
	}
	if got.ExtractedValue != entry.ExtractedValue || got.Confidence != entry.Confidence {
		t.Fatalf("entry did not round-trip: %+v", got)
	}
	if got.Provenance == nil || *got.Provenance != *entry.Provenance {
		t.Fatalf("locator = %+v, want %+v", got.Provenance, entry.Provenance)
	}

	pending, err := queue.ListForGoal(ctx, goalID, store.QueuePending)
	if err != nil {
		t.Fatalf("list pending: %v", err)
	}
	if len(pending) != 1 || pending[0].OutcomeID != entry.OutcomeID {
		t.Fatalf("expected the queued entry to be listed as pending: %+v", pending)
	}

	if err := queue.Resolve(ctx, entry.OutcomeID, store.ResolutionCorrected, "2026-02-01"); err != nil {
		t.Fatalf("resolve: %v", err)
	}
	resolved, err := queue.GetByOutcome(ctx, entry.OutcomeID)
	if err != nil {
		t.Fatalf("get resolved: %v", err)
	}
	if resolved.Status != store.QueueResolved || resolved.Resolution != store.ResolutionCorrected ||
		resolved.CorrectedValue != "2026-02-01" || resolved.ResolvedAt == nil {
		t.Fatalf("resolved entry = %+v, want a stamped correction", resolved)
	}

	stillPending, err := queue.ListForGoal(ctx, goalID, store.QueuePending)
	if err != nil {
		t.Fatalf("list pending after resolve: %v", err)
	}
	for _, e := range stillPending {
		if e.OutcomeID == entry.OutcomeID {
			t.Fatalf("a resolved entry must leave the pending list: %+v", e)
		}
	}

	if _, err := queue.GetByOutcome(ctx, testutil.NewID(t)); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("error = %v, want pgx.ErrNoRows for an outcome that was never queued", err)
	}
}

// The claim is what keeps two resolutions of one outcome from writing different
// verdicts to the graph, so exactly one caller may win it.
func TestVerificationQueueClaimIsExclusive(t *testing.T) {
	ctx := context.Background()
	cfg := setup(t, ctx)
	p := pool(t, ctx, cfg.Postgres.OrchestratorDSN())
	queue := store.NewVerificationQueue(p)
	goalID := seedGoal(t, ctx, p)

	t.Run("a second resolution loses the claim", func(t *testing.T) {
		entry := queuedEntry(t, goalID)
		if err := queue.Enqueue(ctx, entry); err != nil {
			t.Fatalf("enqueue: %v", err)
		}
		if err := queue.Resolve(ctx, entry.OutcomeID, store.ResolutionConfirmed, ""); err != nil {
			t.Fatalf("first resolve: %v", err)
		}
		if err := queue.Resolve(ctx, entry.OutcomeID, store.ResolutionRejected, ""); !errors.Is(err, store.ErrAlreadyResolved) {
			t.Fatalf("error = %v, want ErrAlreadyResolved", err)
		}
		got, err := queue.GetByOutcome(ctx, entry.OutcomeID)
		if err != nil {
			t.Fatalf("get: %v", err)
		}
		if got.Resolution != store.ResolutionConfirmed {
			t.Fatalf("the first verdict must stand, got %q", got.Resolution)
		}
	})

	t.Run("an on-demand insert conflicts rather than duplicating the outcome", func(t *testing.T) {
		entry := queuedEntry(t, goalID)
		if err := queue.EnqueueResolved(ctx, entry, store.ResolutionConfirmed, ""); err != nil {
			t.Fatalf("first on-demand resolution: %v", err)
		}
		second := entry
		second.QueueID = testutil.NewID(t)
		if err := queue.EnqueueResolved(ctx, second, store.ResolutionRejected, ""); !errors.Is(err, store.ErrAlreadyResolved) {
			t.Fatalf("error = %v, want ErrAlreadyResolved", err)
		}
	})
}

// Compensation must be scoped to the claim its caller wrote, so a request that
// never claimed can never revert another's resolution.
func TestVerificationQueueUnclaimIsScopedToTheClaim(t *testing.T) {
	ctx := context.Background()
	cfg := setup(t, ctx)
	p := pool(t, ctx, cfg.Postgres.OrchestratorDSN())
	queue := store.NewVerificationQueue(p)
	goalID := seedGoal(t, ctx, p)

	t.Run("reverts the caller's own claim", func(t *testing.T) {
		entry := queuedEntry(t, goalID)
		if err := queue.Enqueue(ctx, entry); err != nil {
			t.Fatalf("enqueue: %v", err)
		}
		if err := queue.Resolve(ctx, entry.OutcomeID, store.ResolutionCorrected, "2026-02-01"); err != nil {
			t.Fatalf("resolve: %v", err)
		}
		if err := queue.Unclaim(ctx, entry.OutcomeID, store.ResolutionCorrected); err != nil {
			t.Fatalf("unclaim: %v", err)
		}
		got, err := queue.GetByOutcome(ctx, entry.OutcomeID)
		if err != nil {
			t.Fatalf("get: %v", err)
		}
		if got.Status != store.QueuePending || got.Resolution != "" || got.CorrectedValue != "" || got.ResolvedAt != nil {
			t.Fatalf("an unclaimed entry must be fully back to pending: %+v", got)
		}
	})

	t.Run("leaves a claim written by another resolution alone", func(t *testing.T) {
		entry := queuedEntry(t, goalID)
		if err := queue.Enqueue(ctx, entry); err != nil {
			t.Fatalf("enqueue: %v", err)
		}
		if err := queue.Resolve(ctx, entry.OutcomeID, store.ResolutionConfirmed, ""); err != nil {
			t.Fatalf("resolve: %v", err)
		}
		// A different verdict means a different claim, which this caller does not own.
		if err := queue.Unclaim(ctx, entry.OutcomeID, store.ResolutionRejected); err != nil {
			t.Fatalf("unclaim: %v", err)
		}
		got, err := queue.GetByOutcome(ctx, entry.OutcomeID)
		if err != nil {
			t.Fatalf("get: %v", err)
		}
		if got.Status != store.QueueResolved || got.Resolution != store.ResolutionConfirmed {
			t.Fatalf("another caller's resolution must survive: %+v", got)
		}
	})

	t.Run("is a no-op on a pending entry", func(t *testing.T) {
		entry := queuedEntry(t, goalID)
		if err := queue.Enqueue(ctx, entry); err != nil {
			t.Fatalf("enqueue: %v", err)
		}
		if err := queue.Unclaim(ctx, entry.OutcomeID, store.ResolutionConfirmed); err != nil {
			t.Fatalf("unclaim: %v", err)
		}
		got, err := queue.GetByOutcome(ctx, entry.OutcomeID)
		if err != nil {
			t.Fatalf("get: %v", err)
		}
		if got.Status != store.QueuePending {
			t.Fatalf("entry = %+v, want it left pending", got)
		}
	})
}

// The queue is orchestrator-owned: the service role may read it for context but
// must never record or alter a review.
func TestVerificationQueueGrants(t *testing.T) {
	ctx := context.Background()
	cfg := setup(t, ctx)

	orchestrator := pool(t, ctx, cfg.Postgres.OrchestratorDSN())
	goalID := seedGoal(t, ctx, orchestrator)
	entry := queuedEntry(t, goalID)
	if err := store.NewVerificationQueue(orchestrator).Enqueue(ctx, entry); err != nil {
		t.Fatalf("orchestrator enqueue: %v", err)
	}

	service := pool(t, ctx, cfg.Postgres.ServiceDSN())
	var count int
	if err := service.QueryRow(ctx, "SELECT count(*) FROM verification_queue").Scan(&count); err != nil {
		t.Fatalf("service select verification_queue: %v", err)
	}
	if _, err := service.Exec(ctx,
		"INSERT INTO verification_queue (queue_id, optimization_function_id, outcome_id, field, extracted_value, confidence) "+
			"VALUES ($1,$2,$3,'f','v',0.5)",
		testutil.NewID(t), goalID, testutil.NewID(t),
	); err == nil {
		t.Fatal("expected service INSERT on verification_queue to be denied")
	}
	if _, err := service.Exec(ctx,
		"UPDATE verification_queue SET status = 'resolved' WHERE outcome_id = $1", entry.OutcomeID,
	); err == nil {
		t.Fatal("expected service UPDATE on verification_queue to be denied")
	}
}

// The per-goal review settings have to survive the round trip, since they are
// what the loop reads back to decide whether to queue and whether to wait.
func TestGoalRegistryReviewSettings(t *testing.T) {
	ctx := context.Background()
	cfg := setup(t, ctx)
	p := pool(t, ctx, cfg.Postgres.OrchestratorDSN())
	goals := store.NewGoalRegistry(p)

	t.Run("defaults to the service threshold and non-blocking mode", func(t *testing.T) {
		goalID := seedGoal(t, ctx, p)
		got, err := goals.Get(ctx, goalID)
		if err != nil {
			t.Fatalf("get goal: %v", err)
		}
		if got.ConfidenceThreshold != nil {
			t.Fatalf("threshold = %v, want nil so the service default applies", *got.ConfidenceThreshold)
		}
		if got.EpochMode != store.EpochSpeculative {
			t.Fatalf("epoch mode = %q, want speculative", got.EpochMode)
		}
	})

	t.Run("persists an analyst's overrides", func(t *testing.T) {
		threshold := 0.55
		goalID := testutil.NewID(t)
		if err := goals.Insert(ctx, store.Goal{
			OptimizationFunctionID: goalID,
			GoalText:               "extract the effective dates",
			TargetFields:           []domain.TargetField{{Name: "effective_date"}},
			DataSourceRef:          "s3://arborette/contract.pdf",
			ConfidenceThreshold:    &threshold,
			EpochMode:              store.EpochBlocking,
		}); err != nil {
			t.Fatalf("insert goal: %v", err)
		}
		got, err := goals.Get(ctx, goalID)
		if err != nil {
			t.Fatalf("get goal: %v", err)
		}
		if got.ConfidenceThreshold == nil || *got.ConfidenceThreshold != threshold {
			t.Fatalf("threshold = %v, want %v", got.ConfidenceThreshold, threshold)
		}
		if got.EpochMode != store.EpochBlocking {
			t.Fatalf("epoch mode = %q, want blocking", got.EpochMode)
		}
	})
}
