package orchestrator

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/arborette/arborette/internal/domain"
	"github.com/arborette/arborette/internal/graph"
	"github.com/arborette/arborette/internal/store"
)

// maxVerificationBytes caps a resolution body. It holds one corrected field
// value, so it needs far less headroom than an upload or a chat transcript.
const maxVerificationBytes = 1 << 20

// An analyst's verdict is ground truth, so confirming or correcting an
// extraction pins its PRODUCED-edge confidence at certainty. A rejection zeroes
// it instead: the outcome is excluded from the Sleep-Cycle search by its
// verification status either way, and zeroing makes the run's live confidence
// distribution reflect the rejection rather than leaving a discredited value
// weighted as the model reported it.
const (
	verifiedConfidence = 1.0
	rejectedConfidence = 0.0
)

// The DTOs below select and snake_case what the verification UI needs, for the
// same reason the heuristics DTOs exist: the store and graph types carry no json
// tags, so marshaling them directly would emit PascalCase keys and leak internal
// fields.

type provenanceDTO struct {
	Page      int `json:"page"`
	CharStart int `json:"char_start"`
	CharEnd   int `json:"char_end"`
}

type verificationEntryDTO struct {
	QueueID        string         `json:"queue_id"`
	OutcomeID      string         `json:"outcome_id"`
	Field          string         `json:"field"`
	ExtractedValue string         `json:"extracted_value"`
	Provenance     *provenanceDTO `json:"provenance"`
	Confidence     float64        `json:"confidence"`
	Status         string         `json:"status"`
	Resolution     string         `json:"resolution,omitempty"`
	CorrectedValue string         `json:"corrected_value,omitempty"`
	CreatedAt      time.Time      `json:"created_at"`
	ResolvedAt     *time.Time     `json:"resolved_at,omitempty"`
}

// verificationListDTO carries the goal's review settings alongside its entries
// so the UI can render the queue -- and say which threshold produced it --
// without a second call.
type verificationListDTO struct {
	EffectiveThreshold float64                `json:"effective_threshold"`
	EpochMode          string                 `json:"epoch_mode"`
	Entries            []verificationEntryDTO `json:"entries"`
}

type extractionOutcomeDTO struct {
	OutcomeID          string         `json:"outcome_id"`
	Field              string         `json:"field"`
	Method             string         `json:"method"`
	Value              map[string]any `json:"value"`
	Provenance         *provenanceDTO `json:"provenance"`
	VerificationStatus string         `json:"verification_status"`
	Confidence         float64        `json:"confidence"`
}

// excerptDTO is the source text behind an extracted value. Fallback marks the
// whole-document shape, where Pages is populated and the pinpoint fields are not.
type excerptDTO struct {
	Fallback  bool     `json:"fallback"`
	Page      int      `json:"page"`
	CharStart int      `json:"char_start"`
	CharEnd   int      `json:"char_end"`
	Excerpt   string   `json:"excerpt,omitempty"`
	PageText  string   `json:"page_text,omitempty"`
	Pages     []string `json:"pages,omitempty"`
}

type resolveRequest struct {
	Action         string `json:"action"`
	CorrectedValue string `json:"corrected_value"`
}

// handleListVerifications lists a goal's review queue, optionally narrowed to
// pending or resolved entries.
func (s *Server) handleListVerifications(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	goal, ok := s.lookupGoal(ctx, w, r.PathValue("id"))
	if !ok {
		return
	}
	status := store.QueueStatus(r.URL.Query().Get("status"))
	if status != "" && status != store.QueuePending && status != store.QueueResolved {
		writeErr(w, http.StatusBadRequest, `status must be "pending" or "resolved"`)
		return
	}

	entries, err := s.queue.ListForGoal(ctx, goal.OptimizationFunctionID, status)
	if err != nil {
		log.Printf("orchestrator: list verifications for %q: %v", goal.OptimizationFunctionID, err)
		writeErr(w, http.StatusInternalServerError, "internal error")
		return
	}
	out := make([]verificationEntryDTO, 0, len(entries))
	for _, e := range entries {
		out = append(out, toVerificationEntryDTO(e))
	}
	writeJSON(w, http.StatusOK, verificationListDTO{
		EffectiveThreshold: s.effectiveThreshold(goal),
		EpochMode:          string(epochModeOf(goal)),
		Entries:            out,
	})
}

