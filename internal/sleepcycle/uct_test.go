package sleepcycle

import (
	"testing"

	"github.com/arborette/arborette/internal/domain"
	"github.com/arborette/arborette/internal/graph"
)

// atomsFor builds a ranked vocabulary from single-predicate constraints, the shape
// both policies consume.
func atomsFor(t *testing.T, constraints ...domain.Constraint) []atom {
	t.Helper()
	atoms := make([]atom, 0, len(constraints))
	for _, c := range constraints {
		atoms = append(atoms, atom{key: mustCanonical(t, []domain.Constraint{c}), constraint: c})
	}
	return rankAtoms(atoms)
}

// drainPolicy runs a policy to completion against a fixed measurement, collecting
// every candidate it generated. It is the harness the invariant and termination
// tests share: the driver itself is not involved, so a policy that misbehaves shows
// up here rather than as a search that merely looks short.
func drainPolicy(t *testing.T, p SearchPolicy, m Measurement) []*Node {
	t.Helper()
	var generated []*Node
	for i := 0; !p.Done(); i++ {
		if i > 10000 {
			t.Fatal("policy did not terminate")
		}
		node, ok := p.Select()
		if !ok {
			t.Fatalf("policy selected nothing while reporting not-Done, at candidate %d", i)
		}
		generated = append(generated, node)
		p.Update(node, m)
	}
	return generated
}

// TestUCTUpdateIsIdempotent guards the backpropagation accounting the way the beam's
// level advance is guarded. The driver memoizes measurements but still calls Update
// on a re-selected node, and this policy re-selects by design, so a second Update
// would count one simulation twice all the way to the root.
func TestUCTUpdateIsIdempotent(t *testing.T) {
	atoms := atomsFor(t, atomOn("a"), atomOn("b"))
	p := newUCTPolicy(atoms, nil, nil, nil, uctConfig(), 0, domain.Maximize)

	node, ok := p.Select()
	if !ok {
		t.Fatal("expected a first candidate")
	}
	m := Measurement{Value: 1, Support: 100}
	p.Update(node, m)
	visits, total := p.root.visits, p.root.total

	p.Update(node, m)
	if p.root.visits != visits || p.root.total != total {
		t.Fatalf("a repeated Update backpropagated twice: visits %d -> %d, total %v -> %v",
			visits, p.root.visits, total, p.root.total)
	}
}

// TestUCTSelectNeverContradictsDone is the seam contract's other half: the driver
// records an ok=false Select as a policy violation, so a policy that runs out must
// have reported Done first. Settling eagerly inside Update is what makes that true.
func TestUCTSelectNeverContradictsDone(t *testing.T) {
	atoms := atomsFor(t, atomOn("a"), atomOn("b"), atomOn("c"))
	p := newUCTPolicy(atoms, nil, nil, nil, uctConfig(), 0, domain.Maximize)

	drainPolicy(t, p, Measurement{Value: 1, Support: 100})
}

// TestUCTIsDoneWithNothingToSearch: with no atoms and no proposals there is no
// candidate at all, and the driver asks Done before its first Select.
func TestUCTIsDoneWithNothingToSearch(t *testing.T) {
	p := newUCTPolicy(nil, nil, nil, nil, uctConfig(), 0, domain.Maximize)
	if !p.Done() {
		t.Fatal("a policy with no moves must report Done before the first Select")
	}
}

// TestUCTGeneratesNoDuplicateFieldOpConjunction is the policy contract's candidate
// invariant, at the vocabulary shape that makes it bite: one column contributing
// many equality atoms and many threshold cuts, which is exactly what the
// schema-derived vocabulary produces. Two equalities on one column match nothing and
// two thresholds restate one bound, so either would spend budget saying nothing
// about interaction.
func TestUCTGeneratesNoDuplicateFieldOpConjunction(t *testing.T) {
	atoms, err := buildSchemaAtoms(schemaFixture(), 4)
	if err != nil {
		t.Fatalf("build schema atoms: %v", err)
	}
	p := newUCTPolicy(rankAtoms(atoms), nil, nil, nil, uctConfig(), 0, domain.Maximize)

	for _, node := range drainPolicy(t, p, Measurement{Value: 1, Support: 100}) {
		assertNoDuplicateFieldOp(t, "uct", node)
	}
}

