package sleepcycle

import (
	"context"
	"errors"
	"testing"

	"github.com/arborette/arborette/internal/domain"
	"github.com/arborette/arborette/internal/graph"
	"github.com/arborette/arborette/internal/llm"
	"github.com/arborette/arborette/internal/sandboxclient"
	"github.com/arborette/arborette/internal/store"
)

// groundingSchema is a data source the proposals below are validated against: one
// value-constrained column and one continuous one.
func groundingSchema() sandboxclient.Schema {
	return sandboxclient.Schema{Columns: []sandboxclient.Column{
		{Name: "HomePlanet", Type: "VARCHAR", DistinctValues: []string{"Earth", "Mars"}},
		{Name: "Age", Type: "DOUBLE"},
	}}
}

func eqOn(field, value string) domain.Constraint {
	return domain.Constraint{Field: field, Op: domain.Equal, Operand: stringOperand(value)}
}

// groundingHarness wires a worker whose corpus holds one retrievable heuristic, the
// setup every grounding case starts from.
func groundingHarness(t *testing.T, cfg Config) *harness {
	t.Helper()
	h := newHarness(t, cfg)
	h.repo.corpus["mh-1"] = domain.MetaHeuristic{
		ID:         "mh-1",
		Definition: "[Primary Population Center] raises [System Output]",
		GoalID:     "other-goal",
	}
	h.embeddings.scored = []store.ScoredRef{{NodeID: "mh-1", Distance: 0.4}}
	return h
}

func groundOne(t *testing.T, h *harness) []groundedProposal {
	t.Helper()
	target := searchTarget{goalID: "g1", dataSourceRef: "ref.csv", namespace: goalNamespace("g1")}
	return h.worker.groundProposals(context.Background(), target, "grow revenue", groundingSchema())
}

// TestGroundingValidatesAgainstTheTargetSchema is the drop-not-repair rule. Each
// case is a conjunction the model could plausibly return and the search must never
// measure: a column this data source lacks, a value outside the column's set, a
// conjunction restating one (field, op), and orders outside what the search forms.
func TestGroundingValidatesAgainstTheTargetSchema(t *testing.T) {
	cases := []struct {
		name    string
		filters []domain.Constraint
		kept    bool
	}{
		{"valid conjunction", []domain.Constraint{eqOn("HomePlanet", "Mars"), atomOn("Age")}, true},
		{"unknown column", []domain.Constraint{eqOn("HomePlanet", "Mars"), atomOn("Nonexistent")}, false},
		{"unknown value", []domain.Constraint{eqOn("HomePlanet", "Jupiter"), atomOn("Age")}, false},
		{"duplicate field and operator", []domain.Constraint{eqOn("HomePlanet", "Mars"), eqOn("HomePlanet", "Earth")}, false},
		// The column check resolves case-insensitively, so a model answering in a
		// different spelling would otherwise slip two equalities on one column — an
		// empty segment — past the candidate invariant.
		{"duplicate under a different spelling", []domain.Constraint{eqOn("HomePlanet", "Mars"), eqOn("homeplanet", "Earth")}, false},
		{"single predicate", []domain.Constraint{eqOn("HomePlanet", "Mars")}, false},
		{"over the order cap", []domain.Constraint{
			eqOn("HomePlanet", "Mars"), atomOn("Age"),
			{Field: "Age", Op: domain.LessThan, Value: 90}, {Field: "HomePlanet", Op: domain.NotEqual, Operand: stringOperand("Earth")},
		}, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			h := groundingHarness(t, uctConfig())
			h.claude.grounded = map[string][]domain.Constraint{
				h.repo.corpus["mh-1"].Definition: c.filters,
			}

			proposals := groundOne(t, h)

			if c.kept != (len(proposals) == 1) {
				t.Fatalf("kept = %v, want %v (proposals %+v)", len(proposals) == 1, c.kept, proposals)
			}
			if c.kept {
				return
			}
			if _, dropped := h.audits.find("sleepcycle_proposal_dropped"); !dropped {
				t.Fatal("a dropped proposal must be recorded, not discarded silently")
			}
			for _, p := range proposals {
				if len(p.filters) != len(c.filters) {
					t.Fatalf("a proposal must be dropped whole, never repaired: %+v", p.filters)
				}
			}
		})
	}
}

