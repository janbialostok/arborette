package orchestrator

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/arborette/arborette/internal/domain"
	"github.com/arborette/arborette/internal/llm"
	"github.com/arborette/arborette/internal/store"
)

// newDocumentLoopServer wires the collaborators a document run needs: a PDF to
// extract from, page text to locate values in, and a queue to route low
// confidence onto.
func newDocumentLoopServer(queue verificationQueue, repo *fakeRepo, claude *fakeClaude) *Server {
	sandbox := &fakeSandbox{docPages: []string{"Effective 2026-01-01"}}
	objects := &fakeObjects{getData: []byte("%PDF-1.4 fake")}
	return newTestServerQueue(repo, queue, &fakeGoals{}, &fakeAudits{}, objects, &fakeHeur{}, claude, sandbox)
}

func oneField() store.Goal {
	return documentGoal([]domain.TargetField{{Name: "effective_date"}})
}

func TestExtractionQueuesOnlyBelowTheThreshold(t *testing.T) {
	t.Run("a low-confidence extraction is routed for review", func(t *testing.T) {
		queue := newFakeQueue()
		repo := &fakeRepo{}
		// Below the 0.8 default and non-improving, so each root method writes one
		// triplet and stops.
		claude := &fakeClaude{extractValue: "2026-01-01", extractConfidence: 0.0}
		srv := newDocumentLoopServer(queue, repo, claude)

		srv.runLoop(context.Background(), oneField(), "run-doc")

		if len(queue.enqueued) != len(extractionMethods) {
			t.Fatalf("expected every low-confidence extraction queued, got %d of %d", len(queue.enqueued), len(extractionMethods))
		}
		entry := queue.enqueued[0]
		if entry.OutcomeID != repo.outcomes[0].ID {
			t.Fatalf("queued entry %q does not reference the outcome it reviews (%q)", entry.OutcomeID, repo.outcomes[0].ID)
		}
		if entry.ExtractedValue != "2026-01-01" || entry.Field != "effective_date" || entry.Confidence != 0.0 {
			t.Fatalf("queued entry did not snapshot the extraction: %+v", entry)
		}
		if entry.Provenance == nil {
			t.Fatalf("the located value's provenance must be snapshotted with it: %+v", entry)
		}
	})

	t.Run("an extraction exactly on the threshold is trusted, not queued", func(t *testing.T) {
		// The gate is `confidence >= threshold`, so the boundary itself decides
		// whether an analyst is asked to look at a result.
		queue := newFakeQueue()
		claude := &fakeClaude{extractValue: "2026-01-01", extractConfidence: testHITLThreshold}
		srv := newDocumentLoopServer(queue, &fakeRepo{}, claude)

		srv.runLoop(context.Background(), oneField(), "run-doc")

		if len(queue.enqueued) != 0 {
			t.Fatalf("confidence equal to the threshold clears it: %+v", queue.enqueued)
		}
	})

	t.Run("the goal's own threshold governs routing, not the service default", func(t *testing.T) {
		queue := newFakeQueue()
		// Above the 0.8 service default, below this goal's stricter 0.99.
		claude := &fakeClaude{extractValue: "2026-01-01", extractConfidence: 0.9}
		srv := newDocumentLoopServer(queue, &fakeRepo{}, claude)

		goal := oneField()
		stricter := 0.99
		goal.ConfidenceThreshold = &stricter
		srv.runLoop(context.Background(), goal, "run-doc")

		if len(queue.enqueued) == 0 {
			t.Fatal("the goal's stricter threshold must route this extraction for review")
		}
	})

	t.Run("a confident extraction proceeds unqueued", func(t *testing.T) {
		queue := newFakeQueue()
		claude := &fakeClaude{extractValue: "2026-01-01", extractConfidence: 0.95}
		srv := newDocumentLoopServer(queue, &fakeRepo{}, claude)

		srv.runLoop(context.Background(), oneField(), "run-doc")

		if len(queue.enqueued) != 0 {
			t.Fatalf("an above-threshold extraction must not be queued: %+v", queue.enqueued)
		}
	})

	t.Run("a queue failure does not cost the measurement", func(t *testing.T) {
		queue := newFakeQueue()
		queue.enqueueErr = context.DeadlineExceeded
		repo := &fakeRepo{}
		claude := &fakeClaude{extractValue: "2026-01-01", extractConfidence: 0.0}
		srv := newDocumentLoopServer(queue, repo, claude)

		srv.runLoop(context.Background(), oneField(), "run-doc")

		if len(repo.outcomes) != len(extractionMethods) {
			t.Fatalf("every extraction must still be persisted, got %d outcomes", len(repo.outcomes))
		}
	})
}

