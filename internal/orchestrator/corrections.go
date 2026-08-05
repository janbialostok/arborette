package orchestrator

import (
	"context"
	"encoding/json"
	"errors"
	"log"
	"maps"
	"net/http"
	"strings"
	"time"

	"github.com/arborette/arborette/internal/domain"
	"github.com/arborette/arborette/internal/graph"
	"github.com/arborette/arborette/internal/service"
	"github.com/arborette/arborette/internal/store"
)

// defaultCorrectionLockWait bounds how long a correction waits for a concurrent
// correction (or a discovery run) to release the (goal, data-source) lock before
// answering 409, and defaultCorrectionLockPoll paces the retry. The wait is short on
// purpose: the analyst is holding an open request, and "try again" is a better answer
// than a request that hangs behind a discovery sweep.
const (
	defaultCorrectionLockWait = 3 * time.Second
	defaultCorrectionLockPoll = 200 * time.Millisecond
)

// maxCorrectionBytes caps the correction body. A correction is four short strings, so
// this is generous.
const maxCorrectionBytes = 1 << 20

// correctionFanoutTimeout bounds everything that runs after the graph write commits:
// marking the invalidated verifications stale, re-dispatching them, and recording the
// correction. It is sized for the fan-out, not for a single write -- StaleReverifyCap
// dispatches each allowed a launcher timeout would blow any shorter budget, and the
// audit runs last, so a budget the dispatches could exhaust would drop precisely the
// record this endpoint cannot afford to lose.
const correctionFanoutTimeout = 2 * time.Minute

// correctionRequest is the analyst's edit to the discovered causal graph. from and to
// name the edge, read as cause→effect for flip and add; direction overrides that when
// the analyst wants an explicit orientation (including returning an edge to
// undirected).
type correctionRequest struct {
	Op        string `json:"op"`
	From      string `json:"from"`
	To        string `json:"to"`
	Direction string `json:"direction,omitempty"`
}

// handleCausalCorrection applies one analyst correction to a goal's causal graph and
// invalidates what it contradicts: the correction is served as a new graph version,
// the verification records whose adjustment set touched the corrected columns are
// marked stale, and those are re-dispatched under the staleness cap.
//
// The re-dispatches are analyst-initiated and so exempt from the autonomous budget --
// a correction is domain knowledge arriving, and the system must be able to act on it
// even after automation has spent its budget.
func (s *Server) handleCausalCorrection(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	goal, ok := s.lookupGoal(ctx, w, r.PathValue("id"))
	if !ok {
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxCorrectionBytes)
	var req correctionRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		service.WriteErr(w, http.StatusBadRequest, "invalid request body")
		return
	}
	correction, err := parseCorrection(req)
	if err != nil {
		service.WriteErr(w, http.StatusBadRequest, err.Error())
		return
	}

	// columns comes back in the graph's own spelling, which is what the adjustment
	// sets hold -- the analyst's spelling would invalidate nothing.
	version, columns, ok := s.applyCorrection(ctx, w, goal, correction)
	if !ok {
		return
	}

	// Everything below runs after the graph write committed, so it must not be lost to
	// a client that hung up: the endpoint takes no credential, and the audit record is
	// the only attribution this class of write has.
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), correctionFanoutTimeout)
	defer cancel()

	redispatched := s.reverifyStale(ctx, goal, columns)

	// One payload behind all three surfaces, so a field added to the correction cannot
	// reach the audit trail and go missing from the live stream (or the reverse).
	payload := map[string]any{
		"op":            string(correction.Op),
		"columns":       columns,
		"graph_version": version,
		"redispatched":  redispatched,
	}
	detail := maps.Clone(payload)
	detail["optimization_function_id"] = goal.OptimizationFunctionID
	if err := s.recordAudit(ctx, "causal_graph_corrected", "causal_verification", detail); err != nil {
		log.Printf("orchestrator: append audit: %v", err)
	}
	s.hub.PublishLive(goal.OptimizationFunctionID, Event{Type: "causal_graph_corrected", Payload: payload})

	service.WriteJSON(w, http.StatusOK, payload)
}

