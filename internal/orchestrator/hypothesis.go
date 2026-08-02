package orchestrator

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/arborette/arborette/internal/domain"
	"github.com/arborette/arborette/internal/graph"
	"github.com/arborette/arborette/internal/llm"
	"github.com/arborette/arborette/internal/objective"
	"github.com/arborette/arborette/internal/sandboxclient"
	"github.com/arborette/arborette/internal/service"
	"github.com/arborette/arborette/internal/store"
)

var (
	errExpressionTooDeep = errors.New("objective value expression nests too deeply")

	// errWindowedWithoutBindings is a repairable fit failure: the fitted objective
	// uses a windowed value expression, but the goal bound no entity/time columns for
	// the window to compile against, so the repair loop must strip or replace it.
	errWindowedWithoutBindings = errors.New("objective uses a windowed value expression but the goal has no entity/time bindings")

	// These live in internal/objective, shared with the Sleep-Cycle Worker.
	errNonNumericValue    = objective.ErrNonNumericValue
	errNoObjective        = objective.ErrNoObjective
	errMissingAggregation = objective.ErrMissingAggregation
)

// loopTimeout bounds a whole hypothesis run. The loop runs on a background
// context (decoupled from the SSE stream), so without a deadline a wedged
// dependency would leak the goroutine indefinitely. It is sized for the depth-4
// sequential fan-out — one sandbox Execute per node plus one Xhigh proposal per
// improving internal node, all sequential — so a broad deep run reaches completed
// rather than tripping the deadline and being flipped to failed via ctx.Err();
// raise it in step with defaultDepth.
const loopTimeout = 30 * time.Minute

// statusWriteTimeout bounds a write that must land even though the context which
// triggered it may already be dead -- the run's terminal status, a branch
// failure recorded after the deadline expired, or a resolution's audit after the
// analyst's client hung up. See detached, which derives such a context.
const statusWriteTimeout = 5 * time.Second

// maxProposalRepairs bounds how many times a node's proposal is re-requested when
// candidates reference columns absent from the schema (see domain.UnknownFilterColumns
// for the grounding check). After the bound, any still-invalid candidate is dropped
// so the run always terminates.
const maxProposalRepairs = 3

// handleTriggerLoop starts the Phase-1 hypothesis loop for a goal. It looks the
// goal up (404 on miss), launches the loop in a goroutine on a timeout-bounded
// background context so an SSE disconnect never aborts it, and returns 202.
func (s *Server) handleTriggerLoop(w http.ResponseWriter, r *http.Request) {
	goal, ok := s.lookupGoal(r.Context(), w, r.PathValue("id"))
	if !ok {
		return
	}
	// Create the run row synchronously, before launching the loop, so a client
	// that immediately subscribes and lists always sees a running row rather than
	// racing the goroutine, and a create failure surfaces as a 5xx here.
	runID := uuid.NewString()
	if err := s.runs.Create(r.Context(), runID, goal.OptimizationFunctionID); err != nil {
		log.Printf("orchestrator: create run for %q: %v", goal.OptimizationFunctionID, err)
		service.WriteErr(w, http.StatusInternalServerError, "internal error")
		return
	}
	timeout := s.loopTimeoutFor(goal)
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), timeout)
		defer cancel()
		s.runLoop(ctx, goal, runID)
	}()
	service.WriteJSON(w, http.StatusAccepted, map[string]any{"optimization_function_id": goal.OptimizationFunctionID})
}

// loopTimeoutFor bounds a run. A blocking-mode run waits on human review at
// every queued extraction, so it is bounded on a human timescale instead of the
// machine-paced default, which review would otherwise blow through and flip the
// run to failed.
func (s *Server) loopTimeoutFor(goal store.Goal) time.Duration {
	if epochModeOf(goal) == store.EpochBlocking {
		return s.blockingLoopTimeout
	}
	return loopTimeout
}