// TestBeamGeneratesNoDuplicateFieldOpConjunction pins the same invariant on the
// other implementation, over the same vocabulary.
func TestBeamGeneratesNoDuplicateFieldOpConjunction(t *testing.T) {
	atoms, err := buildSchemaAtoms(schemaFixture(), 4)
	if err != nil {
		t.Fatalf("build schema atoms: %v", err)
	}
	p := newBeamPolicy(rankAtoms(atoms), testConfig(), 0, domain.Maximize)

	for _, node := range drainPolicy(t, p, Measurement{Value: 1, Support: 100}) {
		assertNoDuplicateFieldOp(t, "beam", node)
	}
}

func assertNoDuplicateFieldOp(t *testing.T, policy string, node *Node) {
	t.Helper()
	if duplicateFieldOp(node.filters) {
		t.Fatalf("%s generated a candidate conjoining one (field, op) twice: %s", policy, node.canonical)
	}
}

// TestUCTAdoptsAGroundedProposal: a validated proposal enters as a whole conjunction
// attached to the root, carrying the heuristic that proposed it so the winner's
// provenance survives write-back.
func TestUCTAdoptsAGroundedProposal(t *testing.T) {
	prop := proposalFor(t, "mh-1", 0.9, atomOn("x"), atomOn("y"))
	p := newUCTPolicy(nil, []groundedProposal{prop}, nil, nil, uctConfig(), 0, domain.Maximize)

	node, ok := p.Select()
	if !ok {
		t.Fatal("a validated proposal must be selectable with no atoms at all")
	}
	if node.canonical != prop.canonical {
		t.Fatalf("adopted node = %q, want the proposal's own conjunction %q", node.canonical, prop.canonical)
	}
	if node.proposedBy != "mh-1" {
		t.Fatalf("proposedBy = %q, want the proposing heuristic", node.proposedBy)
	}
	if node.Order() != 2 {
		t.Fatalf("an adopted conjunction keeps its own order, got %d", node.Order())
	}
}

// TestUCTWithNoKnowledgeIsPlainSearch: an empty corpus and a silent critic leave
// every move on the uniform floor prior, which is the degradation that lets this
// policy ship before any knowledge has accumulated.
func TestUCTWithNoKnowledgeIsPlainSearch(t *testing.T) {
	atoms := atomsFor(t, atomOn("a"), atomOn("b"))
	p := newUCTPolicy(atoms, nil, nil, nil, uctConfig(), 0, domain.Maximize)

	if p.floorPrior != 0.5 {
		t.Fatalf("floor prior = %v, want one move's share of the two-move set", p.floorPrior)
	}
	for _, a := range atoms {
		if got := p.atomPrior(a); got != p.floorPrior {
			t.Fatalf("an unranked move must sit on the floor prior, got %v", got)
		}
	}
	drainPolicy(t, p, Measurement{Value: 1, Support: 100})
}

// TestUCTCausalMultiplierWeightsVerifiedEvidence pins the additivity rule: evidence
// scales a value estimate up, and its absence multiplies by exactly one — a finding
// with unknown or missing provenance is scored as the observational finding it is,
// never as a causally supported one.
func TestUCTCausalMultiplierWeightsVerifiedEvidence(t *testing.T) {
	verified := atom{key: "k1", constraint: atomOn("a"), sourceIDs: []string{"i-verified"}}
	plain := atom{key: "k2", constraint: atomOn("b"), sourceIDs: []string{"i-plain"}}
	evidence := map[string]graph.CausalEvidence{"i-verified": {Confidence: 0.5, EffectSize: 2}}
	p := newUCTPolicy(rankAtoms([]atom{verified, plain}), nil, nil, evidence, uctConfig(), 0, domain.Maximize)

	weighted := p.causalMultiplier(&Node{keys: []string{"k1"}})
	unweighted := p.causalMultiplier(&Node{keys: []string{"k2"}})
	if unweighted != 1 {
		t.Fatalf("a candidate with no causal evidence must multiply by exactly 1, got %v", unweighted)
	}
	// The scale is 1 in the test config, so a confidence of 0.5 lands at 1.5.
	if weighted != 1.5 {
		t.Fatalf("a verified component must scale by 1 + scale·confidence, got %v", weighted)
	}
}

