package orchestrator

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/arborette/arborette/internal/domain"
	"github.com/arborette/arborette/internal/graph"
	"github.com/arborette/arborette/internal/store"
)

const (
	testGoalID    = "goal-1"
	testOutcomeID = "outcome-1"
)

// reviewGoal is a registered document goal the verification handlers resolve
// against, with no threshold override so the service default applies.
func reviewGoal() store.Goal {
	return store.Goal{
		OptimizationFunctionID: testGoalID,
		GoalText:               "extract the invoice fields",
		TargetFields:           []domain.TargetField{{Name: "invoice_total"}},
		DataSourceRef:          "datasources/invoice.pdf",
	}
}

// unverifiedOutcome is the extraction under review: below the default threshold,
// located in the source text, and not yet resolved.
func unverifiedOutcome() graph.ExtractionOutcome {
	return graph.ExtractionOutcome{
		OutcomeID:          testOutcomeID,
		GoalID:             testGoalID,
		Field:              "invoice_total",
		Method:             "ocr",
		Value:              map[string]any{"invoice_total": "42.00"},
		Provenance:         &domain.ProvenanceLocator{Page: 0, CharStart: 6, CharEnd: 11},
		VerificationStatus: domain.VerificationUnverified,
		Confidence:         0.4,
	}
}

func pendingEntry() store.VerificationEntry {
	return store.VerificationEntry{
		QueueID:                "queue-1",
		OptimizationFunctionID: testGoalID,
		OutcomeID:              testOutcomeID,
		Field:                  "invoice_total",
		ExtractedValue:         "42.00",
		Confidence:             0.4,
		Status:                 store.QueuePending,
	}
}

// newReviewServer wires a server whose goal, outcome, and queue are all
// consistent with one another, the state every resolution test starts from.
func newReviewServer(t *testing.T, queue *fakeQueue, repo *fakeRepo, audits *fakeAudits, sandbox *fakeSandbox) *Server {
	t.Helper()
	return newTestServerQueue(repo, queue, &fakeGoals{get: reviewGoal()}, audits,
		&fakeObjects{}, &fakeHeur{}, &fakeClaude{}, sandbox)
}

func resolve(srv *Server, outcomeID, body string) *httptest.ResponseRecorder {
	return serve(srv, http.MethodPost, "/goals/"+testGoalID+"/verifications/"+outcomeID, "application/json", body)
}

func TestResolveVerificationConfirm(t *testing.T) {
	queue := newFakeQueue(pendingEntry())
	repo := &fakeRepo{extraction: unverifiedOutcome()}
	audits := &fakeAudits{}
	srv := newReviewServer(t, queue, repo, audits, &fakeSandbox{})

	rec := resolve(srv, testOutcomeID, `{"action":"confirm"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %q)", rec.Code, rec.Body.String())
	}

	if len(repo.verifications) != 1 {
		t.Fatalf("expected one graph write, got %d", len(repo.verifications))
	}
	got := repo.verifications[0]
	if got.status != domain.VerificationConfirmed || got.confidence != verifiedConfidence {
		t.Fatalf("graph write = %+v, want confirmed at confidence 1", got)
	}
	if entry := queue.entry(testOutcomeID); entry.Status != store.QueueResolved || entry.Resolution != store.ResolutionConfirmed {
		t.Fatalf("queue entry = %+v, want resolved/confirmed", entry)
	}
	if len(audits.records) != 1 || audits.records[0].Action != "hitl_verification_resolution" {
		t.Fatalf("expected one resolution audit record: %+v", audits.records)
	}
	if _, repaired := audits.records[0].Detail["repair"]; repaired {
		t.Fatal("a first-time resolution must not be audited as a repair")
	}
	detail := audits.records[0].Detail
	if detail["action"] != string(store.ResolutionConfirmed) || detail["outcome_id"] != testOutcomeID {
		t.Fatalf("audit detail must identify the verdict and its outcome: %+v", detail)
	}
	if detail["confidence_at_queue"] != 0.4 {
		t.Fatalf("audit must record what confidence was under review: %+v", detail)
	}
}

// The analyst's replacement value is the human contribution the audit exists to
// preserve, so it has to reach the record.
func TestResolveVerificationAuditsTheCorrectedValue(t *testing.T) {
	queue := newFakeQueue(pendingEntry())
	repo := &fakeRepo{extraction: unverifiedOutcome()}
	auditLog := &fakeAudits{}
	sandbox := &fakeSandbox{docPages: []string{"Total 99.50 due"}}
	srv := newReviewServer(t, queue, repo, auditLog, sandbox)

	rec := resolve(srv, testOutcomeID, `{"action":"correct","corrected_value":"99.50"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %q)", rec.Code, rec.Body.String())
	}
	if len(auditLog.records) != 1 || auditLog.records[0].Detail["corrected_value"] != "99.50" {
		t.Fatalf("the correction must be recorded in the audit detail: %+v", auditLog.records)
	}
}