// runLoop drives the hypothesis tree: introspect the data source, pin the
// objective and measure the root baseline, then expand each root candidate.
func (s *Server) runLoop(ctx context.Context, goal store.Goal, runID string) {
	id := goal.OptimizationFunctionID
	hist := s.registerHistogram(id)
	// termErr holds a terminal (root) failure; nil means the run completed. Only
	// the root-failure sites below set it, so a per-candidate branch failure (a
	// separate function with no access to it) never flips the run to failed.
	var termErr error
	defer func() {
		// A panic unwinds through this defer with termErr still nil; recover so a
		// crashed run is marked failed rather than mislabeled completed, and one
		// run's panic cannot bring down the orchestrator. Otherwise, a
		// timeout/cancellation that fired anywhere in the run -- including
		// mid-expansion, where per-candidate failures are non-terminal -- also
		// means the run did not complete.
		if r := recover(); r != nil {
			termErr = fmt.Errorf("hypothesis loop panicked: %v", r)
		} else if termErr == nil && ctx.Err() != nil {
			termErr = ctx.Err()
		}
		// Detached from the loop's own deadline/cancellation so the write lands
		// even when the loop terminated because that context expired.
		writeCtx, cancel := context.WithTimeout(context.Background(), statusWriteTimeout)
		defer cancel()
		status, reason := store.RunCompleted, ""
		if termErr != nil {
			status, reason = store.RunFailed, termErr.Error()
		}
		log.Printf("orchestrator: hypothesis loop %q: %s", id, status)
		if reason != "" {
			log.Printf("orchestrator: hypothesis loop %q: failure reason: %s", id, reason)
		}
		if err := s.runs.SetStatus(writeCtx, runID, status, reason); err != nil {
			log.Printf("orchestrator: hypothesis loop %q: set run status: %v", id, err)
		}
		// Stop tracking before the terminal publish: a resolution racing the end
		// of the run must not push a distribution into a run the hub is about to
		// evict (see deregisterHistogram).
		s.deregisterHistogram(id, hist)
		s.hub.Publish(id, Event{Type: "loop_complete"})
		s.hub.Complete(id)
	}()

	// A document goal has no objective to pin or baseline to measure -- its tree is
	// one extraction sub-tree per target field. Branch before pinning the objective so a
	// document goal never trips errNoObjective on the tabular path below.
	if goal.IsDocument() {
		termErr = s.runDocumentLoop(ctx, goal, hist)
		return
	}

	obj, err := objective.Pin(goal.EvaluationMatrix)
	if err != nil {
		log.Printf("orchestrator: hypothesis loop %q: pin objective: %v", id, err)
		termErr = err
		s.branchFailure(ctx, id, nil, err)
		return
	}
	// The window bindings live on the goal, not the matrix, so Pin cannot see them;
	// populate them here so a windowed objective compiles against the entity/time
	// columns for the whole run.
	obj.EntityKeyColumn = goal.EntityKeyColumn
	obj.TimeColumn = goal.TimeColumn
	log.Printf("orchestrator: hypothesis loop %q: objective: %s %s %s", id, obj.Aggregation, obj.Label, obj.Direction)

	introspect, err := s.sandbox.Introspect(ctx, IntrospectRequest{DataSourceRef: goal.DataSourceRef})
	if err != nil {
		log.Printf("orchestrator: hypothesis loop %q: introspect: %v", id, err)
		termErr = err
		s.branchFailure(ctx, id, nil, err)
		return
	}
	schema := toSandboxSchema(introspect.Schema)
	log.Printf("orchestrator: hypothesis loop %q: introspected %d columns", id, len(schema.Columns))

	rootNode := llm.TreeContext{IsRoot: true, Breadth: defaultBreadth}
	rootCandidates, err := s.proposeValidCandidates(ctx, goal, schema, rootNode)
	if err != nil {
		log.Printf("orchestrator: hypothesis loop %q: root proposal: %v", id, err)
		termErr = err
		s.branchFailure(ctx, id, nil, err)
		return
	}
	log.Printf("orchestrator: hypothesis loop %q: %d root candidates proposed", id, len(rootCandidates))

	// Root baseline: the objective measured with no filters. Deeper baselines
	// reuse the parent's outcome value (cumulative nesting makes that valid).
	baseResp, err := s.sandbox.Execute(ctx, objective.ExecuteRequestFor(goal.DataSourceRef, obj, nil))
	if err != nil {
		log.Printf("orchestrator: hypothesis loop %q: root baseline: %v", id, err)
		termErr = err
		s.branchFailure(ctx, id, nil, err)
		return
	}
	baseline, ok := objective.NumericValue(baseResp.Value, obj.Label)
	if !ok {
		termErr = errNonNumericValue
		s.branchFailure(ctx, id, nil, errNonNumericValue)
		return
	}
	rootRowCount, _ := sandboxclient.RowCount(baseResp)
	log.Printf("orchestrator: hypothesis loop %q: root baseline: %.4f (row_count: %d)", id, baseline, rootRowCount)

	s.expandCandidates(ctx, goal, obj, schema, rootNode, nil, baseline, rootCandidates, 1, hist)
}

// maxZeroRowReproposals bounds the zero-row retry to a single re-proposal per
// expansion: replacement candidates are measured but never themselves re-proposed
// on a further zero-row outcome, so the traversal always terminates.
const maxZeroRowReproposals = 1

// expandCandidates measures a set of sibling candidates at one depth, then makes a
// single zero-row re-proposal for any whose segment matched no rows, re-entering
// the replacements at the same depth, parent filters, and baseline as the children
// they replace. It is shared by the root fan-out and each node's child expansion so
// the retry behaves identically at every level. node is the proposal context the
// re-proposal reuses. A replacement that is again zero-row is recorded and pruned
// (the budget is spent) rather than triggering a further re-proposal.
func (s *Server) expandCandidates(ctx context.Context, goal store.Goal, obj objective.Objective, schema llm.SandboxSchema, node llm.TreeContext, parentFilters []domain.Constraint, baseline float64, candidates []llm.CandidateIntervention, depth int, hist *confidenceHistogram) {
	for attempt := 0; ; attempt++ {
		var zeroRow []llm.CandidateIntervention
		for _, c := range candidates {
			if out := s.processCandidate(ctx, goal, obj, schema, parentFilters, baseline, c, depth, hist); out.zeroRow {
				zeroRow = append(zeroRow, c)
			}
		}
		if len(zeroRow) == 0 || attempt >= maxZeroRowReproposals {
			return
		}
		candidates = s.reproposeZeroRow(ctx, goal, schema, node, zeroRow)
		if len(candidates) == 0 {
			return
		}
	}
}

// candidateOutcome is processCandidate's up-signal to its caller. zeroRow marks a
// segment that matched no rows -- a distinct, non-fatal outcome the caller may
// retry with a single re-proposal. A measured, branch-failed, or pruned candidate
// returns the zero value.
type candidateOutcome struct {
	zeroRow bool
}

