package verifier

import (
	"context"
	"sort"
	"strconv"

	"github.com/arborette/arborette/internal/domain"
	"github.com/arborette/arborette/internal/llm"
)

// edgeOrientation is one present edge's final orientation: its canonical pair,
// direction, how it was oriented (provenance), the orientation confidence, and the
// fail-closed status carried from discovery.
type edgeOrientation struct {
	Pair       Pair
	Direction  domain.EdgeDirection
	Provenance domain.EdgeProvenance
	Confidence float64
	Status     domain.EdgeStatus
}

// orientState is the working orientation over the skeleton during collider
// orientation and Meek propagation. Orientation eligibility is status-gated: only
// tested edges live in `undirected` (orientable); unknown/budget_capped edges live
// in `fixed` and inform adjacency and unshielded-triple detection but are never
// oriented. `directed` records from→to for oriented edges.
type orientState struct {
	nodes      []string
	adj        map[string]map[string]bool
	directed   map[[2]string]bool
	undirected map[Pair]bool
	fixed      map[Pair]bool
	confidence map[Pair]float64
	provenance map[Pair]domain.EdgeProvenance
	sepsets    map[Pair][]string
}

// newOrientState seeds the orientation from a discovery skeleton: tested edges are
// orientable-undirected with statistical provenance and confidence 1.0 (the edge
// existence is statistical); unknown/budget_capped edges are fixed-undirected.
func newOrientState(res *DiscoveryResult) *orientState {
	o := &orientState{
		adj:        map[string]map[string]bool{},
		directed:   map[[2]string]bool{},
		undirected: map[Pair]bool{},
		fixed:      map[Pair]bool{},
		confidence: map[Pair]float64{},
		provenance: map[Pair]domain.EdgeProvenance{},
		sepsets:    res.SepSets,
	}
	for _, c := range res.Columns {
		o.nodes = append(o.nodes, c.Name)
		o.adj[c.Name] = map[string]bool{}
	}
	sort.Strings(o.nodes)
	for pair := range res.Edges {
		o.adj[pair.A][pair.B] = true
		o.adj[pair.B][pair.A] = true
		o.confidence[pair] = 1.0
		o.provenance[pair] = domain.ProvenanceStatistical
		if res.Status[pair] == domain.EdgeTested {
			o.undirected[pair] = true
		} else {
			o.fixed[pair] = true
		}
	}
	return o
}

// orientDeterministic runs collider orientation then Meek propagation to a fixpoint
// over the seeded state. It is the deterministic half of orientation; the remaining
// undirected tested edges go to Claude afterward.
func orientDeterministic(res *DiscoveryResult) *orientState {
	o := newOrientState(res)
	o.orientColliders()
	o.meek()
	return o
}

// orientColliders orients every unshielded triple X–Z–Y (X,Y non-adjacent) into a
// collider X→Z←Y when Z is not in the recorded separating set of (X,Y). Only tested
// (orientable) edges are directed; a fixed edge in the triple stays undirected but
// still lets the triple be detected.
func (o *orientState) orientColliders() {
	for _, z := range o.nodes {
		neighbors := o.sortedNeighbors(z)
		for i := 0; i < len(neighbors); i++ {
			for j := i + 1; j < len(neighbors); j++ {
				x, y := neighbors[i], neighbors[j]
				if o.adjacent(x, y) {
					continue // shielded triple
				}
				if o.inSepSet(z, x, y) {
					continue // Z separates X and Y ⇒ non-collider
				}
				o.orient(x, z)
				o.orient(y, z)
			}
		}
	}
}

// meek applies Meek rules R1–R4 repeatedly until no edge changes, propagating the
// orientations the colliders forced. Every rule orients only tested (orientable)
// edges; fixed edges participate only as adjacencies in the premises.
func (o *orientState) meek() {
	for {
		changed := false
		for _, pair := range o.sortedUndirected() {
			a, b := pair.A, pair.B
			// R1: c→a and a–b with c,b non-adjacent ⇒ a→b (and the symmetric case).
			if o.meekR1(a, b) || o.meekR1(b, a) ||
				o.meekR2(a, b) || o.meekR2(b, a) ||
				o.meekR3(a, b) || o.meekR3(b, a) ||
				o.meekR4(a, b) || o.meekR4(b, a) {
				changed = true
			}
		}
		if !changed {
			return
		}
	}
}

