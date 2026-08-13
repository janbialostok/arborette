package orchestrator

import (
	"context"
	"errors"
	"log"
	"net/http"
	"strconv"
	"time"

	"github.com/arborette/arborette/internal/domain"
	"github.com/arborette/arborette/internal/graph"
	"github.com/arborette/arborette/internal/service"
	"github.com/arborette/arborette/internal/store"
)

// verifierJobName is the job name every Verifier dispatch is launched under. The
// HTTP launcher treats it as a log label -- the endpoint URL selects the worker --
// so it is a constant here rather than config.
const verifierJobName = "arborette-verifier"

// The Verifier dispatch kinds, aliased from the domain so this side of the wire and
// the Verifier's routing switch resolve to one declaration.
const (
	verifierKindVerify = domain.DispatchVerify
	verifierKindClaim  = domain.DispatchClaim
)

// verifyArgs builds the launcher args for one finding's verification. budgeted is
// the accounting seam: an autonomous dispatch (auto-promotion, claim-match extras)
// charges the per-goal budget, while an analyst- or agent-initiated one is exempt and
// bounded by the in-flight cap alone, so automation can never spend a human's turn.
func verifyArgs(goalID, interventionID, datasourceRef string, budgeted bool) map[string]string {
	return map[string]string{
		"kind":            verifierKindVerify,
		"goal_id":         goalID,
		"intervention_id": interventionID,
		"datasource_ref":  datasourceRef,
		"budgeted":        strconv.FormatBool(budgeted),
	}
}

// claimArgs builds the launcher args for a verify-track goal's own claim. It carries
// no intervention id -- the Verifier reifies one from the stored claim -- and no
// budget flag, the primary claim verification being exempt by construction.
func claimArgs(goalID, datasourceRef string) map[string]string {
	return map[string]string{
		"kind":           verifierKindClaim,
		"goal_id":        goalID,
		"datasource_ref": datasourceRef,
	}
}

// handleVerifyFinding dispatches one observational finding for causal verification on
// demand. It is the explicit affordance behind the analyst's verify button and the
// agent-facing verify tool, so the dispatch is exempt from the auto-promotion budget:
// a human request must never be refused because automation already spent it.
//
// The response is a 202 -- verification runs for minutes and reports through the
// goal's SSE channel and the audit trail like every other Verifier transition.
func (s *Server) handleVerifyFinding(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	goal, ok := s.lookupGoal(ctx, w, r.PathValue("id"))
	if !ok {
		return
	}
	findingID := r.PathValue("interventionID")
	intervention, err := s.repo.GetIntervention(ctx, findingID)
	if err != nil {
		if errors.Is(err, graph.ErrNotFound) {
			service.WriteErr(w, http.StatusNotFound, "finding not found")
			return
		}
		log.Printf("orchestrator: get intervention %q: %v", findingID, err)
		service.WriteErr(w, http.StatusInternalServerError, "internal error")
		return
	}
	// A finding of another goal would verify against this goal's causal graph and
	// objective, producing a number about neither.
	if intervention.GoalID != goal.OptimizationFunctionID {
		service.WriteErr(w, http.StatusNotFound, "finding not found")
		return
	}

	if err := s.verifierJobs.Launch(ctx, verifierJobName,
		verifyArgs(goal.OptimizationFunctionID, findingID, goal.DataSourceRef, false)); err != nil {
		log.Printf("orchestrator: dispatch verification for %q: %v", findingID, err)
		service.WriteErr(w, http.StatusBadGateway, "verifier unavailable")
		return
	}

	// The dispatch is irreversible once Launch returns, so its record must not be lost
	// to a client that hung up while it was in flight -- the same reason the correction
	// handler detaches after its own committed write.
	auditCtx, cancel := detached(ctx)
	defer cancel()
	if err := s.recordAudit(auditCtx, "causal_verification_dispatched", "causal_verification", map[string]any{
		"optimization_function_id": goal.OptimizationFunctionID,
		"intervention_id":          findingID,
	}); err != nil {
		log.Printf("orchestrator: append audit: %v", err)
	}
	s.hub.PublishLive(goal.OptimizationFunctionID, Event{Type: "causal_verification_dispatched", Payload: map[string]any{
		"intervention_id": findingID,
	}})

	service.WriteJSON(w, http.StatusAccepted, map[string]any{
		"optimization_function_id": goal.OptimizationFunctionID,
		"intervention_id":          findingID,
	})
}