// TestUCTRespectsTheSupportFloor: the anti-monotone rule is the beam's own, and it
// has to hold here too — a node measured below the floor is never expanded, and
// neither is any candidate whose measured subset fell below it.
func TestUCTRespectsTheSupportFloor(t *testing.T) {
	cfg := uctConfig()
	cfg.MinSupport = 50
	atoms := atomsFor(t, atomOn("a"), atomOn("b"), atomOn("c"))
	p := newUCTPolicy(atoms, nil, nil, nil, cfg, 0, domain.Maximize)

	var generated []*Node
	for !p.Done() {
		node, ok := p.Select()
		if !ok {
			t.Fatal("policy selected nothing while reporting not-Done")
		}
		generated = append(generated, node)
		// Every order-1 candidate on column "a" is starved; everything else clears.
		if node.Order() == 1 && conjoins(node.filters, "a") {
			p.Update(node, Measurement{Value: 1, Support: 10})
			continue
		}
		p.Update(node, Measurement{Value: 1, Support: 100})
	}

	for _, node := range generated {
		if node.Order() >= 2 && conjoins(node.filters, "a") {
			t.Fatalf("a superset of a below-floor atom was measured: %s", node.canonical)
		}
	}
}

// TestUCTStopsAtTheOrderCap: MaxOrder bounds the conjunctions the search forms, and
// the tree has to honour it however it descends.
func TestUCTStopsAtTheOrderCap(t *testing.T) {
	cfg := uctConfig()
	cfg.MaxOrder = 2
	atoms := atomsFor(t, atomOn("a"), atomOn("b"), atomOn("c"))
	p := newUCTPolicy(atoms, nil, nil, nil, cfg, 0, domain.Maximize)

	for _, node := range drainPolicy(t, p, Measurement{Value: 1, Support: 100}) {
		if node.Order() > cfg.MaxOrder {
			t.Fatalf("candidate of order %d exceeds the cap %d: %s", node.Order(), cfg.MaxOrder, node.canonical)
		}
	}
}

// TestUCTMeasuresEachConjunctionOnce: the same predicate set is reachable by several
// descents, and the tree must reach it once — otherwise a duplicate node would sit
// unmeasured forever, since the driver serves the second one from its memo and the
// idempotent Update declines to record it.
func TestUCTMeasuresEachConjunctionOnce(t *testing.T) {
	atoms := atomsFor(t, atomOn("a"), atomOn("b"), atomOn("c"))
	p := newUCTPolicy(atoms, nil, nil, nil, uctConfig(), 0, domain.Maximize)

	seen := map[string]bool{}
	for _, node := range drainPolicy(t, p, Measurement{Value: 1, Support: 100}) {
		if seen[node.canonical] {
			t.Fatalf("candidate %s was generated twice", node.canonical)
		}
		seen[node.canonical] = true
	}
}

// proposalFor builds a validated proposal directly, so a policy test does not have
// to drive the whole retrieval and grounding path to get one.
func proposalFor(t *testing.T, heuristicID string, prior float64, filters ...domain.Constraint) groundedProposal {
	t.Helper()
	keys := make([]string, 0, len(filters))
	for _, f := range filters {
		keys = append(keys, mustCanonical(t, []domain.Constraint{f}))
	}
	return groundedProposal{
		filters:     filters,
		keys:        keys,
		canonical:   canonicalKeyOf(keys),
		heuristicID: heuristicID,
		prior:       prior,
	}
}

// TestUCTPriorsSteerWhichMovesAreTried is the pin on the critic's whole value
// chain. The ranking is only worth paying a model call for if it changes what the
// search tries first under a budget that cannot reach everything, so this asserts
// the traversal itself: a ranked column's predicate is expanded before an unranked
// column's, which is false the moment the prior lookup is dropped.
func TestUCTPriorsSteerWhichMovesAreTried(t *testing.T) {
	// "zeta" sorts after "alpha" by canonical key, so the vocabulary's own order
	// would try alpha first — only the prior can reverse it, which is what makes
	// this assertion discriminate a consumed ranking from an ignored one.
	ranked := atomOn("zeta")
	atoms := atomsFor(t, atomOn("alpha"), ranked)
	priors := map[string]float64{mustCanonical(t, []domain.Constraint{ranked}): 1.0}

	unbiased := newUCTPolicy(atoms, nil, nil, nil, uctConfig(), 0, domain.Maximize)
	first, ok := unbiased.Select()
	if !ok {
		t.Fatal("expected a first candidate")
	}
	if first.filters[0].Field != "alpha" {
		t.Fatalf("without a ranking the vocabulary order decides, got %q", first.filters[0].Field)
	}

	p := newUCTPolicy(atoms, nil, priors, nil, uctConfig(), 0, domain.Maximize)
	first, ok = p.Select()
	if !ok {
		t.Fatal("expected a first candidate")
	}
	if first.filters[0].Field != "zeta" {
		t.Fatalf("first candidate = %q, want the critic's top-ranked column tried first", first.filters[0].Field)
	}
}