// handleListOutcomes lists every extract-type outcome for a goal, not only the
// queued ones, so an analyst can pull up and review a result that cleared the
// threshold on its own.
func (s *Server) handleListOutcomes(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	goal, ok := s.lookupGoal(ctx, w, r.PathValue("id"))
	if !ok {
		return
	}
	outcomes, err := s.repo.ListExtractionOutcomes(ctx, goal.OptimizationFunctionID)
	if err != nil {
		log.Printf("orchestrator: list extraction outcomes for %q: %v", goal.OptimizationFunctionID, err)
		writeErr(w, http.StatusInternalServerError, "internal error")
		return
	}
	out := make([]extractionOutcomeDTO, 0, len(outcomes))
	for _, o := range outcomes {
		out = append(out, extractionOutcomeDTO{
			OutcomeID:          o.OutcomeID,
			Field:              o.Field,
			Method:             o.Method,
			Value:              o.Value,
			Provenance:         toProvenanceDTO(o.Provenance),
			VerificationStatus: string(o.VerificationStatus),
			Confidence:         o.Confidence,
		})
	}
	writeJSON(w, http.StatusOK, out)
}

// handleOutcomeExcerpt returns the source text an extracted value came from, so
// the reviewer judges the value against the document rather than against a
// locator's coordinates. It re-extracts the document's text through the sandbox
// -- the same routine that produced the locator, so the offsets still line up.
// The sandbox is stateless, so this re-parses the whole document per view;
// acceptable while queues are short, and the reason a page-text cache is the
// first thing to add if review feels slow.
func (s *Server) handleOutcomeExcerpt(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	goal, ok := s.lookupGoal(ctx, w, r.PathValue("id"))
	if !ok {
		return
	}
	outcome, ok := s.lookupExtractionOutcome(ctx, w, goal, r.PathValue("outcomeID"))
	if !ok {
		return
	}

	text, err := s.sandbox.DocumentText(ctx, DocumentTextRequest{DataSourceRef: goal.DataSourceRef})
	if err != nil {
		s.writeSandboxErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, excerptFor(text.Pages, outcome.Provenance))
}

// handleResolveVerification applies an analyst's verdict to an extraction. It
// writes across Postgres and the graph, so the order is part of the design: the
// queue row is claimed first, and only the winner of that claim writes the
// graph. See resolutionClaim for the state machine the two stores move through
// together and how a half-applied resolution converges.
func (s *Server) handleResolveVerification(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	goal, ok := s.lookupGoal(ctx, w, r.PathValue("id"))
	if !ok {
		return
	}
	outcome, ok := s.lookupExtractionOutcome(ctx, w, goal, r.PathValue("outcomeID"))
	if !ok {
		return
	}

	r.Body = http.MaxBytesReader(w, r.Body, maxVerificationBytes)
	var req resolveRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid request body")
		return
	}
	resolution, ok := parseResolution(req.Action)
	if !ok {
		writeErr(w, http.StatusUnprocessableEntity, `action must be one of "confirm", "correct", "reject"`)
		return
	}
	corrected := strings.TrimSpace(req.CorrectedValue)
	if resolution == store.ResolutionCorrected && corrected == "" {
		writeErr(w, http.StatusUnprocessableEntity, "corrected_value is required to correct an extraction")
		return
	}

	claim, ok := s.claimResolution(ctx, w, goal, outcome, resolutionClaim{resolution: resolution, corrected: corrected})
	if !ok {
		return
	}

	confidence, err := s.applyResolution(ctx, goal, outcome, claim)
	if err != nil {
		log.Printf("orchestrator: apply verification for outcome %q: %v", outcome.OutcomeID, err)
		// Compensation runs detached: the most common way to reach this branch is
		// the request context being cancelled (a closed tab mid-write), and a
		// compensation on that same context is guaranteed to fail exactly when it
		// is needed -- stranding the claim it exists to release.
		compensateCtx, cancelCompensate := detached(ctx)
		defer cancelCompensate()
		s.compensateClaim(compensateCtx, outcome.OutcomeID, claim)
		s.writeSandboxErr(w, err)
		return
	}

	// Auditing every resolution is a guarantee this service owes, so unlike the
	// loop's best-effort audits a failure here is reported rather than logged
	// past: the resolution itself stands, and the response says so.
	detail := map[string]any{
		"optimization_function_id": goal.OptimizationFunctionID,
		"outcome_id":               outcome.OutcomeID,
		"action":                   string(claim.resolution),
		"confidence_at_queue":      outcome.Confidence,
	}
	if claim.corrected != "" {
		detail["corrected_value"] = claim.corrected
	}
	if claim.repair {
		detail["repair"] = true
	}
	// Reflect the resolution in the live view before the audit can fail out: the
	// verdict is already in both stores, and the histogram remembers each
	// outcome for the run's life, so skipping this would wedge that outcome in
	// its pre-resolution bucket for every frame the run still publishes.
	s.rebinConfidence(goal.OptimizationFunctionID, outcome.OutcomeID, confidence)

	auditCtx, cancelAudit := detached(ctx)
	defer cancelAudit()
	if err := s.recordAudit(auditCtx, "hitl_verification_resolution", "verification", detail); err != nil {
		log.Printf("orchestrator: append audit: %v", err)
		writeErr(w, http.StatusInternalServerError, "the resolution was applied but its audit record could not be written")
		return
	}

	if claim.conflict {
		writeErr(w, http.StatusConflict, "this outcome was already resolved as "+string(claim.resolution)+
			resolvedAsSuffix(claim.corrected)+"; that resolution was completed and the requested action was not applied")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"outcome_id":          outcome.OutcomeID,
		"resolution":          string(claim.resolution),
		"verification_status": string(verificationStatusFor(claim.resolution)),
		"confidence":          confidence,
	})
}