// meekR1 orients into->b when some c→into exists with c not adjacent to b, and
// into–b is orientable. Returns whether it oriented an edge.
func (o *orientState) meekR1(into, b string) bool {
	if !o.orientable(into, b) {
		return false
	}
	for _, c := range o.sortedNodes() {
		if o.directed[[2]string{c, into}] && c != b && !o.adjacent(c, b) {
			return o.orient(into, b)
		}
	}
	return false
}

// meekR2 orients a→b when a directed chain a→c→b exists and a–b is orientable.
func (o *orientState) meekR2(a, b string) bool {
	if !o.orientable(a, b) {
		return false
	}
	for _, c := range o.sortedNodes() {
		if o.directed[[2]string{a, c}] && o.directed[[2]string{c, b}] {
			return o.orient(a, b)
		}
	}
	return false
}

// meekR3 orients a→b when a has two non-adjacent undirected neighbors c,d that both
// point into b, and a–b is orientable.
func (o *orientState) meekR3(a, b string) bool {
	if !o.orientable(a, b) {
		return false
	}
	var into []string
	for _, c := range o.sortedNodes() {
		if o.undirectedBetween(a, c) && o.directed[[2]string{c, b}] {
			into = append(into, c)
		}
	}
	for i := 0; i < len(into); i++ {
		for j := i + 1; j < len(into); j++ {
			if !o.adjacent(into[i], into[j]) {
				return o.orient(a, b)
			}
		}
	}
	return false
}

// meekR4 orients a→b when there is an undirected a–c and a directed chain c→d→b
// with b,c non-adjacent, and a–b is orientable — the background-knowledge rule.
func (o *orientState) meekR4(a, b string) bool {
	if !o.orientable(a, b) {
		return false
	}
	for _, c := range o.sortedNodes() {
		if !o.undirectedBetween(a, c) || o.adjacent(b, c) {
			continue
		}
		for _, d := range o.sortedNodes() {
			if o.directed[[2]string{c, d}] && o.directed[[2]string{d, b}] {
				return o.orient(a, b)
			}
		}
	}
	return false
}

// orient directs from→into if that edge is currently an orientable undirected edge,
// reporting whether it changed anything.
func (o *orientState) orient(from, into string) bool {
	pair := pairOf(from, into)
	if !o.undirected[pair] {
		return false
	}
	delete(o.undirected, pair)
	o.directed[[2]string{from, into}] = true
	return true
}

func (o *orientState) orientable(from, into string) bool {
	return o.undirected[pairOf(from, into)]
}

func (o *orientState) undirectedBetween(a, b string) bool {
	return o.undirected[pairOf(a, b)]
}

func (o *orientState) adjacent(a, b string) bool {
	return o.adj[a][b]
}

func (o *orientState) inSepSet(z, x, y string) bool {
	for _, s := range o.sepsets[pairOf(x, y)] {
		if s == z {
			return true
		}
	}
	return false
}

