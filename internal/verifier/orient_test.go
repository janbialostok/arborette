package verifier

import (
	"context"
	"errors"
	"sort"
	"testing"

	"github.com/arborette/arborette/internal/domain"
	"github.com/arborette/arborette/internal/llm"
)

// orientStateOf builds an orientState directly from a set of directed (from→to)
// and undirected edges, so each Meek rule can be tested in isolation without
// deriving the directed seeds through collider orientation (which is fragile to
// construct without accidentally triggering other rules).
func orientStateOf(nodes []string, directed, undirected [][2]string) *orientState {
	o := &orientState{
		adj:        map[string]map[string]bool{},
		directed:   map[[2]string]bool{},
		undirected: map[Pair]bool{},
		confidence: map[Pair]float64{},
		provenance: map[Pair]domain.EdgeProvenance{},
		sepsets:    map[Pair][]string{},
	}
	for _, n := range nodes {
		o.nodes = append(o.nodes, n)
		o.adj[n] = map[string]bool{}
	}
	sort.Strings(o.nodes)
	link := func(a, b string) { o.adj[a][b] = true; o.adj[b][a] = true }
	for _, e := range directed {
		o.directed[[2]string{e[0], e[1]}] = true
		link(e[0], e[1])
	}
	for _, e := range undirected {
		o.undirected[pairOf(e[0], e[1])] = true
		link(e[0], e[1])
	}
	return o
}

// buildResult assembles a DiscoveryResult fixture from node names, present tested
// edges, fixed (unknown-status) edges, and separating sets for removed pairs.
func buildResult(nodes []string, tested [][2]string, unknown [][2]string, sepsets map[Pair][]string) *DiscoveryResult {
	res := &DiscoveryResult{
		Edges:   map[Pair]bool{},
		Status:  map[Pair]domain.EdgeStatus{},
		SepSets: sepsets,
	}
	for _, n := range nodes {
		res.Columns = append(res.Columns, Column{Name: n})
	}
	for _, e := range tested {
		p := pairOf(e[0], e[1])
		res.Edges[p] = true
		res.Status[p] = domain.EdgeTested
	}
	for _, e := range unknown {
		p := pairOf(e[0], e[1])
		res.Edges[p] = true
		res.Status[p] = domain.EdgeUnknown
	}
	return res
}

func dirOf(orientations []edgeOrientation, a, b string) domain.EdgeDirection {
	return directionOf(orientations, pairOf(a, b))
}

func TestColliderOrientation(t *testing.T) {
	// X–Z–Y with X,Y non-adjacent and Z NOT in their separating set ⇒ collider
	// X→Z←Y (canonical X<Z, Y<Z, so both edges orient a_to_b: X→Z, Y→Z).
	res := buildResult(
		[]string{"X", "Y", "Z"},
		[][2]string{{"X", "Z"}, {"Y", "Z"}}, nil,
		map[Pair][]string{pairOf("X", "Y"): {}},
	)
	got := orientDeterministic(res).result(res)
	if d := dirOf(got, "X", "Z"); d != domain.DirectionAToB {
		t.Errorf("X–Z = %q, want a_to_b (X→Z)", d)
	}
	if d := dirOf(got, "Y", "Z"); d != domain.DirectionAToB {
		t.Errorf("Y–Z = %q, want a_to_b (Y→Z)", d)
	}
}

func TestNonColliderStaysUndirected(t *testing.T) {
	// Z IS in the separating set of (X,Y) ⇒ non-collider ⇒ nothing orients.
	res := buildResult(
		[]string{"X", "Y", "Z"},
		[][2]string{{"X", "Z"}, {"Y", "Z"}}, nil,
		map[Pair][]string{pairOf("X", "Y"): {"Z"}},
	)
	got := orientDeterministic(res).result(res)
	if d := dirOf(got, "X", "Z"); d != domain.DirectionUndirected {
		t.Errorf("X–Z = %q, want undirected", d)
	}
	if d := dirOf(got, "Y", "Z"); d != domain.DirectionUndirected {
		t.Errorf("Y–Z = %q, want undirected", d)
	}
}