// TestGroundingCarriesTheRetrievalPrior: the distance ranks a proposal against the
// atom moves, so it has to survive as the move's prior mass.
func TestGroundingCarriesTheRetrievalPrior(t *testing.T) {
	h := groundingHarness(t, uctConfig())
	h.claude.grounded = map[string][]domain.Constraint{
		h.repo.corpus["mh-1"].Definition: {eqOn("HomePlanet", "Mars"), atomOn("Age")},
	}

	proposals := groundOne(t, h)

	if len(proposals) != 1 {
		t.Fatalf("proposals = %+v, want one", proposals)
	}
	if proposals[0].prior != 0.8 {
		t.Fatalf("prior = %v, want 1 - distance/2 for a distance of 0.4", proposals[0].prior)
	}
	if proposals[0].heuristicID != "mh-1" {
		t.Fatalf("heuristicID = %q, want the retrieved heuristic", proposals[0].heuristicID)
	}
}

// TestGroundingRetrievesCrossGoalByDefault: a heuristic abstracted from this goal's
// own findings describes a segment the atom search already reaches, so a goal-scoped
// retrieval would leave the reuse path inert. The kill switch narrows it back.
func TestGroundingRetrievesCrossGoalByDefault(t *testing.T) {
	h := groundingHarness(t, uctConfig())
	groundOne(t, h)
	if !h.embeddings.scoredScope.CrossGoal {
		t.Fatalf("scope = %+v, want cross-goal retrieval", h.embeddings.scoredScope)
	}

	cfg := uctConfig()
	cfg.CrossGoalGrounding = false
	scoped := groundingHarness(t, cfg)
	groundOne(t, scoped)
	if scoped.embeddings.scoredScope.GoalID != "g1" {
		t.Fatalf("scope = %+v, want the run's own goal when cross-goal is off", scoped.embeddings.scoredScope)
	}
}

// TestGroundingReusesSourceConjunctionsOnTheSameDataSource: a heuristic abstracted
// from this very data source already has its segments written in these columns, so
// recovering them costs a graph read rather than a model call.
func TestGroundingReusesSourceConjunctionsOnTheSameDataSource(t *testing.T) {
	h := groundingHarness(t, uctConfig())
	mh := h.repo.corpus["mh-1"]
	mh.OriginDataSourceRef = "ref.csv"
	mh.OntologyTerms = []domain.OntologyTerm{
		{Concrete: "HomePlanet", Ontological: "[Primary Population Center]"},
		{Concrete: "Transported", Ontological: "[System Output]"},
	}
	h.repo.corpus["mh-1"] = mh
	h.repo.sourceFilters["mh-1"] = [][]domain.Constraint{{eqOn("HomePlanet", "Mars"), atomOn("Age")}}

	proposals := groundOne(t, h)

	if len(proposals) != 1 {
		t.Fatalf("proposals = %+v, want the recovered conjunction", proposals)
	}
	if h.claude.groundCalls != 0 {
		t.Fatalf("a same-data-source heuristic must not pay a grounding call, got %d", h.claude.groundCalls)
	}
}

// TestGroundingRoutesAPreProvenanceHeuristicThroughTheModel: a node abstracted
// before the term map was persisted cannot be confirmed to describe those segments,
// so it takes the grounding path even when its origin ref happens to match.
func TestGroundingRoutesAPreProvenanceHeuristicThroughTheModel(t *testing.T) {
	h := groundingHarness(t, uctConfig())
	mh := h.repo.corpus["mh-1"]
	mh.OriginDataSourceRef = "ref.csv"
	h.repo.corpus["mh-1"] = mh
	h.repo.sourceFilters["mh-1"] = [][]domain.Constraint{{eqOn("HomePlanet", "Mars"), atomOn("Age")}}
	h.claude.grounded = map[string][]domain.Constraint{mh.Definition: {eqOn("HomePlanet", "Earth"), atomOn("Age")}}

	proposals := groundOne(t, h)

	if h.claude.groundCalls != 1 {
		t.Fatalf("a heuristic with no persisted terms must be grounded by the model, got %d calls", h.claude.groundCalls)
	}
	if len(proposals) != 1 || proposals[0].filters[0].Operand == nil || *proposals[0].filters[0].Operand.String != "Earth" {
		t.Fatalf("proposals = %+v, want the model's own conjunction", proposals)
	}
}

// TestGroundingDegradesWhenTheModelDeclines: a heuristic with no counterpart here is
// the expected outcome of reaching across datasets, and it must cost the run nothing
// but a recorded failure.
func TestGroundingDegradesWhenTheModelDeclines(t *testing.T) {
	h := groundingHarness(t, uctConfig())
	h.claude.groundErr = errors.New("does not ground to this data source")

	if proposals := groundOne(t, h); len(proposals) != 0 {
		t.Fatalf("proposals = %+v, want none", proposals)
	}
	if _, recorded := h.audits.find("sleepcycle_grounding_failure"); !recorded {
		t.Fatal("a failed grounding call must be recorded")
	}
}

