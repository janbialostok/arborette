package sleepcycle

import (
	"cmp"
	"math"
	"slices"
	"strings"

	"github.com/arborette/arborette/internal/domain"
	"github.com/arborette/arborette/internal/graph"
)

// uctPolicy is the knowledge-guided SearchPolicy: a PUCT tree search over the same
// conjunction lattice the beam walks level-wise, but descending where the evidence
// and the priors point rather than measuring a whole level before looking deeper.
//
// Three kinds of knowledge steer it, and each degrades to nothing when absent. A
// prior biases which move is tried first: retrieval similarity for an adopted
// conjunction, the critic's ranking for an atom, a uniform floor for anything
// unranked — so an empty corpus and a silent critic leave plain UCT. Causal
// evidence multiplies a measured value by 1 + scale·confidence, and a candidate
// with no verified component multiplies by exactly one, which is what keeps a
// finding of unknown provenance scored as the observational finding it is.
// Grounded proposals enter as whole conjunctions attached to the root, so knowledge
// from another goal can seed a segment this goal's own findings never suggested.
//
// It shares the beam's two hard rules. Support is anti-monotone, so a node measured
// below the floor is never expanded and neither is any candidate whose measured
// subset fell below it. And no candidate conjoins two predicates on one (field,
// op) pair — held here at expansion for atoms, and at validation for the whole
// conjunctions adopted from proposals.
//
// The policy does no I/O. Every piece of knowledge arrives through the constructor
// as plain data, which is what keeps the seam's "policy never talks to the sandbox"
// contract true of a policy whose whole point is that it consults more than the
// lattice.
type uctPolicy struct {
	atoms      []atom
	atomsByKey map[string]atom
	atomOrder  []int
	proposals  []groundedProposal
	priors     map[string]float64
	evidence   map[string]graph.CausalEvidence

	exploration float64
	causalScale float64
	grounding   float64
	floorPrior  float64
	maxOrder    int
	minSupport  int64
	baseline    float64
	direction   domain.TargetDirection

	root        *uctNode
	byCanonical map[string]*uctNode
	memo        map[string]Measurement

	minQ      float64
	maxQ      float64
	rangeSeen bool
}

// uctNode is one node of the search tree: the candidate it stands for, its
// backed-up statistics, and the cursors marking how much of its move set has been
// tried. blocked marks a node the anti-monotone rule forbids expanding — a failed
// measurement or one below the support floor — and exhausted marks a subtree with
// nothing left to offer, which is how Done can answer before Select is asked.
type uctNode struct {
	node     *Node
	parent   *uctNode
	children []*uctNode
	prior    float64

	visits int
	total  float64

	measured  bool
	blocked   bool
	exhausted bool

	nextAtom      int
	nextProposal  int
	atomMoves     int
	proposalMoves int
}

// uctMove is one untried expansion: conjoining an atom onto a node, or adopting a
// grounded proposal whole. Exactly one of atom and proposal is set.
type uctMove struct {
	atom     *atom
	proposal *groundedProposal
	prior    float64
}

