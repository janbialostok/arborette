package verifier

import (
	"context"
	"errors"
	"fmt"
	"hash/fnv"
	"log"
	"math/rand"
	"time"

	"github.com/google/uuid"

	"github.com/arborette/arborette/internal/domain"
	"github.com/arborette/arborette/internal/store"
)

// verificationResult is the terminal outcome of one verification run: the status,
// the effects and adjustment set (populated only where meaningful — nil for the
// short-circuit outcomes), the refutation detail, and the objective label the causal
// Outcome's value is keyed under.
type verificationResult struct {
	status          verifyOutcome
	naiveEffect     *float64
	adjustedEffect  *float64
	adjustmentSet   []string
	refutationScore *float64
	confidence      *float64
	refutation      *refutationResult
	objectiveLabel  string
}

// VerifyOne runs one finding's atomic verification lifecycle: resolve the graph
// version, charge the leased record at accept, keep the lease alive while it computes,
// classify the outcome, and — only while it still holds the lease — write the causal
// Outcome. It is the verification twin of RunDiscovery.
func (w *Worker) VerifyOne(ctx context.Context, goalID, interventionID, datasourceRef string, budgeted bool) error {
	// 1. Resolve the graph version first, so the (goal, intervention, version) key is
	// fully determined before accounting even though the graph is (re)ensured below.
	version := discoveryVersion
	if g, ok, err := w.graph.GetCausalGraph(ctx, goalID, datasourceRef); err != nil {
		return fmt.Errorf("resolve graph version: %w", err)
	} else if ok {
		version = g.Meta.Version
	}

	// 2. Charge at accept. A coalesced (live/completed) record, or a budget/cap
	// refusal, ends the call with no run and no duplicate transitions.
	rec := store.CausalVerification{
		ID:             uuid.NewString(),
		GoalID:         goalID,
		InterventionID: interventionID,
		GraphVersion:   version,
		Budgeted:       budgeted,
	}
	record, accepted, err := w.verifications.DispatchAccept(ctx, rec)
	if err != nil {
		if errors.Is(err, store.ErrBudgetExhausted) || errors.Is(err, store.ErrInflightCapReached) {
			w.publishVerification(ctx, goalID, map[string]any{
				"type": "verification_rejected", "intervention_id": interventionID, "reason": err.Error(),
			})
			return nil
		}
		return fmt.Errorf("dispatch verification: %w", err)
	}
	if !accepted {
		return nil
	}
	w.publishVerification(ctx, goalID, map[string]any{
		"type": "verification_dispatched", "intervention_id": interventionID,
		"verification_id": record.ID, "graph_version": version,
	})

	// 3. Start the heartbeat before ensure-graph: a first verification runs full
	// discovery inline (far longer than the lease), so without heartbeat coverage the
	// lease would expire and the terminal write would silently no-op.
	stopHeartbeat := w.startHeartbeat(record.ID)
	defer stopHeartbeat()

	// 4. Ensure the graph, compute the adjustment, and refute.
	res, err := w.runVerification(ctx, goalID, interventionID, datasourceRef, version)
	if err != nil {
		// A compute failure leaves the record pending to be reaped (fail-safe); the run
		// goroutine logs it. No graph write.
		w.publishVerification(ctx, goalID, map[string]any{
			"type": "verification_error", "intervention_id": interventionID, "reason": err.Error(),
		})
		return err
	}

	// 5. Claim-then-write. Complete's guarded UPDATE doubles as an ownership re-check:
	// only if we still hold the lease do we touch the served graph.
	held, err := w.verifications.Complete(ctx, record.ID, string(res.status),
		res.naiveEffect, res.adjustedEffect, res.adjustmentSet, res.refutationScore, res.confidence)
	if err != nil {
		return fmt.Errorf("complete verification: %w", err)
	}
	if !held {
		// The lease was reaped to failed and its budget/cap refunded; skip the graph
		// write so a served causal edge is never backed by a failed record.
		log.Printf("verifier: lease for %q reaped mid-run; skipping graph write", record.ID)
		return nil
	}

	if err := w.writeOutcome(ctx, goalID, interventionID, version, res); err != nil {
		return fmt.Errorf("write causal outcome: %w", err)
	}
	w.publishOutcome(ctx, goalID, interventionID, res)
	return nil
}

