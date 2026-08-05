package sleepcycle

import (
	"context"
	"testing"

	"github.com/arborette/arborette/internal/domain"
	"github.com/arborette/arborette/internal/graph"
	"github.com/arborette/arborette/internal/llm"
	"github.com/arborette/arborette/internal/sandboxclient"
	"github.com/arborette/arborette/internal/store"
)

// TestKnowledgeGuidedRunWritesBackAWinner drives a whole run under the
// knowledge-guided policy — the shipped default — so the stages either side of the
// new policy are exercised together: the vocabulary, the search, the
// materially-better gate, the write-back, and publication.
func TestKnowledgeGuidedRunWritesBackAWinner(t *testing.T) {
	h := newHarness(t, uctConfig())
	h.repo.findings = findingsFor("a", "b")
	h.sandbox.baseline = 1
	h.sandbox.defaultValue = 2
	h.sandbox.measurements["a+b"] = sandboxMeasurement{value: 10, support: 100}

	if err := h.run(t); err != nil {
		t.Fatalf("run: %v", err)
	}

	if !h.didAbstract(t, "a", "b") {
		t.Fatalf("the conjunction the search found was not published")
	}
	record, ok := h.audits.find("sleepcycle_run_complete")
	if !ok {
		t.Fatal("a completed run must report")
	}
	if record.detail["winners"].(int) == 0 {
		t.Fatalf("run reported no winners: %+v", record.detail)
	}
	if _, skipped := record.detail["search_skipped"]; skipped {
		t.Fatalf("the search must have run: %+v", record.detail)
	}
}

// TestKnowledgeGuidedRunSkipsWithoutAQualifyingFinding: with no finding clearing the
// support floor nothing can clear the materially-better gate, so the run must skip
// the search rather than spend the budget producing unwritable winners — and it must
// say so, since a run reporting zero measurements is otherwise ambiguous.
func TestKnowledgeGuidedRunSkipsWithoutAQualifyingFinding(t *testing.T) {
	cfg := uctConfig()
	cfg.MinSupport = 50
	h := newHarness(t, cfg)
	// Two predicates, so a conjunction is formable, but neither finding clears the
	// floor — the case the nested guard exists to short-circuit.
	h.repo.findings = append(h.repo.findings,
		findingWithSupport("a", 5, 10, "a"), findingWithSupport("b", 6, 10, "b"))

	if err := h.run(t); err != nil {
		t.Fatalf("run: %v", err)
	}

	record, ok := h.audits.find("sleepcycle_run_complete")
	if !ok {
		t.Fatal("a completed run must report")
	}
	if record.detail["search_skipped"] != skipNoBestSingle {
		t.Fatalf("search_skipped = %v, want the undefined-best-single reason", record.detail["search_skipped"])
	}
	if h.embeddings.scoredCalls != 0 || h.claude.groundCalls != 0 {
		t.Fatalf("the skip must precede every retrieval and grounding call: %d retrievals, %d grounding calls",
			h.embeddings.scoredCalls, h.claude.groundCalls)
	}
}

// TestAdoptedWinnerCarriesItsProposingHeuristic pins the reuse provenance end to
// end: a winner the corpus proposed records which heuristic produced it on the
// derived Intervention, so a trace of that winner shows the reuse.
//
// The credit follows the adopted conjunction and its refinements, and nothing else.
// A segment the search built from this goal's own predicates records no proposer at
// all rather than an empty one, since an empty id is a valid-looking id that a
// consumer counting reuse would file under a phantom heuristic.
func TestAdoptedWinnerCarriesItsProposingHeuristic(t *testing.T) {
	h := newHarness(t, uctConfig())
	h.repo.findings = findingsFor("a", "b")
	h.repo.corpus["mh-1"] = domain.MetaHeuristic{
		ID:         "mh-1",
		Definition: "[Volume] with [Cohort] raises [System Output]",
		GoalID:     "other-goal",
	}
	h.embeddings.scored = []store.ScoredRef{{NodeID: "mh-1", Distance: 0.2}}
	h.claude.grounded = map[string][]domain.Constraint{
		h.repo.corpus["mh-1"].Definition: {atomOn("x"), atomOn("y")},
	}
	// The proposal is validated against the introspected schema, so the columns it
	// names have to be columns this data source really has.
	h.sandbox.schema.Columns = append(h.sandbox.schema.Columns,
		sandboxclient.Column{Name: "x", Type: "DOUBLE"}, sandboxclient.Column{Name: "y", Type: "DOUBLE"})
	h.sandbox.baseline = 1
	h.sandbox.defaultValue = 2
	h.sandbox.measurements["x+y"] = sandboxMeasurement{value: 20, support: 100}

	if err := h.run(t); err != nil {
		t.Fatalf("run: %v", err)
	}

	adopted := interventionFor(t, h, "x", "y")
	if adopted.Properties[domain.PropProposedBy] != "mh-1" {
		t.Fatalf("the adopted winner must record its proposing heuristic, got %v", adopted.Properties[domain.PropProposedBy])
	}
	credited := 0
	for _, i := range h.repo.interventions {
		by, present := i.Properties[domain.PropProposedBy]
		if !present {
			continue
		}
		credited++
		filters, err := domain.DecodeConstraints(i.Properties[domain.PropNewFilters])
		if err != nil {
			t.Fatalf("decode filters: %v", err)
		}
		if !conjoins(filters, "x") || !conjoins(filters, "y") {
			t.Fatalf("proposer %v credited on a segment that is not the adopted conjunction or a refinement of it: %+v", by, filters)
		}
	}
	if credited == 0 {
		t.Fatal("no winner recorded a proposer")
	}
}