// processCandidate measures one candidate at its effective (cumulative) filter
// set, writes the causal triplet, and — if the candidate improves the objective
// within constraints and the depth cap is not reached — proposes and expands its
// refinement children. It reports a zero-row segment (no rows matched) as a
// distinct non-fatal outcome so the caller can re-propose; every other disposition
// (measured, branch-failed, pruned) reports the zero value.
func (s *Server) processCandidate(ctx context.Context, goal store.Goal, obj objective.Objective, schema llm.SandboxSchema, parentFilters []domain.Constraint, baseline float64, cand llm.CandidateIntervention, depth int, hist *confidenceHistogram) candidateOutcome {
	id := goal.OptimizationFunctionID
	effective := concatFilters(parentFilters, cand.Filters)
	log.Printf("orchestrator: hypothesis loop %q: depth=%d candidate filters=%v", id, depth, renderConstraints(cand.Filters))

	req := objective.ExecuteRequestFor(goal.DataSourceRef, obj, effective)
	req.IncludeRowCount = true
	resp, err := s.sandbox.Execute(ctx, req)
	if err != nil {
		log.Printf("orchestrator: hypothesis loop %q: depth=%d measurement failed: %v", id, depth, err)
		s.branchFailure(ctx, id, effective, err)
		return candidateOutcome{}
	}
	// The row count decides how to read the rest of the response, so it comes first.
	// A zero-row segment supports no causal claim: it is a distinct, non-fatal
	// outcome (no triplet, not counted) the caller may retry -- detected before the
	// non-numeric bailout because avg/sum/min/max over an empty segment scan as SQL
	// NULL, which NumericValue would otherwise read as a non-numeric failure. A
	// missing count (counted == false) is a version-skew signal, not a zero-row
	// segment: it keeps its non-fatal log below and never fires the zero-row outcome.
	support, counted := sandboxclient.RowCount(resp)
	if counted && support == 0 {
		log.Printf("orchestrator: hypothesis loop %q: depth=%d zero-row segment, pruning", id, depth)
		s.zeroRowSegment(ctx, id, effective)
		return candidateOutcome{zeroRow: true}
	}
	value, ok := objective.NumericValue(resp.Value, obj.Label)
	if !ok {
		log.Printf("orchestrator: hypothesis loop %q: depth=%d non-numeric value", id, depth)
		s.branchFailure(ctx, id, effective, errNonNumericValue)
		return candidateOutcome{}
	}
	// A missing count is a version-skew signal worth logging, but non-fatal: the
	// loop's job does not depend on support, so the outcome persists with 0
	// (self-excluding from the Sleep Cycle's S* floor) rather than failing the
	// candidate — unlike the search, where support drives pruning.
	if !counted {
		log.Printf("orchestrator: hypothesis loop %q: sandbox returned no row count for a counted measurement", id)
	}
	log.Printf("orchestrator: hypothesis loop %q: depth=%d measured %.4f (baseline=%.4f effect=%.4f support=%d)", id, depth, value, baseline, value-baseline, support)

	outcomeID, err := s.writeTriplet(ctx, goal, obj, parentFilters, baseline, cand, effective, value, support)
	if err != nil {
		log.Printf("orchestrator: hypothesis loop %q: write triplet: %v", id, err)
		s.branchFailure(ctx, id, effective, err)
		return candidateOutcome{}
	}
	// A query measurement is verified by construction, so it lands in the top
	// bucket; counting it keeps the live distribution spanning the whole run
	// rather than only its extractions.
	s.recordConfidence(id, hist, outcomeID, verifiedConfidence)

	if !objective.Improves(baseline, value, obj.Direction) {
		log.Printf("orchestrator: hypothesis loop %q: depth=%d did not improve, pruning", id, depth)
		return candidateOutcome{}
	}
	if !constraintsSatisfied(goal.EvaluationMatrix.Constraints, objectiveField(obj), value) {
		log.Printf("orchestrator: hypothesis loop %q: depth=%d violates hard constraint, pruning", id, depth)
		s.branchFailure(ctx, id, effective, errors.New("candidate violates a hard constraint on the objective field"))
		return candidateOutcome{}
	}
	if depth >= defaultDepth {
		log.Printf("orchestrator: hypothesis loop %q: depth=%d at depth cap, not expanding", id, depth)
		return candidateOutcome{}
	}
	log.Printf("orchestrator: hypothesis loop %q: depth=%d improved, expanding", id, depth)

	childNode := llm.TreeContext{
		Breadth:        defaultBreadth,
		ObjectiveLabel: obj.Label,
		Direction:      obj.Direction,
		ParentFilters:  effective,
		PriorValue:     &value,
	}
	children, err := s.proposeValidCandidates(ctx, goal, schema, childNode)
	if err != nil {
		log.Printf("orchestrator: hypothesis loop %q: depth=%d child proposal failed: %v", id, depth, err)
		s.branchFailure(ctx, id, effective, err)
		return candidateOutcome{}
	}
	log.Printf("orchestrator: hypothesis loop %q: depth=%d proposing %d children", id, depth, len(children))
	s.expandCandidates(ctx, goal, obj, schema, childNode, effective, value, children, depth+1, hist)
	return candidateOutcome{}
}

// maxInlinePDFBytes bounds a PDF sent inline as a base64 document block. The
// Claude API rejects an inline document request over 32 MB (a 413), so a document
// larger than this cannot be extracted via the MVP inline path — the run fails
// once with a clear reason rather than a 413 on every extraction node. The Files
// API is the scale path for larger documents.
const maxInlinePDFBytes = 32 << 20

// extractionMethods are the competing extraction approaches that form a field
// sub-tree's breadth axis: each is a prompt-strategy discriminator that varies
// how Extract is instructed. Their count is the document loop's breadth, kept at
// defaultBreadth to match the tabular fan-out.
var extractionMethods = []string{
	"Read the document top to bottom and extract the field where it is explicitly stated.",
	"Locate section headings and labels related to the field, then read the value beside them.",
	"Infer the value from surrounding context when it is not explicitly labelled, staying close to the source text.",
}

// runDocumentLoop drives a document goal's tree: it fetches the run-invariant
// per-page text and the raw PDF bytes once (never per node -- a breadth×depth
// sub-tree per field would otherwise re-download and re-parse the whole document
// at every measurement), then expands one extraction sub-tree per target field.
// It returns a terminal error only for a run-fatal setup failure (the text or
// bytes fetch); per-field branch failures are non-terminal, like the tabular
// per-candidate branches.
func (s *Server) runDocumentLoop(ctx context.Context, goal store.Goal, hist *confidenceHistogram) error {
	id := goal.OptimizationFunctionID

	textResp, err := s.sandbox.DocumentText(ctx, DocumentTextRequest{DataSourceRef: goal.DataSourceRef})
	if err != nil {
		log.Printf("orchestrator: hypothesis loop %q: document text: %v", id, err)
		s.branchFailure(ctx, id, nil, err)
		return err
	}
	pdf, err := s.readObject(ctx, goal.DataSourceRef)
	if err != nil {
		log.Printf("orchestrator: hypothesis loop %q: read document: %v", id, err)
		s.branchFailure(ctx, id, nil, err)
		return err
	}
	if len(pdf) > maxInlinePDFBytes {
		err := fmt.Errorf("document is %d bytes, exceeds the %d-byte inline extraction limit", len(pdf), maxInlinePDFBytes)
		log.Printf("orchestrator: hypothesis loop %q: %v", id, err)
		s.branchFailure(ctx, id, nil, err)
		return err
	}

	for _, field := range goal.TargetFields {
		// Each competing method is a sibling at the field root; the field root's
		// baseline confidence is 0, so the first extraction must clear 0 to expand.
		for _, method := range extractionMethods {
			s.processExtractionCandidate(ctx, goal, field, pdf, textResp.Pages, method, 0.0, 1, hist)
		}
	}
	return nil
}

