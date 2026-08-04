package graph_test

import (
	"context"
	"testing"

	"github.com/arborette/arborette/internal/domain"
	"github.com/arborette/arborette/internal/graph"
	"github.com/arborette/arborette/internal/testutil"
)

// seedCausalTriplet creates a State→Intervention→Outcome chain scoped to a goal with an
// observational PRODUCED edge, the shape the collection reads expect.
func seedCausalTriplet(t *testing.T, ctx context.Context, repo *graph.Neo4jRepository, goalID string) (interventionID, outcomeID string) {
	t.Helper()
	stateID, interventionID, outcomeID := testutil.NewID(t), testutil.NewID(t), testutil.NewID(t)
	if err := repo.CreateState(ctx, domain.State{ID: stateID, GoalID: goalID, Properties: map[string]any{"k": 1.0}}); err != nil {
		t.Fatalf("create state: %v", err)
	}
	if err := repo.CreateIntervention(ctx, domain.Intervention{ID: interventionID, GoalID: goalID, Type: domain.InterventionQuery, Properties: map[string]any{"threshold": 0.5}}); err != nil {
		t.Fatalf("create intervention: %v", err)
	}
	if err := repo.CreateOutcome(ctx, domain.Outcome{ID: outcomeID, GoalID: goalID, VerificationStatus: domain.VerificationVerified, Value: map[string]any{"delta": 1.0}}); err != nil {
		t.Fatalf("create outcome: %v", err)
	}
	if err := repo.CreatePreConditionFor(ctx, stateID, interventionID); err != nil {
		t.Fatalf("create pre_condition_for: %v", err)
	}
	if err := repo.CreateProduced(ctx, interventionID, outcomeID, domain.ProducedEdge{EffectSize: 1.0, Confidence: 1.0}); err != nil {
		t.Fatalf("create produced: %v", err)
	}
	return interventionID, outcomeID
}

// TestCausalVerificationEvidenceAndSupersession: WriteCausalVerification is
// idempotent and readable through GetCausalEvidence; a newer version supersedes the
// prior causal edge (universal supersession); and a standalone supersession retracts
// every causal edge so the evidence read returns nothing.
func TestCausalVerificationEvidenceAndSupersession(t *testing.T) {
	ctx := context.Background()
	repo := newRepo(t, ctx)
	goalID := testutil.NewID(t)
	interventionID, _ := seedCausalTriplet(t, ctx, repo, goalID)

	v1Outcome := domain.Outcome{GoalID: goalID, VerificationStatus: domain.VerificationVerified, Value: map[string]any{"avg(Y)": 2.0}}
	v1Edge := domain.ProducedEdge{EffectSize: 2.0, Confidence: 0.9, EpistemicSource: domain.EpistemicCausalInferred}
	if err := repo.WriteCausalVerification(ctx, interventionID, 1, v1Outcome, v1Edge); err != nil {
		t.Fatalf("write v1: %v", err)
	}
	// Idempotent re-run at the same version.
	if err := repo.WriteCausalVerification(ctx, interventionID, 1, v1Outcome, v1Edge); err != nil {
		t.Fatalf("re-run v1: %v", err)
	}

	ev, ok, err := repo.GetCausalEvidence(ctx, interventionID)
	if err != nil || !ok {
		t.Fatalf("get causal evidence: ok=%v err=%v", ok, err)
	}
	if ev.EffectSize != 2.0 || ev.Confidence != 0.9 || ev.GraphVersion != 1 {
		t.Fatalf("evidence = %+v, want effect 2.0 confidence 0.9 version 1", ev)
	}

	// A newer version supersedes the prior causal edge; the evidence read follows it.
	v2Outcome := domain.Outcome{GoalID: goalID, VerificationStatus: domain.VerificationVerified, Value: map[string]any{"avg(Y)": 3.0}}
	v2Edge := domain.ProducedEdge{EffectSize: 3.0, Confidence: 0.95, EpistemicSource: domain.EpistemicCausalInferred}
	if err := repo.WriteCausalVerification(ctx, interventionID, 2, v2Outcome, v2Edge); err != nil {
		t.Fatalf("write v2: %v", err)
	}
	ev, ok, err = repo.GetCausalEvidence(ctx, interventionID)
	if err != nil || !ok || ev.GraphVersion != 2 || ev.EffectSize != 3.0 {
		t.Fatalf("after v2, evidence = %+v (ok=%v err=%v), want version 2 effect 3.0", ev, ok, err)
	}

	// A standalone supersession retracts every causal edge at or below the version.
	if err := repo.SupersedePriorCausalOutcomes(ctx, interventionID, 2); err != nil {
		t.Fatalf("supersede: %v", err)
	}
	if _, ok, err := repo.GetCausalEvidence(ctx, interventionID); err != nil || ok {
		t.Fatalf("after retraction, evidence ok=%v err=%v, want none", ok, err)
	}
}