func newUCTPolicy(
	atoms []atom,
	proposals []groundedProposal,
	priors map[string]float64,
	evidence map[string]graph.CausalEvidence,
	cfg Config,
	baseline float64,
	direction domain.TargetDirection,
) *uctPolicy {
	// Proposals are ordered strongest-prior first (ties on heuristic id) so the move
	// the budget split offers is a deterministic function of the retrieval. Stable,
	// because the tie-break is not a total order: one heuristic contributes
	// several conjunctions at one distance, so equal (prior, id) pairs are ordinary
	// rather than exceptional, and an unstable sort would leave which of them the
	// budget split reaches to the sort implementation.
	ordered := slices.Clone(proposals)
	slices.SortStableFunc(ordered, func(a, b groundedProposal) int {
		if d := cmp.Compare(b.prior, a.prior); d != 0 {
			return d
		}
		return strings.Compare(a.heuristicID, b.heuristicID)
	})

	p := &uctPolicy{
		atoms:       atoms,
		atomsByKey:  make(map[string]atom, len(atoms)),
		proposals:   ordered,
		priors:      priors,
		evidence:    evidence,
		exploration: cfg.UCTExploration,
		causalScale: cfg.CausalMultiplierScale,
		grounding:   cfg.GroundingFraction,
		maxOrder:    cfg.MaxOrder,
		minSupport:  int64(cfg.MinSupport),
		baseline:    baseline,
		direction:   direction,
		byCanonical: map[string]*uctNode{},
		memo:        map[string]Measurement{},
	}
	for _, a := range atoms {
		p.atomsByKey[a.key] = a
	}
	// The floor is one move's share of the whole move set, so an unranked move is
	// weighted as if the prior mass were spread evenly — the neutral reading of "no
	// opinion", and the value every move takes when there is no knowledge at all.
	if moves := len(atoms) + len(ordered); moves > 0 {
		p.floorPrior = 1 / float64(moves)
	}
	p.atomOrder = expansionOrder(atoms, p.atomPrior)

	// The root is the unfiltered segment. It is measured by construction — the run's
	// global baseline is exactly its value — so it is never selected, and its zero
	// delta is the neutral first-play value an untried move inherits.
	p.root = &uctNode{node: &Node{}, measured: true}
	p.byCanonical[""] = p.root
	// Settled up front so a policy with nothing to search reports Done before the
	// driver's first Select, exactly as one that ran to completion does.
	p.settle(p.root)
	return p
}

// Select walks one simulation from the root and returns the unmeasured candidate it
// reaches. A walk that dead-ends marks the dead end exhausted and restarts, so the
// only way out without a candidate is the root itself running dry — which the eager
// settling in Update has already reported through Done.
func (p *uctPolicy) Select() (*Node, bool) {
	for !p.root.exhausted {
		if node, ok := p.descend(); ok {
			return node, true
		}
	}
	return nil, false
}

// descend walks from the root by PUCT until it reaches a node with an untried move,
// and expands that move.
//
// Expanding before descending is what keeps the search honest about breadth: a
// node's own children are the only evidence about which of its refinements is worth
// pursuing, so committing to a subtree before every sibling has been measured once
// would decide the question on the priors alone. The adaptive part is where the
// expansions land — each simulation re-descends from the root, so a strong branch
// has more of its subtree opened while a weak one keeps only the children it
// earned, which is the difference from a level-wise walk that spends its budget
// evenly and then prunes on width.
func (p *uctPolicy) descend() (*Node, bool) {
	cur := p.root
	for {
		if move, hasMove := p.peekMove(cur); hasMove {
			return p.expand(cur, move).node, true
		}
		best, hasChild := p.bestChild(cur)
		if !hasChild {
			// Unreachable while settling stays eager — a node with no move and no live
			// child is already exhausted, so the descent never enters it. Kept as the
			// defense that a dead end costs one restart rather than a spin.
			cur.exhausted = true
			return nil, false
		}
		cur = best
	}
}

// Update records one measurement and settles the tree eagerly, so the driver's next
// Done call already reflects whether anything remains — the same contract the beam's
// level advance meets.
//
// It is idempotent per canonical key: the driver memoizes measurements and still
// calls Update on a re-selected node, and this policy re-selects by design, so a
// second Update must not backpropagate the same simulation twice.
func (p *uctPolicy) Update(node *Node, m Measurement) {
	n, ok := p.byCanonical[node.canonical]
	if !ok || n.measured {
		return
	}
	n.measured = true
	n.blocked = belowFloor(m, p.minSupport)
	p.memo[node.canonical] = m

	value := p.value(node, m)
	for cur := n; cur != nil; cur = cur.parent {
		cur.visits++
		cur.total += value
		p.observe(q(cur))
	}
	p.settle(p.root)
}

func (p *uctPolicy) Done() bool { return p.root.exhausted }

// value is the node's contribution to every ancestor's estimate: how far the
// measurement moved the objective, discounted by the evidence behind it and
// multiplied by the causal weight of the knowledge it was built from.
//
// A failed measurement contributes nothing rather than a penalty — it is the
// absence of information, not evidence of a bad segment — and the node is blocked
// from expansion separately. The shrinkage is the same support-weighted score
// publication ranks on, so the search and the publication stage cannot disagree
// about which of two segments is stronger.
func (p *uctPolicy) value(node *Node, m Measurement) float64 {
	if m.Failed || m.Support <= 0 {
		return 0
	}
	shrunk := domain.ShrunkScore(m.Value, p.baseline, m.Support, float64(p.minSupport), p.direction)
	return shrunk * p.causalMultiplier(node)
}

