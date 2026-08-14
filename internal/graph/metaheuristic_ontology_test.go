package graph_test

import (
	"context"
	"testing"

	"github.com/arborette/arborette/internal/domain"
	"github.com/arborette/arborette/internal/testutil"
)

// TestMetaHeuristicOntologyRoundTrip pins the abstraction provenance a knowledge
// reuse path reads: the term map and the origin persist, survive a re-link that
// carries neither, and decode on every read path.
//
// The survival half is the load-bearing one. The re-link branch re-issues the write
// with only the definition it read back, so an unguarded match-side SET would blank
// the terms every time a later run widened an existing heuristic's evidence —
// leaving a corpus that silently degrades to the pre-persistence state.
func TestMetaHeuristicOntologyRoundTrip(t *testing.T) {
	ctx := context.Background()
	repo := newRepo(t, ctx)
	goalID, otherGoal := testutil.NewID(t), testutil.NewID(t)
	mhID := testutil.NewID(t)
	cfg := testutil.RequireIntegration(t)
	testutil.RegisterGraphNodeCleanup(t, ctx, cfg, mhID)

	terms := []domain.OntologyTerm{
		{Concrete: "HomePlanet", Ontological: "[Primary Population Center]"},
		{Concrete: "Transported", Ontological: "[System Output]"},
	}
	written := domain.MetaHeuristic{
		ID:                  mhID,
		Definition:          "[Primary Population Center] raises [System Output]",
		GoalID:              goalID,
		OntologyTerms:       terms,
		OriginGoalID:        goalID,
		OriginDataSourceRef: "ref.csv",
	}
	if err := repo.CreateMetaHeuristic(ctx, written, nil); err != nil {
		t.Fatalf("create meta-heuristic: %v", err)
	}

	got, err := repo.GetMetaHeuristic(ctx, mhID)
	if err != nil {
		t.Fatalf("get meta-heuristic: %v", err)
	}
	assertProvenance(t, "GetMetaHeuristic", got, written)

	batch, err := repo.GetMetaHeuristics(ctx, []string{mhID})
	if err != nil {
		t.Fatalf("get meta-heuristics: %v", err)
	}
	if len(batch) != 1 {
		t.Fatalf("batch read returned %d nodes, want 1", len(batch))
	}
	assertProvenance(t, "GetMetaHeuristics", batch[0], written)

	listed, err := repo.ListMetaHeuristics(ctx)
	if err != nil {
		t.Fatalf("list meta-heuristics: %v", err)
	}
	found := false
	for _, mh := range listed {
		if mh.ID != mhID {
			continue
		}
		found = true
		assertProvenance(t, "ListMetaHeuristics", mh, written)
	}
	if !found {
		t.Fatalf("the written heuristic is absent from the list read")
	}

	// A second run re-linking the node under a different goal, carrying no terms and
	// no origin — the exact shape the abstraction resume path produces.
	relink := domain.MetaHeuristic{ID: mhID, Definition: written.Definition, GoalID: otherGoal}
	if err := repo.CreateMetaHeuristic(ctx, relink, nil); err != nil {
		t.Fatalf("re-link meta-heuristic: %v", err)
	}
	after, err := repo.GetMetaHeuristic(ctx, mhID)
	if err != nil {
		t.Fatalf("get after re-link: %v", err)
	}
	assertProvenance(t, "after a term-less re-link", after, written)
}

func assertProvenance(t *testing.T, path string, got, want domain.MetaHeuristic) {
	t.Helper()
	if len(got.OntologyTerms) != len(want.OntologyTerms) {
		t.Fatalf("%s: ontology terms = %v, want %v", path, got.OntologyTerms, want.OntologyTerms)
	}
	for i, term := range want.OntologyTerms {
		if got.OntologyTerms[i] != term {
			t.Fatalf("%s: ontology term %d = %+v, want %+v", path, i, got.OntologyTerms[i], term)
		}
	}
	if got.OriginGoalID != want.OriginGoalID || got.OriginDataSourceRef != want.OriginDataSourceRef {
		t.Fatalf("%s: origin = (%q, %q), want (%q, %q)",
			path, got.OriginGoalID, got.OriginDataSourceRef, want.OriginGoalID, want.OriginDataSourceRef)
	}
}

// TestAbstractionSourceFiltersWalksInterventionsOnly pins the same-dataset reuse
// read: it recovers the conjunctions a heuristic generalizes and nothing else, so a
// State or Outcome in the same evidence set cannot be mistaken for a segment.
func TestAbstractionSourceFiltersWalksInterventionsOnly(t *testing.T) {
	ctx := context.Background()
	repo := newRepo(t, ctx)
	goalID := testutil.NewID(t)
	stateID, interventionID, outcomeID := seedGoalTriplet(t, ctx, repo, goalID, domain.VerificationVerified, false)

	mhID := testutil.NewID(t)
	cfg := testutil.RequireIntegration(t)
	testutil.RegisterGraphNodeCleanup(t, ctx, cfg, mhID)
	mh := domain.MetaHeuristic{ID: mhID, Definition: "[Volume] raises [System Output]", GoalID: goalID}
	if err := repo.CreateMetaHeuristic(ctx, mh, []string{stateID, interventionID, outcomeID}); err != nil {
		t.Fatalf("create meta-heuristic: %v", err)
	}

	conjunctions, err := repo.AbstractionSourceFilters(ctx, mhID)
	if err != nil {
		t.Fatalf("abstraction source filters: %v", err)
	}
	if len(conjunctions) != 1 {
		t.Fatalf("conjunctions = %v, want exactly the one Intervention's filters", conjunctions)
	}
	if len(conjunctions[0]) != 1 || conjunctions[0][0].Field != "qty" || conjunctions[0][0].Op != domain.GreaterThan {
		t.Fatalf("recovered conjunction = %+v, want the seeded qty > 1 predicate", conjunctions[0])
	}
}

// TestAbstractionSourceFiltersIsEmptyForAnUnknownHeuristic: an id with no node is a
// heuristic that offers no conjunctions, not a failure — the caller falls through to
// the grounding path.
func TestAbstractionSourceFiltersIsEmptyForAnUnknownHeuristic(t *testing.T) {
	ctx := context.Background()
	repo := newRepo(t, ctx)

	conjunctions, err := repo.AbstractionSourceFilters(ctx, testutil.NewID(t))
	if err != nil {
		t.Fatalf("abstraction source filters: %v", err)
	}
	if len(conjunctions) != 0 {
		t.Fatalf("conjunctions = %v, want none", conjunctions)
	}
}
