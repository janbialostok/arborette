package graph_test

import (
	"context"
	"errors"
	"testing"

	domain "github.com/arborette/arborette/internal/domain"
	"github.com/arborette/arborette/internal/graph"
	"github.com/arborette/arborette/internal/testutil"
)

// TestDeleteMetaHeuristicLive covers the graph side of heuristic removal against
// a live Neo4j (US5): a seeded Meta-Heuristic is visible to search-trace before
// deletion, and after DeleteMetaHeuristic it is gone from every read -- the node,
// its trace chain, the batch-hydration result (the "search neighborhood touches a
// retired node" case), and the goal-scoped list -- while a heuristic abstracted
// from the deleted one keeps its node but loses the ABSTRACTED_FROM edge, so it
// no longer chains to the triplet either. Deleting an id that matches nothing is
// not an error. Per-row assertions only: the shared database holds other nodes.
func TestDeleteMetaHeuristicLive(t *testing.T) {
	ctx := context.Background()
	repo := newRepo(t, ctx)
	_, interventionID, outcomeID := seedTriplet(t, ctx, repo)

	removedID, derivedID := testutil.NewID(t), testutil.NewID(t)
	cfg := testutil.RequireIntegration(t)
	testutil.RegisterGraphNodeCleanup(t, ctx, cfg, removedID)
	testutil.RegisterGraphNodeCleanup(t, ctx, cfg, derivedID)
	if err := repo.CreateMetaHeuristic(ctx,
		domain.MetaHeuristic{ID: removedID, Definition: "to be deleted"},
		[]string{interventionID, outcomeID},
	); err != nil {
		t.Fatalf("create heuristic under test: %v", err)
	}

	// Search-trace sees the heuristic before the delete.
	if triplets, err := repo.TraceCausalChain(ctx, removedID); err != nil || len(triplets) != 1 {
		t.Fatalf("trace before delete = %d triplet(s) err=%v, want 1", len(triplets), err)
	}

	// A second heuristic abstracted from the first exercises the DETACH DELETE
	// edge cleanup: its node must survive the delete with its provenance gone.
	if err := repo.CreateMetaHeuristic(ctx,
		domain.MetaHeuristic{ID: derivedID, Definition: "derived from the deleted one"},
		[]string{removedID},
	); err != nil {
		t.Fatalf("create derived heuristic: %v", err)
	}

	if err := repo.DeleteMetaHeuristic(ctx, removedID); err != nil {
		t.Fatalf("delete heuristic: %v", err)
	}

	// Direct read: the node is gone.
	if _, err := repo.GetMetaHeuristic(ctx, removedID); !errors.Is(err, graph.ErrNotFound) {
		t.Fatalf("get deleted heuristic: err=%v, want graph.ErrNotFound", err)
	}
	// Trace: no node to walk, so no triplets.
	if triplets, err := repo.TraceCausalChain(ctx, removedID); err != nil || len(triplets) != 0 {
		t.Fatalf("trace after delete = %d triplet(s) err=%v, want 0", len(triplets), err)
	}
	// Batch hydration (the search neighbor read): the deleted id is absent, the
	// derived one still answers.
	hydrated, err := repo.GetMetaHeuristics(ctx, []string{removedID, derivedID})
	if err != nil {
		t.Fatalf("batch hydration: %v", err)
	}
	if containsID(hydrated, removedID) || !containsID(hydrated, derivedID) {
		t.Fatalf("batch after delete = %+v, want derived present and removed absent", hydrated)
	}
	// Goal-scoped list: the removed id is absent.
	all, err := repo.ListMetaHeuristics(ctx)
	if err != nil {
		t.Fatalf("list meta-heuristics: %v", err)
	}
	if goalOfID(all, removedID) != "" {
		t.Fatalf("deleted heuristic still listed: %+v", all)
	}
	// The derived node survives but its ABSTRACTED_FROM edge to the deleted
	// heuristic went with it, so it no longer chains to the triplet.
	if got, err := repo.GetMetaHeuristic(ctx, derivedID); err != nil || got.ID != derivedID {
		t.Fatalf("derived heuristic vanished with its source: node=%+v err=%v", got, err)
	}
	if triplets, err := repo.TraceCausalChain(ctx, derivedID); err != nil || len(triplets) != 0 {
		t.Fatalf("derived trace = %d triplet(s) err=%v, want 0 (its ABSTRACTED_FROM edge was DETACH deleted)", len(triplets), err)
	}

	// Deleting an absent id is idempotent, not an error.
	if err := repo.DeleteMetaHeuristic(ctx, removedID); err != nil {
		t.Fatalf("re-delete absent heuristic: %v", err)
	}
	if err := repo.DeleteMetaHeuristic(ctx, testutil.NewID(t)); err != nil {
		t.Fatalf("delete never-created heuristic: %v", err)
	}
}