func TestResolveVerificationCorrectRelocatesTheValue(t *testing.T) {
	t.Run("a value found in the source gets a fresh locator", func(t *testing.T) {
		queue := newFakeQueue(pendingEntry())
		repo := &fakeRepo{extraction: unverifiedOutcome()}
		sandbox := &fakeSandbox{docPages: []string{"Total 99.50 due"}}
		srv := newReviewServer(t, queue, repo, audits(), sandbox)

		rec := resolve(srv, testOutcomeID, `{"action":"correct","corrected_value":"99.50"}`)
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200 (body %q)", rec.Code, rec.Body.String())
		}

		if len(repo.values) != 1 {
			t.Fatalf("expected one value write, got %d", len(repo.values))
		}
		written := repo.values[0]
		if written.value["invoice_total"] != "99.50" {
			t.Fatalf("corrected value = %+v, want the analyst's 99.50 keyed by field", written.value)
		}
		if written.provenance == nil || written.provenance.CharStart != 6 || written.provenance.CharEnd != 11 {
			t.Fatalf("locator = %+v, want the corrected value's own offsets", written.provenance)
		}
		if len(repo.verifications) != 1 || repo.verifications[0].status != domain.VerificationCorrected {
			t.Fatalf("expected a corrected verification write: %+v", repo.verifications)
		}
	})

	t.Run("a value absent from the source falls back to no locator", func(t *testing.T) {
		queue := newFakeQueue(pendingEntry())
		repo := &fakeRepo{extraction: unverifiedOutcome()}
		sandbox := &fakeSandbox{docPages: []string{"Total 99.50 due"}}
		srv := newReviewServer(t, queue, repo, audits(), sandbox)

		rec := resolve(srv, testOutcomeID, `{"action":"correct","corrected_value":"$99.50 USD"}`)
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200 (body %q)", rec.Code, rec.Body.String())
		}
		if len(repo.values) != 1 || repo.values[0].provenance != nil {
			t.Fatalf("a reformatted value has no exact match, so its locator must be nil: %+v", repo.values)
		}
	})
}

func TestResolveVerificationRejectZeroesConfidence(t *testing.T) {
	queue := newFakeQueue(pendingEntry())
	repo := &fakeRepo{extraction: unverifiedOutcome()}
	srv := newReviewServer(t, queue, repo, audits(), &fakeSandbox{})

	rec := resolve(srv, testOutcomeID, `{"action":"reject"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %q)", rec.Code, rec.Body.String())
	}
	got := repo.verifications[0]
	if got.status != domain.VerificationRejected || got.confidence != rejectedConfidence {
		t.Fatalf("graph write = %+v, want rejected at confidence 0", got)
	}
}

// A resolution for an outcome the loop never queued still has to be recorded, so
// the queue stays the full history of what a human reviewed.
func TestResolveVerificationOnDemandRecordsTheReview(t *testing.T) {
	queue := newFakeQueue()
	repo := &fakeRepo{extraction: unverifiedOutcome()}
	srv := newReviewServer(t, queue, repo, audits(), &fakeSandbox{})

	rec := resolve(srv, testOutcomeID, `{"action":"confirm"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %q)", rec.Code, rec.Body.String())
	}
	if len(queue.enqueued) != 1 {
		t.Fatalf("expected the review to be recorded, got %d entries", len(queue.enqueued))
	}
	entry := queue.enqueued[0]
	if entry.Status != store.QueueResolved || entry.Resolution != store.ResolutionConfirmed {
		t.Fatalf("recorded entry = %+v, want resolved/confirmed", entry)
	}
	// The row's NOT NULL columns come from the graph outcome, not the request.
	if entry.ExtractedValue != "42.00" || entry.Confidence != 0.4 || entry.Field != "invoice_total" {
		t.Fatalf("recorded entry did not snapshot the outcome: %+v", entry)
	}
}

func TestResolveVerificationRepairsAStrandedResolution(t *testing.T) {
	t.Run("completes the stored verdict when the graph never caught up", func(t *testing.T) {
		stranded := pendingEntry()
		stranded.Status = store.QueueResolved
		stranded.Resolution = store.ResolutionConfirmed
		queue := newFakeQueue(stranded)
		repo := &fakeRepo{extraction: unverifiedOutcome()}
		auditLog := &fakeAudits{}
		srv := newReviewServer(t, queue, repo, auditLog, &fakeSandbox{})

		rec := resolve(srv, testOutcomeID, `{"action":"confirm"}`)
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200 (body %q)", rec.Code, rec.Body.String())
		}
		if len(repo.verifications) != 1 || repo.verifications[0].status != domain.VerificationConfirmed {
			t.Fatalf("the repair must complete the write-through: %+v", repo.verifications)
		}
		if len(auditLog.records) != 1 || auditLog.records[0].Detail["repair"] != true {
			t.Fatalf("a repair must be audited and marked as one: %+v", auditLog.records)
		}
	})

	t.Run("writes the stored verdict, not the requested one, and reports the conflict", func(t *testing.T) {
		stranded := pendingEntry()
		stranded.Status = store.QueueResolved
		stranded.Resolution = store.ResolutionRejected
		queue := newFakeQueue(stranded)
		repo := &fakeRepo{extraction: unverifiedOutcome()}
		srv := newReviewServer(t, queue, repo, audits(), &fakeSandbox{})

		rec := resolve(srv, testOutcomeID, `{"action":"confirm"}`)
		if rec.Code != http.StatusConflict {
			t.Fatalf("status = %d, want 409 (body %q)", rec.Code, rec.Body.String())
		}
		if len(repo.verifications) != 1 || repo.verifications[0].status != domain.VerificationRejected {
			t.Fatalf("the recorded rejection is what must land, not the requested confirm: %+v", repo.verifications)
		}
	})

	t.Run("an entry already resolved in the graph is a plain conflict", func(t *testing.T) {
		resolved := pendingEntry()
		resolved.Status = store.QueueResolved
		resolved.Resolution = store.ResolutionConfirmed
		queue := newFakeQueue(resolved)
		outcome := unverifiedOutcome()
		outcome.VerificationStatus = domain.VerificationConfirmed
		repo := &fakeRepo{extraction: outcome}
		srv := newReviewServer(t, queue, repo, audits(), &fakeSandbox{})

		rec := resolve(srv, testOutcomeID, `{"action":"reject"}`)
		if rec.Code != http.StatusConflict {
			t.Fatalf("status = %d, want 409 (body %q)", rec.Code, rec.Body.String())
		}
		if len(repo.verifications) != 0 {
			t.Fatalf("a settled resolution must not be rewritten: %+v", repo.verifications)
		}
	})
}

