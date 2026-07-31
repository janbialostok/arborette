package orchestrator

import (
	"context"
	"encoding/json"
	"log"
	"net/http"

	"github.com/arborette/arborette/internal/service"
	"github.com/arborette/arborette/internal/store"
)

// recordAudit is the single write path to audit_log, stamping Actor from the
// Identity seam. This service's own events (goal submit, interventions,
// outcomes, branch failures, sleep-cycle triggers) call it in-process; the
// Sleep-Cycle Worker reaches it over HTTP via handleAudit. Either way every
// record carries the stub identity, and the orchestrator remains the sole
// audit-table writer via OrchestratorDSN.
func (s *Server) recordAudit(ctx context.Context, action, eventType string, detail map[string]any) error {
	analyst, err := s.identity.Current(ctx)
	if err != nil {
		return err
	}
	return s.audits.Append(ctx, store.AuditRecord{
		Actor:     analyst.ID,
		Action:    action,
		EventType: eventType,
		Detail:    detail,
	})
}

// maxAuditBytes caps the audit request body so a large or deeply nested detail
// document cannot exhaust memory.
const maxAuditBytes = 1 << 20

type auditRequest struct {
	Action    string         `json:"action"`
	EventType string         `json:"event_type"`
	Detail    map[string]any `json:"detail"`
}

// handleAudit is the HTTP entry the Sleep-Cycle Worker uses to log without
// direct database access. It stamps the identity and appends one record.
func (s *Server) handleAudit(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, maxAuditBytes)
	var req auditRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		service.WriteErr(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if req.Action == "" || req.EventType == "" {
		service.WriteErr(w, http.StatusBadRequest, "action and event_type are required")
		return
	}
	if err := s.recordAudit(r.Context(), req.Action, req.EventType, req.Detail); err != nil {
		log.Printf("orchestrator: append audit: %v", err)
		service.WriteErr(w, http.StatusInternalServerError, "internal error")
		return
	}
	w.WriteHeader(http.StatusCreated)
}
