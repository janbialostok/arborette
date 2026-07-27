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
	"github.com/arborette/arborette/internal/llm"
	"github.com/arborette/arborette/internal/objective"
	"github.com/arborette/arborette/internal/store"
)

var (
	errExpressionTooDeep = errors.New("objective value expression nests too deeply")

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

// statusWriteTimeout bounds the run's terminal status write. It runs on a
// context detached from the loop's own deadline/cancellation, so a run whose
// loopTimeout expired still records its terminal status instead of stranding at
// running.
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
		writeErr(w, http.StatusInternalServerError, "internal error")
		return
	}
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), loopTimeout)
		defer cancel()
		s.runLoop(ctx, goal, runID)
	}()
	writeJSON(w, http.StatusAccepted, map[string]any{"optimization_function_id": goal.OptimizationFunctionID})
}

// runLoop drives the hypothesis tree: introspect the data source, pin the
// objective and measure the root baseline, then expand each root candidate.
func (s *Server) runLoop(ctx context.Context, goal store.Goal, runID string) {
	id := goal.OptimizationFunctionID
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
		if err := s.runs.SetStatus(writeCtx, runID, status, reason); err != nil {
			log.Printf("orchestrator: hypothesis loop %q: set run status: %v", id, err)
		}
		s.hub.Publish(id, Event{Type: "loop_complete"})
		s.hub.Complete(id)
	}()

	// A document goal has no objective to pin or baseline to measure -- its tree is
	// one extraction sub-tree per target field. Branch before pinning the objective so a
	// document goal never trips errNoObjective on the tabular path below.
	if goal.IsDocument() {
		termErr = s.runDocumentLoop(ctx, goal)
		return
	}

	obj, err := objective.Pin(goal.EvaluationMatrix)
	if err != nil {
		log.Printf("orchestrator: hypothesis loop %q: pin objective: %v", id, err)
		termErr = err
		s.branchFailure(ctx, id, nil, err)
		return
	}

	introspect, err := s.sandbox.Introspect(ctx, IntrospectRequest{DataSourceRef: goal.DataSourceRef})
	if err != nil {
		log.Printf("orchestrator: hypothesis loop %q: introspect: %v", id, err)
		termErr = err
		s.branchFailure(ctx, id, nil, err)
		return
	}
	schema := toSandboxSchema(introspect.Schema)

	rootCandidates, err := s.proposeValidCandidates(ctx, goal, schema,
		llm.TreeContext{IsRoot: true, Breadth: defaultBreadth})
	if err != nil {
		log.Printf("orchestrator: hypothesis loop %q: root proposal: %v", id, err)
		termErr = err
		s.branchFailure(ctx, id, nil, err)
		return
	}

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

	for _, cand := range rootCandidates {
		s.processCandidate(ctx, goal, obj, schema, nil, baseline, cand, 1)
	}
}