// TestGroundingDegradesWhenRetrievalFails pins the same disposition one layer up: a
// broken vector store costs the run its proposals, never the run.
func TestGroundingDegradesWhenRetrievalFails(t *testing.T) {
	h := groundingHarness(t, uctConfig())
	h.embeddings.scoredErr = errors.New("pgvector down")

	if proposals := groundOne(t, h); len(proposals) != 0 {
		t.Fatalf("proposals = %+v, want none", proposals)
	}
	if _, recorded := h.audits.find("sleepcycle_grounding_failure"); !recorded {
		t.Fatal("a failed retrieval must be recorded")
	}
}

// TestSparseFindingsGoalStillSearchesWithAProposal is the Cold-Start case the whole
// reuse path exists for: one predicate of its own is not a conjunction, but a
// validated proposal is — so the goal searches under the knowledge-guided policy
// while the beam, which counts only atoms, still skips it.
func TestSparseFindingsGoalStillSearchesWithAProposal(t *testing.T) {
	h := groundingHarness(t, uctConfig())
	h.claude.grounded = map[string][]domain.Constraint{
		h.repo.corpus["mh-1"].Definition: {eqOn("HomePlanet", "Mars"), atomOn("Age")},
	}
	atoms := atomsFor(t, atomOn("Age"))
	best := 10.0

	policy, _, skipped := h.worker.selectPolicy(context.Background(),
		searchTarget{goalID: "g1", dataSourceRef: "ref.csv"}, "grow revenue",
		objectiveFor(t, domain.Maximize), groundingSchema(), atoms, nil, 0, &best)

	if skipped != "" {
		t.Fatalf("skipped = %q, want a search driven by the grounded proposal", skipped)
	}
	if policy == nil {
		t.Fatal("expected a policy")
	}

	beam := groundingHarness(t, testConfig())
	_, _, beamSkipped := beam.worker.selectPolicy(context.Background(),
		searchTarget{goalID: "g1", dataSourceRef: "ref.csv"}, "grow revenue",
		objectiveFor(t, domain.Maximize), groundingSchema(), atoms, nil, 0, &best)
	if beamSkipped != skipNoConjunction {
		t.Fatalf("beam skipped = %q, want the unchanged atom-count guard", beamSkipped)
	}
}

// TestUndefinedBestSingleSkipsBeforeAnySpend is the nesting that makes the cheap
// clauses load-bearing. With no finding clearing the support floor nothing can clear
// the materially-better gate, so every winner would be structurally unwritable — and
// the skip has to happen before a single retrieval or model call is paid for.
func TestUndefinedBestSingleSkipsBeforeAnySpend(t *testing.T) {
	h := groundingHarness(t, uctConfig())
	atoms := atomsFor(t, atomOn("a"), atomOn("b"))

	policy, _, skipped := h.worker.selectPolicy(context.Background(),
		searchTarget{goalID: "g1", dataSourceRef: "ref.csv"}, "grow revenue",
		objectiveFor(t, domain.Maximize), groundingSchema(), atoms, nil, 0, nil)

	if skipped != skipNoBestSingle {
		t.Fatalf("skipped = %q, want the undefined-best-single reason", skipped)
	}
	if policy != nil {
		t.Fatal("a skipped search must carry no policy")
	}
	if h.embeddings.scoredCalls != 0 || h.claude.groundCalls != 0 || h.provider.queryCalls != 0 {
		t.Fatalf("nothing may be spent before the skip: %d retrievals, %d grounding calls, %d embeddings",
			h.embeddings.scoredCalls, h.claude.groundCalls, h.provider.queryCalls)
	}
}

// TestCausalEvidenceFailureDegradesToNoWeighting: losing the weighting is a worse
// search, and losing the run is a worse outcome.
func TestCausalEvidenceFailureDegradesToNoWeighting(t *testing.T) {
	h := groundingHarness(t, uctConfig())
	h.repo.evidenceErr = errors.New("neo4j down")

	evidence := h.worker.causalEvidence(context.Background(), "g1",
		[]graph.CausalTriplet{finding("a", 5, testObjectiveLabel, "a")}, nil)

	if len(evidence) != 0 {
		t.Fatalf("evidence = %+v, want none", evidence)
	}
	if _, recorded := h.audits.find("sleepcycle_causal_evidence_failure"); !recorded {
		t.Fatal("a failed evidence read must be recorded")
	}
}