// TestUCTRetrievalPriorReachesTheMove: the retrieval distance is the only thing
// ranking one adopted conjunction against another, so it has to arrive as the
// move's prior mass rather than stopping at the proposal struct.
func TestUCTRetrievalPriorReachesTheMove(t *testing.T) {
	near := proposalFor(t, "mh-near", 0.95, atomOn("p"), atomOn("q"))
	far := proposalFor(t, "mh-far", 0.10, atomOn("r"), atomOn("s"))
	p := newUCTPolicy(nil, []groundedProposal{far, near}, nil, nil, uctConfig(), 0, domain.Maximize)

	move, ok := p.peekMove(p.root)
	if !ok {
		t.Fatal("expected a proposal move")
	}
	if move.prior != 0.95 {
		t.Fatalf("move prior = %v, want the retrieval similarity of the nearest proposal", move.prior)
	}
	if move.proposal.heuristicID != "mh-near" {
		t.Fatalf("the strongest-prior proposal must be offered first, got %q", move.proposal.heuristicID)
	}
}

// TestUCTGroundingFractionSplitsRootExpansions pins the budget knob: with atoms and
// proposals both available, the share of the root's expansions spent adopting
// conjunctions follows the configured fraction rather than exhausting one kind
// first.
func TestUCTGroundingFractionSplitsRootExpansions(t *testing.T) {
	atoms := atomsFor(t, atomOn("a"), atomOn("b"), atomOn("c"), atomOn("d"))
	proposals := []groundedProposal{
		proposalFor(t, "mh-1", 0.9, atomOn("p"), atomOn("q")),
		proposalFor(t, "mh-2", 0.8, atomOn("r"), atomOn("s")),
	}

	// At a fraction of 0 no expansion is spent on a proposal while any atom remains.
	none := uctConfig()
	none.GroundingFraction = 0
	p := newUCTPolicy(atoms, proposals, nil, nil, none, 0, domain.Maximize)
	for i := 0; i < len(atoms); i++ {
		node, ok := p.Select()
		if !ok {
			t.Fatalf("expected an atom candidate at %d", i)
		}
		if node.proposedBy != "" {
			t.Fatalf("a fraction of 0 must exhaust atoms first, got the proposal %q at %d", node.proposedBy, i)
		}
		p.Update(node, Measurement{Value: 1, Support: 100})
	}

	// At a fraction of 1 the very first root expansion is a proposal.
	all := uctConfig()
	all.GroundingFraction = 1
	q := newUCTPolicy(atoms, proposals, nil, nil, all, 0, domain.Maximize)
	first, ok := q.Select()
	if !ok {
		t.Fatal("expected a first candidate")
	}
	if first.proposedBy == "" {
		t.Fatalf("a fraction of 1 must spend the first root expansion on a proposal, got %s", first.canonical)
	}
}

// TestUCTFailedMeasurementBlocksExpansion: a segment whose sandbox call errored has
// unknown support, which the anti-monotone rule treats as insufficient — so its
// subtree is never opened, exactly as a below-floor one is.
func TestUCTFailedMeasurementBlocksExpansion(t *testing.T) {
	atoms := atomsFor(t, atomOn("a"), atomOn("b"), atomOn("c"))
	p := newUCTPolicy(atoms, nil, nil, nil, uctConfig(), 0, domain.Maximize)

	var generated []*Node
	for !p.Done() {
		node, ok := p.Select()
		if !ok {
			t.Fatal("policy selected nothing while reporting not-Done")
		}
		generated = append(generated, node)
		if node.Order() == 1 && conjoins(node.filters, "a") {
			p.Update(node, Measurement{Failed: true})
			continue
		}
		p.Update(node, Measurement{Value: 1, Support: 100})
	}

	for _, node := range generated {
		if node.Order() >= 2 && conjoins(node.filters, "a") {
			t.Fatalf("a superset of a failed measurement was generated: %s", node.canonical)
		}
	}
}

