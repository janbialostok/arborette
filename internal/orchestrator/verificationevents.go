package orchestrator

import (
	"encoding/json"
	"log"
	"net/http"

	"github.com/arborette/arborette/internal/domain"
	"github.com/arborette/arborette/internal/service"
)

// verificationEventRequest is the Verifier's push body: the goal whose channel the
// event belongs to, and the event payload (its own top-level "type" discriminates the
// transition).
type verificationEventRequest struct {
	GoalID string         `json:"goal_id"`
	Event  map[string]any `json:"event"`
}

// handleVerificationEvent is the internal entry the Verifier uses to deliver a
// verification transition through its two sinks in one authenticated call: it streams
// the event on the goal's SSE channel and appends it to the audit trail. The Hub is
// in-process, so an out-of-process service can only reach it this way. PublishLive
// drops the frame when no run is streaming rather than resurrecting evicted state; the
// audit append is the durable record regardless.
func (s *Server) handleVerificationEvent(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, maxAuditBytes)
	var req verificationEventRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		service.WriteErr(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if req.GoalID == "" {
		service.WriteErr(w, http.StatusBadRequest, "goal_id is required")
		return
	}

	s.hub.PublishLive(req.GoalID, Event{Type: "verification", Payload: req.Event})

	action, _ := req.Event["type"].(string)
	if action == "" {
		action = "verification_event"
	}
	// A claim the Verifier could not construct is the one event that also changes
	// durable goal state: registration validated the claim against the schema, and a
	// re-validation failure at dispatch time has no run to fail, so the reason is
	// recorded on the goal itself. A write failure is logged, not fatal -- the audit
	// append below is still the durable trail, and refusing the whole delivery would
	// cost the analyst the SSE frame too.
	if action == domain.ClaimConstructionFailed {
		reason, _ := req.Event["reason"].(string)
		if err := s.goals.SetClaimError(r.Context(), req.GoalID, reason); err != nil {
			log.Printf("orchestrator: set claim error for %q: %v", req.GoalID, err)
		}
	}
	if err := s.recordAudit(r.Context(), action, "causal_verification", req.Event); err != nil {
		log.Printf("orchestrator: append verification audit: %v", err)
		service.WriteErr(w, http.StatusInternalServerError, "internal error")
		return
	}
	w.WriteHeader(http.StatusCreated)
}
