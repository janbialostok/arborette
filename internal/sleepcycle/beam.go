package sleepcycle

import (
	"cmp"
	"slices"
	"strings"

	"github.com/arborette/arborette/internal/domain"
)

// beamPolicy is the V1 SearchPolicy: a level-wise beam over the subset lattice
// with anti-monotone (Apriori) minimum-support pruning.
//
// Level 1 is every atom. Level k conjoins each surviving frontier node with each
// atom ranked above its own top atom, so every subset is generated exactly once —
// its only generator is the prefix without its top-ranked atom. That makes this a
// walk of the subset lattice, not the permutation tree.
//
// Only two things prune. Support: conjoining can only shrink the row set, so a
// node below the floor has every superset below it too, provably. Width: nodes
// outside the top BeamWidth at their level are dropped. A merely non-improving
// node is never pruned — interaction effects are the whole reason this search
// exists, so a predicate that underperforms alone may still form an improving
// conjunction. Delta ranks; it does not exclude.
//
// Termination is by construction: levels are bounded by maxOrder and each level's
// candidate count by BeamWidth × atoms, so the policy runs out of candidates
// without any stopping-rule machinery.
type beamPolicy struct {
	atoms      []atom
	beamWidth  int
	maxOrder   int
	minSupport int64
	baseline   float64
	direction  domain.TargetDirection

	level     int
	pending   []*Node
	next      int
	results   []measuredNode
	memo      map[string]Measurement
	exhausted bool
}

func newBeamPolicy(atoms []atom, cfg Config, baseline float64, direction domain.TargetDirection) *beamPolicy {
	p := &beamPolicy{
		atoms:      atoms,
		beamWidth:  cfg.BeamWidth,
		maxOrder:   cfg.MaxOrder,
		minSupport: int64(cfg.MinSupport),
		baseline:   baseline,
		direction:  direction,
		level:      1,
		memo:       map[string]Measurement{},
	}
	p.pending = p.level1()
	p.exhausted = len(p.pending) == 0
	return p
}

// delta is the frontier's ranking key.
func (p *beamPolicy) delta(m Measurement) float64 {
	return directionalDelta(m.Value, p.baseline, p.direction)
}

func (p *beamPolicy) level1() []*Node {
	nodes := make([]*Node, 0, len(p.atoms))
	for _, a := range p.atoms {
		nodes = append(nodes, &Node{
			keys:      []string{a.key},
			ranks:     []int{a.rank},
			filters:   []domain.Constraint{a.constraint},
			canonical: a.key,
		})
	}
	return nodes
}

func (p *beamPolicy) Select() (*Node, bool) {
	if p.next >= len(p.pending) {
		return nil, false
	}
	node := p.pending[p.next]
	p.next++
	return node, true
}

// Update records one measurement and, when it closes out the level, advances
// eagerly: it picks the frontier and generates the next level before returning,
// so the driver's next Done call already reflects whether anything remains.
//
// It is idempotent per node. The beam generates each canonical key exactly once,
// so a repeat is not reachable from this policy — but the driver memoizes
// measurements and still calls Update on a re-selected node, so a policy that
// re-selects (the V2 seam's whole point) would otherwise double-count outcomes
// and advance the level before its candidates had all been measured.
func (p *beamPolicy) Update(node *Node, m Measurement) {
	if _, recorded := p.memo[node.canonical]; recorded {
		return
	}
	p.memo[node.canonical] = m
	p.results = append(p.results, measuredNode{node: node, measurement: m})
	if len(p.results) < len(p.pending) {
		return
	}
	p.advance()
}

func (p *beamPolicy) Done() bool { return p.exhausted }

// advance closes the current level: rank the survivors into the frontier, then
// expand it into the next level's candidates.
func (p *beamPolicy) advance() {
	frontier := p.frontier()
	p.results = nil
	p.next = 0

	if p.level >= p.maxOrder || len(frontier) == 0 {
		p.pending = nil
		p.exhausted = true
		return
	}
	p.level++
	p.pending = p.expand(frontier)
	p.exhausted = len(p.pending) == 0
}