func TestMeekR1Propagates(t *testing.T) {
	// Collider A→B←C forces A→B, then Meek R1 propagates B→D across the undirected
	// B–D with A,D (and C,D) non-adjacent.
	res := buildResult(
		[]string{"A", "B", "C", "D"},
		[][2]string{{"A", "B"}, {"B", "C"}, {"B", "D"}}, nil,
		map[Pair][]string{
			pairOf("A", "C"): {},
			pairOf("A", "D"): {"B"},
			pairOf("C", "D"): {"B"},
		},
	)
	got := orientDeterministic(res).result(res)
	if d := dirOf(got, "A", "B"); d != domain.DirectionAToB {
		t.Errorf("A–B = %q, want a_to_b (A→B)", d)
	}
	if d := dirOf(got, "B", "C"); d != domain.DirectionBToA {
		t.Errorf("B–C = %q, want b_to_a (C→B)", d)
	}
	if d := dirOf(got, "B", "D"); d != domain.DirectionAToB {
		t.Errorf("B–D = %q, want a_to_b (B→D, Meek R1)", d)
	}
}

func TestUnknownEdgeStaysUndirected(t *testing.T) {
	// The A–B edge is unknown (fail-closed): the collider that would orient A→B skips
	// it as an orientation target, so it stays unknown while C→B still orients.
	res := buildResult(
		[]string{"A", "B", "C"},
		[][2]string{{"B", "C"}},
		[][2]string{{"A", "B"}},
		map[Pair][]string{pairOf("A", "C"): {}},
	)
	got := orientDeterministic(res).result(res)
	if d := dirOf(got, "A", "B"); d != domain.DirectionUnknown {
		t.Errorf("A–B = %q, want unknown (fail-closed, not oriented)", d)
	}
	if d := dirOf(got, "B", "C"); d != domain.DirectionBToA {
		t.Errorf("B–C = %q, want b_to_a (C→B)", d)
	}
}

func TestMeekR2(t *testing.T) {
	// A→C→B with A–B undirected ⇒ A→B (Meek R2). C,A adjacent, so R1 does not fire.
	o := orientStateOf([]string{"A", "B", "C"},
		[][2]string{{"A", "C"}, {"C", "B"}}, [][2]string{{"A", "B"}})
	o.meek()
	if !o.directed[[2]string{"A", "B"}] {
		t.Fatalf("R2 did not orient A→B; directed=%v", o.directed)
	}
}

func TestMeekR3(t *testing.T) {
	// A has two non-adjacent undirected neighbors C,D that both point into B ⇒ A→B.
	o := orientStateOf([]string{"A", "B", "C", "D"},
		[][2]string{{"C", "B"}, {"D", "B"}}, [][2]string{{"A", "B"}, {"A", "C"}, {"A", "D"}})
	o.meek()
	if !o.directed[[2]string{"A", "B"}] {
		t.Fatalf("R3 did not orient A→B; directed=%v", o.directed)
	}
}

func TestMeekR4(t *testing.T) {
	// A–B, A–C undirected with a chain C→D→B and B,C non-adjacent (A,D adjacent so
	// R1 does not fire on D→B) ⇒ A→B (Meek R4).
	o := orientStateOf([]string{"A", "B", "C", "D"},
		[][2]string{{"C", "D"}, {"D", "B"}}, [][2]string{{"A", "B"}, {"A", "C"}, {"A", "D"}})
	o.meek()
	if !o.directed[[2]string{"A", "B"}] {
		t.Fatalf("R4 did not orient A→B; directed=%v", o.directed)
	}
}

// fakeOrienter scripts the batched orientation call and its repair sibling.
type fakeOrienter struct {
	responses [][]llm.OrientDecision
	errs      []error
	call      int
}

func (f *fakeOrienter) next() ([]llm.OrientDecision, error) {
	i := f.call
	f.call++
	var err error
	if i < len(f.errs) {
		err = f.errs[i]
	}
	if i < len(f.responses) {
		return f.responses[i], err
	}
	return nil, err
}