// processExtractionCandidate is the extraction analog of processCandidate: it
// measures one field with one method, writes the extract-typed triplet, and --
// if the extraction's confidence improves on the parent's and the depth cap is
// not reached -- refines with the competing methods one level deeper. The
// improvement signal is model confidence (maximize), so a refinement must raise
// confidence over its parent to avoid pruning. It shares no code with
// processCandidate, which is welded to the tabular objective (aggregation,
// direction, matrix constraints); extraction has none of those, and runs no
// constraint check.
func (s *Server) processExtractionCandidate(ctx context.Context, goal store.Goal, field domain.TargetField, pdf []byte, pages []string, method string, parentConfidence float64, depth int, hist *confidenceHistogram) {
	id := goal.OptimizationFunctionID
	log.Printf("orchestrator: hypothesis loop %q: extract depth=%d field=%q method=%q", id, depth, field.Name, method)

	value, confidence, err := s.claude.Extract(ctx, pdf, field, method)
	if err != nil {
		log.Printf("orchestrator: hypothesis loop %q: extract depth=%d failed: %v", id, depth, err)
		s.branchFailure(ctx, id, nil, err)
		return
	}
	log.Printf("orchestrator: hypothesis loop %q: extract depth=%d value=%q confidence=%.2f", id, depth, value, confidence)

	locator := locateProvenance(pages, value)
	outcomeID, err := s.writeExtractionTriplet(ctx, goal, field, method, parentConfidence, value, confidence, locator)
	if err != nil {
		log.Printf("orchestrator: hypothesis loop %q: write extraction triplet: %v", id, err)
		s.branchFailure(ctx, id, nil, err)
		return
	}
	// Count before routing: once the entry is queued an analyst can resolve it,
	// and a resolution arriving before the outcome is counted would find nothing
	// to move between buckets.
	s.recordConfidence(id, hist, outcomeID, confidence)
	queued, queueErr := s.routeForVerification(ctx, goal, field, outcomeID, value, confidence, locator)
	blocking := epochModeOf(goal) == store.EpochBlocking
	if queueErr != nil && blocking {
		// Blocking mode cannot degrade into speculative behavior on a queue
		// failure, so the branch fails instead.
		s.branchFailure(ctx, id, nil, fmt.Errorf("blocking mode could not queue the extraction for review: %w", queueErr))
		return
	}

	if !objective.Improves(parentConfidence, confidence, domain.Maximize) {
		log.Printf("orchestrator: hypothesis loop %q: extract depth=%d did not improve confidence, pruning", id, depth)
		return
	}
	if depth >= defaultDepth {
		log.Printf("orchestrator: hypothesis loop %q: extract depth=%d at depth cap, not expanding", id, depth)
		return
	}
	// Refining below an unreviewed extraction is what blocking mode exists to
	// prevent, so the wait sits after the pruning checks: a branch that stops
	// here never needed the verification it would otherwise have waited for.
	if queued && blocking {
		status, err := s.awaitResolution(ctx, id, outcomeID)
		if err != nil {
			// The wait only ends in error when the run's deadline expired or it was
			// cancelled, so the audit for it has to be written detached -- on the
			// dead context it would be dropped, losing the one record that explains
			// why a blocking run stopped.
			failCtx, cancelFail := detached(ctx)
			s.branchFailure(failCtx, id, nil, err)
			cancelFail()
			return
		}
		if status == domain.VerificationRejected {
			return
		}
	}
	log.Printf("orchestrator: hypothesis loop %q: extract depth=%d improved, expanding", id, depth)
	for _, m := range extractionMethods {
		s.processExtractionCandidate(ctx, goal, field, pdf, pages, m, confidence, depth+1, hist)
	}
}

// routeForVerification queues an extraction whose confidence falls below the
// goal's review threshold, reporting whether it was queued and why not. A queue
// failure is logged rather than returned to the measurement path -- routing a
// result for review must never cost the measurement itself, the same posture the
// loop takes toward audit failures -- but it is surfaced to the caller, because
// blocking mode cannot treat an unqueued extraction as reviewable.
func (s *Server) routeForVerification(ctx context.Context, goal store.Goal, field domain.TargetField, outcomeID, value string, confidence float64, locator *domain.ProvenanceLocator) (bool, error) {
	if confidence >= s.effectiveThreshold(goal) {
		return false, nil
	}
	entry := store.VerificationEntry{
		QueueID:                uuid.NewString(),
		OptimizationFunctionID: goal.OptimizationFunctionID,
		OutcomeID:              outcomeID,
		Field:                  field.Name,
		ExtractedValue:         value,
		Provenance:             locator,
		Confidence:             confidence,
	}
	if err := s.queue.Enqueue(ctx, entry); err != nil {
		log.Printf("orchestrator: hypothesis loop %q: enqueue verification: %v", goal.OptimizationFunctionID, err)
		return false, err
	}
	return true, nil
}

// defaultVerificationPoll paces the blocking gate's wait. Reviews take minutes
// at best, so the poll is coarse enough to cost nothing over a long wait and
// fine enough that the loop resumes promptly once a verdict lands.
const defaultVerificationPoll = 2 * time.Second

