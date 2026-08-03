package verifier

import (
	"context"
	"fmt"
	"math"
	"strings"

	"github.com/arborette/arborette/internal/domain"
	"github.com/arborette/arborette/internal/objective"
	"github.com/arborette/arborette/internal/sandboxclient"
	"github.com/arborette/arborette/internal/store"
)

// verifyOutcome is a verification's terminal classification: the status the
// causal_verifications record and the graph write both key on. causally_verified is
// the sole confirming outcome (the only one that writes a causal_inferred edge);
// confounded, not_identifiable, and unsupported_objective are first-class
// non-confirming answers surfaced with equal prominence.
type verifyOutcome string

const (
	outcomeCausallyVerified     verifyOutcome = "causally_verified"
	outcomeConfounded           verifyOutcome = "confounded"
	outcomeNotIdentifiable      verifyOutcome = "not_identifiable"
	outcomeUnsupportedObjective verifyOutcome = "unsupported_objective"
)

// adjustmentPlan is a verification's resolved computation target: the pinned
// objective, the segment predicate reconstructed from the finding's intervention,
// and the backdoor adjustment set Z (as binnable analyze columns, plus its plain
// names for reporting). An empty adjustCols is the legal Z-less case — a finding with
// no discovered confounders, whose adjusted effect equals the naive effect.
type adjustmentPlan struct {
	objective   objective.Objective
	segment     []domain.Constraint
	adjustCols  []sandboxclient.AnalyzeColumn
	adjustNames []string
	// placeboExclude are the columns adjacent to an objective column in the graph —
	// potential causes of the outcome. The placebo refutation draws a pseudo-segment
	// from outside this set, so a column that genuinely moves the objective is never
	// mistaken for a null placebo (which would wrongly depress a real finding's score).
	placeboExclude []string
}

// deriveAdjustment resolves a finding to its adjustment plan or a terminal outcome,
// entirely from the goal, the finding's Intervention, and the committed causal graph
// — no sandbox call. It pins the objective (O's columns), reconstructs the full
// segment conjunction (T's columns) from the intervention's effective filters, and
// derives Z = parents(T) \ (T ∪ O) from directed CAUSES edges. It short-circuits to a
// terminal outcome when: the objective is windowed (unsupported_objective); a
// treatment column is absent from the discovered graph, or any edge incident to T or
// Z is unoriented, so the backdoor set cannot be trusted (not_identifiable). A
// non-empty verifyOutcome means terminal; otherwise the plan is returned.
func deriveAdjustment(goal store.Goal, intervention domain.Intervention, graph domain.CausalGraph, cfg Config) (adjustmentPlan, verifyOutcome, error) {
	obj, err := objective.Pin(goal.EvaluationMatrix)
	if err != nil {
		return adjustmentPlan{}, "", fmt.Errorf("pin objective: %w", err)
	}
	obj.EntityKeyColumn = goal.EntityKeyColumn
	obj.TimeColumn = goal.TimeColumn

	// A windowed/entity-relative objective's causal effect is out of scope: its value
	// is a function of an entity's history, not a stratifiable cross-section, so it
	// short-circuits before any query.
	if domain.HasWindowKind(obj.Expr) {
		return adjustmentPlan{}, outcomeUnsupportedObjective, nil
	}

	segment, err := domain.DecodeConstraints(intervention.Properties[domain.PropEffectiveFilters])
	if err != nil {
		return adjustmentPlan{}, "", fmt.Errorf("decode segment filters: %w", err)
	}

	treatment := constraintColumns(segment)
	outcomeCols := lowerSet(expressionColumns(obj.Expr))
	kinds := columnKinds(graph)

	// Every treatment column must be in the discovered graph: without its adjacencies
	// the backdoor set cannot be reasoned about, so the effect is not identifiable.
	for _, t := range treatment {
		if _, ok := kinds[strings.ToLower(t)]; !ok {
			return adjustmentPlan{}, outcomeNotIdentifiable, nil
		}
	}

	treatmentSet := lowerSet(treatment)
	// An unoriented edge incident to a treatment column hides whether that neighbor is
	// a confounder (a parent, belonging in Z) or a descendant (excluded), so the
	// backdoor set is undetermined — not identifiable.
	if hasUnorientedIncidentEdge(graph.Edges, treatmentSet) {
		return adjustmentPlan{}, outcomeNotIdentifiable, nil
	}

	adjust := adjustmentColumns(graph.Edges, treatmentSet, outcomeCols)
	adjustSet := lowerSet(adjust)
	if hasUnorientedIncidentEdge(graph.Edges, adjustSet) {
		return adjustmentPlan{}, outcomeNotIdentifiable, nil
	}

	cols := make([]sandboxclient.AnalyzeColumn, 0, len(adjust))
	for _, z := range adjust {
		bins := 0
		if kinds[strings.ToLower(z)] {
			bins = cfg.Bins
		}
		cols = append(cols, sandboxclient.AnalyzeColumn{Name: z, Bins: bins})
	}
	return adjustmentPlan{
		objective:      obj,
		segment:        segment,
		adjustCols:     cols,
		adjustNames:    adjust,
		placeboExclude: incidentColumns(graph.Edges, outcomeCols),
	}, "", nil
}

