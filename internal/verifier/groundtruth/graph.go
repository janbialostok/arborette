package groundtruth

import "sort"

// Edge is one planted directed cause→effect relationship in the ground-truth
// graph. The generator plants exactly these; the discovery regression compares
// its recovered skeleton and oriented subset against them.
type Edge struct {
	From string
	To   string
}

// Graph is the planted causal structure the generated rows encode. Nodes lists
// every column (including the pure-noise region, which must stay unconnected);
// Directed is the set of true cause→effect edges.
type Graph struct {
	Nodes    []string
	Directed []Edge
}

// GroundTruth returns the planted graph for the bundled dataset: Z confounds X
// and Y, A causes Y directly, and the B×C interaction makes both B and C causes
// of Y. region has no edge.
func GroundTruth() Graph {
	return Graph{
		Nodes: Header(),
		Directed: []Edge{
			{From: ColZ, To: ColX},
			{From: ColZ, To: ColY},
			{From: ColA, To: ColY},
			{From: ColB, To: ColY},
			{From: ColC, To: ColY},
		},
	}
}

// SkeletonPair is a canonical undirected pair (A < B lexicographically), the key
// the regression scores skeleton precision and recall on.
type SkeletonPair struct {
	A string
	B string
}

// Skeleton returns the undirected edge set of the ground-truth graph as canonical
// pairs, so recovered and planted skeletons are compared direction-agnostically.
func (g Graph) Skeleton() map[SkeletonPair]bool {
	pairs := make(map[SkeletonPair]bool, len(g.Directed))
	for _, e := range g.Directed {
		pairs[CanonicalPair(e.From, e.To)] = true
	}
	return pairs
}

// CanonicalPair orders two column names lexicographically into a SkeletonPair,
// the one spelling every undirected-edge key uses.
func CanonicalPair(a, b string) SkeletonPair {
	if a <= b {
		return SkeletonPair{A: a, B: b}
	}
	return SkeletonPair{A: b, B: a}
}

// SortedNodes returns the node list in a stable order, so a test iterating the
// graph is deterministic regardless of map iteration.
func (g Graph) SortedNodes() []string {
	nodes := append([]string(nil), g.Nodes...)
	sort.Strings(nodes)
	return nodes
}
