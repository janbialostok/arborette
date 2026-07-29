package graph_test

import (
	"context"
	"errors"
	"testing"

	"github.com/arborette/arborette/internal/domain"
	"github.com/arborette/arborette/internal/graph"
	"github.com/arborette/arborette/internal/testutil"
)

// seedExtractionTriplet creates the document-path analog of seedTriplet: an
// extract-type intervention producing an unverified outcome, with the field and
// method the review surface reads back off the intervention.
func seedExtractionTriplet(t *testing.T, ctx context.Context, repo *graph.Neo4jRepository, goalID, field, value string, confidence float64) string {
	t.Helper()
	stateID, interventionID, outcomeID := testutil.NewID(t), testutil.NewID(t), testutil.NewID(t)

	if err := repo.CreateState(ctx, domain.State{ID: stateID, GoalID: goalID, Properties: map[string]any{"field": field}}); err != nil {
		t.Fatalf("create state: %v", err)
	}
	if err := repo.CreateIntervention(ctx, domain.Intervention{
		ID: interventionID, GoalID: goalID, Type: domain.InterventionExtract,
		Properties: map[string]any{"field": field, "method": "ocr"},
	}); err != nil {
		t.Fatalf("create intervention: %v", err)
	}
	if err := repo.CreateOutcome(ctx, domain.Outcome{
		ID: outcomeID, GoalID: goalID, VerificationStatus: domain.VerificationUnverified,
		Value:      map[string]any{field: value},
		Provenance: &domain.ProvenanceLocator{Page: 1, CharStart: 4, CharEnd: 9},
	}); err != nil {
		t.Fatalf("create outcome: %v", err)
	}
	if err := repo.CreatePreConditionFor(ctx, stateID, interventionID); err != nil {
		t.Fatalf("create pre_condition_for: %v", err)
	}
	if err := repo.CreateProduced(ctx, interventionID, outcomeID, domain.ProducedEdge{
		EffectSize: confidence, Confidence: confidence,
	}); err != nil {
		t.Fatalf("create produced: %v", err)
	}
	return outcomeID
}

// The review surface needs what a bare node read cannot give it: the producing
// intervention's field and method, and the confidence carried on the edge.
func TestGetExtractionOutcome(t *testing.T) {
	ctx := context.Background()
	repo := newRepo(t, ctx)
	goalID := testutil.NewID(t)
	outcomeID := seedExtractionTriplet(t, ctx, repo, goalID, "effective_date", "2026-01-01", 0.42)

	got, err := repo.GetExtractionOutcome(ctx, outcomeID)
	if err != nil {
		t.Fatalf("get extraction outcome: %v", err)
	}
	if got.OutcomeID != outcomeID || got.GoalID != goalID {
		t.Fatalf("outcome identity = %+v, want %q scoped to %q", got, outcomeID, goalID)
	}
	if got.Field != "effective_date" || got.Method != "ocr" {
		t.Fatalf("field/method must come from the producing intervention: %+v", got)
	}
	if got.Confidence != 0.42 {
		t.Fatalf("confidence = %v, want the PRODUCED edge's 0.42", got.Confidence)
	}
	if got.Value["effective_date"] != "2026-01-01" {
		t.Fatalf("value = %+v, want the extracted date keyed by field", got.Value)
	}
	if got.Provenance == nil || got.Provenance.Page != 1 {
		t.Fatalf("locator = %+v, want the seeded one", got.Provenance)
	}
	if got.VerificationStatus != domain.VerificationUnverified {
		t.Fatalf("status = %q, want unverified", got.VerificationStatus)
	}
}

// A query outcome is verified by construction and never human-resolvable, so the
// review surface must not be able to reach one.
func TestGetExtractionOutcomeRejectsNonExtractions(t *testing.T) {
	ctx := context.Background()
	repo := newRepo(t, ctx)
	_, _, queryOutcomeID := seedTriplet(t, ctx, repo)

	if _, err := repo.GetExtractionOutcome(ctx, queryOutcomeID); !errors.Is(err, graph.ErrNotFound) {
		t.Fatalf("error = %v, want ErrNotFound for a query outcome", err)
	}
	if _, err := repo.GetExtractionOutcome(ctx, testutil.NewID(t)); !errors.Is(err, graph.ErrNotFound) {
		t.Fatalf("error = %v, want ErrNotFound for an unknown id", err)
	}
}