// TestUCTAdoptedProposalRespectsTheSupportFloor: the anti-monotone rule binds an
// adopted conjunction too. A proposal whose predicate was already measured below the
// floor cannot clear it, so measuring it would spend budget the winner gate must
// reject.
//
// The grounding fraction is 0 so the atoms are measured before any proposal is
// offered, which is what makes the evidence available to prune on. The rule is
// evidence-based, so a proposal offered before its predicates were measured is
// admissible by construction — the reachable case is the one pinned here, where the
// root reaches a proposal after its own vocabulary has been measured.
func TestUCTAdoptedProposalRespectsTheSupportFloor(t *testing.T) {
	cfg := uctConfig()
	cfg.MinSupport = 50
	cfg.GroundingFraction = 0
	starved := atomOn("a")
	atoms := atomsFor(t, starved, atomOn("b"))
	proposal := proposalFor(t, "mh-1", 0.1, starved, atomOn("z"))
	p := newUCTPolicy(atoms, []groundedProposal{proposal}, nil, nil, cfg, 0, domain.Maximize)

	measured := map[string]bool{}
	for !p.Done() {
		node, ok := p.Select()
		if !ok {
			t.Fatal("policy selected nothing while reporting not-Done")
		}
		measured[node.canonical] = true
		if node.Order() == 1 && conjoins(node.filters, "a") {
			p.Update(node, Measurement{Value: 1, Support: 10})
			continue
		}
		p.Update(node, Measurement{Value: 1, Support: 100})
	}

	if measured[proposal.canonical] {
		t.Fatalf("a proposal containing a below-floor predicate was measured: %s", proposal.canonical)
	}
}

// TestExpansionOrderCoversColumnsBeforeRepeatingOne: the vocabulary enumerates many
// near-restatements per column, so a node must reach a second column before a
// second predicate on the first — and the critic's ranking must order the columns
// without collapsing that interleaving, which is what makes the ranking useful
// rather than counterproductive.
func TestExpansionOrderCoversColumnsBeforeRepeatingOne(t *testing.T) {
	atoms, err := buildSchemaAtoms(schemaFixture(), 3)
	if err != nil {
		t.Fatalf("build schema atoms: %v", err)
	}
	atoms = rankAtoms(atoms)

	assertInterleaved := func(name string, order []int) {
		t.Helper()
		seen := map[string]bool{}
		columns := 0
		for _, c := range schemaFixture().Columns {
			for _, a := range atoms {
				if a.constraint.Field == c.Name {
					columns++
					break
				}
			}
		}
		for i, at := range order[:columns] {
			field := atoms[at].constraint.Field
			if seen[field] {
				t.Fatalf("%s: column %q repeated at position %d before every column was covered", name, field, i)
			}
			seen[field] = true
		}
	}

	unranked := newUCTPolicy(atoms, nil, nil, nil, uctConfig(), 0, domain.Maximize)
	assertInterleaved("unranked", unranked.atomOrder)

	// A ranking over one column must move it to the front without grouping all of
	// its predicates there.
	priors := map[string]float64{}
	for _, a := range atoms {
		if a.constraint.Field == "Age" {
			priors[a.key] = 1.0
		}
	}
	ranked := newUCTPolicy(atoms, nil, priors, nil, uctConfig(), 0, domain.Maximize)
	if atoms[ranked.atomOrder[0]].constraint.Field != "Age" {
		t.Fatalf("the ranked column must be tried first, got %q", atoms[ranked.atomOrder[0]].constraint.Field)
	}
	assertInterleaved("ranked", ranked.atomOrder)
}