// TestObservationalReadFilterExcludesCausal: the observational collection read
// excludes causal_inferred edges, while TraceCausalChain returns both kinds, each
// triplet labeled with its edge's epistemic source.
func TestObservationalReadFilterExcludesCausal(t *testing.T) {
	ctx := context.Background()
	repo := newRepo(t, ctx)
	goalID := testutil.NewID(t)
	interventionID, obsOutcomeID := seedCausalTriplet(t, ctx, repo, goalID)

	// Add a causal_inferred edge from the same intervention.
	if err := repo.WriteCausalVerification(ctx, interventionID, 1,
		domain.Outcome{GoalID: goalID, VerificationStatus: domain.VerificationVerified, Value: map[string]any{"avg(Y)": 2.0}},
		domain.ProducedEdge{EffectSize: 2.0, Confidence: 0.9, EpistemicSource: domain.EpistemicCausalInferred},
	); err != nil {
		t.Fatalf("write causal: %v", err)
	}

	// The eligible-findings read returns only the observational triplet.
	findings, err := repo.ListEligibleFindings(ctx, goalID)
	if err != nil {
		t.Fatalf("list eligible findings: %v", err)
	}
	if len(findings) != 1 {
		t.Fatalf("eligible findings = %d, want 1 (observational only)", len(findings))
	}
	if findings[0].Outcome.ID != obsOutcomeID {
		t.Fatalf("eligible finding outcome = %q, want the observational %q", findings[0].Outcome.ID, obsOutcomeID)
	}
	if findings[0].EpistemicSource != domain.EpistemicObservational {
		t.Fatalf("eligible finding source = %q, want observational", findings[0].EpistemicSource)
	}

	// TraceCausalChain over the intervention returns both edges, labeled.
	mhID := testutil.NewID(t)
	if err := repo.CreateMetaHeuristic(ctx, domain.MetaHeuristic{ID: mhID, GoalID: goalID, Definition: "d"}, []string{interventionID}); err != nil {
		t.Fatalf("create meta-heuristic: %v", err)
	}
	triplets, err := repo.TraceCausalChain(ctx, mhID)
	if err != nil {
		t.Fatalf("trace causal chain: %v", err)
	}
	sources := map[domain.EpistemicSource]bool{}
	for _, tr := range triplets {
		sources[tr.EpistemicSource] = true
	}
	if !sources[domain.EpistemicObservational] || !sources[domain.EpistemicCausalInferred] {
		t.Fatalf("trace must return both epistemic sources, got %v", sources)
	}
}

// TestCausalEvidenceForHeuristics is the batched read that labels served heuristics:
// it walks each heuristic's ABSTRACTED_FROM links to the interventions it generalized
// and returns the strongest surviving causal edge behind each. Every part of it is
// silent when wrong -- a reversed arrow, a broken supersession filter, or a wrong
// label all return "no evidence", which the caller cannot tell from a healthy answer
// about an unverified heuristic.
func TestCausalEvidenceForHeuristics(t *testing.T) {
	ctx := context.Background()
	repo := newRepo(t, ctx)
	goalID := testutil.NewID(t)

	// A heuristic abstracted from two verified findings: the stronger one is what it
	// must be served with, so a weakly-confirmed component cannot understate the rest.
	weak, _ := seedCausalTriplet(t, ctx, repo, goalID)
	strong, _ := seedCausalTriplet(t, ctx, repo, goalID)
	// A second heuristic whose only causal edge is retracted, and a third with none.
	retracted, _ := seedCausalTriplet(t, ctx, repo, goalID)
	plain, _ := seedCausalTriplet(t, ctx, repo, goalID)

	verify := func(interventionID string, confidence float64) {
		t.Helper()
		if err := repo.WriteCausalVerification(ctx, interventionID, 1,
			domain.Outcome{GoalID: goalID, VerificationStatus: domain.VerificationVerified, Value: map[string]any{"e": confidence}},
			domain.ProducedEdge{EffectSize: confidence, Confidence: confidence, EpistemicSource: domain.EpistemicCausalInferred},
		); err != nil {
			t.Fatalf("write causal verification: %v", err)
		}
	}
	verify(weak, 0.7)
	verify(strong, 0.95)
	verify(retracted, 0.9)
	if err := repo.SupersedePriorCausalOutcomes(ctx, retracted, 1); err != nil {
		t.Fatalf("supersede: %v", err)
	}

	verified, retractedMH, plainMH := testutil.NewID(t), testutil.NewID(t), testutil.NewID(t)
	for id, from := range map[string][]string{
		verified:    {weak, strong},
		retractedMH: {retracted},
		plainMH:     {plain},
	} {
		if err := repo.CreateMetaHeuristic(ctx, domain.MetaHeuristic{ID: id, GoalID: goalID, Definition: "d"}, from); err != nil {
			t.Fatalf("create meta-heuristic %q: %v", id, err)
		}
	}

	evidence, err := repo.CausalEvidenceForHeuristics(ctx, []string{verified, retractedMH, plainMH})
	if err != nil {
		t.Fatalf("causal evidence for heuristics: %v", err)
	}
	got, ok := evidence[verified]
	if !ok {
		t.Fatalf("a heuristic with live causal evidence returned none: %+v", evidence)
	}
	if got.Confidence != 0.95 || got.InterventionID != strong {
		t.Fatalf("evidence = %+v, want the strongest surviving edge (%q at 0.95)", got, strong)
	}
	if _, ok := evidence[retractedMH]; ok {
		t.Fatalf("a retracted causal edge is still served as evidence")
	}
	if _, ok := evidence[plainMH]; ok {
		t.Fatalf("an observational-only heuristic was labelled causal")
	}

	// An empty request must not query at all -- an unbounded IN would match the whole
	// corpus.
	if out, err := repo.CausalEvidenceForHeuristics(ctx, nil); err != nil || len(out) != 0 {
		t.Fatalf("empty request = %v (err %v), want an empty map", out, err)
	}
}