// runVerification ensures a committed graph, derives the adjustment, measures the
// backdoor-adjusted effect, and refutes it — returning the terminal outcome. A
// deterministic short-circuit (unsupported objective, not identifiable, confounded)
// returns early with the outcome and whatever effects were measured; otherwise the
// refutation score decides causally_verified vs confounded.
func (w *Worker) runVerification(ctx context.Context, goalID, interventionID, datasourceRef string, version int) (verificationResult, error) {
	if err := w.RunDiscovery(ctx, goalID, datasourceRef); err != nil {
		return verificationResult{}, fmt.Errorf("ensure graph: %w", err)
	}
	graph, ok, err := w.graph.GetCausalGraph(ctx, goalID, datasourceRef)
	if err != nil {
		return verificationResult{}, err
	}
	if !ok {
		return verificationResult{}, fmt.Errorf("no causal graph after discovery for goal %q", goalID)
	}

	goal, err := w.goals.Get(ctx, goalID)
	if err != nil {
		return verificationResult{}, fmt.Errorf("get goal %q: %w", goalID, err)
	}
	intervention, err := w.graph.GetIntervention(ctx, interventionID)
	if err != nil {
		return verificationResult{}, fmt.Errorf("get intervention %q: %w", interventionID, err)
	}

	plan, outcome, err := deriveAdjustment(goal, intervention, graph, w.cfg)
	if err != nil {
		return verificationResult{}, err
	}
	if outcome != "" {
		return verificationResult{status: outcome, objectiveLabel: plan.objective.Label}, nil
	}

	adj := &adjuster{analyzer: w.analyzer, datasourceRef: datasourceRef, cfg: w.cfg}
	effect, outcome, err := adj.computeEffect(ctx, plan)
	if err != nil {
		return verificationResult{}, err
	}
	res := verificationResult{objectiveLabel: plan.objective.Label}
	if outcome == outcomeNotIdentifiable {
		res.status = outcome
		return res, nil
	}
	// The confounded and survives-to-refutation branches both carry a fully measured
	// naive and adjusted effect.
	naive, adjusted := effect.naiveEffect, effect.adjustedEffect
	res.naiveEffect, res.adjustedEffect, res.adjustmentSet = &naive, &adjusted, effect.adjustmentSet
	if outcome == outcomeConfounded {
		res.status = outcome
		return res, nil
	}

	ref := &refuter{adjuster: adj, cfg: w.cfg, rng: rand.New(rand.NewSource(seedFrom(interventionID)))}
	refutation, err := ref.refute(ctx, plan, effect.adjustedEffect)
	if err != nil {
		return verificationResult{}, err
	}
	score := refutation.score
	res.refutation = &refutation
	res.refutationScore = &score
	if score >= w.cfg.RefutationTau {
		res.status = outcomeCausallyVerified
		res.confidence = &score
	} else {
		res.status = outcomeConfounded
	}
	return res, nil
}

// writeOutcome performs the graph write a completed verification implies: a confirming
// result writes the causal Outcome and its causal_inferred edge; every non-confirming
// result supersedes any prior causal edge, so a correction that invalidates a claim
// removes it from serving.
func (w *Worker) writeOutcome(ctx context.Context, goalID, interventionID string, version int, res verificationResult) error {
	if res.status != outcomeCausallyVerified {
		return w.graph.SupersedePriorCausalOutcomes(ctx, interventionID, version)
	}
	outcome := domain.Outcome{
		GoalID:             goalID,
		VerificationStatus: domain.VerificationVerified,
		Value:              map[string]any{res.objectiveLabel: *res.adjustedEffect},
	}
	edge := domain.ProducedEdge{
		EffectSize:      *res.adjustedEffect,
		Confidence:      *res.confidence,
		EpistemicSource: domain.EpistemicCausalInferred,
	}
	return w.graph.WriteCausalVerification(ctx, interventionID, version, outcome, edge)
}

// startHeartbeat renews the lease on a ticker until the returned stop is called,
// covering the whole leased window including the inline discovery a first verification
// runs. A heartbeat failure is logged, not fatal — the terminal write's own lease
// guard is the backstop.
func (w *Worker) startHeartbeat(id string) func() {
	ticker := time.NewTicker(w.heartbeatEvery)
	done := make(chan struct{})
	go func() {
		for {
			select {
			case <-done:
				return
			case <-ticker.C:
				beatCtx, cancel := context.WithTimeout(context.Background(), w.heartbeatEvery)
				if err := w.verifications.Heartbeat(beatCtx, id); err != nil {
					log.Printf("verifier: heartbeat %q: %v", id, err)
				}
				cancel()
			}
		}
	}()
	return func() {
		ticker.Stop()
		close(done)
	}
}

// publishVerification pushes one verification transition to the orchestrator (SSE +
// audit), logging a swallowed failure rather than sinking a completed run — the same
// non-fatal convention the discovery audit path follows.
func (w *Worker) publishVerification(ctx context.Context, goalID string, event map[string]any) {
	if err := w.audit.PublishVerification(ctx, goalID, event); err != nil {
		log.Printf("verifier: publish verification event for %q: %v", goalID, err)
	}
}

// publishOutcome emits the terminal verification event plus, when present, the
// refutation detail, so a subscriber sees both the verdict and how it was reached.
func (w *Worker) publishOutcome(ctx context.Context, goalID, interventionID string, res verificationResult) {
	if res.refutation != nil {
		w.publishVerification(ctx, goalID, map[string]any{
			"type": "verification_refutation", "intervention_id": interventionID,
			"placebo": res.refutation.placebo, "stability": res.refutation.stability,
			"random_confounder": res.refutation.randconf, "score": res.refutation.score,
		})
	}
	event := map[string]any{
		"type": "verification_outcome", "intervention_id": interventionID, "status": string(res.status),
		"adjustment_set": res.adjustmentSet,
	}
	if res.naiveEffect != nil {
		event["naive_effect"] = *res.naiveEffect
	}
	if res.adjustedEffect != nil {
		event["adjusted_effect"] = *res.adjustedEffect
	}
	if res.confidence != nil {
		event["confidence"] = *res.confidence
	}
	w.publishVerification(ctx, goalID, event)
}

// seedFrom derives a per-intervention RNG seed via FNV-1a, so the placebo draw varies
// by finding without a shared mutable RNG that concurrent verifications would race.
func seedFrom(s string) int64 {
	h := fnv.New64a()
	_, _ = h.Write([]byte(s))
	return int64(h.Sum64())
}
