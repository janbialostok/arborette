package graph_test

import (
	"context"
	"testing"

	"github.com/arborette/arborette/internal/domain"
	"github.com/arborette/arborette/internal/testutil"
)

// TestCausalGraphRoundTrip verifies the commit protocol end to end against a live
// Neo4j: a graph reads as absent until its meta commits (edges-without-meta is a torn
// write), then GetCausalGraph returns exactly what the upserts wrote; a re-run at the
// same version 1 is idempotent; and DeleteCausalGraphVersion clears it.
func TestCausalGraphRoundTrip(t *testing.T) {
	ctx := context.Background()
	repo := newRepo(t, ctx)

	goalID := testutil.NewID(t)
	ref := "causal/" + testutil.NewID(t)
	columns := []domain.DataColumn{
		{GoalID: goalID, DatasourceRef: ref, Name: "X", Kind: "numeric"},
		{GoalID: goalID, DatasourceRef: ref, Name: "Y", Kind: "numeric"},
	}
	edges := []domain.CausalEdge{{
		GoalID: goalID, DatasourceRef: ref, Version: 1,
		ColA: "X", ColB: "Y", Direction: domain.DirectionAToB,
		Provenance: domain.ProvenanceStatistical, Confidence: 1.0, Status: domain.EdgeTested,
	}}

	if err := repo.UpsertDataColumns(ctx, columns); err != nil {
		t.Fatalf("upsert columns: %v", err)
	}
	if err := repo.UpsertCausalEdges(ctx, edges); err != nil {
		t.Fatalf("upsert edges: %v", err)
	}

	// Edges without meta must read as absent — the commit marker has not landed.
	if _, found, err := repo.GetCausalGraph(ctx, goalID, ref); err != nil {
		t.Fatalf("get before meta: %v", err)
	} else if found {
		t.Fatalf("graph read as present before its meta committed")
	}

	meta := domain.CausalGraphMeta{
		GoalID: goalID, DatasourceRef: ref, Version: 1,
		ExcludedColumns: []string{}, TestCount: 3, DiscoveredAt: "2026-08-03T00:00:00Z",
		Tuning: map[string]any{"alpha": 0.05},
	}
	if err := repo.UpsertCausalGraphMeta(ctx, meta); err != nil {
		t.Fatalf("upsert meta: %v", err)
	}

	graph, found, err := repo.GetCausalGraph(ctx, goalID, ref)
	if err != nil || !found {
		t.Fatalf("get after meta: found=%v err=%v", found, err)
	}
	if len(graph.Columns) != 2 || len(graph.Edges) != 1 {
		t.Fatalf("graph = %d columns, %d edges; want 2, 1", len(graph.Columns), len(graph.Edges))
	}
	e := graph.Edges[0]
	if e.ColA != "X" || e.ColB != "Y" || e.Direction != domain.DirectionAToB ||
		e.Provenance != domain.ProvenanceStatistical || e.Status != domain.EdgeTested {
		t.Fatalf("edge round-tripped wrong: %+v", e)
	}
	if graph.Meta.Version != 1 || graph.Meta.TestCount != 3 {
		t.Fatalf("meta round-tripped wrong: %+v", graph.Meta)
	}

	// Idempotent re-run at version 1 must not duplicate nodes or edges.
	if err := repo.UpsertDataColumns(ctx, columns); err != nil {
		t.Fatalf("re-upsert columns: %v", err)
	}
	if err := repo.UpsertCausalEdges(ctx, edges); err != nil {
		t.Fatalf("re-upsert edges: %v", err)
	}
	if err := repo.UpsertCausalGraphMeta(ctx, meta); err != nil {
		t.Fatalf("re-upsert meta: %v", err)
	}
	graph2, _, err := repo.GetCausalGraph(ctx, goalID, ref)
	if err != nil {
		t.Fatalf("get after re-run: %v", err)
	}
	if len(graph2.Columns) != 2 || len(graph2.Edges) != 1 {
		t.Fatalf("re-run duplicated: %d columns, %d edges", len(graph2.Columns), len(graph2.Edges))
	}

	if err := repo.DeleteCausalGraphVersion(ctx, goalID, ref, 1); err != nil {
		t.Fatalf("delete version: %v", err)
	}
	if _, found, err := repo.GetCausalGraph(ctx, goalID, ref); err != nil {
		t.Fatalf("get after delete: %v", err)
	} else if found {
		t.Fatalf("graph still present after version delete")
	}
}