// TestPoliciesNeverMeasureASupersetOfAStarvedPredicate pins the anti-monotone rule
// on both implementations, at the shape that defeats the (k−1) subset family alone:
// the starved predicate is the highest-ranked atom, so the beam reaches the order-3
// candidate from a frequent order-2 frontier node whose own subsets say nothing
// against it, and the tree search reaches it by conjoining in an order where the
// starved pairs were refused and therefore never measured.
//
// Support cannot grow by conjoining, so every such candidate is provably below the
// floor and the measurement is pure waste — bounded only by the vocabulary size and
// the order cap, which the schema-derived vocabulary makes large.
func TestPoliciesNeverMeasureASupersetOfAStarvedPredicate(t *testing.T) {
	cfg := testConfig()
	cfg.MinSupport = 50
	cfg.MaxOrder = 3
	// Canonical-key order makes "zzz" the highest-ranked atom, which is the case the
	// beam's ascending-rank generation cannot protect against.
	starved := atomOn("zzz")
	atoms := atomsFor(t, atomOn("aaa"), atomOn("bbb"), starved)

	policies := map[string]SearchPolicy{
		"beam": newBeamPolicy(atoms, cfg, 0, domain.Maximize),
		"uct":  newUCTPolicy(atoms, nil, nil, nil, uctWithSupport(cfg), 0, domain.Maximize),
	}
	for name, p := range policies {
		t.Run(name, func(t *testing.T) {
			for !p.Done() {
				node, ok := p.Select()
				if !ok {
					t.Fatal("policy selected nothing while reporting not-Done")
				}
				if node.Order() >= 2 && conjoins(node.filters, "zzz") {
					t.Fatalf("measured a superset of a starved predicate: %s", node.canonical)
				}
				if conjoins(node.filters, "zzz") {
					p.Update(node, Measurement{Value: 1, Support: 5})
					continue
				}
				p.Update(node, Measurement{Value: 1, Support: 100})
			}
		})
	}
}

// uctWithSupport carries a beam-shaped config onto the knowledge-guided policy, so
// both run the same tuning in a shared table.
func uctWithSupport(cfg Config) Config {
	cfg.Policy = policyUCT
	return cfg
}

// TestUCTCausalEvidenceChangesTheValueEstimate pins the compounding claim end to
// end, not just the multiplier arithmetic. Verified evidence has to reach the value
// a subtree is scored on — through `value()`, through the constructor, and for both
// the atom and the proposing-heuristic sides — or the whole Engine-B→Engine-A link
// is decorative.
func TestUCTCausalEvidenceChangesTheValueEstimate(t *testing.T) {
	a := atom{key: "k1", constraint: atomOn("a"), sourceIDs: []string{"i-verified"}}
	measurement := Measurement{Value: 3, Support: 100}
	node := &Node{keys: []string{"k1"}}

	plain := newUCTPolicy(rankAtoms([]atom{a}), nil, nil, nil, uctConfig(), 1, domain.Maximize)
	weighted := newUCTPolicy(rankAtoms([]atom{a}), nil, nil,
		map[string]graph.CausalEvidence{"i-verified": {Confidence: 1}}, uctConfig(), 1, domain.Maximize)

	unweightedValue := plain.value(node, measurement)
	weightedValue := weighted.value(node, measurement)
	if unweightedValue <= 0 {
		t.Fatalf("an improving measurement must carry a positive estimate, got %v", unweightedValue)
	}
	// Scale is 1 and confidence is 1, so a fully-verified component doubles it.
	if weightedValue != unweightedValue*2 {
		t.Fatalf("verified evidence must scale the estimate: %v vs %v", weightedValue, unweightedValue)
	}

	// The proposing heuristic is the other credit path, and it is reached only
	// through the node's proposedBy rather than through any atom's source ids.
	proposed := &Node{keys: []string{"k1"}, proposedBy: "mh-1"}
	byHeuristic := newUCTPolicy(rankAtoms([]atom{a}), nil, nil,
		map[string]graph.CausalEvidence{"mh-1": {Confidence: 1}}, uctConfig(), 1, domain.Maximize)
	if got := byHeuristic.value(proposed, measurement); got != unweightedValue*2 {
		t.Fatalf("a verified proposing heuristic must scale the estimate too, got %v", got)
	}
}

// TestUCTAdoptedProposalStarvedAtOrderThree is the shape that discriminates the
// per-predicate rule from the (k−1) family: for a two-predicate proposal both
// families check the same singletons, so only a three-predicate conjunction whose
// starved predicate never had a measured pair can tell them apart.
func TestUCTAdoptedProposalStarvedAtOrderThree(t *testing.T) {
	cfg := uctConfig()
	cfg.MinSupport = 50
	cfg.MaxOrder = 3
	cfg.GroundingFraction = 0
	starved := atomOn("a")
	atoms := atomsFor(t, starved, atomOn("b"))
	// The proposal's other two predicates are outside the vocabulary, so no pair of
	// them is ever measured and the (k−1) family has nothing to say.
	proposal := proposalFor(t, "mh-1", 0.1, starved, atomOn("y"), atomOn("z"))
	p := newUCTPolicy(atoms, []groundedProposal{proposal}, nil, nil, cfg, 0, domain.Maximize)

	for !p.Done() {
		node, ok := p.Select()
		if !ok {
			t.Fatal("policy selected nothing while reporting not-Done")
		}
		if node.canonical == proposal.canonical {
			t.Fatalf("measured a proposal containing a starved predicate: %s", node.canonical)
		}
		if node.Order() == 1 && conjoins(node.filters, "a") {
			p.Update(node, Measurement{Value: 1, Support: 10})
			continue
		}
		p.Update(node, Measurement{Value: 1, Support: 100})
	}
}

