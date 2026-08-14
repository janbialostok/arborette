package graph_test

import (
	"context"
	"errors"
	"testing"

	"github.com/arborette/arborette/internal/domain"
	"github.com/arborette/arborette/internal/graph"
	"github.com/arborette/arborette/internal/testutil"
)

// seedGraph commits a three-edge graph at version 1 for a fresh (goal, data-source)
// pair: X→Y, Z→Y, and an undirected A—B. It returns the pair.
func seedGraph(t *testing.T, ctx context.Context, repo *graph.Neo4jRepository) (goalID, ref string) {
	t.Helper()
	goalID = testutil.NewID(t)
	ref = "correction/" + testutil.NewID(t)
	cfg := testutil.RequireIntegration(t)
	testutil.RegisterGoalGraphCleanup(t, ctx, cfg, goalID)

	columns := []domain.DataColumn{
		{GoalID: goalID, DatasourceRef: ref, Name: "X", Kind: "numeric"},
		{GoalID: goalID, DatasourceRef: ref, Name: "Y", Kind: "numeric"},
		{GoalID: goalID, DatasourceRef: ref, Name: "Z", Kind: "numeric"},
		{GoalID: goalID, DatasourceRef: ref, Name: "A", Kind: "numeric"},
		{GoalID: goalID, DatasourceRef: ref, Name: "B", Kind: "categorical"},
	}
	edge := func(a, b string, dir domain.EdgeDirection) domain.CausalEdge {
		return domain.CausalEdge{
			GoalID: goalID, DatasourceRef: ref, Version: 1, ColA: a, ColB: b,
			Direction: dir, Provenance: domain.ProvenanceStatistical, Confidence: 0.9, Status: domain.EdgeTested,
		}
	}
	edges := []domain.CausalEdge{
		edge("X", "Y", domain.DirectionAToB),
		edge("Y", "Z", domain.DirectionBToA),
		edge("A", "B", domain.DirectionUndirected),
	}
	if err := repo.UpsertDataColumns(ctx, columns); err != nil {
		t.Fatalf("upsert columns: %v", err)
	}
	if err := repo.UpsertCausalEdges(ctx, edges); err != nil {
		t.Fatalf("upsert edges: %v", err)
	}
	if err := repo.UpsertCausalGraphMeta(ctx, domain.CausalGraphMeta{
		GoalID: goalID, DatasourceRef: ref, Version: 1,
		ExcludedColumns: []string{}, TestCount: 9, DiscoveredAt: "2026-08-03T00:00:00Z",
	}); err != nil {
		t.Fatalf("upsert meta: %v", err)
	}
	return goalID, ref
}

func edgeFor(t *testing.T, g domain.CausalGraph, a, b string) (domain.CausalEdge, bool) {
	t.Helper()
	colA, colB := domain.CanonicalColumnPair(a, b)
	for _, e := range g.Edges {
		if e.ColA == colA && e.ColB == colB {
			return e, true
		}
	}
	return domain.CausalEdge{}, false
}

// TestCorrectCausalEdgeCopiesForward is the regression the version-keyed edge storage
// makes possible: a correction must serve the whole corrected graph at the new
// version, not just the edge it touched. Bumping the meta alone would strand every
// uncorrected edge at the old version and leave verification adjusting against an
// empty graph.
func TestCorrectCausalEdgeCopiesForward(t *testing.T) {
	ctx := context.Background()
	repo := newRepo(t, ctx)
	goalID, ref := seedGraph(t, ctx, repo)

	version, _, err := repo.CorrectCausalEdge(ctx, goalID, ref, domain.EdgeCorrection{
		Op: domain.CorrectionFlip, From: "Y", To: "X",
	})
	if err != nil {
		t.Fatalf("correct: %v", err)
	}
	if version != 2 {
		t.Fatalf("version = %d, want 2", version)
	}

	served, found, err := repo.GetCausalGraph(ctx, goalID, ref)
	if err != nil || !found {
		t.Fatalf("get after correction: found=%v err=%v", found, err)
	}
	if served.Meta.Version != 2 {
		t.Fatalf("served version = %d, want 2", served.Meta.Version)
	}
	if len(served.Edges) != 3 {
		t.Fatalf("served %d edges, want the whole set of 3 copied forward", len(served.Edges))
	}
	flipped, ok := edgeFor(t, served, "X", "Y")
	if !ok {
		t.Fatalf("corrected edge is missing from the served graph")
	}
	if flipped.Direction != domain.DirectionBToA {
		t.Fatalf("direction = %q, want b_to_a (Y causes X)", flipped.Direction)
	}
	if flipped.Provenance != domain.ProvenanceAnalyst || flipped.Confidence != 1.0 {
		t.Fatalf("corrected edge is not stamped analyst: %+v", flipped)
	}
	// An untouched edge keeps its own provenance -- the copy is not a re-stamp.
	if untouched, _ := edgeFor(t, served, "Y", "Z"); untouched.Provenance != domain.ProvenanceStatistical {
		t.Fatalf("copy-forward overwrote an untouched edge's provenance: %+v", untouched)
	}
}