// The loop's own routing write can land between an on-demand resolution's read
// and its insert. The row it leaves is pending with no verdict, so the request
// must claim it rather than repair from values that were never recorded.
func TestResolveVerificationClaimsARowRacedInByTheLoop(t *testing.T) {
	queue := &raceQueue{fakeQueue: newFakeQueue(), pendingOnConflict: pendingEntry()}
	repo := &fakeRepo{extraction: unverifiedOutcome()}
	srv := newReviewServer(t, queue.fakeQueue, repo, audits(), &fakeSandbox{})
	srv.queue = queue

	rec := resolve(srv, testOutcomeID, `{"action":"confirm"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %q)", rec.Code, rec.Body.String())
	}
	if len(repo.verifications) != 1 || repo.verifications[0].status != domain.VerificationConfirmed {
		t.Fatalf("the request's own verdict must land: %+v", repo.verifications)
	}
	if entry := queue.entry(testOutcomeID); entry.Resolution != store.ResolutionConfirmed {
		t.Fatalf("the raced-in row must end up claimed: %+v", entry)
	}
}

// raceQueue makes the loop's routing write appear between the on-demand read and
// the insert that follows it: the read finds nothing, the insert conflicts, and
// the re-read finds the pending row the loop just wrote.
type raceQueue struct {
	*fakeQueue
	pendingOnConflict store.VerificationEntry
	conflicted        bool
}

func (q *raceQueue) EnqueueResolved(_ context.Context, _ store.VerificationEntry, _ store.QueueResolution, _ string) error {
	q.conflicted = true
	q.entries[q.pendingOnConflict.OutcomeID] = q.pendingOnConflict
	return store.ErrAlreadyResolved
}

func TestResolveVerificationFailureCompensates(t *testing.T) {
	t.Run("a failed graph write returns the claim and reports 500", func(t *testing.T) {
		queue := newFakeQueue(pendingEntry())
		repo := &fakeRepo{extraction: unverifiedOutcome(), verifyErr: errors.New("neo4j down")}
		srv := newReviewServer(t, queue, repo, audits(), &fakeSandbox{})

		rec := resolve(srv, testOutcomeID, `{"action":"confirm"}`)
		if rec.Code != http.StatusInternalServerError {
			t.Fatalf("status = %d, want 500 (body %q)", rec.Code, rec.Body.String())
		}
		if entry := queue.entry(testOutcomeID); entry.Status != store.QueuePending {
			t.Fatalf("the claim must be returned so the analyst can retry: %+v", entry)
		}
	})

	t.Run("a sandbox fault on the correction path is a 502 and still compensates", func(t *testing.T) {
		queue := newFakeQueue(pendingEntry())
		repo := &fakeRepo{extraction: unverifiedOutcome()}
		sandbox := &fakeSandbox{docTextErr: &SandboxError{Status: http.StatusInternalServerError, Message: "boom"}}
		srv := newReviewServer(t, queue, repo, audits(), sandbox)

		rec := resolve(srv, testOutcomeID, `{"action":"correct","corrected_value":"99.50"}`)
		if rec.Code != http.StatusBadGateway {
			t.Fatalf("status = %d, want 502 (body %q)", rec.Code, rec.Body.String())
		}
		if entry := queue.entry(testOutcomeID); entry.Status != store.QueuePending {
			t.Fatalf("a fault before any graph write must still return the claim: %+v", entry)
		}
		if len(repo.values) != 0 {
			t.Fatalf("no value should have been written: %+v", repo.values)
		}
	})

	t.Run("a claim whose verdict a repairer already landed is left alone", func(t *testing.T) {
		queue := newFakeQueue(pendingEntry())
		// The graph write fails for this request but the outcome reads back
		// resolved, which is what a concurrent repairer finishing first looks like.
		outcome := unverifiedOutcome()
		outcome.VerificationStatus = domain.VerificationConfirmed
		repo := &fakeRepo{extraction: outcome, verifyErr: errors.New("lost the write")}
		srv := newReviewServer(t, queue, repo, audits(), &fakeSandbox{})

		rec := resolve(srv, testOutcomeID, `{"action":"confirm"}`)
		if rec.Code != http.StatusInternalServerError {
			t.Fatalf("status = %d, want 500 (body %q)", rec.Code, rec.Body.String())
		}
		if len(queue.unclaimed) != 0 {
			t.Fatalf("a resolution that did land must not be reverted: %+v", queue.unclaimed)
		}
	})
}

// R13 requires every resolution to be audited, so a missing record is surfaced
// rather than logged past -- while the resolution itself stands.
func TestResolveVerificationAuditFailureIsReported(t *testing.T) {
	queue := newFakeQueue(pendingEntry())
	repo := &fakeRepo{extraction: unverifiedOutcome()}
	srv := newReviewServer(t, queue, repo, audits(), &fakeSandbox{})
	srv.audits = &failingAudits{}

	rec := resolve(srv, testOutcomeID, `{"action":"confirm"}`)
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500 (body %q)", rec.Code, rec.Body.String())
	}
	if len(repo.verifications) != 1 {
		t.Fatalf("the resolution itself must stand: %+v", repo.verifications)
	}
	if entry := queue.entry(testOutcomeID); entry.Status != store.QueueResolved {
		t.Fatalf("the queue row must stay resolved: %+v", entry)
	}
}

type failingAudits struct{}

func (failingAudits) Append(_ context.Context, _ store.AuditRecord) error {
	return errors.New("audit table unreachable")
}

func TestResolveVerificationValidation(t *testing.T) {
	otherGoalOutcome := unverifiedOutcome()
	otherGoalOutcome.GoalID = "another-goal"

	cases := []struct {
		name       string
		repo       *fakeRepo
		body       string
		wantStatus int
	}{
		{"unknown action", &fakeRepo{extraction: unverifiedOutcome()}, `{"action":"maybe"}`, http.StatusUnprocessableEntity},
		{"correction without a value", &fakeRepo{extraction: unverifiedOutcome()}, `{"action":"correct"}`, http.StatusUnprocessableEntity},
		{"correction with a blank value", &fakeRepo{extraction: unverifiedOutcome()}, `{"action":"correct","corrected_value":"  "}`, http.StatusUnprocessableEntity},
		{"malformed body", &fakeRepo{extraction: unverifiedOutcome()}, `{`, http.StatusBadRequest},
		{"missing outcome", &fakeRepo{extractionErr: graph.ErrNotFound}, `{"action":"confirm"}`, http.StatusNotFound},
		{"outcome of another goal", &fakeRepo{extraction: otherGoalOutcome}, `{"action":"confirm"}`, http.StatusNotFound},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			queue := newFakeQueue(pendingEntry())
			srv := newReviewServer(t, queue, c.repo, audits(), &fakeSandbox{})
			rec := resolve(srv, testOutcomeID, c.body)
			if rec.Code != c.wantStatus {
				t.Fatalf("status = %d, want %d (body %q)", rec.Code, c.wantStatus, rec.Body.String())
			}
			if len(c.repo.verifications) != 0 {
				t.Fatalf("a rejected request must not reach the graph: %+v", c.repo.verifications)
			}
			if entry := queue.entry(testOutcomeID); entry.Status != store.QueuePending {
				t.Fatalf("a rejected request must not claim the entry: %+v", entry)
			}
		})
	}
}

func TestListVerifications(t *testing.T) {
	resolved := pendingEntry()
	resolved.OutcomeID = "outcome-2"
	resolved.QueueID = "queue-2"
	resolved.Status = store.QueueResolved
	resolved.Resolution = store.ResolutionConfirmed
	queue := newFakeQueue(pendingEntry(), resolved)
	srv := newReviewServer(t, queue, &fakeRepo{}, audits(), &fakeSandbox{})

	t.Run("carries the goal's review settings alongside the entries", func(t *testing.T) {
		rec := serve(srv, http.MethodGet, "/goals/"+testGoalID+"/verifications", "", "")
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200 (body %q)", rec.Code, rec.Body.String())
		}
		var out verificationListDTO
		if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
			t.Fatalf("decode: %v", err)
		}
		if out.EffectiveThreshold != testHITLThreshold || out.EpochMode != string(store.EpochSpeculative) {
			t.Fatalf("review settings = %+v, want the service default and speculative mode", out)
		}
		if len(out.Entries) != 2 {
			t.Fatalf("expected both entries, got %d", len(out.Entries))
		}
	})

	t.Run("filters by status", func(t *testing.T) {
		rec := serve(srv, http.MethodGet, "/goals/"+testGoalID+"/verifications?status=pending", "", "")
		var out verificationListDTO
		if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
			t.Fatalf("decode: %v", err)
		}
		if len(out.Entries) != 1 || out.Entries[0].Status != string(store.QueuePending) {
			t.Fatalf("expected only the pending entry: %+v", out.Entries)
		}
	})

	t.Run("rejects an unknown status", func(t *testing.T) {
		rec := serve(srv, http.MethodGet, "/goals/"+testGoalID+"/verifications?status=elsewhere", "", "")
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("status = %d, want 400", rec.Code)
		}
	})

	t.Run("reports a goal's own threshold when it overrides the default", func(t *testing.T) {
		override := 0.5
		goal := reviewGoal()
		goal.ConfidenceThreshold = &override
		goal.EpochMode = store.EpochBlocking
		overridden := newTestServerQueue(&fakeRepo{}, newFakeQueue(), &fakeGoals{get: goal}, audits(),
			&fakeObjects{}, &fakeHeur{}, &fakeClaude{}, &fakeSandbox{})

		rec := serve(overridden, http.MethodGet, "/goals/"+testGoalID+"/verifications", "", "")
		var out verificationListDTO
		if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
			t.Fatalf("decode: %v", err)
		}
		if out.EffectiveThreshold != override || out.EpochMode != string(store.EpochBlocking) {
			t.Fatalf("review settings = %+v, want the goal's overrides", out)
		}
	})
}

// The browse endpoint exposes every extraction, not just the queued ones, so an
// analyst can review a result that cleared the threshold on its own.
func TestListOutcomesIncludesAboveThresholdResults(t *testing.T) {
	confident := unverifiedOutcome()
	confident.OutcomeID = "outcome-2"
	confident.Confidence = 0.95
	repo := &fakeRepo{extractionList: []graph.ExtractionOutcome{unverifiedOutcome(), confident}}
	srv := newReviewServer(t, newFakeQueue(), repo, audits(), &fakeSandbox{})

	rec := serve(srv, http.MethodGet, "/goals/"+testGoalID+"/outcomes", "", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %q)", rec.Code, rec.Body.String())
	}
	var out []extractionOutcomeDTO
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(out) != 2 {
		t.Fatalf("expected both outcomes, got %d", len(out))
	}
	if out[1].Confidence != 0.95 || out[1].VerificationStatus != string(domain.VerificationUnverified) {
		t.Fatalf("an above-threshold outcome must be listed with its own confidence and status: %+v", out[1])
	}
}

func TestOutcomeExcerpt(t *testing.T) {
	t.Run("returns the located span and its page", func(t *testing.T) {
		repo := &fakeRepo{extraction: unverifiedOutcome()}
		sandbox := &fakeSandbox{docPages: []string{"Total 42.00 due", "page two"}}
		srv := newReviewServer(t, newFakeQueue(), repo, audits(), sandbox)

		rec := serve(srv, http.MethodGet, "/goals/"+testGoalID+"/outcomes/"+testOutcomeID+"/excerpt", "", "")
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200 (body %q)", rec.Code, rec.Body.String())
		}
		var out excerptDTO
		if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
			t.Fatalf("decode: %v", err)
		}
		if out.Fallback || out.Excerpt != "42.00" || out.PageText != "Total 42.00 due" {
			t.Fatalf("excerpt = %+v, want the pinpoint span and its page", out)
		}
	})

	t.Run("falls back to the whole document without a locator", func(t *testing.T) {
		outcome := unverifiedOutcome()
		outcome.Provenance = nil
		repo := &fakeRepo{extraction: outcome}
		sandbox := &fakeSandbox{docPages: []string{"page one", "page two"}}
		srv := newReviewServer(t, newFakeQueue(), repo, audits(), sandbox)

		rec := serve(srv, http.MethodGet, "/goals/"+testGoalID+"/outcomes/"+testOutcomeID+"/excerpt", "", "")
		var out excerptDTO
		if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
			t.Fatalf("decode: %v", err)
		}
		if !out.Fallback || len(out.Pages) != 2 {
			t.Fatalf("excerpt = %+v, want the whole-document fallback", out)
		}
	})

	t.Run("degrades rather than panicking on a locator the document outgrew", func(t *testing.T) {
		cases := map[string]*domain.ProvenanceLocator{
			"page beyond the document": {Page: 7, CharStart: 0, CharEnd: 1},
			"span beyond the page":     {Page: 0, CharStart: 2, CharEnd: 900},
			"inverted span":            {Page: 0, CharStart: 5, CharEnd: 2},
			"negative offset":          {Page: 0, CharStart: -1, CharEnd: 3},
			"negative page":            {Page: -1, CharStart: 0, CharEnd: 1},
		}
		for name, locator := range cases {
			t.Run(name, func(t *testing.T) {
				outcome := unverifiedOutcome()
				outcome.Provenance = locator
				repo := &fakeRepo{extraction: outcome}
				sandbox := &fakeSandbox{docPages: []string{"short"}}
				srv := newReviewServer(t, newFakeQueue(), repo, audits(), sandbox)

				rec := serve(srv, http.MethodGet, "/goals/"+testGoalID+"/outcomes/"+testOutcomeID+"/excerpt", "", "")
				if rec.Code != http.StatusOK {
					t.Fatalf("status = %d, want 200 (body %q)", rec.Code, rec.Body.String())
				}
				var out excerptDTO
				if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
					t.Fatalf("decode: %v", err)
				}
				if !out.Fallback {
					t.Fatalf("an unusable locator must fall back: %+v", out)
				}
			})
		}
	})

	t.Run("a sandbox fault is a 502", func(t *testing.T) {
		repo := &fakeRepo{extraction: unverifiedOutcome()}
		sandbox := &fakeSandbox{docTextErr: &SandboxError{Status: http.StatusInternalServerError, Message: "boom"}}
		srv := newReviewServer(t, newFakeQueue(), repo, audits(), sandbox)

		rec := serve(srv, http.MethodGet, "/goals/"+testGoalID+"/outcomes/"+testOutcomeID+"/excerpt", "", "")
		if rec.Code != http.StatusBadGateway {
			t.Fatalf("status = %d, want 502 (body %q)", rec.Code, rec.Body.String())
		}
	})
}

func TestConfidenceHistogram(t *testing.T) {
	t.Run("bins by confidence and clamps a perfect score into the top bin", func(t *testing.T) {
		hist := newConfidenceHistogram()
		hist.Add("a", 0.05)
		hist.Add("b", 0.42)
		hist.Add("c", 1.0)

		// A model that reports outside 0..1 must land in a real bucket rather than
		// indexing past either end of the array.
		hist.Add("d", -0.1)
		hist.Add("e", 1.7)

		payload := hist.Payload()
		bins := payload["bins"].([]int)
		if bins[0] != 2 || bins[4] != 1 || bins[confidenceBinCount-1] != 2 {
			t.Fatalf("bins = %v, want the out-of-range weights clamped into the first and last", bins)
		}
		if payload["total"] != 5 {
			t.Fatalf("total = %v, want 5", payload["total"])
		}
	})

	t.Run("a resolution moves an outcome between bins without changing the total", func(t *testing.T) {
		hist := newConfidenceHistogram()
		hist.Add("a", 0.4)
		if !hist.Rebin("a", verifiedConfidence) {
			t.Fatal("rebinning a counted outcome must report that it was counted")
		}
		payload := hist.Payload()
		bins := payload["bins"].([]int)
		if bins[4] != 0 || bins[confidenceBinCount-1] != 1 {
			t.Fatalf("bins = %v, want the outcome moved to the top bin", bins)
		}
		if payload["total"] != 1 {
			t.Fatalf("total = %v, want the count unchanged at 1", payload["total"])
		}
	})

	t.Run("an outcome this run never counted is not rebinned", func(t *testing.T) {
		hist := newConfidenceHistogram()
		if hist.Rebin("from-an-earlier-run", verifiedConfidence) {
			t.Fatal("an untracked outcome must report that it was not counted")
		}
		if hist.Payload()["total"] != 0 {
			t.Fatal("an untracked outcome must not be added by a rebin")
		}
	})

	t.Run("the published payload is a snapshot", func(t *testing.T) {
		hist := newConfidenceHistogram()
		hist.Add("a", 0.95)
		bins := hist.Payload()["bins"].([]int)
		hist.Add("b", 0.95)
		if bins[confidenceBinCount-1] != 1 {
			t.Fatalf("later counting mutated an already-published payload: %v", bins)
		}
	})
}

// A resolution has to reach the live view, but only while a run is streaming:
// after completion the hub has evicted the run, and publishing would resurrect
// state nothing will ever clean up.
func TestRebinConfidencePublishesOnlyForALiveRun(t *testing.T) {
	srv := newReviewServer(t, newFakeQueue(), &fakeRepo{}, audits(), &fakeSandbox{})

	hist := srv.registerHistogram(testGoalID)
	srv.recordConfidence(testGoalID, hist, testOutcomeID, 0.4)
	srv.rebinConfidence(testGoalID, testOutcomeID, verifiedConfidence)

	replay, _, cancel := srv.hub.Subscribe(testGoalID)
	cancel()
	distributions := 0
	for _, ev := range replay {
		if ev.Type == "confidence_distribution" {
			distributions++
			bins := ev.Payload["bins"].([]int)
			if bins[confidenceBinCount-1] != 1 {
				t.Fatalf("the replayed distribution must reflect the resolution: %v", bins)
			}
		}
	}
	if distributions != 1 {
		t.Fatalf("coalescing must leave exactly one distribution frame, got %d", distributions)
	}

	srv.deregisterHistogram(testGoalID, hist)
	srv.hub.Complete(testGoalID)
	srv.rebinConfidence(testGoalID, testOutcomeID, rejectedConfidence)
	if len(srv.hub.runs) != 0 {
		t.Fatal("a resolution after the run ended must not recreate hub state")
	}
}

// A re-trigger replaces the tracked histogram; the older run ending must not
// tear down the newer one's tracking.
func TestDeregisterHistogramLeavesALaterRunAlone(t *testing.T) {
	srv := newReviewServer(t, newFakeQueue(), &fakeRepo{}, audits(), &fakeSandbox{})

	first := srv.registerHistogram(testGoalID)
	second := srv.registerHistogram(testGoalID)
	srv.deregisterHistogram(testGoalID, first)

	if srv.histograms[testGoalID] != second {
		t.Fatal("the older run's teardown must leave the newer run's histogram tracked")
	}
}

func audits() *fakeAudits { return &fakeAudits{} }

// A blocking-mode run emits nothing while it waits on a human, so the stream has
// to say something on its own or intermediaries will reap the connection.
func TestStreamSendsKeepalivesWhileIdle(t *testing.T) {
	srv := newReviewServer(t, newFakeQueue(), &fakeRepo{}, audits(), &fakeSandbox{})

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	req := httptest.NewRequestWithContext(ctx, http.MethodGet, "/goals/"+testGoalID+"/stream", nil)
	req.SetPathValue("id", testGoalID)
	rec := httptest.NewRecorder()
	srv.handleStream(rec, req)

	// SSE comment frames are ignorable padding, so they must not parse as events.
	body := rec.Body.String()
	if !strings.Contains(body, ": keepalive") {
		t.Fatalf("an idle stream must emit keepalives, got %q", body)
	}
	if strings.Contains(body, "data:") {
		t.Fatalf("a keepalive must not look like an event frame: %q", body)
	}
}

// A resolution that reaches the stores must not lose its compensation or its
// audit record because the analyst's browser hung up mid-write.
func TestResolveVerificationSurvivesClientDisconnect(t *testing.T) {
	t.Run("the audit still lands when the client leaves mid-write", func(t *testing.T) {
		// The window that matters: the graph write succeeds and only then does the
		// client hang up, so the resolution is real but its record is at risk.
		queue := newFakeQueue(pendingEntry())
		ctx, cancel := context.WithCancel(context.Background())
		repo := &fakeRepo{extraction: unverifiedOutcome(), onVerify: cancel}
		auditLog := &fakeAudits{}
		srv := newReviewServer(t, queue, repo, auditLog, &fakeSandbox{})

		resolveWithContext(t, srv, ctx, `{"action":"confirm"}`)

		if len(repo.verifications) != 1 {
			t.Fatalf("the graph write must have landed for this case to mean anything: %+v", repo.verifications)
		}
		if len(auditLog.records) != 1 || auditLog.records[0].Action != "hitl_verification_resolution" {
			t.Fatalf("a resolution that reached the graph must still be audited: %+v", auditLog.records)
		}
	})

	t.Run("a failed write still releases the claim", func(t *testing.T) {
		// The client leaves at the moment the graph write is attempted, and that
		// write then fails -- so the compensation has to run on a context the
		// disconnect cannot have already killed.
		queue := newFakeQueue(pendingEntry())
		ctx, cancel := context.WithCancel(context.Background())
		repo := &fakeRepo{extraction: unverifiedOutcome(), verifyErr: errors.New("neo4j down"), onVerify: cancel}
		srv := newReviewServer(t, queue, repo, audits(), &fakeSandbox{})

		resolveWithContext(t, srv, ctx, `{"action":"confirm"}`)

		if entry := queue.entry(testOutcomeID); entry.Status != store.QueuePending {
			t.Fatalf("the claim must be released even though the request was cancelled: %+v", entry)
		}
	})
}

// resolveWithContext drives a resolution on a caller-supplied request context,
// so a test can choose exactly when the analyst's browser hangs up.
func resolveWithContext(t *testing.T, srv *Server, ctx context.Context, body string) {
	t.Helper()
	req := httptest.NewRequestWithContext(ctx, http.MethodPost,
		"/goals/"+testGoalID+"/verifications/"+testOutcomeID, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.SetPathValue("id", testGoalID)
	req.SetPathValue("outcomeID", testOutcomeID)
	srv.handleResolveVerification(httptest.NewRecorder(), req)
}

// A repairer finishes someone else's stranded resolution. It holds no claim of
// its own, so a failure on its path must leave the row resolved -- reverting it
// would undo a review that really happened.
func TestRepairFailureLeavesTheClaimAlone(t *testing.T) {
	stranded := pendingEntry()
	stranded.Status = store.QueueResolved
	stranded.Resolution = store.ResolutionConfirmed
	queue := newFakeQueue(stranded)
	repo := &fakeRepo{extraction: unverifiedOutcome(), verifyErr: errors.New("neo4j down")}
	srv := newReviewServer(t, queue, repo, audits(), &fakeSandbox{})

	rec := resolve(srv, testOutcomeID, `{"action":"confirm"}`)
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500 (body %q)", rec.Code, rec.Body.String())
	}
	if entry := queue.entry(testOutcomeID); entry.Status != store.QueueResolved ||
		entry.Resolution != store.ResolutionConfirmed {
		t.Fatalf("a repairer must not revert the claim it never owned: %+v", entry)
	}
	if len(queue.unclaimed) != 0 {
		t.Fatalf("no un-claim should have been attempted: %+v", queue.unclaimed)
	}
}

// The stream resolves the goal before subscribing: the keepalive holds an
// otherwise silent connection open indefinitely, so an unknown id must not be
// able to pin hub state nothing will reap.
func TestStreamRejectsAnUnknownGoal(t *testing.T) {
	srv := newTestServerQueue(&fakeRepo{}, newFakeQueue(), &fakeGoals{getErr: pgx.ErrNoRows}, audits(),
		&fakeObjects{}, &fakeHeur{}, &fakeClaude{}, &fakeSandbox{})

	rec := serve(srv, http.MethodGet, "/goals/nope/stream", "", "")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404 (body %q)", rec.Code, rec.Body.String())
	}
	if len(srv.hub.runs) != 0 {
		t.Fatalf("an unknown goal must not create hub state: %+v", srv.hub.runs)
	}
}

// A resolution is an outsider to the run, so it must never recreate hub state a
// completed run already evicted -- nothing would be left to clean it up.
func TestDistributionIsNotPublishedIntoACompletedRun(t *testing.T) {
	srv := newReviewServer(t, newFakeQueue(), &fakeRepo{}, audits(), &fakeSandbox{})

	hist := srv.registerHistogram(testGoalID)
	srv.recordConfidence(testGoalID, hist, testOutcomeID, 0.4)
	srv.hub.Complete(testGoalID)

	srv.rebinConfidence(testGoalID, testOutcomeID, verifiedConfidence)
	if len(srv.hub.runs) != 0 {
		t.Fatalf("a resolution after completion must not resurrect the run: %+v", srv.hub.runs)
	}
}

// A superseded run must stop feeding the live view: the registry entry belongs
// to whichever run registered last.
func TestSupersededRunStopsPublishing(t *testing.T) {
	srv := newReviewServer(t, newFakeQueue(), &fakeRepo{}, audits(), &fakeSandbox{})

	first := srv.registerHistogram(testGoalID)
	srv.registerHistogram(testGoalID) // a re-trigger replaces the entry
	srv.hub.Publish(testGoalID, Event{Type: "triplet"})
	srv.recordConfidence(testGoalID, first, testOutcomeID, 0.4)

	replay, _, cancel := srv.hub.Subscribe(testGoalID)
	cancel()
	for _, ev := range replay {
		if ev.Type == "confidence_distribution" {
			t.Fatalf("the superseded run must not publish into the newer run's stream: %+v", replay)
		}
	}
}

// detached() has to keep the request's values while dropping its cancellation,
// and stay bounded so a wedged store cannot hold the handler open.
func TestDetachedContext(t *testing.T) {
	type key struct{}
	parent, cancel := context.WithCancel(context.WithValue(context.Background(), key{}, "actor"))

	ctx, release := detached(parent)
	defer release()
	cancel()

	if err := ctx.Err(); err != nil {
		t.Fatalf("a detached context must survive its parent's cancellation, got %v", err)
	}
	if ctx.Value(key{}) != "actor" {
		t.Fatal("a detached context must keep the request's values for the identity seam")
	}
	deadline, ok := ctx.Deadline()
	if !ok {
		t.Fatal("a detached context must stay bounded")
	}
	if until := time.Until(deadline); until <= 0 || until > statusWriteTimeout {
		t.Fatalf("deadline is %v out, want within %v", until, statusWriteTimeout)
	}
}

// A queue read that fails for any reason other than "never queued" is this
// service's failure, not a signal to treat the outcome as unqueued.
func TestResolveVerificationQueueReadFailureIs500(t *testing.T) {
	queue := newFakeQueue(pendingEntry())
	queue.getErr = errors.New("postgres unreachable")
	repo := &fakeRepo{extraction: unverifiedOutcome()}
	srv := newReviewServer(t, queue, repo, audits(), &fakeSandbox{})

	rec := resolve(srv, testOutcomeID, `{"action":"confirm"}`)
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500 (body %q)", rec.Code, rec.Body.String())
	}
	if len(queue.enqueued) != 0 {
		t.Fatalf("a failed read must not be mistaken for an unqueued outcome: %+v", queue.enqueued)
	}
	if len(repo.verifications) != 0 {
		t.Fatalf("nothing should reach the graph: %+v", repo.verifications)
	}
}

// A repair writes the stored correction, so a caller who sent different text has
// to be told theirs was discarded -- the value is the human contribution.
func TestRepairOfADivergentCorrectionConflicts(t *testing.T) {
	stranded := pendingEntry()
	stranded.Status = store.QueueResolved
	stranded.Resolution = store.ResolutionCorrected
	stranded.CorrectedValue = "42.99"
	queue := newFakeQueue(stranded)
	repo := &fakeRepo{extraction: unverifiedOutcome()}
	sandbox := &fakeSandbox{docPages: []string{"Total 42.99 due"}}
	srv := newReviewServer(t, queue, repo, audits(), sandbox)

	rec := resolve(srv, testOutcomeID, `{"action":"correct","corrected_value":"99.50"}`)
	if rec.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409 (body %q)", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "42.99") {
		t.Fatalf("the conflict must name the value that actually stands: %q", rec.Body.String())
	}
	if len(repo.values) != 1 || repo.values[0].value["invoice_total"] != "42.99" {
		t.Fatalf("the stored correction is what must land: %+v", repo.values)
	}
}

// Store failures behind the read-only review surface are this service's, and
// must not read as a missing goal or outcome.
func TestReviewReadFailuresAre500(t *testing.T) {
	t.Run("queue list", func(t *testing.T) {
		queue := newFakeQueue()
		queue.getErr = errors.New("postgres unreachable")
		srv := newReviewServer(t, queue, &fakeRepo{}, audits(), &fakeSandbox{})
		if rec := serve(srv, http.MethodGet, "/goals/"+testGoalID+"/verifications", "", ""); rec.Code != http.StatusInternalServerError {
			t.Fatalf("status = %d, want 500", rec.Code)
		}
	})

	t.Run("outcome list", func(t *testing.T) {
		repo := &fakeRepo{extractionErr: errors.New("neo4j unreachable")}
		srv := newReviewServer(t, newFakeQueue(), repo, audits(), &fakeSandbox{})
		if rec := serve(srv, http.MethodGet, "/goals/"+testGoalID+"/outcomes", "", ""); rec.Code != http.StatusInternalServerError {
			t.Fatalf("status = %d, want 500", rec.Code)
		}
	})

	t.Run("outcome read that is not a miss", func(t *testing.T) {
		repo := &fakeRepo{extractionErr: errors.New("neo4j unreachable")}
		srv := newReviewServer(t, newFakeQueue(), repo, audits(), &fakeSandbox{})
		rec := resolve(srv, testOutcomeID, `{"action":"confirm"}`)
		if rec.Code != http.StatusInternalServerError {
			t.Fatalf("status = %d, want 500 -- a failed read must not read as 'not found'", rec.Code)
		}
	})
}

// The DTOs exist to fix a snake_case wire contract for the review UI, so at
// least one test has to read the keys rather than round-tripping the struct.
func TestReviewResponsesUseSnakeCaseKeys(t *testing.T) {
	located := pendingEntry()
	located.Provenance = &domain.ProvenanceLocator{Page: 0, CharStart: 6, CharEnd: 11}
	queue := newFakeQueue(located)
	repo := &fakeRepo{extractionList: []graph.ExtractionOutcome{unverifiedOutcome()}}
	srv := newReviewServer(t, queue, repo, audits(), &fakeSandbox{})

	var list map[string]any
	decodeBody(t, serve(srv, http.MethodGet, "/goals/"+testGoalID+"/verifications", "", ""), &list)
	requireKeys(t, list, "effective_threshold", "epoch_mode", "entries")
	entry := list["entries"].([]any)[0].(map[string]any)
	requireKeys(t, entry, "queue_id", "outcome_id", "extracted_value", "confidence", "status", "created_at")
	requireKeys(t, entry["provenance"].(map[string]any), "page", "char_start", "char_end")

	var outcomes []any
	decodeBody(t, serve(srv, http.MethodGet, "/goals/"+testGoalID+"/outcomes", "", ""), &outcomes)
	requireKeys(t, outcomes[0].(map[string]any), "outcome_id", "verification_status", "confidence")
}

func decodeBody(t *testing.T, rec *httptest.ResponseRecorder, into any) {
	t.Helper()
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %q)", rec.Code, rec.Body.String())
	}
	if err := json.Unmarshal(rec.Body.Bytes(), into); err != nil {
		t.Fatalf("decode: %v", err)
	}
}

func requireKeys(t *testing.T, obj map[string]any, keys ...string) {
	t.Helper()
	for _, k := range keys {
		if _, ok := obj[k]; !ok {
			t.Fatalf("response is missing the %q key the review UI reads: %+v", k, obj)
		}
	}
}

// The verdict is already in both stores by the time the audit runs, so a failed
// audit must not also strand the outcome in its pre-resolution bucket for every
// remaining frame of the run.
func TestAuditFailureStillUpdatesTheLiveDistribution(t *testing.T) {
	queue := newFakeQueue(pendingEntry())
	repo := &fakeRepo{extraction: unverifiedOutcome()}
	srv := newReviewServer(t, queue, repo, audits(), &fakeSandbox{})
	srv.audits = &failingAudits{}

	hist := srv.registerHistogram(testGoalID)
	srv.recordConfidence(testGoalID, hist, testOutcomeID, 0.4)

	if rec := resolve(srv, testOutcomeID, `{"action":"confirm"}`); rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500 (body %q)", rec.Code, rec.Body.String())
	}
	bins := hist.Payload()["bins"].([]int)
	if bins[confidenceBinCount-1] != 1 {
		t.Fatalf("the confirmed outcome must have moved to the top bin: %v", bins)
	}
}

// A blocking run can be silent for a long time, so the response head has to
// reach the client immediately -- otherwise an open stream is indistinguishable
// from a hang.
func TestStreamFlushesItsHeadBeforeWaiting(t *testing.T) {
	srv := newReviewServer(t, newFakeQueue(), &fakeRepo{}, audits(), &fakeSandbox{})

	// A context already done: the handler writes the head, then returns without
	// ever publishing a frame.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	req := httptest.NewRequestWithContext(ctx, http.MethodGet, "/goals/"+testGoalID+"/stream", nil)
	req.SetPathValue("id", testGoalID)
	rec := httptest.NewRecorder()
	srv.handleStream(rec, req)

	if !rec.Flushed {
		t.Fatal("the response head must be flushed before the handler waits for events")
	}
	if ct := rec.Header().Get("Content-Type"); ct != "text/event-stream" {
		t.Fatalf("content-type = %q, want text/event-stream", ct)
	}
}