// resolutionClaim is one resolution's authority to write the graph, and the
// values it must write. Exactly one request wins a claim; a loser becomes a
// repairer, which writes the same values read from the same stored row, so an
// overlap costs at most a second audit record (marked as a repair) -- which an
// append-only log tolerates far better than a missing one.
//
// Every failure state this leaves is conservative: the outcome stays unverified,
// which keeps it out of the Sleep-Cycle search, and the next resolution attempt
// repairs it.
type resolutionClaim struct {
	resolution store.QueueResolution
	corrected  string
	// repair marks a claim completing another request's stranded resolution.
	repair bool
	// conflict marks a repair whose stored verdict differs from the one this
	// request asked for; the stored verdict is what gets written.
	conflict bool
}

// claimResolution wins (or inherits) the right to write this resolution. The
// claim precedes every graph write so a double submission cannot apply two
// different verdicts to one outcome.
func (s *Server) claimResolution(ctx context.Context, w http.ResponseWriter, goal store.Goal, outcome graph.ExtractionOutcome, want resolutionClaim) (resolutionClaim, bool) {
	entry, err := s.queue.GetByOutcome(ctx, outcome.OutcomeID)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		// Reviewing an outcome the loop never queued: the insert is the claim.
		newEntry, buildErr := verificationEntryFor(goal, outcome)
		if buildErr != nil {
			log.Printf("orchestrator: build verification entry: %v", buildErr)
			writeErr(w, http.StatusInternalServerError, "internal error")
			return resolutionClaim{}, false
		}
		err = s.queue.EnqueueResolved(ctx, newEntry, want.resolution, want.corrected)
	case err != nil:
		log.Printf("orchestrator: get verification for outcome %q: %v", outcome.OutcomeID, err)
		writeErr(w, http.StatusInternalServerError, "internal error")
		return resolutionClaim{}, false
	case entry.Status == store.QueuePending:
		err = s.queue.Resolve(ctx, outcome.OutcomeID, want.resolution, want.corrected)
	default:
		err = store.ErrAlreadyResolved
	}

	if err == nil {
		return want, true
	}
	if !errors.Is(err, store.ErrAlreadyResolved) {
		log.Printf("orchestrator: claim verification for outcome %q: %v", outcome.OutcomeID, err)
		writeErr(w, http.StatusInternalServerError, "internal error")
		return resolutionClaim{}, false
	}
	return s.repairClaim(ctx, w, outcome.OutcomeID, want)
}