// TestCorrectCausalEdgeDeleteIsDurable: a deleted edge is absent from the served
// version and stays absent across a later correction. Durability is by omission --
// discovery only ever writes its initial version, so nothing can re-add it at the
// version being served.
func TestCorrectCausalEdgeDeleteIsDurable(t *testing.T) {
	ctx := context.Background()
	repo := newRepo(t, ctx)
	goalID, ref := seedGraph(t, ctx, repo)

	if _, _, err := repo.CorrectCausalEdge(ctx, goalID, ref, domain.EdgeCorrection{
		Op: domain.CorrectionDelete, From: "X", To: "Y",
	}); err != nil {
		t.Fatalf("delete correction: %v", err)
	}
	served, _, err := repo.GetCausalGraph(ctx, goalID, ref)
	if err != nil {
		t.Fatalf("get after delete: %v", err)
	}
	if _, ok := edgeFor(t, served, "X", "Y"); ok {
		t.Fatalf("deleted edge is still served")
	}
	if len(served.Edges) != 2 {
		t.Fatalf("served %d edges, want the remaining 2", len(served.Edges))
	}

	// A second correction carries both the deletion and the first analyst stamp
	// forward: provenance survives because the copy preserves it per edge.
	version, _, err := repo.CorrectCausalEdge(ctx, goalID, ref, domain.EdgeCorrection{
		Op: domain.CorrectionFlip, From: "B", To: "A",
	})
	if err != nil {
		t.Fatalf("second correction: %v", err)
	}
	if version != 3 {
		t.Fatalf("version = %d, want 3", version)
	}
	served, _, err = repo.GetCausalGraph(ctx, goalID, ref)
	if err != nil {
		t.Fatalf("get after second correction: %v", err)
	}
	if _, ok := edgeFor(t, served, "X", "Y"); ok {
		t.Fatalf("a deleted edge came back after a later correction")
	}
	oriented, ok := edgeFor(t, served, "A", "B")
	if !ok || oriented.Direction != domain.DirectionBToA || oriented.Provenance != domain.ProvenanceAnalyst {
		t.Fatalf("second correction did not apply: %+v", oriented)
	}
}

// TestCorrectCausalEdgeAddsAndValidates: an add appends an edge the skeleton never
// produced (the known-confounder case), and a correction naming a column outside the
// graph is analyst-fixable rather than a fault.
func TestCorrectCausalEdgeAddsAndValidates(t *testing.T) {
	ctx := context.Background()
	repo := newRepo(t, ctx)
	goalID, ref := seedGraph(t, ctx, repo)

	if _, _, err := repo.CorrectCausalEdge(ctx, goalID, ref, domain.EdgeCorrection{
		Op: domain.CorrectionAdd, From: "Z", To: "X",
	}); err != nil {
		t.Fatalf("add correction: %v", err)
	}
	served, _, err := repo.GetCausalGraph(ctx, goalID, ref)
	if err != nil {
		t.Fatalf("get after add: %v", err)
	}
	added, ok := edgeFor(t, served, "Z", "X")
	if !ok {
		t.Fatalf("added edge is not served: %+v", served.Edges)
	}
	if added.Direction != domain.DirectionBToA || added.Provenance != domain.ProvenanceAnalyst || added.Status != domain.EdgeTested {
		t.Fatalf("added edge wrong: %+v", added)
	}
	if len(served.Edges) != 4 {
		t.Fatalf("served %d edges, want 4 after the add", len(served.Edges))
	}

	_, _, err = repo.CorrectCausalEdge(ctx, goalID, ref, domain.EdgeCorrection{
		Op: domain.CorrectionFlip, From: "X", To: "no_such_column",
	})
	if !errors.Is(err, graph.ErrUnknownCausalColumn) {
		t.Fatalf("error = %v, want ErrUnknownCausalColumn", err)
	}
}