// causalMultiplier weights a candidate by the strongest causal evidence behind any
// component it was built from: a verified finding it conjoins, or the heuristic
// that proposed it. With no evidence the multiplier is exactly one, which is the
// rule that keeps a finding of unknown or missing epistemic provenance scored as
// plain observational rather than as causally supported.
func (p *uctPolicy) causalMultiplier(node *Node) float64 {
	best := 0.0
	if node.proposedBy != "" {
		if e, ok := p.evidence[node.proposedBy]; ok {
			best = e.Confidence
		}
	}
	for _, key := range node.keys {
		for _, id := range p.atomsByKey[key].sourceIDs {
			if e, ok := p.evidence[id]; ok && e.Confidence > best {
				best = e.Confidence
			}
		}
	}
	return 1 + p.causalScale*best
}

// childScore is the PUCT ranking of one child against its siblings: how well it has
// measured, plus an exploration bonus that its prior scales and its own visit count
// decays. A child that keeps being descended into has to keep earning it, while a
// strong prior buys a branch attention before it has any measurements to argue with.
func (p *uctPolicy) childScore(parent *uctNode, child *uctNode) float64 {
	return p.normalized(q(child)) +
		p.exploration*child.prior*math.Sqrt(float64(parent.visits))/float64(1+child.visits)
}

func q(n *uctNode) float64 {
	if n.visits == 0 {
		return 0
	}
	return n.total / float64(n.visits)
}

// normalized rescales an estimate into [0, 1] against the range this run has
// actually seen, so the exploration constant means the same thing on every
// objective.
//
// Without it the constant would be uncomparable across goals and useless as a
// default: estimates are objective-scaled deltas, so a revenue objective measured in
// thousands would drown any exploration bonus while a rate objective in [0, 1] would
// be drowned by it. The same scaling is what gives the search its widening
// behaviour — a bonus that grows with √N eventually overtakes a normalized estimate
// that cannot exceed 1, so a run that has exploited its best branch returns to the
// moves it never tried. Before two distinct estimates exist the range is degenerate
// and every node scores alike, which leaves the priors deciding.
func (p *uctPolicy) normalized(value float64) float64 {
	if p.maxQ <= p.minQ {
		return 0
	}
	return (value - p.minQ) / (p.maxQ - p.minQ)
}

// observe widens the normalization range to include one estimate. The range spans
// every node's estimate rather than the raw measurements, because it is estimates
// the comparison ranks.
func (p *uctPolicy) observe(value float64) {
	if !p.rangeSeen || value < p.minQ {
		p.minQ = value
	}
	if !p.rangeSeen || value > p.maxQ {
		p.maxQ = value
	}
	p.rangeSeen = true
}

// bestChild is the highest-scoring child whose subtree still has something to
// offer. Ties break on canonical filter so a re-run walks the same path.
func (p *uctPolicy) bestChild(n *uctNode) (*uctNode, bool) {
	var best *uctNode
	var bestScore float64
	for _, c := range n.children {
		if c.exhausted {
			continue
		}
		score := p.childScore(n, c)
		if best == nil || score > bestScore ||
			(score == bestScore && strings.Compare(c.node.canonical, best.node.canonical) < 0) {
			best, bestScore = c, score
		}
	}
	return best, best != nil
}

// peekMove returns the next admissible untried move at n without consuming it, so
// the caller can weigh expanding against descending before committing to either.
//
// The per-node cursors only ever advance, which is sound because every reason a
// move is inadmissible is permanent: the order cap and the (field, op) conflict are
// properties of the node, a canonical key already in the tree stays in it, and the
// Apriori evidence that pruned a candidate only accumulates.
func (p *uctPolicy) peekMove(n *uctNode) (uctMove, bool) {
	if n.blocked {
		return uctMove{}, false
	}
	proposal, hasProposal := p.peekProposal(n)
	atomMove, hasAtom := p.peekAtom(n)
	switch {
	case hasProposal && (!hasAtom || p.preferProposal(n)):
		return proposal, true
	case hasAtom:
		return atomMove, true
	default:
		return uctMove{}, false
	}
}