// TestGroundingDegradationPaths walks the remaining ways the reuse path can fail.
// Each one must cost the run its proposals and nothing else, because grounding is
// an enrichment: a run that aborted when the corpus was unreachable would be
// strictly worse than one that never consulted it.
func TestGroundingDegradationPaths(t *testing.T) {
	cases := []struct {
		name    string
		break_  func(*harness)
		audited string
	}{
		{"goal embedding fails", func(h *harness) { h.provider.queryErr = errors.New("ollama down") }, "sleepcycle_grounding_failure"},
		{"corpus hydration fails", func(h *harness) { h.repo.corpusErr = errors.New("neo4j down") }, "sleepcycle_grounding_failure"},
		{"source conjunctions fail", func(h *harness) {
			mh := h.repo.corpus["mh-1"]
			mh.OriginDataSourceRef = "ref.csv"
			mh.OntologyTerms = []domain.OntologyTerm{{Concrete: "HomePlanet", Ontological: "[Primary Population Center]"}}
			mh.Definition = "[Primary Population Center] raises output"
			h.repo.corpus["mh-1"] = mh
			h.repo.sourceFiltersErr = errors.New("neo4j down")
		}, "sleepcycle_grounding_failure"},
		{"model declines the heuristic", func(h *harness) { h.claude.groundErr = llm.ErrNoGroundedFilters }, "sleepcycle_proposal_dropped"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			h := groundingHarness(t, uctConfig())
			c.break_(h)

			if proposals := groundOne(t, h); len(proposals) != 0 {
				t.Fatalf("proposals = %+v, want none", proposals)
			}
			if _, recorded := h.audits.find(c.audited); !recorded {
				t.Fatalf("expected a %s record", c.audited)
			}
		})
	}
}

// TestGroundingSkipsAStaleHeuristic: a heuristic whose supporting evidence an
// analyst rejected must not seed this goal's search. It is still in the corpus and
// still retrievable, so the skip is the only thing keeping retracted knowledge from
// being re-proposed as a segment to measure.
func TestGroundingSkipsAStaleHeuristic(t *testing.T) {
	h := groundingHarness(t, uctConfig())
	mh := h.repo.corpus["mh-1"]
	mh.Stale = true
	h.repo.corpus["mh-1"] = mh
	h.claude.grounded = map[string][]domain.Constraint{
		mh.Definition: {eqOn("HomePlanet", "Mars"), atomOn("Age")},
	}

	if proposals := groundOne(t, h); len(proposals) != 0 {
		t.Fatalf("a stale heuristic must not propose: %+v", proposals)
	}
	if h.claude.groundCalls != 0 {
		t.Fatalf("a stale heuristic must not even be grounded, got %d calls", h.claude.groundCalls)
	}
}

// TestGroundingRequiresEveryBracketedTermToResolve is the other half of the
// same-data-source check. An origin match alone is not enough: if the definition
// names a term the persisted map does not carry, the definition has drifted from
// the segments it was abstracted from, and adopting them would attribute a
// conjunction to a heuristic that no longer describes it.
func TestGroundingRequiresEveryBracketedTermToResolve(t *testing.T) {
	h := groundingHarness(t, uctConfig())
	mh := h.repo.corpus["mh-1"]
	mh.OriginDataSourceRef = "ref.csv"
	mh.Definition = "[Primary Population Center] with [Unmapped Term] raises [System Output]"
	mh.OntologyTerms = []domain.OntologyTerm{
		{Concrete: "HomePlanet", Ontological: "[Primary Population Center]"},
		{Concrete: "Transported", Ontological: "[System Output]"},
	}
	h.repo.corpus["mh-1"] = mh
	h.repo.sourceFilters["mh-1"] = [][]domain.Constraint{{eqOn("HomePlanet", "Mars"), atomOn("Age")}}
	h.claude.grounded = map[string][]domain.Constraint{mh.Definition: {eqOn("HomePlanet", "Earth"), atomOn("Age")}}

	proposals := groundOne(t, h)

	if h.claude.groundCalls != 1 {
		t.Fatalf("an unresolved term must route to the model, got %d grounding calls", h.claude.groundCalls)
	}
	if h.repo.sourceFilterCalls != 0 {
		t.Fatalf("the source conjunctions must not be adopted, got %d reads", h.repo.sourceFilterCalls)
	}
	if len(proposals) != 1 || *proposals[0].filters[0].Operand.String != "Earth" {
		t.Fatalf("proposals = %+v, want the model's own conjunction", proposals)
	}
}