func (f *fakeOrienter) OrientCausalEdges(_ context.Context, _ string, _ []llm.ColumnSemantics, _ []llm.OrientEdge) ([]llm.OrientDecision, error) {
	return f.next()
}
func (f *fakeOrienter) RepairOrientCausalEdges(_ context.Context, _ string, _ []llm.ColumnSemantics, _ []llm.OrientEdge, _ []llm.OrientDecision, _ string) ([]llm.OrientDecision, error) {
	return f.next()
}

// singleEdge builds a two-node one-edge skeleton Meek leaves undirected, so the one
// edge (id "e0") is what reaches the LLM stage.
func singleEdge() *DiscoveryResult {
	return buildResult([]string{"A", "B"}, [][2]string{{"A", "B"}}, nil, map[Pair][]string{})
}

func TestLLMOrientationApplied(t *testing.T) {
	res := singleEdge()
	o := &fakeOrienter{responses: [][]llm.OrientDecision{{{EdgeID: "e0", Decision: llm.OrientFirstCausesSecond, Confidence: 0.9}}}}
	got := orientEdges(context.Background(), res, o, "goal", res.Columns, 2)
	if d := dirOf(got, "A", "B"); d != domain.DirectionAToB {
		t.Fatalf("A–B = %q, want a_to_b from the LLM", d)
	}
	for _, e := range got {
		if e.Provenance != domain.ProvenanceLLMPrior || e.Confidence != 0.9 {
			t.Fatalf("LLM edge provenance/confidence = %v/%v, want llm_prior/0.9", e.Provenance, e.Confidence)
		}
	}
}

func TestLLMOrientationRepairThenApply(t *testing.T) {
	res := singleEdge()
	// First response names an edge id it was not given ⇒ repair ⇒ valid.
	o := &fakeOrienter{responses: [][]llm.OrientDecision{
		{{EdgeID: "e9", Decision: llm.OrientFirstCausesSecond, Confidence: 0.5}},
		{{EdgeID: "e0", Decision: llm.OrientSecondCausesFirst, Confidence: 0.7}},
	}}
	got := orientEdges(context.Background(), res, o, "goal", res.Columns, 2)
	if d := dirOf(got, "A", "B"); d != domain.DirectionBToA {
		t.Fatalf("A–B = %q, want b_to_a after repair", d)
	}
}

func TestLLMOrientationExhaustedStaysUndirected(t *testing.T) {
	res := singleEdge()
	// Every attempt names an unknown edge id ⇒ repair exhausted ⇒ fail closed.
	o := &fakeOrienter{responses: [][]llm.OrientDecision{
		{{EdgeID: "e9", Decision: llm.OrientFirstCausesSecond}},
		{{EdgeID: "e9", Decision: llm.OrientFirstCausesSecond}},
		{{EdgeID: "e9", Decision: llm.OrientFirstCausesSecond}},
	}}
	got := orientEdges(context.Background(), res, o, "goal", res.Columns, 2)
	if d := dirOf(got, "A", "B"); d != domain.DirectionUndirected {
		t.Fatalf("A–B = %q, want undirected after repair exhaustion", d)
	}
}

func TestLLMAbstainStaysUndirected(t *testing.T) {
	res := singleEdge()
	o := &fakeOrienter{responses: [][]llm.OrientDecision{{{EdgeID: "e0", Decision: llm.OrientAbstain}}}}
	got := orientEdges(context.Background(), res, o, "goal", res.Columns, 2)
	if d := dirOf(got, "A", "B"); d != domain.DirectionUndirected {
		t.Fatalf("A–B = %q, want undirected on abstain", d)
	}
}

func TestLLMTransportErrorStaysUndirected(t *testing.T) {
	res := singleEdge()
	// A transport error is not repairable by re-prompting; the batch fails closed
	// with the edge left undirected.
	o := &fakeOrienter{errs: []error{errors.New("boom")}}
	got := orientEdges(context.Background(), res, o, "goal", res.Columns, 2)
	if d := dirOf(got, "A", "B"); d != domain.DirectionUndirected {
		t.Fatalf("A–B = %q, want undirected on transport error", d)
	}
}