// repairClaim decides what a lost claim means. Losing to a pending row is a
// race with the loop's own queue routing, so the caller simply claims that row.
// Losing to a resolved row is either work already finished -- a 409 -- or a
// resolution stranded between its claim and its graph write, which this request
// completes from the stored verdict.
func (s *Server) repairClaim(ctx context.Context, w http.ResponseWriter, outcomeID string, want resolutionClaim) (resolutionClaim, bool) {
	entry, err := s.queue.GetByOutcome(ctx, outcomeID)
	if err != nil {
		log.Printf("orchestrator: re-read verification for outcome %q: %v", outcomeID, err)
		writeErr(w, http.StatusInternalServerError, "internal error")
		return resolutionClaim{}, false
	}
	if entry.Status == store.QueuePending {
		if err := s.queue.Resolve(ctx, outcomeID, want.resolution, want.corrected); err != nil {
			if errors.Is(err, store.ErrAlreadyResolved) {
				writeErr(w, http.StatusConflict, "this outcome has already been resolved")
				return resolutionClaim{}, false
			}
			log.Printf("orchestrator: claim verification for outcome %q: %v", outcomeID, err)
			writeErr(w, http.StatusInternalServerError, "internal error")
			return resolutionClaim{}, false
		}
		return want, true
	}

	// Re-read the graph rather than trusting the status read before the claim: a
	// concurrent repairer may have landed the write-through in between.
	outcome, err := s.repo.GetExtractionOutcome(ctx, outcomeID)
	if err != nil {
		log.Printf("orchestrator: re-read outcome %q: %v", outcomeID, err)
		writeErr(w, http.StatusInternalServerError, "internal error")
		return resolutionClaim{}, false
	}
	if outcome.VerificationStatus != domain.VerificationUnverified {
		writeErr(w, http.StatusConflict, "this outcome has already been resolved")
		return resolutionClaim{}, false
	}
	return resolutionClaim{
		resolution: entry.Resolution,
		corrected:  entry.CorrectedValue,
		repair:     true,
		// The stored corrected value counts as much as the verdict: repairing a
		// correction writes the text the earlier analyst supplied, so a caller
		// who sent different text must be told theirs was not applied.
		conflict: entry.Resolution != want.resolution || entry.CorrectedValue != want.corrected,
	}, true
}

// applyResolution writes the verdict through to the graph and reports the
// outcome's new confidence. A correction relocates the value in the source text
// exactly as the initial extraction did, so a value the model reformatted lands
// with a null locator and falls back to the whole document on review -- and its
// value, locator, and status are written together, so a failure can never leave
// a corrected value sitting under an unreviewed status.
func (s *Server) applyResolution(ctx context.Context, goal store.Goal, outcome graph.ExtractionOutcome, claim resolutionClaim) (float64, error) {
	status := verificationStatusFor(claim.resolution)
	if claim.resolution == store.ResolutionCorrected {
		text, err := s.sandbox.DocumentText(ctx, DocumentTextRequest{DataSourceRef: goal.DataSourceRef})
		if err != nil {
			return 0, err
		}
		locator := locateProvenance(text.Pages, claim.corrected)
		value := map[string]any{outcome.Field: claim.corrected}
		return verifiedConfidence, s.repo.CorrectOutcome(ctx, outcome.OutcomeID, value, locator, status, verifiedConfidence)
	}
	confidence := verifiedConfidence
	if claim.resolution == store.ResolutionRejected {
		confidence = rejectedConfidence
	}
	return confidence, s.repo.UpdateOutcomeVerification(ctx, outcome.OutcomeID, status, confidence)
}

// detached derives a context for a write that must land even though the request
// that triggered it may already be gone. It keeps the request's values (the
// identity seam reads them) while dropping its cancellation, and bounds the
// write so a wedged dependency cannot hold the handler open. Callers defer the
// returned cancel, exactly as the hypothesis loop does for its terminal status
// write.
func detached(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.WithoutCancel(ctx), statusWriteTimeout)
}

