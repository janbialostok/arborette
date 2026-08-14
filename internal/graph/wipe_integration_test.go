package graph_test

import (
	"context"
	"testing"

	"github.com/arborette/arborette/internal/testutil"
)

// TestWipeDeletesEveryNode seeds a few nodes, confirms DeleteNode removes only
// the named node and Wipe then empties the whole graph, and that deleting an
// absent id and wiping an empty graph are both idempotent no-ops. All but the
// final wipe run against the current graph's residual state, so assertions are
// relative (counts grow by what this test created), never absolute.
func TestWipeDeletesEveryNode(t *testing.T) {
	ctx := context.Background()
	repo := newRepo(t, ctx)

	baseline, err := repo.CountNodes(ctx)
	if err != nil {
		t.Fatalf("count baseline: %v", err)
	}
	stateID, interventionID, outcomeID := seedTriplet(t, ctx, repo)

	// DeleteNode removes exactly one of the fixture nodes.
	removed, err := repo.DeleteNode(ctx, outcomeID)
	if err != nil {
		t.Fatalf("delete node: %v", err)
	}
	if removed != 1 {
		t.Fatalf("DeleteNode removed %d nodes for an existing id, want 1", removed)
	}

	// Deleting an absent id is a no-op, not an error.
	removed, err = repo.DeleteNode(ctx, testutil.NewID(t))
	if err != nil {
		t.Fatalf("delete absent node: %v", err)
	}
	if removed != 0 {
		t.Fatalf("DeleteNode removed %d nodes for an absent id, want 0", removed)
	}

	got, err := repo.CountNodes(ctx)
	if err != nil {
		t.Fatalf("count after delete: %v", err)
	}
	if want := baseline + 2; got != want {
		t.Fatalf("count after deleting one of three nodes = %d, want %d", got, want)
	}

	// Wipe empties the whole graph, returning how many nodes it removed.
	wiped, err := repo.Wipe(ctx)
	if err != nil {
		t.Fatalf("wipe: %v", err)
	}
	if want := baseline + 2; wiped != want {
		t.Fatalf("wipe removed %d nodes, want %d (the state and intervention remaining)", wiped, want)
	}
	if got, err := repo.CountNodes(ctx); err != nil || got != 0 {
		t.Fatalf("count after wipe = %d (err %v), want 0", got, err)
	}

	// Wiping an empty graph is a no-op and succeeds.
	wipedAgain, err := repo.Wipe(ctx)
	if err != nil {
		t.Fatalf("second wipe: %v", err)
	}
	if wipedAgain != 0 {
		t.Fatalf("second wipe removed %d nodes, want 0", wipedAgain)
	}

	// Sanity-check the fixture nodes themselves are gone (their ids mapped the
	// whole graph, so this doubles as the graph really being empty).
	for _, id := range []string{stateID, interventionID} {
		removed, err := repo.DeleteNode(ctx, id)
		if err != nil {
			t.Fatalf("delete fixture node %s: %v", id, err)
		}
		if removed != 0 {
			t.Fatalf("fixture node %s still present after wipe (deleted %d)", id, removed)
		}
	}
}