// applyCorrection takes the (goal, data-source) lock, applies the correction, and
// releases the lock before returning. The lock is a session advisory lock, which pins
// one pooled Postgres connection for as long as it is held, so it covers the
// read-modify-write and nothing else -- holding it across the re-verification
// fan-out would pin that connection through a series of outbound dispatches.
// The bool is false when a response has already been written.
func (s *Server) applyCorrection(ctx context.Context, w http.ResponseWriter, goal store.Goal, correction domain.EdgeCorrection) (int, []string, bool) {
	release, acquired, err := s.acquireGraphLock(ctx, goal)
	if err != nil {
		log.Printf("orchestrator: acquire graph lock for %q: %v", goal.OptimizationFunctionID, err)
		service.WriteErr(w, http.StatusInternalServerError, "internal error")
		return 0, nil, false
	}
	if !acquired {
		service.WriteErr(w, http.StatusConflict, "another correction or discovery is in progress for this goal")
		return 0, nil, false
	}
	defer release()

	version, columns, err := s.repo.CorrectCausalEdge(ctx, goal.OptimizationFunctionID, goal.DataSourceRef, correction)
	if err != nil {
		s.writeCorrectionErr(w, goal.OptimizationFunctionID, err)
		return 0, nil, false
	}
	return version, columns, true
}

// parseCorrection validates the request into a domain correction. Every failure here
// is analyst-fixable, so every one of them is a 400.
func parseCorrection(req correctionRequest) (domain.EdgeCorrection, error) {
	correction := domain.EdgeCorrection{
		Op:        domain.EdgeCorrectionOp(strings.TrimSpace(req.Op)),
		From:      strings.TrimSpace(req.From),
		To:        strings.TrimSpace(req.To),
		Direction: domain.EdgeDirection(strings.TrimSpace(req.Direction)),
	}
	switch correction.Op {
	case domain.CorrectionFlip, domain.CorrectionDelete, domain.CorrectionAdd:
	default:
		return domain.EdgeCorrection{}, errors.New(`op must be "flip", "delete", or "add"`)
	}
	if correction.From == "" || correction.To == "" {
		return domain.EdgeCorrection{}, errors.New("from and to are required")
	}
	if strings.EqualFold(correction.From, correction.To) {
		return domain.EdgeCorrection{}, errors.New("from and to must name different columns")
	}
	switch correction.Direction {
	case "", domain.DirectionAToB, domain.DirectionBToA, domain.DirectionUndirected:
	default:
		return domain.EdgeCorrection{}, errors.New(`direction must be "a_to_b", "b_to_a", or "undirected"`)
	}
	return correction, nil
}

// acquireGraphLock try-acquires the (goal, data-source) advisory lock, polling briefly
// rather than blocking on it: a blocking wait would pin a pool connection for the
// whole of a concurrent discovery sweep.
func (s *Server) acquireGraphLock(ctx context.Context, goal store.Goal) (func(), bool, error) {
	deadline := time.Now().Add(s.correctionLockWait)
	for {
		release, acquired, err := s.graphLock.TryAcquireDiscoveryLock(ctx, goal.OptimizationFunctionID, goal.DataSourceRef)
		if err != nil || acquired {
			return release, acquired, err
		}
		if time.Now().After(deadline) {
			return nil, false, nil
		}
		select {
		case <-ctx.Done():
			return nil, false, ctx.Err()
		case <-time.After(s.correctionLockPoll):
		}
	}
}

// writeCorrectionErr maps a correction failure to a status: the analyst-fixable ones
// surface their own message, everything else is a masked fault.
func (s *Server) writeCorrectionErr(w http.ResponseWriter, goalID string, err error) {
	switch {
	case errors.Is(err, graph.ErrNotFound):
		service.WriteErr(w, http.StatusNotFound, "no causal graph discovered for this goal yet")
	case errors.Is(err, graph.ErrUnknownCausalColumn), errors.Is(err, graph.ErrNoSuchCausalEdge):
		service.WriteErr(w, http.StatusUnprocessableEntity, err.Error())
	default:
		log.Printf("orchestrator: correct causal edge for %q: %v", goalID, err)
		service.WriteErr(w, http.StatusInternalServerError, "internal error")
	}
}

// reverifyStale marks the verifications this correction invalidated and re-dispatches
// them, capped. It reports how many it dispatched.
//
// The re-dispatch is a new record rather than a duplicate because the correction
// bumped the graph version, which is part of the dispatch key. Failures here are
// logged and audited rather than failing the correction: the correction itself is
// durable, and an analyst's edit must not be rejected because the Verifier is down.
func (s *Server) reverifyStale(ctx context.Context, goal store.Goal, columns []string) int {
	stale, err := s.causalVerifications.MarkStale(ctx, goal.OptimizationFunctionID, columns)
	if err != nil {
		log.Printf("orchestrator: mark stale verifications for %q: %v", goal.OptimizationFunctionID, err)
		return 0
	}
	if limit := s.router.StaleReverifyCap; limit >= 0 && len(stale) > limit {
		log.Printf("orchestrator: %d stale verifications for %q, re-dispatching the first %d",
			len(stale), goal.OptimizationFunctionID, limit)
		stale = stale[:limit]
	}
	return s.dispatchVerifications(ctx, goal, stale, false, "causal_reverification_failure")
}