// TestCorrectCausalEdgeSurvivesRediscovery: a crashed discovery re-run writes at its
// own initial version, so a corrected graph keeps being served. Version separation --
// not a provenance guard -- is what protects an analyst's edits.
func TestCorrectCausalEdgeSurvivesRediscovery(t *testing.T) {
	ctx := context.Background()
	repo := newRepo(t, ctx)
	goalID, ref := seedGraph(t, ctx, repo)

	if _, _, err := repo.CorrectCausalEdge(ctx, goalID, ref, domain.EdgeCorrection{
		Op: domain.CorrectionDelete, From: "X", To: "Y",
	}); err != nil {
		t.Fatalf("delete correction: %v", err)
	}

	// Discovery re-running over the same pair rewrites version 1 only.
	if err := repo.UpsertCausalEdges(ctx, []domain.CausalEdge{{
		GoalID: goalID, DatasourceRef: ref, Version: 1, ColA: "X", ColB: "Y",
		Direction: domain.DirectionAToB, Provenance: domain.ProvenanceStatistical,
		Confidence: 0.9, Status: domain.EdgeTested,
	}}); err != nil {
		t.Fatalf("re-discovery upsert: %v", err)
	}

	served, _, err := repo.GetCausalGraph(ctx, goalID, ref)
	if err != nil {
		t.Fatalf("get after re-discovery: %v", err)
	}
	if served.Meta.Version != 2 {
		t.Fatalf("served version = %d, want the corrected 2", served.Meta.Version)
	}
	if _, ok := edgeFor(t, served, "X", "Y"); ok {
		t.Fatalf("re-discovery resurrected an analyst-deleted edge at the served version")
	}
}

// TestCorrectCausalEdgeResolvesColumnCase is the regression behind the silent-success
// class: a correction is accepted case-insensitively, so every downstream consumer
// must see the graph's own spelling. The analyst's spelling would hash to a
// DataColumn id no MATCH binds (writing nothing), could transpose the canonical pair
// (matching nothing), and would miss the byte-exact adjustment-set overlap the caller
// invalidates with.
func TestCorrectCausalEdgeResolvesColumnCase(t *testing.T) {
	ctx := context.Background()
	repo := newRepo(t, ctx)
	goalID, ref := seedGraph(t, ctx, repo)

	// "y"/"x" lower-cased transposes the canonical pair: 'X' (88) < 'x' (120), so the
	// raw names would sort as ("X","y") and match no stored edge.
	version, columns, err := repo.CorrectCausalEdge(ctx, goalID, ref, domain.EdgeCorrection{
		Op: domain.CorrectionFlip, From: "y", To: "x",
	})
	if err != nil {
		t.Fatalf("a case-insensitive correction must apply: %v", err)
	}
	if version != 2 {
		t.Fatalf("version = %d, want 2", version)
	}
	if len(columns) != 2 || columns[0] != "Y" || columns[1] != "X" {
		t.Fatalf("resolved columns = %v, want the graph's own spelling [Y X]", columns)
	}

	served, _, err := repo.GetCausalGraph(ctx, goalID, ref)
	if err != nil {
		t.Fatalf("get after correction: %v", err)
	}
	flipped, ok := edgeFor(t, served, "X", "Y")
	if !ok {
		t.Fatalf("case-insensitive correction matched no edge: %+v", served.Edges)
	}
	if flipped.Direction != domain.DirectionBToA || flipped.Provenance != domain.ProvenanceAnalyst {
		t.Fatalf("correction did not apply: %+v", flipped)
	}

	// An add under mismatched case must write an edge the graph can actually serve --
	// the id is a hash of the column name, so the raw spelling would silently drop it.
	if _, _, err := repo.CorrectCausalEdge(ctx, goalID, ref, domain.EdgeCorrection{
		Op: domain.CorrectionAdd, From: "z", To: "a",
	}); err != nil {
		t.Fatalf("add correction: %v", err)
	}
	served, _, err = repo.GetCausalGraph(ctx, goalID, ref)
	if err != nil {
		t.Fatalf("get after add: %v", err)
	}
	if _, ok := edgeFor(t, served, "A", "Z"); !ok {
		t.Fatalf("lower-cased add wrote an edge nothing binds: %+v", served.Edges)
	}
}