// awaitResolution blocks until a queued extraction's verdict has landed on the
// outcome itself, returning the resulting verification status. A read failure is
// logged and retried rather than ending the wait: at the poll cadence a
// day-long wait issues tens of thousands of reads, and abandoning the branch on
// one transient fault would discard the very review a human is in the middle of.
// It watches the
// graph rather than the queue row deliberately: the row is claimed before the
// verdict is written through, so resuming on the claim would let the loop prune
// or refine on a resolution that could still fail and be rolled back -- the
// opposite of what blocking mode promises. The loop is a single goroutine, so
// this stalls the whole traversal behind this node, the trade a goal makes by
// choosing blocking mode. The run's deadline bounds the wait: an expiry (or a
// cancelled run) ends it as a branch failure rather than hanging.
func (s *Server) awaitResolution(ctx context.Context, goalID, outcomeID string) (domain.VerificationStatus, error) {
	ticker := time.NewTicker(s.verificationPoll)
	defer ticker.Stop()
	for {
		if err := ctx.Err(); err != nil {
			return "", err
		}
		outcome, err := s.repo.GetExtractionOutcome(ctx, outcomeID)
		switch {
		case errors.Is(err, graph.ErrNotFound):
			// The outcome is gone; no verdict can ever arrive for it.
			return "", err
		case err != nil:
			log.Printf("orchestrator: hypothesis loop %q: read verdict for %q: %v", goalID, outcomeID, err)
		case outcome.VerificationStatus != domain.VerificationUnverified:
			return outcome.VerificationStatus, nil
		}
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case <-ticker.C:
		}
	}
}

// readObject returns the object's raw bytes opaquely -- no PDF parsing in the
// orchestrator; parsing stays behind the sandbox's /document/text endpoint.
func (s *Server) readObject(ctx context.Context, ref string) ([]byte, error) {
	r, err := s.objects.Get(ctx, ref)
	if err != nil {
		return nil, err
	}
	defer r.Close()
	return io.ReadAll(r)
}

// locateProvenance finds the first exact occurrence of the extracted value across
// the cached per-page text and records its locator (0-based page index and byte
// offsets into that page). It yields nil for an empty value or when no exact
// match is found -- no fuzzy matching in the MVP.
func locateProvenance(pages []string, needle string) *domain.ProvenanceLocator {
	if needle == "" {
		return nil
	}
	for i, page := range pages {
		if idx := strings.Index(page, needle); idx >= 0 {
			return &domain.ProvenanceLocator{Page: i, CharStart: idx, CharEnd: idx + len(needle)}
		}
	}
	return nil
}

// writeTriplet persists one State→Intervention→Outcome triplet and audits the
// intervention and outcome. The persisted finding is observational (a correlation,
// not a causal effect); the PRODUCED edge is tagged accordingly (see
// domain.EpistemicSource). The start state is the objective measured at the
// parent's effective filters (the baseline the candidate is judged against);
// query outcomes are always verified with PRODUCED confidence fixed at 1.0. It
// returns the outcome's id so the caller can count it in the run's confidence
// distribution.
func (s *Server) writeTriplet(ctx context.Context, goal store.Goal, obj objective.Objective, parentFilters []domain.Constraint, baseline float64, cand llm.CandidateIntervention, effective []domain.Constraint, value float64, support int64) (string, error) {
	stateID := uuid.NewString()
	state := domain.State{ID: stateID, GoalID: goal.OptimizationFunctionID, Properties: map[string]any{
		domain.PropDataSourceRef:        goal.DataSourceRef,
		domain.PropObjectiveAggregation: obj.Aggregation,
		domain.PropObjectiveLabel:       obj.Label,
		domain.PropEffectiveFilters:     parentFilters,
		"value":                         baseline,
	}}
	if err := s.repo.CreateState(ctx, state); err != nil {
		return "", err
	}

	interventionID := uuid.NewString()
	intervention := domain.Intervention{ID: interventionID, GoalID: goal.OptimizationFunctionID, Type: domain.InterventionQuery, Properties: map[string]any{
		domain.PropObjectiveAggregation: obj.Aggregation,
		domain.PropObjectiveLabel:       obj.Label,
		domain.PropNewFilters:           cand.Filters,
		domain.PropEffectiveFilters:     effective,
	}}
	if err := s.repo.CreateIntervention(ctx, intervention); err != nil {
		return "", err
	}

	outcomeID := uuid.NewString()
	outcome := domain.Outcome{ID: outcomeID, GoalID: goal.OptimizationFunctionID, VerificationStatus: domain.VerificationVerified, Value: map[string]any{obj.Label: value}, Support: support}
	if err := s.repo.CreateOutcome(ctx, outcome); err != nil {
		return "", err
	}

	if err := s.repo.CreatePreConditionFor(ctx, stateID, interventionID); err != nil {
		return "", err
	}
	if err := s.repo.CreateProduced(ctx, interventionID, outcomeID, domain.ProducedEdge{EffectSize: value - baseline, Confidence: verifiedConfidence, EpistemicSource: domain.EpistemicObservational}); err != nil {
		return "", err
	}

	// Audit every intervention and outcome, not only failures.
	if err := s.recordAudit(ctx, "hypothesis_intervention", "intervention", map[string]any{
		"optimization_function_id": goal.OptimizationFunctionID,
		"intervention_id":          interventionID,
		domain.PropNewFilters:      cand.Filters,
	}); err != nil {
		log.Printf("orchestrator: append audit: %v", err)
	}
	if err := s.recordAudit(ctx, "hypothesis_outcome", "outcome", map[string]any{
		"optimization_function_id": goal.OptimizationFunctionID,
		"outcome_id":               outcomeID,
		"value":                    value,
		"support":                  support,
	}); err != nil {
		log.Printf("orchestrator: append audit: %v", err)
	}

	s.hub.Publish(goal.OptimizationFunctionID, Event{Type: "triplet", Payload: map[string]any{
		"state_id":                stateID,
		"intervention_id":         interventionID,
		"outcome_id":              outcomeID,
		"baseline":                baseline,
		"value":                   value,
		"effect_size":             value - baseline,
		domain.PropObjectiveLabel: obj.Label,
		"direction":               string(obj.Direction),
		"filters":                 renderConstraints(effective),
		domain.PropNewFilters:     renderConstraints(cand.Filters),
	}})
	return outcomeID, nil
}