// Speculative mode is the default, and its whole point is that the loop never
// waits on a human.
func TestSpeculativeModeNeverWaitsOnReview(t *testing.T) {
	queue := newFakeQueue()
	// Improving confidence below the threshold, so a queued node would expand --
	// and would wait, were the goal in blocking mode.
	repo := &fakeRepo{}
	claude := &fakeClaude{extractValue: "2026-01-01", extractConfidence: 0.5}
	srv := newDocumentLoopServer(queue, repo, claude)

	srv.runLoop(context.Background(), oneField(), "run-doc")

	if len(queue.enqueued) == 0 {
		t.Fatal("this fixture is meant to queue; nothing was routed for review")
	}
	if repo.polls() != 0 {
		t.Fatalf("speculative mode must never poll for a verdict, polled %d times", repo.polls())
	}
}

func TestBlockingModeWaitsForTheVerdict(t *testing.T) {
	t.Run("a rejection prunes the branch", func(t *testing.T) {
		queue := newFakeQueue()
		repo := &fakeRepo{}
		claude := &fakeClaude{extractValue: "2026-01-01", extractConfidence: 0.5}
		srv := newDocumentLoopServer(queue, repo, claude)
		resolveWhenQueued(t, srv, queue, store.ResolutionRejected)

		goal := oneField()
		goal.EpochMode = store.EpochBlocking
		srv.runLoop(context.Background(), goal, "run-doc")

		// Each root method waits, is rejected, and stops: no refinement level runs.
		if claude.extractCalls != len(extractionMethods) {
			t.Fatalf("extract calls = %d, want %d -- a rejected extraction must not be refined",
				claude.extractCalls, len(extractionMethods))
		}
		if repo.polls() == 0 {
			t.Fatal("blocking mode must poll the outcome for a landed verdict")
		}
	})

	t.Run("a confirmation lets the branch expand", func(t *testing.T) {
		queue := newFakeQueue()
		claude := &fakeClaude{extractValue: "2026-01-01", extractConfidence: 0.5}
		srv := newDocumentLoopServer(queue, &fakeRepo{}, claude)
		resolveWhenQueued(t, srv, queue, store.ResolutionConfirmed)

		goal := oneField()
		goal.EpochMode = store.EpochBlocking
		srv.runLoop(context.Background(), goal, "run-doc")

		if claude.extractCalls <= len(extractionMethods) {
			t.Fatalf("extract calls = %d, want more than the %d root methods -- a confirmed extraction refines",
				claude.extractCalls, len(extractionMethods))
		}
	})

	t.Run("a correction lets the branch expand, like a confirmation", func(t *testing.T) {
		// Correcting is the outcome human effort is most invested in; pruning it
		// would discard the analyst's better value.
		queue := newFakeQueue()
		claude := &fakeClaude{extractValue: "2026-01-01", extractConfidence: 0.5}
		srv := newDocumentLoopServer(queue, &fakeRepo{}, claude)
		resolveWhenQueued(t, srv, queue, store.ResolutionCorrected)

		goal := oneField()
		goal.EpochMode = store.EpochBlocking
		srv.runLoop(context.Background(), goal, "run-doc")

		if claude.extractCalls <= len(extractionMethods) {
			t.Fatalf("extract calls = %d, want more than the %d root methods -- a corrected extraction refines",
				claude.extractCalls, len(extractionMethods))
		}
	})

	t.Run("an above-threshold extraction never waits", func(t *testing.T) {
		// Blocking mode gates on review, and a result that needs no review has
		// nothing to wait for -- it must refine immediately.
		queue := newFakeQueue()
		claude := &fakeClaude{extractValue: "2026-01-01", extractConfidence: 0.95}
		srv := newDocumentLoopServer(queue, &fakeRepo{}, claude)

		goal := oneField()
		goal.EpochMode = store.EpochBlocking
		srv.runLoop(context.Background(), goal, "run-doc")

		if claude.extractCalls <= len(extractionMethods) {
			t.Fatalf("extract calls = %d, want expansion without any wait", claude.extractCalls)
		}
	})

	t.Run("an unqueueable extraction fails the branch rather than degrading", func(t *testing.T) {
		// Speculative mode shrugs off a queue failure; blocking mode cannot, or it
		// would refine below an extraction no human will ever see.
		queue := newFakeQueue()
		queue.enqueueErr = errors.New("queue unreachable")
		audits := &fakeAudits{}
		claude := &fakeClaude{extractValue: "2026-01-01", extractConfidence: 0.5}
		srv := newDocumentLoopServer(queue, &fakeRepo{}, claude)
		srv.audits = audits

		goal := oneField()
		goal.EpochMode = store.EpochBlocking
		srv.runLoop(context.Background(), goal, "run-doc")

		if claude.extractCalls != len(extractionMethods) {
			t.Fatalf("extract calls = %d, want no expansion past the %d root methods",
				claude.extractCalls, len(extractionMethods))
		}
		found := false
		for _, r := range audits.records {
			if r.Action == "hypothesis_branch_failure" {
				found = true
			}
		}
		if !found {
			t.Fatalf("the degradation must be recorded, not silent: %+v", audits.records)
		}
	})

	t.Run("a cancelled run ends the wait as a branch failure", func(t *testing.T) {
		queue := newFakeQueue()
		claude := &fakeClaude{extractValue: "2026-01-01", extractConfidence: 0.5}
		srv := newDocumentLoopServer(queue, &fakeRepo{}, claude)

		goal := oneField()
		goal.EpochMode = store.EpochBlocking
		// Subscribe first: the branch failure is observed on the stream, which an
		// expired run context cannot suppress the way it suppresses an audit write.
		_, ch, cancelSub := srv.hub.Subscribe("doc-goal")
		defer cancelSub()

		// Nothing ever resolves, so the wait runs until the run's deadline.
		ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
		defer cancel()
		srv.runLoop(ctx, goal, "run-doc")

		found := false
		for ev := range ch {
			if ev.Type == "branch_failure" {
				found = true
			}
		}
		if !found {
			t.Fatal("an expired wait must end the branch rather than hang")
		}
	})
}