func (o *orientState) sortedNeighbors(node string) []string {
	out := make([]string, 0, len(o.adj[node]))
	for n := range o.adj[node] {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}

func (o *orientState) sortedNodes() []string { return o.nodes }

func (o *orientState) sortedUndirected() []Pair {
	pairs := make([]Pair, 0, len(o.undirected))
	for p := range o.undirected {
		pairs = append(pairs, p)
	}
	sort.Slice(pairs, func(i, j int) bool {
		if pairs[i].A != pairs[j].A {
			return pairs[i].A < pairs[j].A
		}
		return pairs[i].B < pairs[j].B
	})
	return pairs
}

// result projects the final orientation of every present edge into edgeOrientation
// records: directed edges carry their a_to_b/b_to_a direction, undirected tested
// edges stay undirected, and fixed edges carry direction unknown (fail-closed). The
// status is carried from discovery.
func (o *orientState) result(res *DiscoveryResult) []edgeOrientation {
	out := make([]edgeOrientation, 0, len(res.Edges))
	for _, pair := range sortedPairs(res.Edges) {
		eo := edgeOrientation{
			Pair:       pair,
			Confidence: o.confidence[pair],
			Provenance: o.provenance[pair],
			Status:     res.Status[pair],
		}
		switch {
		case o.directed[[2]string{pair.A, pair.B}]:
			eo.Direction = domain.DirectionAToB
		case o.directed[[2]string{pair.B, pair.A}]:
			eo.Direction = domain.DirectionBToA
		case o.fixed[pair]:
			eo.Direction = domain.DirectionUnknown
		default:
			eo.Direction = domain.DirectionUndirected
		}
		out = append(out, eo)
	}
	return out
}

// orientEdges runs the full orientation: deterministic collider + Meek, then one
// batched Claude call over the edges Meek left unoriented (bounded repair on an
// invalid response; on exhaustion those edges stay undirected — abstain-equivalent,
// fail-closed). Claude may only orient or abstain, never add or delete an edge:
// decisions naming an unknown or already-oriented edge id are a repair error, and
// LLM-oriented edges get llm_prior provenance and the model's confidence.
func orientEdges(ctx context.Context, res *DiscoveryResult, o orienter, goalText string, columns []Column, maxRepairs int) []edgeOrientation {
	state := orientDeterministic(res)

	pending := state.sortedUndirected()
	if len(pending) > 0 && o != nil {
		state.applyLLM(ctx, o, goalText, columns, pending, maxRepairs)
	}
	return state.result(res)
}

// applyLLM builds the batched orientation request for the still-undirected edges,
// validates the decoded decisions deterministically (each id must exist and still
// be unoriented), runs a bounded repair loop on an invalid batch, and applies the
// surviving decisions. Abstain and any un-decided or repair-exhausted edge stays
// undirected.
func (o *orientState) applyLLM(ctx context.Context, orient orienter, goalText string, columns []Column, pending []Pair, maxRepairs int) {
	edges := make([]llm.OrientEdge, 0, len(pending))
	valid := map[string]Pair{}
	for i, p := range pending {
		id := edgeID(i)
		edges = append(edges, llm.OrientEdge{ID: id, First: p.A, Second: p.B})
		valid[id] = p
	}
	semantics := columnSemantics(columns)

	decisions, err := orient.OrientCausalEdges(ctx, goalText, semantics, edges)
	for attempt := 0; attempt < maxRepairs; attempt++ {
		if err == nil {
			if verr := validateDecisions(decisions, valid); verr == "" {
				break
			} else {
				decisions, err = orient.RepairOrientCausalEdges(ctx, goalText, semantics, edges, decisions, verr)
				continue
			}
		}
		// A transport error is not repairable by re-prompting the model on a validation
		// message; fail closed with the edges left undirected.
		return
	}
	if err != nil || validateDecisions(decisions, valid) != "" {
		return // repair exhausted: leave the batch undirected (abstain-equivalent)
	}

	for _, d := range decisions {
		pair, ok := valid[d.EdgeID]
		if !ok || !o.undirected[pair] {
			continue
		}
		switch d.Decision {
		case llm.OrientFirstCausesSecond:
			o.applyLLMDirection(pair, domain.DirectionForCause(pair.A, pair.B), d.Confidence)
		case llm.OrientSecondCausesFirst:
			o.applyLLMDirection(pair, domain.DirectionForCause(pair.B, pair.A), d.Confidence)
		}
	}
}

// applyLLMDirection records a Claude orientation on an edge: it becomes directed
// with llm_prior provenance and the model's confidence.
func (o *orientState) applyLLMDirection(pair Pair, dir domain.EdgeDirection, confidence float64) {
	delete(o.undirected, pair)
	from, into := pair.A, pair.B
	if dir == domain.DirectionBToA {
		from, into = pair.B, pair.A
	}
	o.directed[[2]string{from, into}] = true
	o.provenance[pair] = domain.ProvenanceLLMPrior
	o.confidence[pair] = confidence
}

// validateDecisions checks a decoded batch: every decision must name a listed,
// still-orientable edge id, and no edge id may be decided twice. It returns a
// human-readable rejection reason for the repair loop, or "" when the batch is
// valid. Abstain decisions are always valid.
func validateDecisions(decisions []llm.OrientDecision, valid map[string]Pair) string {
	seen := map[string]bool{}
	for _, d := range decisions {
		if _, ok := valid[d.EdgeID]; !ok {
			return "decision references unknown edge id " + d.EdgeID
		}
		if seen[d.EdgeID] {
			return "edge id " + d.EdgeID + " decided more than once"
		}
		seen[d.EdgeID] = true
		switch d.Decision {
		case llm.OrientFirstCausesSecond, llm.OrientSecondCausesFirst, llm.OrientAbstain:
		default:
			return "unknown decision value " + d.Decision + " for edge id " + d.EdgeID
		}
	}
	return ""
}

func columnSemantics(columns []Column) []llm.ColumnSemantics {
	out := make([]llm.ColumnSemantics, 0, len(columns))
	for _, c := range columns {
		typ := "categorical"
		if c.Numeric {
			typ = "numeric"
		}
		out = append(out, llm.ColumnSemantics{Name: c.Name, Type: typ, Samples: c.Samples})
	}
	return out
}

func edgeID(i int) string {
	return "e" + strconv.Itoa(i)
}