// writeExtractionTriplet is the extract-path analog of writeTriplet: it persists
// one State→Intervention→Outcome triplet for an extraction measurement. Unlike
// the query path it writes Intervention.Type extract, an unverified outcome (which
// feeds the HITL queue and gates Sleep-Cycle clustering), the deterministic
// provenance locator, and a PRODUCED confidence that is the model's self-reported
// value rather than the fixed 1.0. The effect size is the confidence delta over
// the parent, mirroring the query path's value delta over its baseline. The
// epistemic source stays observational. It returns the outcome's id so the
// caller can route it for review and count it in the run's distribution.
func (s *Server) writeExtractionTriplet(ctx context.Context, goal store.Goal, field domain.TargetField, method string, parentConfidence float64, value string, confidence float64, locator *domain.ProvenanceLocator) (string, error) {
	stateID := uuid.NewString()
	state := domain.State{ID: stateID, GoalID: goal.OptimizationFunctionID, Properties: map[string]any{
		domain.PropDataSourceRef: goal.DataSourceRef,
		"field":                  field.Name,
		"confidence":             parentConfidence,
	}}
	if err := s.repo.CreateState(ctx, state); err != nil {
		return "", err
	}

	interventionID := uuid.NewString()
	intervention := domain.Intervention{ID: interventionID, GoalID: goal.OptimizationFunctionID, Type: domain.InterventionExtract, Properties: map[string]any{
		"field":  field.Name,
		"method": method,
	}}
	if err := s.repo.CreateIntervention(ctx, intervention); err != nil {
		return "", err
	}

	outcomeID := uuid.NewString()
	// The stored Outcome.Value is keyed by the field name, mirroring the query
	// path's label-keyed value; the llm layer returns just the raw string.
	outcome := domain.Outcome{
		ID:                 outcomeID,
		GoalID:             goal.OptimizationFunctionID,
		VerificationStatus: domain.VerificationUnverified,
		Value:              map[string]any{field.Name: value},
		Provenance:         locator,
	}
	if err := s.repo.CreateOutcome(ctx, outcome); err != nil {
		return "", err
	}

	if err := s.repo.CreatePreConditionFor(ctx, stateID, interventionID); err != nil {
		return "", err
	}
	if err := s.repo.CreateProduced(ctx, interventionID, outcomeID, domain.ProducedEdge{
		EffectSize:      confidence - parentConfidence,
		Confidence:      confidence,
		EpistemicSource: domain.EpistemicObservational,
	}); err != nil {
		return "", err
	}

	if err := s.recordAudit(ctx, "hypothesis_intervention", "intervention", map[string]any{
		"optimization_function_id": goal.OptimizationFunctionID,
		"intervention_id":          interventionID,
		"field":                    field.Name,
	}); err != nil {
		log.Printf("orchestrator: append audit: %v", err)
	}
	if err := s.recordAudit(ctx, "hypothesis_outcome", "outcome", map[string]any{
		"optimization_function_id": goal.OptimizationFunctionID,
		"outcome_id":               outcomeID,
		"confidence":               confidence,
	}); err != nil {
		log.Printf("orchestrator: append audit: %v", err)
	}

	// The extract path reuses the "triplet" SSE type but deliberately carries a
	// different payload than writeTriplet's query triplet (field/method/string
	// value/provenance vs baseline/effect_size/filters). A document run's stream is
	// homogeneous, so the web client reads it by the goal kind.
	s.hub.Publish(goal.OptimizationFunctionID, Event{Type: "triplet", Payload: map[string]any{
		"state_id":        stateID,
		"intervention_id": interventionID,
		"outcome_id":      outcomeID,
		"field":           field.Name,
		"method":          method,
		"confidence":      confidence,
		"value":           value,
		// The domain locator carries no json tags, so publishing it directly would
		// put PascalCase keys on a wire that is snake_case everywhere else.
		"provenance": toProvenanceDTO(locator),
	}})
	return outcomeID, nil
}

// renderConstraints renders each filter to a predicate chip for the triplet SSE
// payload, so the web card shows which segment the triplet measured. A non-nil
// empty slice marshals to [] rather than null.
func renderConstraints(filters []domain.Constraint) []string {
	chips := make([]string, 0, len(filters))
	for _, f := range filters {
		chips = append(chips, domain.RenderConstraint(f))
	}
	return chips
}

// branchFailure records a branch-failure audit event and emits a failure SSE
// event; siblings continue.
func (s *Server) branchFailure(ctx context.Context, id string, filters []domain.Constraint, cause error) {
	if err := s.recordAudit(ctx, "hypothesis_branch_failure", "failure", map[string]any{
		"optimization_function_id": id,
		"filters":                  filters,
		"error":                    cause.Error(),
	}); err != nil {
		log.Printf("orchestrator: append audit: %v", err)
	}
	s.hub.Publish(id, Event{Type: "branch_failure", Payload: map[string]any{"error": cause.Error()}})
}

// zeroRowSegment records a zero-row-segment audit event and emits a distinct SSE
// event. It is deliberately separate from branchFailure so a segment that simply
// matched no rows is never confused with a measurement failure: it is non-fatal,
// writes no triplet, and is not counted in the run's confidence distribution
// (an empty segment supports no causal claim, so it carries no value). Siblings
// continue, and the caller may make one re-proposal in its place.
func (s *Server) zeroRowSegment(ctx context.Context, id string, filters []domain.Constraint) {
	if err := s.recordAudit(ctx, "hypothesis_zero_row_segment", "outcome", map[string]any{
		"optimization_function_id": id,
		"filters":                  filters,
	}); err != nil {
		log.Printf("orchestrator: append audit: %v", err)
	}
	s.hub.Publish(id, Event{Type: "zero_row_segment", Payload: map[string]any{"filters": renderConstraints(filters)}})
}