// resolveWhenQueued stands in for an analyst: it watches the queue and stamps
// the given verdict on whatever the loop routes for review, so the blocking gate
// sees a verdict arrive while it polls.
func resolveWhenQueued(t *testing.T, srv *Server, queue *fakeQueue, resolution store.QueueResolution) {
	t.Helper()
	done := make(chan struct{})
	t.Cleanup(func() { close(done) })
	go func() {
		for {
			select {
			case <-done:
				return
			default:
			}
			for _, id := range queue.pendingOutcomes() {
				// Mirror the resolution handler: claim the row, then write the
				// verdict through to the graph, which is what the gate watches.
				_ = queue.Resolve(context.Background(), id, resolution, "")
				_ = srv.repo.UpdateOutcomeVerification(context.Background(), id,
					verificationStatusFor(resolution), verifiedConfidence)
			}
			time.Sleep(time.Millisecond)
		}
	}()
}

// The live view has to reflect the whole run, so both goal kinds publish a
// distribution as their triplets land.
func TestLoopPublishesTheConfidenceDistribution(t *testing.T) {
	queue := newFakeQueue()
	claude := &fakeClaude{extractValue: "2026-01-01", extractConfidence: 0.0}
	srv := newDocumentLoopServer(queue, &fakeRepo{}, claude)

	// Subscribe before the run so the frames are observed live rather than by
	// replay, which Complete would have discarded.
	_, ch, cancel := srv.hub.Subscribe("doc-goal")
	defer cancel()

	srv.runLoop(context.Background(), oneField(), "run-doc")

	distributions := 0
	for ev := range ch {
		if ev.Type == "confidence_distribution" {
			distributions++
			bins := ev.Payload["bins"].([]int)
			if bins[0] == 0 {
				t.Fatalf("a zero-confidence extraction belongs in the first bin: %v", bins)
			}
		}
	}
	if distributions != len(extractionMethods) {
		t.Fatalf("distribution frames = %d, want one per triplet (%d)", distributions, len(extractionMethods))
	}
}