// TestUCTProposalsAttachToTheRootOnly: an adopted conjunction is already the whole
// segment its heuristic describes, so conjoining one onto a node would measure a
// segment neither the heuristic nor this goal proposed.
func TestUCTProposalsAttachToTheRootOnly(t *testing.T) {
	atoms := atomsFor(t, atomOn("a"), atomOn("b"))
	proposal := proposalFor(t, "mh-1", 0.9, atomOn("x"), atomOn("y"))
	p := newUCTPolicy(atoms, []groundedProposal{proposal}, nil, nil, uctConfig(), 0, domain.Maximize)

	for _, node := range drainPolicy(t, p, Measurement{Value: 1, Support: 100}) {
		if node.proposedBy == "" || node.canonical == proposal.canonical {
			continue
		}
		// A refinement of the adopted node is legitimate; anything else means a
		// proposal was adopted below the root.
		if !conjoins(node.filters, "x") || !conjoins(node.filters, "y") {
			t.Fatalf("a proposal was adopted somewhere other than the root: %s", node.canonical)
		}
	}
}

// TestUCTPriorFloorsAtZero: a zero prior would make a move unreachable by the
// exploration bonus rather than merely unfavoured, so both prior paths fall to the
// uniform floor instead of through.
func TestUCTPriorFloorsAtZero(t *testing.T) {
	a := atom{key: "k1", constraint: atomOn("a")}
	p := newUCTPolicy(rankAtoms([]atom{a}), nil, map[string]float64{"k1": 0}, nil, uctConfig(), 0, domain.Maximize)

	if got := p.atomPrior(a); got != p.floorPrior {
		t.Fatalf("a zero ranked prior must fall to the floor, got %v", got)
	}
	if got := p.proposalPrior(0); got != p.floorPrior {
		t.Fatalf("a zero supplied prior must fall to the floor, got %v", got)
	}
}

// TestUCTGroundingFractionInterleavesAtTheShippedDefault covers the knob where it
// actually ships. The two endpoints are decided by short-circuits — a fraction of 0
// by the atom fallback, a fraction of 1 by the empty-expansion case — so only an
// intermediate value exercises the running ratio itself, which is what decides the
// order a truncated budget spends its expansions in.
func TestUCTGroundingFractionInterleavesAtTheShippedDefault(t *testing.T) {
	atoms := atomsFor(t, atomOn("a"), atomOn("b"), atomOn("c"), atomOn("d"),
		atomOn("e"), atomOn("f"), atomOn("g"), atomOn("h"))
	var proposals []groundedProposal
	for _, id := range []string{"mh-1", "mh-2", "mh-3", "mh-4"} {
		proposals = append(proposals, proposalFor(t, id, 0.9, atomOn("p"+id), atomOn("q"+id)))
	}
	cfg := uctConfig() // GroundingFraction is the shipped 0.3.
	p := newUCTPolicy(atoms, proposals, nil, nil, cfg, 0, domain.Maximize)

	var kinds []bool
	for i := 0; i < len(atoms)+len(proposals); i++ {
		node, ok := p.Select()
		if !ok {
			break
		}
		kinds = append(kinds, node.proposedBy != "")
		p.Update(node, Measurement{Value: 1, Support: 100})
	}

	proposalsBy := func(kinds []bool, n int) int {
		taken := 0
		for _, isProposal := range kinds[:n] {
			if isProposal {
				taken++
			}
		}
		return taken
	}

	// The ratio must hold as the run goes, not only once it ends. Eight expansions in,
	// a 0.3 share has taken about three proposals; a policy that adopted one and then
	// stopped preferring them — or one that ignored the knob until atoms ran out —
	// would still be at one.
	if got := proposalsBy(kinds, 8); got < 2 {
		t.Fatalf("%d proposals in the first eight expansions; the running share never applies: %v", got, kinds)
	}
	if got := proposalsBy(kinds, 4); got > 2 {
		t.Fatalf("%d of the first four expansions were proposals, well past the 0.3 share: %v", got, kinds)
	}

	// The control: at a fraction of 0 the same vocabulary must take no proposal until
	// the atoms are gone, which is what shows the assertions above are reading the
	// knob rather than the fallback.
	off := cfg
	off.GroundingFraction = 0
	q := newUCTPolicy(atoms, proposals, nil, nil, off, 0, domain.Maximize)
	var offKinds []bool
	for i := 0; i < len(atoms)+len(proposals); i++ {
		node, ok := q.Select()
		if !ok {
			break
		}
		offKinds = append(offKinds, node.proposedBy != "")
		q.Update(node, Measurement{Value: 1, Support: 100})
	}
	if got := proposalsBy(offKinds, 8); got != 0 {
		t.Fatalf("a fraction of 0 must take no proposal while atoms remain, got %d: %v", got, offKinds)
	}
}