func conjoins(filters []domain.Constraint, field string) bool {
	for _, f := range filters {
		if f.Field == field {
			return true
		}
	}
	return false
}

// interventionFor finds the derived Intervention for a segment by the deterministic
// id the write-back mints, so the assertion addresses it the same way production does.
func interventionFor(t *testing.T, h *harness, fields ...string) domain.Intervention {
	t.Helper()
	id := derivedID(goalNamespace("g1"), roleIntervention, canonicalFor(t, fields...))
	for _, i := range h.repo.interventions {
		if i.ID == id {
			return i
		}
	}
	t.Fatalf("no derived intervention for %v was written", fields)
	return domain.Intervention{}
}

// TestRunCollectsCausalEvidenceForItsFindings pins the Engine-B → Engine-A link end
// to end. The multiplier arithmetic is unit-tested elsewhere; what this covers is
// the wiring — that the run actually asks for its findings' verified effects and
// hands them to the policy, which is the whole of the compounding claim.
func TestRunCollectsCausalEvidenceForItsFindings(t *testing.T) {
	h := newHarness(t, uctConfig())
	h.repo.findings = findingsFor("a", "b")
	h.repo.interventionEvidence = map[string]graph.CausalEvidence{
		"i-a": {InterventionID: "i-a", Confidence: 0.9, EffectSize: 2},
	}
	h.sandbox.baseline = 1
	h.sandbox.defaultValue = 2

	evidence := h.worker.causalEvidence(context.Background(), "g1", h.repo.findings, nil)

	got, ok := evidence["i-a"]
	if !ok || got.Confidence != 0.9 {
		t.Fatalf("evidence = %+v (present %v), want the verified finding's effect", got, ok)
	}
	if _, present := evidence["i-b"]; present {
		t.Fatalf("an unverified finding must be absent, got %+v", evidence["i-b"])
	}
}

// TestCausalEvidenceCoversProposingHeuristics: a proposal's credit comes from the
// heuristic that made it, which is a separate read from the findings' own — so a
// run that grounded proposals must ask for both.
func TestCausalEvidenceCoversProposingHeuristics(t *testing.T) {
	h := newHarness(t, uctConfig())
	h.repo.heuristicEvidence = map[string]graph.CausalEvidence{
		"mh-1": {InterventionID: "i-x", Confidence: 0.75},
	}
	proposals := []groundedProposal{proposalFor(t, "mh-1", 0.9, atomOn("x"), atomOn("y"))}

	evidence := h.worker.causalEvidence(context.Background(), "g1", nil, proposals)

	if got := evidence["mh-1"]; got.Confidence != 0.75 {
		t.Fatalf("evidence = %+v, want the proposing heuristic's verified effect", got)
	}
}

// TestQuantileProbeIsRequestedOnlyForTheSchemaVocabulary: the cuts cost a scan per
// numeric column, so a run that will not derive threshold predicates must not ask
// for them — and one that will must.
func TestQuantileProbeIsRequestedOnlyForTheSchemaVocabulary(t *testing.T) {
	off := newHarness(t, uctConfig())
	off.repo.findings = findingsFor("a", "b")
	if err := off.run(t); err != nil {
		t.Fatalf("run: %v", err)
	}
	if off.sandbox.introspectReq.QuantileBins != 0 {
		t.Fatalf("quantile_bins = %d, want none without the schema vocabulary",
			off.sandbox.introspectReq.QuantileBins)
	}

	cfg := uctConfig()
	cfg.SchemaAtoms = true
	on := newHarness(t, cfg)
	on.repo.findings = findingsFor("a", "b")
	if err := on.run(t); err != nil {
		t.Fatalf("run: %v", err)
	}
	if on.sandbox.introspectReq.QuantileBins != cfg.QuantileBins {
		t.Fatalf("quantile_bins = %d, want the configured %d",
			on.sandbox.introspectReq.QuantileBins, cfg.QuantileBins)
	}
}