// processCandidate measures one candidate at its effective (cumulative) filter
// set, writes the causal triplet, and — if the candidate improves the objective
// within constraints and the depth cap is not reached — proposes and expands its
// refinement children.
func (s *Server) processCandidate(ctx context.Context, goal store.Goal, obj objective.Objective, schema llm.SandboxSchema, parentFilters []domain.Constraint, baseline float64, cand llm.CandidateIntervention, depth int) {
	id := goal.OptimizationFunctionID
	effective := concatFilters(parentFilters, cand.Filters)

	resp, err := s.sandbox.Execute(ctx, objective.ExecuteRequestFor(goal.DataSourceRef, obj, effective))
	if err != nil {
		s.branchFailure(ctx, id, effective, err)
		return
	}
	value, ok := objective.NumericValue(resp.Value, obj.Label)
	if !ok {
		s.branchFailure(ctx, id, effective, errNonNumericValue)
		return
	}

	if err := s.writeTriplet(ctx, goal, obj, parentFilters, baseline, cand, effective, value); err != nil {
		log.Printf("orchestrator: hypothesis loop %q: write triplet: %v", id, err)
		s.branchFailure(ctx, id, effective, err)
		return
	}

	if !objective.Improves(baseline, value, obj.Direction) {
		return
	}
	if !constraintsSatisfied(goal.EvaluationMatrix.Constraints, objectiveField(obj), value) {
		s.branchFailure(ctx, id, effective, errors.New("candidate violates a hard constraint on the objective field"))
		return
	}
	if depth >= defaultDepth {
		return
	}

	children, err := s.proposeValidCandidates(ctx, goal, schema, llm.TreeContext{
		Breadth:        defaultBreadth,
		ObjectiveLabel: obj.Label,
		Direction:      obj.Direction,
		ParentFilters:  effective,
		PriorValue:     &value,
	})
	if err != nil {
		s.branchFailure(ctx, id, effective, err)
		return
	}
	for _, c := range children {
		s.processCandidate(ctx, goal, obj, schema, effective, value, c, depth+1)
	}
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
func (s *Server) runDocumentLoop(ctx context.Context, goal store.Goal) error {
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
			s.processExtractionCandidate(ctx, goal, field, pdf, textResp.Pages, method, 0.0, 1)
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
func (s *Server) processExtractionCandidate(ctx context.Context, goal store.Goal, field domain.TargetField, pdf []byte, pages []string, method string, parentConfidence float64, depth int) {
	id := goal.OptimizationFunctionID

	value, confidence, err := s.claude.Extract(ctx, pdf, field, method)
	if err != nil {
		s.branchFailure(ctx, id, nil, err)
		return
	}

	locator := locateProvenance(pages, value)
	if err := s.writeExtractionTriplet(ctx, goal, field, method, parentConfidence, value, confidence, locator); err != nil {
		log.Printf("orchestrator: hypothesis loop %q: write extraction triplet: %v", id, err)
		s.branchFailure(ctx, id, nil, err)
		return
	}

	if !objective.Improves(parentConfidence, confidence, domain.Maximize) {
		return
	}
	if depth >= defaultDepth {
		return
	}
	for _, m := range extractionMethods {
		s.processExtractionCandidate(ctx, goal, field, pdf, pages, m, confidence, depth+1)
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
// query outcomes are always verified with PRODUCED confidence fixed at 1.0.
func (s *Server) writeTriplet(ctx context.Context, goal store.Goal, obj objective.Objective, parentFilters []domain.Constraint, baseline float64, cand llm.CandidateIntervention, effective []domain.Constraint, value float64) error {
	stateID := uuid.NewString()
	state := domain.State{ID: stateID, GoalID: goal.OptimizationFunctionID, Properties: map[string]any{
		domain.PropDataSourceRef:        goal.DataSourceRef,
		domain.PropObjectiveAggregation: obj.Aggregation,
		domain.PropObjectiveLabel:       obj.Label,
		domain.PropEffectiveFilters:     parentFilters,
		"value":                         baseline,
	}}
	if err := s.repo.CreateState(ctx, state); err != nil {
		return err
	}

	interventionID := uuid.NewString()
	intervention := domain.Intervention{ID: interventionID, GoalID: goal.OptimizationFunctionID, Type: domain.InterventionQuery, Properties: map[string]any{
		domain.PropObjectiveAggregation: obj.Aggregation,
		domain.PropObjectiveLabel:       obj.Label,
		domain.PropNewFilters:           cand.Filters,
		domain.PropEffectiveFilters:     effective,
	}}
	if err := s.repo.CreateIntervention(ctx, intervention); err != nil {
		return err
	}

	outcomeID := uuid.NewString()
	outcome := domain.Outcome{ID: outcomeID, GoalID: goal.OptimizationFunctionID, VerificationStatus: domain.VerificationVerified, Value: map[string]any{obj.Label: value}}
	if err := s.repo.CreateOutcome(ctx, outcome); err != nil {
		return err
	}

	if err := s.repo.CreatePreConditionFor(ctx, stateID, interventionID); err != nil {
		return err
	}
	if err := s.repo.CreateProduced(ctx, interventionID, outcomeID, domain.ProducedEdge{EffectSize: value - baseline, Confidence: 1.0, EpistemicSource: domain.EpistemicObservational}); err != nil {
		return err
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
	return nil
}

// writeExtractionTriplet is the extract-path analog of writeTriplet: it persists
// one State→Intervention→Outcome triplet for an extraction measurement. Unlike
// the query path it writes Intervention.Type extract, an unverified outcome (which
// feeds the HITL queue and gates Sleep-Cycle clustering), the deterministic
// provenance locator, and a PRODUCED confidence that is the model's self-reported
// value rather than the fixed 1.0. The effect size is the confidence delta over
// the parent, mirroring the query path's value delta over its baseline. The
// epistemic source stays observational.
func (s *Server) writeExtractionTriplet(ctx context.Context, goal store.Goal, field domain.TargetField, method string, parentConfidence float64, value string, confidence float64, locator *domain.ProvenanceLocator) error {
	stateID := uuid.NewString()
	state := domain.State{ID: stateID, GoalID: goal.OptimizationFunctionID, Properties: map[string]any{
		domain.PropDataSourceRef: goal.DataSourceRef,
		"field":                  field.Name,
		"confidence":             parentConfidence,
	}}
	if err := s.repo.CreateState(ctx, state); err != nil {
		return err
	}

	interventionID := uuid.NewString()
	intervention := domain.Intervention{ID: interventionID, GoalID: goal.OptimizationFunctionID, Type: domain.InterventionExtract, Properties: map[string]any{
		"field":  field.Name,
		"method": method,
	}}
	if err := s.repo.CreateIntervention(ctx, intervention); err != nil {
		return err
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
		return err
	}

	if err := s.repo.CreatePreConditionFor(ctx, stateID, interventionID); err != nil {
		return err
	}
	if err := s.repo.CreateProduced(ctx, interventionID, outcomeID, domain.ProducedEdge{
		EffectSize:      confidence - parentConfidence,
		Confidence:      confidence,
		EpistemicSource: domain.EpistemicObservational,
	}); err != nil {
		return err
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
		"provenance":      locator,
	}})
	return nil
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

// proposeValidCandidates proposes one node's candidates and repairs any that
// reference columns absent from the schema, re-proposing only the invalid ones
// against the naming error. Earlier-round valid candidates accumulate and are never
// re-proposed, so no candidate is dropped or duplicated. A transport error from the
// initial proposal is returned as-is, so callers keep the terminal-at-root /
// non-terminal-in-child distinction; a repair-call transport error is best-effort —
// it keeps the candidates already validated and records the fault as a branch
// failure rather than sinking the run. After the repair bound, still-invalid
// candidates are dropped and a branch failure naming the unknown columns is recorded
// (siblings continue); if none remain valid, the returned slice is empty and that
// branch stops expanding.
func (s *Server) proposeValidCandidates(ctx context.Context, goal store.Goal, schema llm.SandboxSchema, node llm.TreeContext) ([]llm.CandidateIntervention, error) {
	id := goal.OptimizationFunctionID
	proposal, err := s.claude.ProposeInterventionTree(ctx, goal.GoalText, goal.EvaluationMatrix, schema, node)
	if err != nil {
		return nil, err
	}
	cols := columnNames(schema)
	valid, invalid, unknown := splitByColumns(proposal.Candidates, cols)

	for attempts := 0; len(invalid) > 0 && attempts < maxProposalRepairs; attempts++ {
		repaired, rerr := s.claude.RepairInterventionTree(ctx, goal.GoalText, goal.EvaluationMatrix, schema, node,
			llm.Proposal{Candidates: invalid}, unknownColumnsMessage(unknown))
		if rerr != nil {
			log.Printf("orchestrator: hypothesis loop %q: repair intervention tree: %v", id, rerr)
			s.branchFailure(ctx, id, node.ParentFilters, rerr)
			return valid, nil
		}
		newValid, newInvalid, newUnknown := splitByColumns(repaired.Candidates, cols)
		valid = append(valid, newValid...)
		invalid, unknown = newInvalid, newUnknown
	}

	if len(invalid) > 0 {
		s.branchFailure(ctx, id, node.ParentFilters,
			fmt.Errorf("dropped %d candidate(s) referencing unknown columns: %s", len(invalid), strings.Join(unknown, ", ")))
	}
	return valid, nil
}

// splitByColumns partitions candidates into those whose filter columns all exist in
// the schema and those referencing at least one unknown column, collecting the
// unknown column names (deduplicated, first-seen) for the repair prompt.
func splitByColumns(candidates []llm.CandidateIntervention, columns []string) (valid, invalid []llm.CandidateIntervention, unknown []string) {
	seen := map[string]bool{}
	for _, cand := range candidates {
		miss := domain.UnknownFilterColumns(cand.Filters, columns)
		if len(miss) == 0 {
			valid = append(valid, cand)
			continue
		}
		invalid = append(invalid, cand)
		for _, u := range miss {
			key := strings.ToLower(u)
			if !seen[key] {
				seen[key] = true
				unknown = append(unknown, u)
			}
		}
	}
	return valid, invalid, unknown
}

func columnNames(schema llm.SandboxSchema) []string {
	names := make([]string, 0, len(schema.Columns))
	for _, c := range schema.Columns {
		names = append(names, c.Name)
	}
	return names
}

func unknownColumnsMessage(unknown []string) string {
	return "these filter columns are not in the schema: " + strings.Join(unknown, ", ") +
		"; re-propose using only columns from the Available columns list"
}

// dryRunObjective validates a fitted matrix by executing its pinned objective
// against the sandbox with no filters — the same request the root baseline runs,
// pinned identically. A nil return means the objective compiles and measures; a
// non-nil error is either a pin failure or the sandbox's execute error.
func (s *Server) dryRunObjective(ctx context.Context, ref string, matrix domain.EvaluationMatrix) error {
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
	_, err = s.sandbox.Execute(ctx, objective.ExecuteRequestFor(ref, obj, nil))
	return err
}

// isObjectiveValidationFailure reports whether a dry-run error is a repairable
// objective-fit failure: a missing/absent aggregation the model can supply, or a
// sandbox 400 compile/type error. A sandbox 4xx≠400, 5xx, or transport error is a
// fault, not an unfixable objective.
func isObjectiveValidationFailure(err error) bool {
	if errors.Is(err, errNoObjective) || errors.Is(err, errMissingAggregation) || errors.Is(err, errExpressionTooDeep) {
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
		cols = append(cols, llm.SandboxColumn{Name: c.Name, Type: c.Type})
	}
	return llm.SandboxSchema{Columns: cols}
}