// causalVerificationDTO is the analyst-facing projection of one causal-verification
// record. store.CausalVerification carries no json tags, so marshalling it directly
// would emit PascalCase keys; the nullable effects stay pointers so a record that has
// not reached a terminal outcome reports null rather than a misleading zero.
type causalVerificationDTO struct {
	ID              string    `json:"id"`
	InterventionID  string    `json:"intervention_id"`
	ObjectiveLabel  string    `json:"objective_label"`
	Filters         []string  `json:"filters"`
	GraphVersion    int       `json:"graph_version"`
	Status          string    `json:"status"`
	NaiveEffect     *float64  `json:"naive_effect"`
	AdjustedEffect  *float64  `json:"adjusted_effect"`
	AdjustmentSet   []string  `json:"adjustment_set"`
	RefutationScore *float64  `json:"refutation_score"`
	Confidence      *float64  `json:"confidence"`
	Stale           bool      `json:"stale"`
	CreatedAt       time.Time `json:"created_at"`
	UpdatedAt       time.Time `json:"updated_at"`
}

// handleListCausalVerifications serves a goal's causal-verification records, newest
// first. It is deliberately a separate surface from the HITL /verifications routes:
// those key on an extraction outcome and answer "did a human confirm this value",
// while these key on a finding's intervention and answer "does the data support this
// as a causal effect".
func (s *Server) handleListCausalVerifications(w http.ResponseWriter, r *http.Request) {
	goal, ok := s.lookupGoal(r.Context(), w, r.PathValue("id"))
	if !ok {
		return
	}
	records, err := s.causalVerifications.ListForGoal(r.Context(), goal.OptimizationFunctionID)
	if err != nil {
		log.Printf("orchestrator: list causal verifications: %v", err)
		service.WriteErr(w, http.StatusInternalServerError, "internal error")
		return
	}
	out := make([]causalVerificationDTO, 0, len(records))
	for _, rec := range records {
		objectiveLabel, filters := s.resolveSegment(r.Context(), rec.InterventionID)
		out = append(out, toCausalVerificationDTO(rec, objectiveLabel, filters))
	}
	service.WriteJSON(w, http.StatusOK, out)
}

// resolveSegment reads a verified finding's human-readable segment -- its objective
// label and full cumulative filter set -- off the Intervention node the record keys
// on, so the effect on the card is self-describing. The segment is purely additive
// over the effects, so a finding whose intervention no longer resolves, whose filters
// fail to decode, or that carries the baseline-style shape with no effective filters
// degrades to an empty segment (never the label alone) rather than failing the list --
// the same skip-on-error convention rankFindings applies to the identical property.
func (s *Server) resolveSegment(ctx context.Context, interventionID string) (string, []string) {
	iv, err := s.repo.GetIntervention(ctx, interventionID)
	if err != nil {
		if !errors.Is(err, graph.ErrNotFound) {
			log.Printf("orchestrator: resolve segment for %q: %v", interventionID, err)
		}
		return "", []string{}
	}
	filters, err := domain.DecodeConstraints(iv.Properties[domain.PropEffectiveFilters])
	if err != nil || len(filters) == 0 {
		return "", []string{}
	}
	objectiveLabel, _ := iv.Properties[domain.PropObjectiveLabel].(string)
	return objectiveLabel, renderConstraints(filters)
}

func toCausalVerificationDTO(rec store.CausalVerification, objectiveLabel string, filters []string) causalVerificationDTO {
	set := rec.AdjustmentSet
	if set == nil {
		set = []string{}
	}
	return causalVerificationDTO{
		ID:              rec.ID,
		InterventionID:  rec.InterventionID,
		ObjectiveLabel:  objectiveLabel,
		Filters:         filters,
		GraphVersion:    rec.GraphVersion,
		Status:          rec.Status,
		NaiveEffect:     rec.NaiveEffect,
		AdjustedEffect:  rec.AdjustedEffect,
		AdjustmentSet:   set,
		RefutationScore: rec.RefutationScore,
		Confidence:      rec.Confidence,
		Stale:           rec.Stale,
		CreatedAt:       rec.CreatedAt,
		UpdatedAt:       rec.UpdatedAt,
	}
}

// dispatchVerifications launches one verification per record, isolating each failure:
// a launcher fault is audited and skipped rather than abandoning the rest, because a
// batch dispatch is best-effort by contract. It reports how many were dispatched.
func (s *Server) dispatchVerifications(ctx context.Context, goal store.Goal, interventionIDs []string, budgeted bool, action string) int {
	dispatched := 0
	for _, id := range interventionIDs {
		if err := s.verifierJobs.Launch(ctx, verifierJobName,
			verifyArgs(goal.OptimizationFunctionID, id, goal.DataSourceRef, budgeted)); err != nil {
			log.Printf("orchestrator: dispatch verification for %q: %v", id, err)
			if auditErr := s.recordAudit(ctx, action, "causal_verification", map[string]any{
				"optimization_function_id": goal.OptimizationFunctionID,
				"intervention_id":          id,
				"error":                    err.Error(),
			}); auditErr != nil {
				log.Printf("orchestrator: append audit: %v", auditErr)
			}
			continue
		}
		dispatched++
	}
	return dispatched
}