// proposeValidCandidates proposes one node's candidates and repairs any that fail
// grounding — a filter column absent from the schema or an equality/membership
// value absent from the column's known value set — re-proposing only the invalid
// ones against the offending columns and values. Earlier-round valid candidates
// accumulate and are never re-proposed, so no candidate is dropped or duplicated. A
// transport error from the initial proposal is returned as-is, so callers keep the
// terminal-at-root / non-terminal-in-child distinction; a repair-call transport
// error is best-effort — it keeps the candidates already validated and records the
// fault as a branch failure rather than sinking the run. After the repair bound,
// still-invalid candidates are dropped and a branch failure naming the unknown
// columns and values is recorded (siblings continue); if none remain valid, the
// returned slice is empty and that branch stops expanding.
func (s *Server) proposeValidCandidates(ctx context.Context, goal store.Goal, schema llm.SandboxSchema, node llm.TreeContext) ([]llm.CandidateIntervention, error) {
	id := goal.OptimizationFunctionID
	proposal, err := s.claude.ProposeInterventionTree(ctx, goal.GoalText, goal.EvaluationMatrix, schema, node)
	if err != nil {
		return nil, err
	}
	cols := columnNames(schema)
	vals := columnValues(schema)
	valid, invalid, unknownCols, unknownVals := splitByGrounding(proposal.Candidates, cols, vals)

	for attempts := 0; len(invalid) > 0 && attempts < maxProposalRepairs; attempts++ {
		repaired, rerr := s.claude.RepairInterventionTree(ctx, goal.GoalText, goal.EvaluationMatrix, schema, node,
			llm.Proposal{Candidates: invalid}, groundingErrorMessage(unknownCols, unknownVals))
		if rerr != nil {
			log.Printf("orchestrator: hypothesis loop %q: repair intervention tree: %v", id, rerr)
			s.branchFailure(ctx, id, node.ParentFilters, rerr)
			return valid, nil
		}
		newValid, newInvalid, newCols, newVals := splitByGrounding(repaired.Candidates, cols, vals)
		valid = append(valid, newValid...)
		invalid, unknownCols, unknownVals = newInvalid, newCols, newVals
	}

	if len(invalid) > 0 {
		s.branchFailure(ctx, id, node.ParentFilters,
			fmt.Errorf("dropped %d candidate(s) that failed grounding: %s", len(invalid), groundingErrorMessage(unknownCols, unknownVals)))
	}
	return valid, nil
}

// reproposeZeroRow makes the one allowed re-proposal for candidates whose segments
// matched zero rows, seeding RepairInterventionTree with the empty candidates and a
// message naming their filter sets. Replacements pass the same grounding
// post-checks as any proposal; those that fail grounding are dropped -- there is no
// further repair, the budget is this single call. A transport error yields no
// replacements and is recorded as a branch failure, matching proposeValidCandidates'
// best-effort repair posture.
func (s *Server) reproposeZeroRow(ctx context.Context, goal store.Goal, schema llm.SandboxSchema, node llm.TreeContext, zeroRow []llm.CandidateIntervention) []llm.CandidateIntervention {
	id := goal.OptimizationFunctionID
	repaired, err := s.claude.RepairInterventionTree(ctx, goal.GoalText, goal.EvaluationMatrix, schema, node,
		llm.Proposal{Candidates: zeroRow}, zeroRowReproposalMessage(zeroRow))
	if err != nil {
		log.Printf("orchestrator: hypothesis loop %q: zero-row re-proposal: %v", id, err)
		s.branchFailure(ctx, id, node.ParentFilters, err)
		return nil
	}
	valid, _, _, _ := splitByGrounding(repaired.Candidates, columnNames(schema), columnValues(schema))
	log.Printf("orchestrator: hypothesis loop %q: zero-row re-proposal returned %d replacement(s)", id, len(valid))
	return valid
}

func zeroRowReproposalMessage(zeroRow []llm.CandidateIntervention) string {
	sets := make([]string, 0, len(zeroRow))
	for _, c := range zeroRow {
		sets = append(sets, "{"+strings.Join(renderConstraints(c.Filters), ", ")+"}")
	}
	return "these candidate filter sets matched zero rows: " + strings.Join(sets, "; ") +
		"; propose different candidates whose segments contain rows"
}

// splitByGrounding partitions candidates into those fully grounded in the schema —
// every filter column known and every equality/membership value drawn from its
// column's listed value set — and those referencing at least one unknown column or
// value, collecting both offender lists (deduplicated, first-seen) for the repair
// prompt. A candidate with any unknown column or value is invalid; the two lists
// are gathered independently so the repair message can name each concern.
func splitByGrounding(candidates []llm.CandidateIntervention, columns []string, values map[string][]string) (valid, invalid []llm.CandidateIntervention, unknownCols, unknownVals []string) {
	seenCol := map[string]bool{}
	seenVal := map[string]bool{}
	for _, cand := range candidates {
		missCols := domain.UnknownFilterColumns(cand.Filters, columns)
		missVals := domain.UnknownFilterValues(cand.Filters, values)
		if len(missCols) == 0 && len(missVals) == 0 {
			valid = append(valid, cand)
			continue
		}
		invalid = append(invalid, cand)
		for _, u := range missCols {
			key := strings.ToLower(u)
			if !seenCol[key] {
				seenCol[key] = true
				unknownCols = append(unknownCols, u)
			}
		}
		for _, u := range missVals {
			key := strings.ToLower(u)
			if !seenVal[key] {
				seenVal[key] = true
				unknownVals = append(unknownVals, u)
			}
		}
	}
	return valid, invalid, unknownCols, unknownVals
}