// writeSandboxErr answers a resolution or excerpt whose source-document read
// failed. A sandbox fault is classified exactly as the intake path classifies
// it, so an unreadable or missing source reads the same wherever the analyst
// meets it; anything else is this service's own failure and is masked.
func (s *Server) writeSandboxErr(w http.ResponseWriter, err error) {
	var se *SandboxError
	if errors.As(err, &se) {
		writeSandboxStatus(w, se, "review")
		return
	}
	writeErr(w, http.StatusInternalServerError, "internal error")
}

// compensateClaim returns a failed resolution's row to pending so the analyst
// can retry it.
//
// One window is narrower than the claim machinery's guarantees but not closed:
// the graph check and the un-claim below are two steps against two stores, so a
// repairer that lands the verdict between them leaves the graph resolved while
// this row returns to pending -- an outcome that reads as unreviewed while
// already being search-eligible, which a later, different verdict would then
// overwrite. Closing it needs a guard the two stores cannot share today; it
// takes a failed graph write and a concurrent duplicate resolution to reach. A repairer skips this: it holds no claim of its own. So does an
// owner whose outcome is no longer unverified, which means a concurrent repairer
// already landed the verdict this owner's own write lost to -- the resolution
// did happen, so the row must stay resolved. A failure here is logged rather
// than surfaced: the caller is already answering an error, and the stranded row
// is what the repair path exists to heal.
func (s *Server) compensateClaim(ctx context.Context, outcomeID string, claim resolutionClaim) {
	if claim.repair {
		return
	}
	if outcome, err := s.repo.GetExtractionOutcome(ctx, outcomeID); err == nil &&
		outcome.VerificationStatus != domain.VerificationUnverified {
		return
	}
	if err := s.queue.Unclaim(ctx, outcomeID, claim.resolution); err != nil {
		log.Printf("orchestrator: unclaim verification for outcome %q: %v", outcomeID, err)
	}
}

// lookupExtractionOutcome resolves an outcome within a goal, writing a 404 when
// it is missing, is not an extraction, or belongs to another goal -- the last so
// a goal-scoped URL can never reach another goal's document. The bool is false
// when a response has already been written.
func (s *Server) lookupExtractionOutcome(ctx context.Context, w http.ResponseWriter, goal store.Goal, outcomeID string) (graph.ExtractionOutcome, bool) {
	outcome, err := s.repo.GetExtractionOutcome(ctx, outcomeID)
	if err != nil {
		if errors.Is(err, graph.ErrNotFound) {
			writeErr(w, http.StatusNotFound, "outcome not found")
			return graph.ExtractionOutcome{}, false
		}
		log.Printf("orchestrator: get extraction outcome %q: %v", outcomeID, err)
		writeErr(w, http.StatusInternalServerError, "internal error")
		return graph.ExtractionOutcome{}, false
	}
	if outcome.GoalID != goal.OptimizationFunctionID {
		writeErr(w, http.StatusNotFound, "outcome not found")
		return graph.ExtractionOutcome{}, false
	}
	return outcome, true
}

// verificationEntryFor builds the queue row for an outcome reviewed on demand,
// snapshotting what the loop would have recorded had the outcome been queued.
// The value is keyed by field exactly as the extraction wrote it, so anything
// else is a data-integrity failure rather than a caller error.
func verificationEntryFor(goal store.Goal, outcome graph.ExtractionOutcome) (store.VerificationEntry, error) {
	value, ok := outcome.Value[outcome.Field].(string)
	if !ok {
		return store.VerificationEntry{}, fmt.Errorf("outcome %q carries no string value for field %q", outcome.OutcomeID, outcome.Field)
	}
	return store.VerificationEntry{
		QueueID:                uuid.NewString(),
		OptimizationFunctionID: goal.OptimizationFunctionID,
		OutcomeID:              outcome.OutcomeID,
		Field:                  outcome.Field,
		ExtractedValue:         value,
		Provenance:             outcome.Provenance,
		Confidence:             outcome.Confidence,
	}, nil
}

// resolvedAsSuffix names the value a repaired correction actually wrote, so a
// caller whose own replacement text lost the race can see what stands instead.
func resolvedAsSuffix(corrected string) string {
	if corrected == "" {
		return ""
	}
	return " with the value " + strconv.Quote(corrected)
}

func parseResolution(action string) (store.QueueResolution, bool) {
	switch action {
	case "confirm":
		return store.ResolutionConfirmed, true
	case "correct":
		return store.ResolutionCorrected, true
	case "reject":
		return store.ResolutionRejected, true
	default:
		return "", false
	}
}