// frontier is the top-BeamWidth surviving nodes of the level just measured,
// ranked by directional delta against the run's baseline. Surviving means
// measured successfully and at or above the support floor; a failed measurement
// counts as unknown support, which the anti-monotone rule treats as insufficient.
// Ties break on canonical filter so runs are reproducible.
//
// When a level has no more survivors than the beam is wide the beam keeps them
// all, degenerating to full enumeration — the usual case on the small lattices
// V1 sees.
func (p *beamPolicy) frontier() []*Node {
	survivors := make([]measuredNode, 0, len(p.results))
	for _, r := range p.results {
		if !r.measurement.Failed && r.measurement.Support >= p.minSupport {
			survivors = append(survivors, r)
		}
	}
	slices.SortFunc(survivors, func(a, b measuredNode) int {
		if d := cmp.Compare(p.delta(b.measurement), p.delta(a.measurement)); d != 0 {
			return d
		}
		return strings.Compare(a.node.canonical, b.node.canonical)
	})
	if len(survivors) > p.beamWidth {
		survivors = survivors[:p.beamWidth]
	}
	nodes := make([]*Node, 0, len(survivors))
	for _, s := range survivors {
		nodes = append(nodes, s.node)
	}
	return nodes
}

// expand conjoins each frontier node with every higher-ranked atom, dropping any
// candidate the Apriori rule already rules out.
func (p *beamPolicy) expand(frontier []*Node) []*Node {
	var candidates []*Node
	for _, f := range frontier {
		top := f.ranks[len(f.ranks)-1]
		for _, a := range p.atoms {
			if a.rank <= top {
				continue
			}
			child := conjoin(f, a)
			if p.prunedBySubset(child) {
				continue
			}
			candidates = append(candidates, child)
		}
	}
	return candidates
}

// prunedBySubset applies the Apriori rule against measured evidence: drop a
// candidate unmeasured if ANY of its (k−1)-subsets came back below the support
// floor or failed, since support cannot grow by conjoining.
//
// Checking every subset rather than just the generating prefix is what makes the
// guarantee hold off-prefix: {1,2,3} generated from a frequent {1,2} still
// contains {1,3}, and must be dropped when that was below floor. Only *measured*
// evidence prunes — an unmeasured subset (never generated, or dropped by width)
// says nothing about support, and letting it prune would disguise width-pruning
// as support-pruning.
func (p *beamPolicy) prunedBySubset(node *Node) bool {
	for skip := range node.keys {
		subset := make([]string, 0, len(node.keys)-1)
		for i, k := range node.keys {
			if i != skip {
				subset = append(subset, k)
			}
		}
		m, measured := p.memo[canonicalKeyOf(subset)]
		if measured && (m.Failed || m.Support < p.minSupport) {
			return true
		}
	}
	return false
}

// conjoin builds the child node adding one higher-ranked atom to a frontier node.
// Ranks stay ascending, so the child's own top rank is the atom just added.
func conjoin(parent *Node, a atom) *Node {
	keys := append(append([]string{}, parent.keys...), a.key)
	ranks := append(append([]int{}, parent.ranks...), a.rank)
	filters := append(append([]domain.Constraint{}, parent.filters...), a.constraint)
	return &Node{keys: keys, ranks: ranks, filters: filters, canonical: canonicalKeyOf(keys)}
}

// canonicalKeyOf joins per-atom canonical keys into a node key. Each atom key is
// already a CanonicalFilters encoding of a single predicate, and atoms are held
// in ascending rank order, so sorting the parts makes the node key a pure
// function of the atom set — matching CanonicalFilters' own set semantics, down
// to the newline delimiter it joins on.
func canonicalKeyOf(keys []string) string {
	parts := slices.Clone(keys)
	slices.Sort(parts)
	return strings.Join(parts, canonicalSeparator)
}