func columnNames(schema llm.SandboxSchema) []string {
	names := make([]string, 0, len(schema.Columns))
	for _, c := range schema.Columns {
		names = append(names, c.Name)
	}
	return names
}

// columnValues maps each value-constrained column to its listed distinct values,
// keyed by the column's exact name. High-cardinality/continuous columns (nil
// DistinctValues) are omitted, so the value post-check skips them.
func columnValues(schema llm.SandboxSchema) map[string][]string {
	values := map[string][]string{}
	for _, c := range schema.Columns {
		if len(c.DistinctValues) > 0 {
			values[c.Name] = c.DistinctValues
		}
	}
	return values
}

// groundingErrorMessage assembles the repair-prompt validation error from the
// unknown-column and unknown-value offender lists, naming only the concerns that
// actually occurred.
func groundingErrorMessage(unknownCols, unknownVals []string) string {
	var parts []string
	if len(unknownCols) > 0 {
		parts = append(parts, unknownColumnsMessage(unknownCols))
	}
	if len(unknownVals) > 0 {
		parts = append(parts, unknownValuesMessage(unknownVals))
	}
	return strings.Join(parts, " ")
}

func unknownColumnsMessage(unknown []string) string {
	return "these filter columns are not in the schema: " + strings.Join(unknown, ", ") +
		"; re-propose using only columns from the Available columns list"
}

func unknownValuesMessage(unknown []string) string {
	return "these filter values are not present in their column: " + strings.Join(unknown, ", ") +
		"; re-propose using only values from each column's listed value set"
}

// dryRunObjective validates a fitted matrix by executing its pinned objective
// against the sandbox with no filters — the same request the root baseline runs,
// pinned identically. A nil return means the objective compiles and measures; a
// non-nil error is either a pin failure or the sandbox's execute error.
func (s *Server) dryRunObjective(ctx context.Context, ref string, matrix domain.EvaluationMatrix, entityKey, timeColumn string) error {
	obj, err := objective.Pin(matrix)
	if err != nil {
		return err
	}
	// The output schema does not bound expression nesting (the value is a plain
	// JSON string), so guard depth here — the message flows verbatim into
	// RepairEvaluationMatrix via the existing repair loop.
	if domain.ExpressionDepth(obj.Expr) > domain.MaxObjectiveExpressionDepth {
		return fmt.Errorf("%w (max %d)", errExpressionTooDeep, domain.MaxObjectiveExpressionDepth)
	}
	// A malformed window (nested, or an out-of-range offset/size) and a windowed
	// objective on a goal that bound no entity/time columns are both repairable
	// fit failures the repair loop can strip or replace, so they are validation
	// failures rather than sandbox faults. The bindings come from the submitted
	// request (the goal row does not exist yet), so without threading them a valid
	// windowed goal would dry-run bindings-less and 422 spuriously.
	if err := domain.ValidateWindowShape(obj.Expr); err != nil {
		return err
	}
	if domain.HasWindowKind(obj.Expr) && (entityKey == "" || timeColumn == "") {
		return errWindowedWithoutBindings
	}
	obj.EntityKeyColumn = entityKey
	obj.TimeColumn = timeColumn
	_, err = s.sandbox.Execute(ctx, objective.ExecuteRequestFor(ref, obj, nil))
	return err
}

// isObjectiveValidationFailure reports whether a dry-run error is a repairable
// objective-fit failure: a missing/absent aggregation the model can supply, or a
// sandbox 400 compile/type error. A sandbox 4xx≠400, 5xx, or transport error is a
// fault, not an unfixable objective.
func isObjectiveValidationFailure(err error) bool {
	if errors.Is(err, errNoObjective) || errors.Is(err, errMissingAggregation) || errors.Is(err, errExpressionTooDeep) ||
		errors.Is(err, errWindowedWithoutBindings) ||
		errors.Is(err, domain.ErrWindowNested) || errors.Is(err, domain.ErrWindowBounds) {
		return true
	}
	var se *SandboxError
	return errors.As(err, &se) && se.Status == http.StatusBadRequest
}

// objectiveField is the constraint field a hard constraint can be checked
// against: the objective's column only when it is a bare ColumnRef. A compound
// expression has no single column, so it yields "" and matches no constraint
// (expression-based constraints are deferred).
func objectiveField(obj objective.Objective) string {
	if obj.Expr.Kind == domain.ColumnRefKind {
		return obj.Expr.Column
	}
	return ""
}

// constraintsSatisfied checks the measured objective value against any hard
// constraint on the objective field. Constraints on other fields cannot be
// verified from a single objective aggregate and are treated as satisfied.
func constraintsSatisfied(constraints []domain.Constraint, field string, value float64) bool {
	for _, c := range constraints {
		if c.Field != field {
			continue
		}
		switch c.Op {
		case domain.LessThan:
			if !(value < c.Value) {
				return false
			}
		case domain.LessThanOrEqual:
			if !(value <= c.Value) {
				return false
			}
		case domain.GreaterThan:
			if !(value > c.Value) {
				return false
			}
		case domain.GreaterThanOrEqual:
			if !(value >= c.Value) {
				return false
			}
		}
	}
	return true
}

// concatFilters returns a fresh slice of the parent's effective filters plus the
// candidate's newly proposed filters (cumulative nesting), never aliasing the
// parent slice.
func concatFilters(parent, added []domain.Constraint) []domain.Constraint {
	out := make([]domain.Constraint, 0, len(parent)+len(added))
	out = append(out, parent...)
	out = append(out, added...)
	return out
}

func toSandboxSchema(schema schemaDTO) llm.SandboxSchema {
	cols := make([]llm.SandboxColumn, 0, len(schema.Columns))
	for _, c := range schema.Columns {
		cols = append(cols, llm.SandboxColumn{Name: c.Name, Type: c.Type, DistinctValues: c.DistinctValues})
	}
	return llm.SandboxSchema{Columns: cols}
}