// A tabular run has no extractions at all, but its measurements still belong in
// the run's distribution -- otherwise the live view is empty for half the goal
// kinds the service supports.
func TestQueryLoopPublishesTheConfidenceDistribution(t *testing.T) {
	repo := &fakeRepo{}
	claude := &fakeClaude{proposal: llm.Proposal{Candidates: []llm.CandidateIntervention{{Filters: nil}}}}
	sandbox := &fakeSandbox{
		introspect: IntrospectResponse{Schema: schemaDTO{Columns: []columnDTO{{Name: "revenue", Type: "DOUBLE"}}}},
		execResps: []ExecuteResponse{
			{Value: map[string]any{"avg(revenue)": 10.0}}, // root baseline
			{Value: map[string]any{"avg(revenue)": 8.0}},  // candidate, no improvement
		},
	}
	srv := newTestServerQueue(repo, newFakeQueue(), &fakeGoals{}, &fakeAudits{},
		&fakeObjects{}, &fakeHeur{}, claude, sandbox)

	_, ch, cancel := srv.hub.Subscribe("goal-tabular")
	defer cancel()

	srv.runLoop(context.Background(), store.Goal{
		OptimizationFunctionID: "goal-tabular",
		EvaluationMatrix: domain.EvaluationMatrix{Targets: []domain.Target{
			{Field: "revenue", Direction: domain.Maximize, Aggregation: "avg"},
		}},
	}, "run-tabular")

	distributions := 0
	for ev := range ch {
		if ev.Type == "confidence_distribution" {
			distributions++
			// A query measurement is verified by construction, so it belongs in the
			// top bucket.
			if bins := ev.Payload["bins"].([]int); bins[confidenceBinCount-1] == 0 {
				t.Fatalf("a query triplet belongs in the top bin: %v", bins)
			}
		}
	}
	if distributions == 0 {
		t.Fatal("a tabular run must publish its confidence distribution too")
	}
}

// A run that waits on a human needs a human-scale deadline; bounding it by the
// machine-paced default would fail every blocking run mid-review.
func TestLoopTimeoutForBlockingGoals(t *testing.T) {
	srv := newDocumentLoopServer(newFakeQueue(), &fakeRepo{}, &fakeClaude{})

	if got := srv.loopTimeoutFor(oneField()); got != loopTimeout {
		t.Fatalf("speculative timeout = %v, want the standard %v", got, loopTimeout)
	}
	blocking := oneField()
	blocking.EpochMode = store.EpochBlocking
	if got := srv.loopTimeoutFor(blocking); got != srv.blockingLoopTimeout {
		t.Fatalf("blocking timeout = %v, want the human-scale %v", got, srv.blockingLoopTimeout)
	}
}

// A day-long wait issues tens of thousands of reads, so one transient graph
// fault must not discard the branch a human is in the middle of reviewing.
func TestBlockingGateRetriesATransientVerdictRead(t *testing.T) {
	queue := newFakeQueue()
	repo := &fakeRepo{extractionReadErrs: 3}
	claude := &fakeClaude{extractValue: "2026-01-01", extractConfidence: 0.5}
	srv := newDocumentLoopServer(queue, repo, claude)
	resolveWhenQueued(t, srv, queue, store.ResolutionConfirmed)

	goal := oneField()
	goal.EpochMode = store.EpochBlocking
	srv.runLoop(context.Background(), goal, "run-doc")

	if repo.polls() <= 3 {
		t.Fatalf("the gate polled %d times; it must have retried past the transient failures", repo.polls())
	}
	if claude.extractCalls <= len(extractionMethods) {
		t.Fatalf("extract calls = %d, want the branch to have survived and refined", claude.extractCalls)
	}
}

// An expired blocking wait is the one event that explains why a run stopped, so
// its record must not be lost to the very deadline that caused it.
func TestExpiredBlockingWaitIsAudited(t *testing.T) {
	queue := newFakeQueue()
	audits := &fakeAudits{}
	claude := &fakeClaude{extractValue: "2026-01-01", extractConfidence: 0.5}
	srv := newDocumentLoopServer(queue, &fakeRepo{}, claude)
	srv.audits = audits

	goal := oneField()
	goal.EpochMode = store.EpochBlocking
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	srv.runLoop(ctx, goal, "run-doc")

	found := false
	for _, r := range audits.records {
		if r.Action == "hypothesis_branch_failure" {
			found = true
		}
	}
	if !found {
		t.Fatalf("the branch failure must be recorded despite the dead context: %+v", audits.records)
	}
}