// TestCausalEvidenceForInterventions pins the batched read the Sleep-Cycle search
// weights its value estimates by. Every clause it filters on is exercised: only
// causal_inferred edges count, a retracted one does not, an intervention with no
// verification is simply absent, and a re-verified intervention is served at its
// newest version rather than whichever row the scan reached last.
func TestCausalEvidenceForInterventions(t *testing.T) {
	ctx := context.Background()
	repo := newRepo(t, ctx)
	goalID := testutil.NewID(t)

	verified, _ := seedCausalTriplet(t, ctx, repo, goalID)
	reverified, _ := seedCausalTriplet(t, ctx, repo, goalID)
	retracted, _ := seedCausalTriplet(t, ctx, repo, goalID)
	observational, _ := seedCausalTriplet(t, ctx, repo, goalID)

	verify := func(interventionID string, version int, confidence float64) {
		t.Helper()
		if err := repo.WriteCausalVerification(ctx, interventionID, version,
			domain.Outcome{GoalID: goalID, VerificationStatus: domain.VerificationVerified, Value: map[string]any{"e": confidence}},
			domain.ProducedEdge{EffectSize: confidence, Confidence: confidence, EpistemicSource: domain.EpistemicCausalInferred},
		); err != nil {
			t.Fatalf("write causal verification: %v", err)
		}
	}
	verify(verified, 1, 0.8)
	verify(reverified, 1, 0.2)
	verify(reverified, 2, 0.9)
	verify(retracted, 1, 0.95)
	if err := repo.SupersedePriorCausalOutcomes(ctx, retracted, 1); err != nil {
		t.Fatalf("supersede: %v", err)
	}

	evidence, err := repo.CausalEvidenceForInterventions(ctx,
		[]string{verified, reverified, retracted, observational})
	if err != nil {
		t.Fatalf("causal evidence for interventions: %v", err)
	}

	got, ok := evidence[verified]
	if !ok || got.Confidence != 0.8 || got.InterventionID != verified {
		t.Fatalf("verified intervention = %+v (present %v), want its own edge at confidence 0.8", got, ok)
	}
	// Ascending graph_version with a last-row-wins overwrite is only correct because
	// of the ORDER BY; without it the older verification could be served.
	if latest := evidence[reverified]; latest.Confidence != 0.9 || latest.GraphVersion != 2 {
		t.Fatalf("re-verified intervention = %+v, want the newest version's edge", latest)
	}
	if _, present := evidence[retracted]; present {
		t.Fatalf("a retracted verification must not be served: %+v", evidence[retracted])
	}
	if _, present := evidence[observational]; present {
		t.Fatalf("an intervention with only an observational edge must be absent: %+v", evidence[observational])
	}
}

// TestCausalEvidenceForInterventionsShortCircuitsOnNoIDs: the empty case must not
// reach the database, since the search calls it on every run and a goal with no
// eligible finding is ordinary.
func TestCausalEvidenceForInterventionsShortCircuitsOnNoIDs(t *testing.T) {
	ctx := context.Background()
	repo := newRepo(t, ctx)

	evidence, err := repo.CausalEvidenceForInterventions(ctx, nil)
	if err != nil {
		t.Fatalf("causal evidence for no interventions: %v", err)
	}
	if len(evidence) != 0 {
		t.Fatalf("expected an empty map, got %+v", evidence)
	}
}
