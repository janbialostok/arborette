package verifier

import (
	"context"
	"errors"
	"testing"

	"github.com/arborette/arborette/internal/domain"
	"github.com/arborette/arborette/internal/sandboxclient"
	"github.com/arborette/arborette/internal/verifier/groundtruth"
)

// TestColumnCapSelection drives the over-cap selection path: with a cap below the
// column count, the priority column is kept, the remainder is ranked by association
// with it, and the dropped columns are recorded as excluded.
func TestColumnCapSelection(t *testing.T) {
	ds := groundtruth.Generate(groundtruth.DefaultConfig())
	cfg := regressionConfig()
	cfg.ColumnCap = 4

	res, err := Discover(context.Background(), newFakeAnalyzer(ds), "demo", cfg, discoveryColumns(), []string{groundtruth.ColY})
	if err != nil {
		t.Fatalf("discover: %v", err)
	}
	if len(res.Columns) != 4 {
		t.Fatalf("selected %d columns, want 4 (the cap)", len(res.Columns))
	}
	if len(res.Excluded) != 3 {
		t.Fatalf("excluded %d columns, want 3", len(res.Excluded))
	}
	if !columnSelected(res.Columns, groundtruth.ColY) {
		t.Fatalf("priority column Y was not selected; selected=%v", res.Columns)
	}
	// Excluded columns are recorded sorted for a stable persisted list.
	for i := 1; i < len(res.Excluded); i++ {
		if res.Excluded[i-1] > res.Excluded[i] {
			t.Fatalf("excluded columns not sorted: %v", res.Excluded)
		}
	}
}

// TestBudgetTruncation drives the budget guard: a tiny test budget stops the sweep
// before level 0 runs, so every pair is retained fail-closed as budget_capped and
// the truncation is recorded.
func TestBudgetTruncation(t *testing.T) {
	ds := groundtruth.Generate(groundtruth.DefaultConfig())
	cfg := regressionConfig()
	cfg.MaxTests = 5 // 7 columns → 21 level-0 pairs, far over budget

	res, err := Discover(context.Background(), newFakeAnalyzer(ds), "demo", cfg, discoveryColumns(), nil)
	if err != nil {
		t.Fatalf("discover: %v", err)
	}
	if !res.Truncated {
		t.Fatalf("expected Truncated=true when the level-0 budget is exceeded")
	}
	if len(res.Edges) != 21 {
		t.Fatalf("edges = %d, want 21 (no pair removed under truncation)", len(res.Edges))
	}
	capped := 0
	for _, s := range res.Status {
		if s == domain.EdgeBudgetCapped {
			capped++
		}
	}
	if capped != 21 {
		t.Fatalf("budget_capped pairs = %d, want 21", capped)
	}
}

// TestFailClosedUnknownEdge drives the discovery-side fail-closed path: when a
// pair's test errors, the edge is retained with unknown status rather than removed
// or oriented.
func TestFailClosedUnknownEdge(t *testing.T) {
	ds := groundtruth.Generate(groundtruth.DefaultConfig())
	analyzer := erroringAnalyzer{inner: newFakeAnalyzer(ds), failColumn: groundtruth.ColRegion}

	res, err := Discover(context.Background(), analyzer, "demo", regressionConfig(), discoveryColumns(), nil)
	if err != nil {
		t.Fatalf("discover: %v", err)
	}
	found := false
	for pair, status := range res.Status {
		if (pair.A == groundtruth.ColRegion || pair.B == groundtruth.ColRegion) && status == domain.EdgeUnknown {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected a region-incident pair retained with unknown status; statuses=%v", res.Status)
	}
	orientations := orientEdges(context.Background(), res, nil, "goal", res.Columns, 0)
	for _, o := range orientations {
		if (o.Pair.A == groundtruth.ColRegion || o.Pair.B == groundtruth.ColRegion) &&
			o.Status == domain.EdgeUnknown && o.Direction != domain.DirectionUnknown {
			t.Fatalf("unknown region edge %v was oriented %q (must stay unknown)", o.Pair, o.Direction)
		}
	}
}

func TestStratifiedGTestGuards(t *testing.T) {
	// A cell with fewer than two endpoint values is malformed → not ok.
	if r := stratifiedGTest([]sandboxclient.ContingencyCell{{Values: []string{"a"}, Count: 5}}); r.ok {
		t.Fatalf("expected not-ok for malformed cells")
	}
	// A single-row stratum has dof 0 → untestable → not ok.
	single := []sandboxclient.ContingencyCell{
		{Values: []string{"a", "x"}, Count: 5}, {Values: []string{"a", "y"}, Count: 7},
	}
	if r := stratifiedGTest(single); r.ok {
		t.Fatalf("expected not-ok for a degenerate single-row stratum")
	}
	// A well-formed 2×2 stratum yields a valid p-value.
	table := []sandboxclient.ContingencyCell{
		{Values: []string{"a", "x"}, Count: 10}, {Values: []string{"a", "y"}, Count: 20},
		{Values: []string{"b", "x"}, Count: 30}, {Values: []string{"b", "y"}, Count: 40},
	}
	if r := stratifiedGTest(table); !r.ok || r.p < 0 || r.p > 1 {
		t.Fatalf("2x2 stratum: ok=%v p=%v", r.ok, r.p)
	}
}

func TestCorrelationMatrixGuards(t *testing.T) {
	if _, ok := correlationMatrix(0, []float64{0, 0}, []float64{0, 0}, []float64{0}); ok {
		t.Fatalf("expected not-ok for n<=1")
	}
	// A constant second variable has zero scatter → not ok.
	if _, ok := correlationMatrix(3, []float64{6, 6}, []float64{14, 12}, []float64{12}); ok {
		t.Fatalf("expected not-ok for a constant column")
	}
	// x=1..4, y=2..8 (perfectly correlated) → corr[0][1] ≈ 1.
	corr, ok := correlationMatrix(4, []float64{10, 20}, []float64{30, 120}, []float64{60})
	if !ok {
		t.Fatalf("expected ok for a well-formed 2-variable moment set")
	}
	if corr[0][1] < 0.999 {
		t.Fatalf("corr[0][1] = %.4f, want ~1", corr[0][1])
	}
}

func columnSelected(cols []Column, name string) bool {
	for _, c := range cols {
		if c.Name == name {
			return true
		}
	}
	return false
}

// erroringAnalyzer wraps an analyzer and errors on any request referencing a target
// column, so the fail-closed path can be driven for a chosen pair.
type erroringAnalyzer struct {
	inner      analyzer
	failColumn string
}

func (e erroringAnalyzer) Introspect(ctx context.Context, req sandboxclient.IntrospectRequest) (sandboxclient.IntrospectResponse, error) {
	return e.inner.Introspect(ctx, req)
}

func (e erroringAnalyzer) Execute(ctx context.Context, req sandboxclient.ExecuteRequest) (sandboxclient.ExecuteResponse, error) {
	return e.inner.Execute(ctx, req)
}

func (e erroringAnalyzer) Analyze(ctx context.Context, req sandboxclient.AnalyzeRequest) (sandboxclient.AnalyzeResponse, error) {
	for _, c := range req.Columns {
		if c.Name == e.failColumn {
			return sandboxclient.AnalyzeResponse{}, errors.New("boom")
		}
	}
	for _, v := range append(req.Variables, req.GroupBy...) {
		if v == e.failColumn {
			return sandboxclient.AnalyzeResponse{}, errors.New("boom")
		}
	}
	return e.inner.Analyze(ctx, req)
}