// TestUCTOffersProposalsAtTheRootOnly exercises the guard directly rather than
// through a descent that happens never to reach it. An adopted node carries the
// proposal's predicates and hangs off the root whichever way the guard goes today,
// because expansion drains the root before descending — so only asking a non-root
// node for a move can tell the documented rule from that coincidence, and the rule
// becomes load-bearing the moment the descent order changes.
func TestUCTOffersProposalsAtTheRootOnly(t *testing.T) {
	atoms := atomsFor(t, atomOn("a"), atomOn("b"))
	proposal := proposalFor(t, "mh-1", 0.9, atomOn("x"), atomOn("y"))
	cfg := uctConfig()
	// A fraction of 0 takes atoms first, which leaves the proposal unadopted and so
	// still offerable — the state this asks both nodes about.
	cfg.GroundingFraction = 0
	p := newUCTPolicy(atoms, []groundedProposal{proposal}, nil, nil, cfg, 0, domain.Maximize)

	first, ok := p.Select()
	if !ok {
		t.Fatal("expected a first candidate")
	}
	p.Update(first, Measurement{Value: 1, Support: 100})
	child, ok := p.byCanonical[first.canonical]
	if !ok || child == p.root {
		t.Fatalf("expected a non-root node to ask, got %+v", child)
	}

	if move, offered := p.peekProposal(child); offered {
		t.Fatalf("a proposal was offered below the root: %+v", move.proposal)
	}
	if _, offered := p.peekProposal(p.root); !offered {
		t.Fatal("the root must still be offered the proposal")
	}
}

// TestGeneratedNodesKeepAscendingRanks pins the shape Node documents. The tree
// search walks its own prior-ordered move list rather than the vocabulary's rank
// order, so it genuinely conjoins lower-ranked atoms onto higher-ranked nodes —
// which is exactly why the ordered insert exists rather than a plain append.
func TestGeneratedNodesKeepAscendingRanks(t *testing.T) {
	schemaAtoms, err := buildSchemaAtoms(schemaFixture(), 3)
	if err != nil {
		t.Fatalf("build schema atoms: %v", err)
	}
	atoms := rankAtoms(schemaAtoms)
	// The proposal names two vocabulary predicates in descending rank order, which is
	// the only shape whose reified node needs sorting — predicates outside the
	// vocabulary take synthetic ranks in filter order and are ascending already.
	high, low := atoms[len(atoms)-1].constraint, atoms[0].constraint
	proposal := proposalFor(t, "mh-1", 0.9, high, low)

	policies := map[string]SearchPolicy{
		"beam": newBeamPolicy(atoms, testConfig(), 0, domain.Maximize),
		"uct":  newUCTPolicy(atoms, []groundedProposal{proposal}, nil, nil, uctConfig(), 0, domain.Maximize),
	}
	for name, p := range policies {
		t.Run(name, func(t *testing.T) {
			for _, node := range drainPolicy(t, p, Measurement{Value: 1, Support: 100}) {
				for i := 1; i < len(node.ranks); i++ {
					if node.ranks[i-1] > node.ranks[i] {
						t.Fatalf("node ranks must ascend, got %v for %s", node.ranks, node.canonical)
					}
				}
			}
		})
	}
}