// incidentColumns returns the distinct columns adjacent to any column in set (either
// endpoint of an edge touching it), in first-seen order.
func incidentColumns(edges []domain.CausalEdge, set map[string]bool) []string {
	seen := map[string]bool{}
	var out []string
	add := func(name string) {
		key := strings.ToLower(name)
		if set[key] || seen[key] {
			return
		}
		seen[key] = true
		out = append(out, name)
	}
	for _, e := range edges {
		if set[strings.ToLower(e.ColA)] {
			add(e.ColB)
		}
		if set[strings.ToLower(e.ColB)] {
			add(e.ColA)
		}
	}
	return out
}

// effectResult is the adjusted-effect stage output: the naive observational effect
// (segment minus baseline, unstratified), the backdoor-adjusted effect, and the
// adjustment-set names. A terminal outcome (not_identifiable / confounded) is set
// when the strata violate positivity or the adjusted effect collapses.
type effectResult struct {
	naiveEffect    float64
	adjustedEffect float64
	adjustmentSet  []string
}

// adjuster runs the stratified-effect sandbox calls for one verification, mirroring
// the discovery sweep's collaborator/knob bundle.
type adjuster struct {
	analyzer      analyzer
	datasourceRef string
	cfg           Config
}

// computeEffect measures the naive and backdoor-adjusted effects via two
// stratified_effect calls: a Z-less call for the naive segment-vs-baseline effect,
// and a Z-stratified call reweighted by stratum mass. It returns a terminal outcome
// when any Z-stratum violates positivity (either arm below the support floor, or a
// null aggregate in an above-floor stratum → not_identifiable) or the adjusted effect
// collapses relative to the naive effect (sign flip or magnitude below the collapse
// ratio → confounded). An empty verifyOutcome means the effect survives to
// refutation.
func (a *adjuster) computeEffect(ctx context.Context, plan adjustmentPlan) (effectResult, verifyOutcome, error) {
	naive, err := a.effectCall(ctx, plan, nil, 0, 0)
	if err != nil {
		return effectResult{}, "", err
	}
	naiveEffect, _, ok := reweight(naive.Strata, a.cfg.SupportFloor)
	if !ok {
		// The naive (Z-less) call is a single stratum; if its own arms are below the
		// floor the finding has too little support to verify at all.
		return effectResult{}, outcomeNotIdentifiable, nil
	}

	adjusted, err := a.effectCall(ctx, plan, plan.adjustCols, 0, 0)
	if err != nil {
		return effectResult{}, "", err
	}
	adjEffect, positivity, ok := reweight(adjusted.Strata, a.cfg.SupportFloor)
	if !positivity || !ok {
		return effectResult{}, outcomeNotIdentifiable, nil
	}

	res := effectResult{naiveEffect: naiveEffect, adjustedEffect: adjEffect, adjustmentSet: plan.adjustNames}
	if collapsed(naiveEffect, adjEffect, a.cfg.CollapseRatio) {
		return res, outcomeConfounded, nil
	}
	return res, "", nil
}

// effectCall issues one stratified or sampled effect call for the plan's objective
// and segment over the given adjustment columns. randomBins > 0 adds the synthetic
// random stratifier (the random-confounder refutation); sampleFraction > 0 switches
// to the sampled kind (subsample stability).
func (a *adjuster) effectCall(ctx context.Context, plan adjustmentPlan, adjust []sandboxclient.AnalyzeColumn, randomBins int, sampleFraction float64) (sandboxclient.AnalyzeResponse, error) {
	kind := sandboxclient.AnalyzeStratifiedEffect
	if sampleFraction > 0 {
		kind = sandboxclient.AnalyzeSampledEffect
	}
	expr := plan.objective.Expr
	req := sandboxclient.AnalyzeRequest{
		DataSourceRef:        a.datasourceRef,
		Kind:                 kind,
		Aggregation:          plan.objective.Aggregation,
		ValueExpression:      &expr,
		Segment:              plan.segment,
		Adjust:               adjust,
		SampleFraction:       sampleFraction,
		RandomStratifierBins: randomBins,
	}
	callCtx, cancel := context.WithTimeout(ctx, a.cfg.CallTimeout)
	defer cancel()
	return a.analyzer.Analyze(callCtx, req)
}