func TestListExtractionOutcomesIsScopedToTheGoal(t *testing.T) {
	ctx := context.Background()
	repo := newRepo(t, ctx)
	goalID, otherGoalID := testutil.NewID(t), testutil.NewID(t)

	first := seedExtractionTriplet(t, ctx, repo, goalID, "effective_date", "2026-01-01", 0.42)
	second := seedExtractionTriplet(t, ctx, repo, goalID, "invoice_total", "42.00", 0.91)
	seedExtractionTriplet(t, ctx, repo, otherGoalID, "effective_date", "2020-01-01", 0.5)
	// A query outcome on the same goal is not an extraction and must not appear.
	seedTriplet(t, ctx, repo)

	outcomes, err := repo.ListExtractionOutcomes(ctx, goalID)
	if err != nil {
		t.Fatalf("list extraction outcomes: %v", err)
	}
	if len(outcomes) != 2 {
		t.Fatalf("expected exactly this goal's two extractions, got %d: %+v", len(outcomes), outcomes)
	}
	byID := map[string]graph.ExtractionOutcome{}
	for _, o := range outcomes {
		byID[o.OutcomeID] = o
	}
	if byID[first].Confidence != 0.42 || byID[second].Confidence != 0.91 {
		t.Fatalf("each outcome must carry its own edge confidence: %+v", outcomes)
	}
}

// A correction replaces the value, the locator recomputed against it, and the
// verdict -- in one write -- and leaves the outcome's identity alone.
func TestCorrectOutcome(t *testing.T) {
	ctx := context.Background()
	repo := newRepo(t, ctx)
	goalID := testutil.NewID(t)
	outcomeID := seedExtractionTriplet(t, ctx, repo, goalID, "effective_date", "2026-01-01", 0.42)

	corrected := &domain.ProvenanceLocator{Page: 0, CharStart: 1, CharEnd: 11}
	if err := repo.CorrectOutcome(ctx, outcomeID, map[string]any{"effective_date": "2026-02-01"}, corrected,
		domain.VerificationCorrected, 1.0); err != nil {
		t.Fatalf("correct outcome: %v", err)
	}
	got, err := repo.GetExtractionOutcome(ctx, outcomeID)
	if err != nil {
		t.Fatalf("get extraction outcome: %v", err)
	}
	if got.Value["effective_date"] != "2026-02-01" {
		t.Fatalf("value = %+v, want the corrected date", got.Value)
	}
	if got.Provenance == nil || *got.Provenance != *corrected {
		t.Fatalf("locator = %+v, want %+v", got.Provenance, corrected)
	}
	if got.GoalID != goalID || got.Field != "effective_date" {
		t.Fatalf("a correction must not disturb the outcome's identity: %+v", got)
	}
	// Value, locator, status, and edge confidence move together, so a reader can
	// never see a corrected value that no verdict backs.
	if got.VerificationStatus != domain.VerificationCorrected || got.Confidence != 1.0 {
		t.Fatalf("status/confidence = %q/%v, want corrected at 1.0 in the same write", got.VerificationStatus, got.Confidence)
	}

	// A value the analyst reformatted has no exact match in the source, so the
	// correction nulls the locator and review falls back to the whole document.
	if err := repo.CorrectOutcome(ctx, outcomeID, map[string]any{"effective_date": "Feb 1, 2026"}, nil,
		domain.VerificationCorrected, 1.0); err != nil {
		t.Fatalf("correct outcome without a locator: %v", err)
	}
	got, err = repo.GetExtractionOutcome(ctx, outcomeID)
	if err != nil {
		t.Fatalf("get extraction outcome: %v", err)
	}
	if got.Provenance != nil {
		t.Fatalf("locator = %+v, want it cleared", got.Provenance)
	}
}