// TestCorrectCausalEdgeRejectsUnmatchedEdge: a flip or delete naming a pair with no
// edge changes nothing, so it must say so rather than commit a version and report
// success. An add is the one op that is meaningful without a match.
func TestCorrectCausalEdgeRejectsUnmatchedEdge(t *testing.T) {
	ctx := context.Background()
	repo := newRepo(t, ctx)
	goalID, ref := seedGraph(t, ctx, repo)

	for _, op := range []domain.EdgeCorrectionOp{domain.CorrectionFlip, domain.CorrectionDelete} {
		if _, _, err := repo.CorrectCausalEdge(ctx, goalID, ref, domain.EdgeCorrection{
			Op: op, From: "X", To: "A",
		}); !errors.Is(err, graph.ErrNoSuchCausalEdge) {
			t.Fatalf("%s on a pair with no edge: error = %v, want ErrNoSuchCausalEdge", op, err)
		}
	}
	// The rejected corrections must not have moved the served version.
	served, _, err := repo.GetCausalGraph(ctx, goalID, ref)
	if err != nil {
		t.Fatalf("get after rejections: %v", err)
	}
	if served.Meta.Version != 1 {
		t.Fatalf("served version = %d, want 1 -- a rejected correction must not commit", served.Meta.Version)
	}
}

// TestCorrectCausalEdgeAppliesExplicitDirection: the analyst can override the
// From→To orientation, which is how an edge is returned to undirected — the statistics
// oriented it, and the analyst is saying they do not know.
func TestCorrectCausalEdgeAppliesExplicitDirection(t *testing.T) {
	ctx := context.Background()
	repo := newRepo(t, ctx)
	goalID, ref := seedGraph(t, ctx, repo)

	if _, _, err := repo.CorrectCausalEdge(ctx, goalID, ref, domain.EdgeCorrection{
		Op: domain.CorrectionFlip, From: "X", To: "Y", Direction: domain.DirectionUndirected,
	}); err != nil {
		t.Fatalf("correct: %v", err)
	}
	served, _, err := repo.GetCausalGraph(ctx, goalID, ref)
	if err != nil {
		t.Fatalf("get after correction: %v", err)
	}
	edge, ok := edgeFor(t, served, "X", "Y")
	if !ok {
		t.Fatalf("edge missing after correction")
	}
	if edge.Direction != domain.DirectionUndirected {
		t.Fatalf("direction = %q, want the explicit undirected override", edge.Direction)
	}
}

// TestCorrectCausalEdgeClearsTornWrite pins the pre-write cleanup: a crashed earlier
// correction that wrote an edge at the target version would otherwise survive the
// MERGE-based upsert and be served the moment the meta commits — the one way a
// deleted edge could come back.
func TestCorrectCausalEdgeClearsTornWrite(t *testing.T) {
	ctx := context.Background()
	repo := newRepo(t, ctx)
	goalID, ref := seedGraph(t, ctx, repo)

	// A torn write at version 2: edges landed, meta never committed, so the graph
	// still serves version 1.
	if err := repo.UpsertCausalEdges(ctx, []domain.CausalEdge{{
		GoalID: goalID, DatasourceRef: ref, Version: 2, ColA: "X", ColB: "Y",
		Direction: domain.DirectionAToB, Provenance: domain.ProvenanceStatistical,
		Confidence: 0.9, Status: domain.EdgeTested,
	}}); err != nil {
		t.Fatalf("seed torn write: %v", err)
	}

	if _, _, err := repo.CorrectCausalEdge(ctx, goalID, ref, domain.EdgeCorrection{
		Op: domain.CorrectionDelete, From: "X", To: "Y",
	}); err != nil {
		t.Fatalf("delete correction: %v", err)
	}
	served, _, err := repo.GetCausalGraph(ctx, goalID, ref)
	if err != nil {
		t.Fatalf("get after correction: %v", err)
	}
	if _, ok := edgeFor(t, served, "X", "Y"); ok {
		t.Fatalf("a torn write resurrected the deleted edge: %+v", served.Edges)
	}
}