// preferProposal applies the expansion budget split: grounding moves take their
// configured share of the root's expansions, and atom moves take the rest. It is a
// running ratio rather than a reserved quota so the split holds at every point in
// the run, not only once the budget is spent.
func (p *uctPolicy) preferProposal(n *uctNode) bool {
	total := n.atomMoves + n.proposalMoves
	if total == 0 {
		return p.grounding > 0
	}
	return float64(n.proposalMoves)/float64(total) < p.grounding
}

// peekProposal advances past proposals already in the tree and returns the next
// adoptable one. Proposals attach to the root only: each is a whole conjunction,
// already the segment its heuristic describes, so conjoining one onto a node would
// measure a segment neither the heuristic nor this goal proposed.
func (p *uctPolicy) peekProposal(n *uctNode) (uctMove, bool) {
	if n != p.root {
		return uctMove{}, false
	}
	for n.nextProposal < len(p.proposals) {
		prop := p.proposals[n.nextProposal]
		// The anti-monotone rule binds an adopted conjunction exactly as it binds one
		// the search conjoined: support cannot grow by conjoining, so a proposal whose
		// predicate was already measured below the floor is provably below it too, and
		// measuring it would spend budget on a segment the winner gate must reject.
		_, present := p.byCanonical[prop.canonical]
		if present || prunedBySubset(prop.keys, p.memo, p.minSupport) {
			n.nextProposal++
			continue
		}
		return uctMove{proposal: &p.proposals[n.nextProposal], prior: p.proposalPrior(prop.prior)}, true
	}
	return uctMove{}, false
}

// expansionOrder is the order a node tries its atom moves in: one predicate per
// column before a second predicate on any column, with the columns themselves
// visited strongest-prior first.
//
// The interleaving is what keeps a budget from being spent on one axis. The
// vocabulary is enumerated per column, so a column contributes a predicate per
// distinct value and two per quantile cut, and those are near-restatements of each
// other — a node that tried them in vocabulary order would spend its first several
// expansions re-cutting a single column before reaching a second one. Interaction
// effects live between columns, so covering columns first is what puts them within
// reach of a run that cannot try everything.
//
// The prior orders the columns rather than the individual atoms, which is the only
// way both properties survive: priors are assigned per column, so sorting atoms by
// prior would group each column's predicates back together and undo the
// interleaving exactly when a critic has an opinion — the case it is most needed.
func expansionOrder(atoms []atom, columnPrior func(a atom) float64) []int {
	byColumn := map[string][]int{}
	var columns []string
	for i, a := range atoms {
		field := strings.ToLower(a.constraint.Field)
		if _, seen := byColumn[field]; !seen {
			columns = append(columns, field)
		}
		byColumn[field] = append(byColumn[field], i)
	}
	// Stable over first-appearance order, which is the vocabulary's own rank order,
	// so unranked columns keep a reproducible sequence.
	slices.SortStableFunc(columns, func(a, b string) int {
		return cmp.Compare(columnPrior(atoms[byColumn[b][0]]), columnPrior(atoms[byColumn[a][0]]))
	})

	order := make([]int, 0, len(atoms))
	for round := 0; len(order) < len(atoms); round++ {
		for _, field := range columns {
			if round < len(byColumn[field]) {
				order = append(order, byColumn[field][round])
			}
		}
	}
	return order
}

// peekAtom advances past atoms already tried or inadmissible here and returns the
// next one to conjoin. The walk follows the prior order, not the vocabulary's own
// rank order, so the critic's ranking decides which predicates a node tries first
// under a budget that may not reach them all.
func (p *uctPolicy) peekAtom(n *uctNode) (uctMove, bool) {
	if n.node.Order() >= p.maxOrder {
		return uctMove{}, false
	}
	for n.nextAtom < len(p.atomOrder) {
		at := p.atomOrder[n.nextAtom]
		if !p.admissibleAtom(n, p.atoms[at]) {
			n.nextAtom++
			continue
		}
		return uctMove{atom: &p.atoms[at], prior: p.atomPrior(p.atoms[at])}, true
	}
	return uctMove{}, false
}