// TestGroundingCapsOneHeuristicsContribution: the root expands every move before it
// descends, so one heuristic offering many conjunctions could spend the whole
// measurement budget before the search conjoins anything of its own.
func TestGroundingCapsOneHeuristicsContribution(t *testing.T) {
	h := groundingHarness(t, uctConfig())
	mh := h.repo.corpus["mh-1"]
	mh.OriginDataSourceRef = "ref.csv"
	mh.Definition = "[Primary Population Center] raises [System Output]"
	mh.OntologyTerms = []domain.OntologyTerm{
		{Concrete: "HomePlanet", Ontological: "[Primary Population Center]"},
		{Concrete: "Transported", Ontological: "[System Output]"},
	}
	h.repo.corpus["mh-1"] = mh
	var offered [][]domain.Constraint
	for _, value := range []string{"Mars", "Earth"} {
		for _, age := range []float64{1, 2, 3} {
			offered = append(offered, []domain.Constraint{
				eqOn("HomePlanet", value), {Field: "Age", Op: domain.GreaterThan, Value: age},
			})
		}
	}
	h.repo.sourceFilters["mh-1"] = offered

	proposals := groundOne(t, h)

	if len(proposals) != maxProposalsPerHeuristic {
		t.Fatalf("proposals = %d, want the per-heuristic cap of %d", len(proposals), maxProposalsPerHeuristic)
	}
	record, ok := h.audits.find("sleepcycle_proposals_truncated")
	if !ok {
		t.Fatal("a truncated contribution must be recorded, since it reads like a heuristic that had less to offer")
	}
	if record.detail["offered"].(int) != len(offered) {
		t.Fatalf("the audit must record what was offered, got %+v", record.detail)
	}
}

// TestNoKnowledgeSpendBeforeTheSkipGates is the reason selectPolicy nests its
// decision: every knowledge call — the vocabulary's critic, the retrieval, the goal
// embedding, the grounding — is paid per run, and a goal that cannot write back a
// winner should pay none of them. The schema vocabulary is enabled here precisely so
// the critic call is reachable and its absence means something.
func TestNoKnowledgeSpendBeforeTheSkipGates(t *testing.T) {
	cfg := uctConfig()
	cfg.SchemaAtoms = true
	atoms := atomsFor(t, atomOn("a"), atomOn("b"))

	cases := map[string]struct {
		cfg        Config
		bestSingle *float64
		want       string
	}{
		"no qualifying finding": {cfg, nil, skipNoBestSingle},
		"order cap forbids conjoining": func() struct {
			cfg        Config
			bestSingle *float64
			want       string
		} {
			capped := cfg
			capped.MaxOrder = 1
			best := 10.0
			return struct {
				cfg        Config
				bestSingle *float64
				want       string
			}{capped, &best, skipNoConjunction}
		}(),
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			h := groundingHarness(t, c.cfg)

			_, _, skipped := h.worker.selectPolicy(context.Background(),
				searchTarget{goalID: "g1", dataSourceRef: "ref.csv"}, "grow revenue",
				objectiveFor(t, domain.Maximize), groundingSchema(), atoms, nil, 0, c.bestSingle)

			if skipped != c.want {
				t.Fatalf("skipped = %q, want %q", skipped, c.want)
			}
			if h.claude.critiqueCall != 0 {
				t.Fatalf("the vocabulary critic must not be paid before the skip, got %d calls", h.claude.critiqueCall)
			}
			if h.embeddings.scoredCalls != 0 || h.claude.groundCalls != 0 || h.provider.queryCalls != 0 {
				t.Fatalf("no retrieval or grounding may be paid before the skip: %d retrievals, %d grounding calls, %d embeddings",
					h.embeddings.scoredCalls, h.claude.groundCalls, h.provider.queryCalls)
			}
		})
	}
}