func verificationStatusFor(resolution store.QueueResolution) domain.VerificationStatus {
	switch resolution {
	case store.ResolutionConfirmed:
		return domain.VerificationConfirmed
	case store.ResolutionCorrected:
		return domain.VerificationCorrected
	default:
		return domain.VerificationRejected
	}
}

// effectiveThreshold is the confidence below which this goal's extractions are
// queued for review: the goal's own override when the analyst set one, else the
// service-wide default.
func (s *Server) effectiveThreshold(goal store.Goal) float64 {
	if goal.ConfidenceThreshold != nil {
		return *goal.ConfidenceThreshold
	}
	return s.hitlThreshold
}

// epochModeOf reads a goal's epoch mode, defaulting an unset one to the
// non-blocking behavior. Persisted goals always carry a mode -- the column is NOT
// NULL with a default, and registration normalizes empty before binding -- so
// this guards a Goal built in memory rather than any row on disk.
func epochModeOf(goal store.Goal) store.EpochMode {
	if goal.EpochMode == "" {
		return store.EpochSpeculative
	}
	return goal.EpochMode
}

// excerptFor resolves a locator against freshly extracted page text. A nil
// locator -- no exact match when the value was extracted -- and one that no
// longer fits the document both degrade to the whole document, so a reformatted
// value or a re-uploaded source stays reviewable instead of erroring.
func excerptFor(pages []string, locator *domain.ProvenanceLocator) excerptDTO {
	if locator == nil || locator.Page < 0 || locator.Page >= len(pages) {
		return excerptDTO{Fallback: true, Pages: pages}
	}
	page := pages[locator.Page]
	if locator.CharStart < 0 || locator.CharStart > locator.CharEnd || locator.CharEnd > len(page) {
		return excerptDTO{Fallback: true, Pages: pages}
	}
	return excerptDTO{
		Page:      locator.Page,
		CharStart: locator.CharStart,
		CharEnd:   locator.CharEnd,
		Excerpt:   page[locator.CharStart:locator.CharEnd],
		PageText:  page,
	}
}

func toVerificationEntryDTO(e store.VerificationEntry) verificationEntryDTO {
	return verificationEntryDTO{
		QueueID:        e.QueueID,
		OutcomeID:      e.OutcomeID,
		Field:          e.Field,
		ExtractedValue: e.ExtractedValue,
		Provenance:     toProvenanceDTO(e.Provenance),
		Confidence:     e.Confidence,
		Status:         string(e.Status),
		Resolution:     string(e.Resolution),
		CorrectedValue: e.CorrectedValue,
		CreatedAt:      e.CreatedAt,
		ResolvedAt:     e.ResolvedAt,
	}
}

func toProvenanceDTO(p *domain.ProvenanceLocator) *provenanceDTO {
	if p == nil {
		return nil
	}
	return &provenanceDTO{Page: p.Page, CharStart: p.CharStart, CharEnd: p.CharEnd}
}

// confidenceBinCount is how many equal-width buckets span the 0.0–1.0 confidence
// range in the distribution the live view renders.
const confidenceBinCount = 10

// confidenceHistogram is one run's distribution of PRODUCED-edge confidence
// weights. It remembers each outcome's contribution, not just the totals,
// because a resolution arriving mid-run has to move that outcome between buckets
// rather than add to them.
//
// It carries no lock of its own: every access runs under Server.histMu, which is
// also what orders a publish against the run's teardown. A second lock here
// would suggest the type is safe to touch outside that one, which would break
// that ordering silently.
type confidenceHistogram struct {
	bins        [confidenceBinCount]int
	confidences map[string]float64
}

func newConfidenceHistogram() *confidenceHistogram {
	return &confidenceHistogram{confidences: map[string]float64{}}
}

// Add counts one triplet's confidence.
func (h *confidenceHistogram) Add(outcomeID string, confidence float64) {
	h.confidences[outcomeID] = confidence
	h.bins[confidenceBin(confidence)]++
}