func (p *uctPolicy) admissibleAtom(n *uctNode, a atom) bool {
	if conflictsWithNode(n.node, a) {
		return false
	}
	child := conjoin(n.node, a)
	if _, present := p.byCanonical[child.canonical]; present {
		return false
	}
	return !prunedBySubset(child.keys, p.memo, p.minSupport)
}

// atomPrior is an atom move's prior mass: what the critic's ranking gave its
// column, or the uniform floor when nothing ranked it.
//
// A non-positive prior falls to the floor rather than through, since a zero would
// make the move unreachable by exploration rather than merely unfavoured.
func (p *uctPolicy) atomPrior(a atom) float64 {
	if ranked, ok := p.priors[a.key]; ok && ranked > 0 {
		return ranked
	}
	return p.floorPrior
}

// proposalPrior is an adopted conjunction's prior mass: the retrieval similarity
// the producer computed, or the floor when the corpus offered no distance to weigh
// it by.
func (p *uctPolicy) proposalPrior(supplied float64) float64 {
	if supplied > 0 {
		return supplied
	}
	return p.floorPrior
}

func (p *uctPolicy) expand(n *uctNode, move uctMove) *uctNode {
	var node *Node
	if move.proposal != nil {
		node = p.proposalNode(*move.proposal)
		n.nextProposal++
		n.proposalMoves++
	} else {
		node = conjoin(n.node, *move.atom)
		n.nextAtom++
		n.atomMoves++
	}
	child := &uctNode{node: node, parent: n, prior: move.prior}
	n.children = append(n.children, child)
	p.byCanonical[node.canonical] = child
	return child
}

// proposalNode reifies an adopted conjunction as a search Node. Each predicate
// carries the vocabulary's rank when the vocabulary already has that predicate and
// a synthetic rank past it otherwise, so the node's ascending-rank shape holds
// without minting vocabulary entries a proposal never earned.
func (p *uctPolicy) proposalNode(prop groundedProposal) *Node {
	type predicate struct {
		key    string
		rank   int
		filter domain.Constraint
	}
	predicates := make([]predicate, 0, len(prop.keys))
	for i, key := range prop.keys {
		rank := len(p.atoms) + i
		if a, ok := p.atomsByKey[key]; ok {
			rank = a.rank
		}
		predicates = append(predicates, predicate{key: key, rank: rank, filter: prop.filters[i]})
	}
	slices.SortFunc(predicates, func(a, b predicate) int { return cmp.Compare(a.rank, b.rank) })

	node := &Node{
		keys:       make([]string, 0, len(predicates)),
		ranks:      make([]int, 0, len(predicates)),
		filters:    make([]domain.Constraint, 0, len(predicates)),
		canonical:  prop.canonical,
		proposedBy: prop.heuristicID,
	}
	for _, pred := range predicates {
		node.keys = append(node.keys, pred.key)
		node.ranks = append(node.ranks, pred.rank)
		node.filters = append(node.filters, pred.filter)
	}
	return node
}

// settle recomputes exhaustion across the whole tree and reports whether n's
// subtree has anything left to offer. A node is exhausted once it has no admissible
// move and every child's subtree is exhausted too.
//
// It walks the whole tree rather than only the path that just changed, because
// expanding one branch can empty another: a conjunction is reachable by several
// descents and is only ever instantiated once, so the branch that lost the race
// silently loses its last move. Settling that branch's ancestors alone would leave
// the root reporting not-Done with nothing to select, which the driver records as a
// seam violation rather than the clean exhaustion it is. The tree is bounded by the
// measurement budget and each node's move check is amortized constant, so the pass
// is cheap enough to run on every update.
func (p *uctPolicy) settle(n *uctNode) bool {
	if n.exhausted {
		return true
	}
	live := false
	for _, c := range n.children {
		if !p.settle(c) {
			live = true
		}
	}
	if !live {
		if _, hasMove := p.peekMove(n); !hasMove {
			n.exhausted = true
		}
	}
	return n.exhausted
}