// TestAbstractionPersistsItsTermsAndOrigin pins the producing half of the reuse
// round trip: the term map and the origin are written from the abstraction and the
// run's own target, which is the only place they are populated. An empty origin ref
// would silently route every same-dataset heuristic through a paid grounding call.
func TestAbstractionPersistsItsTermsAndOrigin(t *testing.T) {
	h := newHarness(t, uctConfig())
	h.repo.findings = findingsFor("a", "b")
	h.claude.terms = []llm.OntologyTerm{{Concrete: "revenue", Ontological: "[System Output]"}}
	h.sandbox.baseline = 1
	h.sandbox.defaultValue = 2
	h.sandbox.measurements["a+b"] = sandboxMeasurement{value: 10, support: 100}

	if err := h.run(t); err != nil {
		t.Fatalf("run: %v", err)
	}

	if len(h.repo.heuristics) == 0 {
		t.Fatal("the run published nothing to assert on")
	}
	for _, mh := range h.repo.heuristics {
		if len(mh.OntologyTerms) != 1 || mh.OntologyTerms[0].Ontological != "[System Output]" {
			t.Fatalf("meta-heuristic %q carries terms %+v, want the abstraction's own", mh.ID, mh.OntologyTerms)
		}
		if mh.OriginGoalID != "g1" {
			t.Fatalf("origin goal = %q, want the running goal", mh.OriginGoalID)
		}
		if mh.OriginDataSourceRef != "ref.csv" {
			t.Fatalf("origin data source = %q, want the goal's own ref", mh.OriginDataSourceRef)
		}
	}
}

// TestSelfBuiltWinnerRecordsNoProposerInTheAudit: the audit records nil rather than
// an empty id, because a consumer counting knowledge reuse would file an empty
// string under a phantom heuristic.
func TestSelfBuiltWinnerRecordsNoProposerInTheAudit(t *testing.T) {
	h := newHarness(t, uctConfig())
	h.repo.findings = findingsFor("a", "b")
	h.sandbox.baseline = 1
	h.sandbox.defaultValue = 2
	h.sandbox.measurements["a+b"] = sandboxMeasurement{value: 10, support: 100}

	if err := h.run(t); err != nil {
		t.Fatalf("run: %v", err)
	}

	record, ok := h.audits.find("sleepcycle_intervention")
	if !ok {
		t.Fatal("a written winner must be audited")
	}
	proposer, present := record.detail["proposed_by_meta_heuristic_id"]
	if !present {
		t.Fatalf("the audit must carry the proposer field even when absent: %+v", record.detail)
	}
	if proposer != nil {
		t.Fatalf("a search-built winner must record a nil proposer, got %#v", proposer)
	}
}

// TestSelectPolicyHandsEvidenceToTheSearch closes the last link in the compounding
// chain. The repo reads are pinned, and the multiplier's effect on a value estimate
// is pinned, but between them sits the wiring that puts one into the other — and a
// policy constructed with no evidence weights every candidate identically while
// every other test stays green.
func TestSelectPolicyHandsEvidenceToTheSearch(t *testing.T) {
	h := newHarness(t, uctConfig())
	findings := findingsFor("a", "b")
	h.repo.findings = findings
	h.repo.interventionEvidence = map[string]graph.CausalEvidence{
		"i-a": {InterventionID: "i-a", Confidence: 0.9},
	}
	best := 10.0

	policy, _, skipped := h.worker.selectPolicy(context.Background(),
		searchTarget{goalID: "g1", dataSourceRef: "ref.csv"}, "grow revenue",
		objectiveFor(t, domain.Maximize), h.sandbox.schema,
		atomsFor(t, atomOn("a"), atomOn("b")), findings, 0, &best)

	if skipped != "" {
		t.Fatalf("skipped = %q, want a search", skipped)
	}
	uct, ok := policy.(*uctPolicy)
	if !ok {
		t.Fatalf("policy = %T, want the knowledge-guided one", policy)
	}
	if got := uct.evidence["i-a"]; got.Confidence != 0.9 {
		t.Fatalf("the policy was constructed with evidence %+v, want the finding's verified effect", uct.evidence)
	}
}