// reweight computes the mass-weighted average treatment effect Σ_z (segment −
// baseline)·N_z/ΣN over the strata, gating positivity: every stratum's segment and
// baseline arms must clear the support floor and carry a non-null aggregate.
// positivity is false when any stratum violates that (the not-identifiable trigger);
// ok is false additionally when there is no stratum mass at all. Strata are never
// silently dropped — a dropped stratum would bias the estimate.
func reweight(strata []sandboxclient.StratumRow, floor int) (effect float64, positivity, ok bool) {
	if len(strata) == 0 {
		return 0, false, false
	}
	var totalN int64
	for _, s := range strata {
		if s.SegmentN < int64(floor) || s.BaselineN < int64(floor) || s.SegmentAgg == nil || s.BaselineAgg == nil {
			return 0, false, false
		}
		totalN += s.N
	}
	if totalN <= 0 {
		return 0, true, false
	}
	for _, s := range strata {
		effect += (*s.SegmentAgg - *s.BaselineAgg) * float64(s.N) / float64(totalN)
	}
	return effect, true, true
}

// reweightLenient computes the mass-weighted effect over only the strata with two
// non-null arms, ignoring the support floor. It is the estimate the refutations use:
// a perturbed run (placebo segment, subsample, random-confounder split) produces
// sparser strata than the primary run, so a floor-gated positivity failure there is
// not itself a not-identifiable verdict — it just means that perturbation contributes
// what mass it can. ok is false when no stratum has both arms.
func reweightLenient(strata []sandboxclient.StratumRow) (effect float64, ok bool) {
	var totalN int64
	for _, s := range strata {
		if s.SegmentAgg == nil || s.BaselineAgg == nil {
			continue
		}
		totalN += s.N
	}
	if totalN <= 0 {
		return 0, false
	}
	for _, s := range strata {
		if s.SegmentAgg == nil || s.BaselineAgg == nil {
			continue
		}
		effect += (*s.SegmentAgg - *s.BaselineAgg) * float64(s.N) / float64(totalN)
	}
	return effect, true
}

// collapsed reports whether the adjusted effect no longer supports the naive effect:
// the sign flipped, or the magnitude shrank below the configured retention ratio. It
// is the confounded trigger — an independent hard check that contains the risk of a
// mass-weighted average masking a within-stratum blowout.
func collapsed(naive, adjusted, ratio float64) bool {
	if naive == 0 {
		return false
	}
	if math.Signbit(naive) != math.Signbit(adjusted) {
		return true
	}
	return math.Abs(adjusted) < ratio*math.Abs(naive)
}

// hasUnorientedIncidentEdge reports whether any edge touches a column in set without
// a resolved direction — an undirected (statistics/Claude abstained) or fail-closed
// (unknown/budget_capped) edge. Such an edge leaves the backdoor set undetermined.
func hasUnorientedIncidentEdge(edges []domain.CausalEdge, set map[string]bool) bool {
	for _, e := range edges {
		if !set[strings.ToLower(e.ColA)] && !set[strings.ToLower(e.ColB)] {
			continue
		}
		if e.Direction != domain.DirectionAToB && e.Direction != domain.DirectionBToA {
			return true
		}
	}
	return false
}

// adjustmentColumns collects the parents of the treatment set from directed CAUSES
// edges — a column with an edge oriented into any treatment column — excluding the
// treatment and outcome columns themselves, deduplicated in first-seen order.
func adjustmentColumns(edges []domain.CausalEdge, treatment, outcome map[string]bool) []string {
	seen := map[string]bool{}
	var out []string
	for _, e := range edges {
		cause, effect, ok := directedEndpoints(e)
		if !ok {
			continue
		}
		if !treatment[strings.ToLower(effect)] {
			continue
		}
		key := strings.ToLower(cause)
		if treatment[key] || outcome[key] || seen[key] {
			continue
		}
		seen[key] = true
		out = append(out, cause)
	}
	return out
}

// directedEndpoints returns the cause and effect column of an oriented edge, or
// ok=false for an unoriented one.
func directedEndpoints(e domain.CausalEdge) (cause, effect string, ok bool) {
	switch e.Direction {
	case domain.DirectionAToB:
		return e.ColA, e.ColB, true
	case domain.DirectionBToA:
		return e.ColB, e.ColA, true
	default:
		return "", "", false
	}
}

// constraintColumns returns the distinct filter columns of a segment predicate in
// first-seen order.
func constraintColumns(filters []domain.Constraint) []string {
	seen := map[string]bool{}
	var out []string
	for _, f := range filters {
		key := strings.ToLower(f.Field)
		if seen[key] {
			continue
		}
		seen[key] = true
		out = append(out, f.Field)
	}
	return out
}

// columnKinds maps each discovered column name (lower-cased) to whether discovery
// treated it as numeric, so the adjustment set bins numeric conditioners.
func columnKinds(graph domain.CausalGraph) map[string]bool {
	kinds := make(map[string]bool, len(graph.Columns))
	for _, c := range graph.Columns {
		kinds[strings.ToLower(c.Name)] = c.Kind == "numeric"
	}
	return kinds
}

// lowerSet builds a case-insensitive membership set over column names.
func lowerSet(names []string) map[string]bool {
	set := make(map[string]bool, len(names))
	for _, n := range names {
		set[strings.ToLower(n)] = true
	}
	return set
}