// TestGroundingShowsTheModelWhatValidationChecks: the grounding call is validated
// against the target's columns and value sets, so the model has to be shown those
// same sets. A model that cannot see them invents values, every proposal is dropped
// by the value check, and the reuse path degrades to nothing while looking healthy.
func TestGroundingShowsTheModelWhatValidationChecks(t *testing.T) {
	h := groundingHarness(t, uctConfig())
	mh := h.repo.corpus["mh-1"]
	mh.OntologyTerms = []domain.OntologyTerm{{Concrete: "Origin", Ontological: "[Primary Population Center]"}}
	h.repo.corpus["mh-1"] = mh
	h.claude.grounded = map[string][]domain.Constraint{
		mh.Definition: {eqOn("HomePlanet", "Mars"), atomOn("Age")},
	}

	groundOne(t, h)

	if len(h.claude.groundSchema.Columns) != len(groundingSchema().Columns) {
		t.Fatalf("the model must be shown every column, got %+v", h.claude.groundSchema.Columns)
	}
	var sawValues bool
	for _, c := range h.claude.groundSchema.Columns {
		if c.Name == "HomePlanet" && len(c.DistinctValues) == 2 {
			sawValues = true
		}
	}
	if !sawValues {
		t.Fatalf("the model must be shown the value sets validation checks against, got %+v", h.claude.groundSchema.Columns)
	}
	if len(h.claude.groundTerms) != 1 || h.claude.groundTerms[0].Ontological != "[Primary Population Center]" {
		t.Fatalf("the model must be shown the heuristic's term map, got %+v", h.claude.groundTerms)
	}
}

// TestGroundingRetrievesTheConfiguredNumberOfHeuristics pins RetrievalK's
// pass-through: the knob is the only thing bounding how much of the corpus one run
// consults.
func TestGroundingRetrievesTheConfiguredNumberOfHeuristics(t *testing.T) {
	cfg := uctConfig()
	cfg.RetrievalK = 5
	h := groundingHarness(t, cfg)

	groundOne(t, h)

	if h.embeddings.scoredK != 5 {
		t.Fatalf("retrieval k = %d, want the configured 5", h.embeddings.scoredK)
	}
}

// TestGroundingBoundsValidationAttempts: every rejection is audited, and the
// same-data-source path routinely offers order-1 conjunctions validation must
// refuse, so bounding only the kept proposals would leave the audit volume unbounded
// on exactly the heuristics that contribute nothing.
func TestGroundingBoundsValidationAttempts(t *testing.T) {
	h := groundingHarness(t, uctConfig())
	mh := h.repo.corpus["mh-1"]
	mh.OriginDataSourceRef = "ref.csv"
	mh.Definition = "[Primary Population Center] raises [System Output]"
	mh.OntologyTerms = []domain.OntologyTerm{
		{Concrete: "HomePlanet", Ontological: "[Primary Population Center]"},
		{Concrete: "Transported", Ontological: "[System Output]"},
	}
	h.repo.corpus["mh-1"] = mh
	// Every conjunction is order-1, so none is ever kept and only the attempt bound
	// can stop the walk.
	var offered [][]domain.Constraint
	for i := 0; i < 50; i++ {
		offered = append(offered, []domain.Constraint{eqOn("HomePlanet", "Mars")})
	}
	h.repo.sourceFilters["mh-1"] = offered

	if proposals := groundOne(t, h); len(proposals) != 0 {
		t.Fatalf("no order-1 conjunction may be adopted, got %+v", proposals)
	}
	if dropped := h.audits.count("sleepcycle_proposal_dropped"); dropped > maxProposalAttempts {
		t.Fatalf("%d rejections audited, want at most the attempt bound of %d", dropped, maxProposalAttempts)
	}
	record, truncated := h.audits.find("sleepcycle_proposals_truncated")
	if !truncated {
		t.Fatal("a truncated walk must be recorded")
	}
	// The attempts bound stops a heuristic that kept nothing, so reporting the cap
	// here would claim contributions it never made.
	if kept := record.detail["kept"].(int); kept != 0 {
		t.Fatalf("kept = %d, want the count this walk actually produced", kept)
	}
}

// TestGroundedProposalsSummaryIsAudited: the retrieved-vs-proposed ratio is the only
// record distinguishing "the corpus does not transfer" from "retrieval found
// nothing".
func TestGroundedProposalsSummaryIsAudited(t *testing.T) {
	h := groundingHarness(t, uctConfig())
	h.claude.grounded = map[string][]domain.Constraint{
		h.repo.corpus["mh-1"].Definition: {eqOn("HomePlanet", "Mars"), atomOn("Age")},
	}

	groundOne(t, h)

	record, ok := h.audits.find("sleepcycle_grounded_proposals")
	if !ok {
		t.Fatal("a successful grounding pass must be summarized")
	}
	if record.detail["retrieved"].(int) != 1 || record.detail["proposals"].(int) != 1 {
		t.Fatalf("the summary must carry both counts, got %+v", record.detail)
	}
}