// Rebin moves an outcome to the bucket its resolved confidence falls in,
// reporting whether this run counted the outcome at all -- it did not if the
// outcome belongs to an earlier run.
func (h *confidenceHistogram) Rebin(outcomeID string, confidence float64) bool {
	prior, counted := h.confidences[outcomeID]
	if !counted {
		return false
	}
	h.bins[confidenceBin(prior)]--
	h.bins[confidenceBin(confidence)]++
	h.confidences[outcomeID] = confidence
	return true
}

// Payload renders the distribution for the wire, copying the bins so the
// published snapshot cannot be mutated by later counting.
func (h *confidenceHistogram) Payload() map[string]any {
	bins := make([]int, confidenceBinCount)
	copy(bins, h.bins[:])
	return map[string]any{"bins": bins, "total": len(h.confidences)}
}

// confidenceBin maps a weight to its bucket, clamping at both ends so a perfect
// 1.0 lands in the top bucket rather than past the last index.
func confidenceBin(confidence float64) int {
	idx := int(confidence * confidenceBinCount)
	if idx < 0 {
		return 0
	}
	if idx >= confidenceBinCount {
		return confidenceBinCount - 1
	}
	return idx
}

// registerHistogram starts tracking a run's confidence distribution. The
// distribution is per-run, but outcomes carry no run id and a goal accumulates
// outcomes across runs, so it is held in memory for the run's lifetime rather
// than recomputed from the graph -- recomputing would fold every past run's
// extractions into the live view.
func (s *Server) registerHistogram(goalID string) *confidenceHistogram {
	hist := newConfidenceHistogram()
	s.histMu.Lock()
	defer s.histMu.Unlock()
	s.histograms[goalID] = hist
	return hist
}

// deregisterHistogram stops tracking, and only for the run that owns hist: a
// re-trigger while this run is still going replaces the entry, and tearing down
// the newer run's tracking would silently stop its updates. Callers deregister
// before the hub's terminal publish, so a resolution racing the end of a run
// either publishes ahead of it or finds nothing and stays quiet -- never
// publishing into a completed run, which would resurrect the hub state that
// completion just evicted.
func (s *Server) deregisterHistogram(goalID string, hist *confidenceHistogram) {
	s.histMu.Lock()
	defer s.histMu.Unlock()
	if s.histograms[goalID] == hist {
		delete(s.histograms, goalID)
	}
}

// recordConfidence counts a freshly written triplet in its own run's
// distribution and pushes the updated view. The run passes the histogram it
// registered rather than having this look one up by goal: the registry is keyed
// by goal, so a re-trigger replaces the entry, and a lookup would file the older
// run's triplets under the newer run's distribution.
func (s *Server) recordConfidence(goalID string, hist *confidenceHistogram, outcomeID string, confidence float64) {
	s.histMu.Lock()
	defer s.histMu.Unlock()
	hist.Add(outcomeID, confidence)
	// Publish only while this run is still the one being streamed; once it has
	// been superseded or torn down, its frames would misrepresent the live view.
	// A plain publish is right here: this run is still going and will evict its
	// own hub state when it ends.
	if s.histograms[goalID] == hist {
		s.hub.Publish(goalID, distributionEvent(hist))
	}
}

// rebinConfidence reflects a resolution in the live distribution. A resolution
// for an outcome no run is tracking publishes nothing: that run's stream is
// closed, and clients read current state from the outcomes endpoint instead.
func (s *Server) rebinConfidence(goalID, outcomeID string, confidence float64) {
	s.histMu.Lock()
	defer s.histMu.Unlock()
	hist := s.histograms[goalID]
	if hist == nil || !hist.Rebin(outcomeID, confidence) {
		return
	}
	// Live-only: a resolution arrives from outside the run, and the hub is keyed
	// by goal, so a concurrent run for this goal may already have completed and
	// evicted the shared state. Recreating it here would leave an entry no run is
	// left to clean up.
	s.hub.PublishLive(goalID, distributionEvent(hist))
}

// distributionEvent renders the run's current distribution as a coalescing
// frame. Callers hold histMu, which keeps a publish from interleaving with the
// deregistration that precedes a run's completion.
func distributionEvent(hist *confidenceHistogram) Event {
	return Event{
		Type:     "confidence_distribution",
		Coalesce: true,
		Payload:  hist.Payload(),
	}
}
