package orchestrator

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/arborette/arborette/internal/domain"
	"github.com/arborette/arborette/internal/llm"
	"github.com/arborette/arborette/internal/store"
)

// tierSchema is the introspected schema the intent tests validate claims against: one
// value-constrained categorical column.
func tierSchema() IntrospectResponse {
	return IntrospectResponse{Schema: schemaDTO{Columns: []columnDTO{
		{Name: "revenue", Type: "DOUBLE"},
		{Name: "tier", Type: "VARCHAR", DistinctValues: []string{"gold", "silver"}},
	}}}
}

// submitTabularGoal registers a tabular goal against the given classifier and returns
// the response with the goal it persisted.
func submitTabularGoal(t *testing.T, claude *fakeClaude, goals *fakeGoals, audits *fakeAudits) *httptest.ResponseRecorder {
	t.Helper()
	claude.matrix = fittedMatrix()
	sandbox := &fakeSandbox{
		introspect: tierSchema(),
		execResps:  []ExecuteResponse{{Value: map[string]any{"avg(revenue)": 10.0}}},
	}
	srv := newTestServer(goals, audits, &fakeObjects{}, &fakeHeur{}, claude, sandbox)

	body, contentType := multipartBody(t, map[string]string{"goal": "do gold-tier accounts spend more?"},
		"file", "data.csv", "revenue,tier\n10,gold\n")
	req := httptest.NewRequest(http.MethodPost, "/goals", body)
	req.Header.Set("Content-Type", contentType)
	rec := httptest.NewRecorder()
	srv.Routes().ServeHTTP(rec, req)
	return rec
}

func goldClaim() *domain.ClaimSpec {
	return &domain.ClaimSpec{
		Filters:   []domain.Constraint{{Field: "tier", Op: domain.Equal, Operand: strLiteral("gold")}},
		Direction: domain.Maximize,
	}
}

// TestSubmitGoalRecordsTrack: a classified goal persists its track, and a verify-track
// goal persists the validated claim as the canonical JSON the Verifier later decodes.
func TestSubmitGoalRecordsTrack(t *testing.T) {
	t.Run("explore", func(t *testing.T) {
		goals := &fakeGoals{}
		claude := &fakeClaude{intent: llm.GoalIntentResult{Track: llm.TrackExplore, Rationale: "no claim asserted"}}

		if code := submitTabularGoal(t, claude, goals, &fakeAudits{}).Code; code != http.StatusCreated {
			t.Fatalf("status = %d, want 201", code)
		}
		if goals.inserted.Track != store.TrackExplore || len(goals.inserted.Claim) != 0 {
			t.Fatalf("explore goal persisted wrong: track=%q claim=%s", goals.inserted.Track, goals.inserted.Claim)
		}
		if goals.inserted.ClaimError != "" {
			t.Fatalf("an explore goal has no claim to have failed: %q", goals.inserted.ClaimError)
		}
	})

	t.Run("verify", func(t *testing.T) {
		goals := &fakeGoals{}
		claude := &fakeClaude{intent: llm.GoalIntentResult{Track: llm.TrackVerify, Claim: goldClaim()}}

		if code := submitTabularGoal(t, claude, goals, &fakeAudits{}).Code; code != http.StatusCreated {
			t.Fatalf("status = %d, want 201", code)
		}
		if goals.inserted.Track != store.TrackVerify {
			t.Fatalf("track = %q, want verify", goals.inserted.Track)
		}
		var stored domain.ClaimSpec
		if err := json.Unmarshal(goals.inserted.Claim, &stored); err != nil {
			t.Fatalf("stored claim does not decode: %v (%s)", err, goals.inserted.Claim)
		}
		if len(stored.Filters) != 1 || stored.Filters[0].Field != "tier" || stored.Direction != domain.Maximize {
			t.Fatalf("stored claim = %+v", stored)
		}
	})

	t.Run("the classifier sees the introspected schema", func(t *testing.T) {
		claude := &fakeClaude{}
		submitTabularGoal(t, claude, &fakeGoals{}, &fakeAudits{})
		if claude.intentCalls != 1 {
			t.Fatalf("classifier called %d times, want once", claude.intentCalls)
		}
		if len(claude.gotIntentSchema.Columns) != 2 {
			t.Fatalf("classifier was not grounded on the introspected columns: %+v", claude.gotIntentSchema)
		}
	})
}

// TestSubmitGoalCannotConstructClaim: a claim naming a column or value the data source
// does not have registers on the verify track with no claim and the validation reason
// recorded. That reason is the analyst-facing surface, and it must say the claim could
// not be built — never that it was tested and found unsupported.
func TestSubmitGoalCannotConstructClaim(t *testing.T) {
	cases := map[string]*domain.ClaimSpec{
		"unknown column": {
			Filters:   []domain.Constraint{{Field: "region", Op: domain.Equal, Operand: strLiteral("emea")}},
			Direction: domain.Maximize,
		},
		"unknown value": {
			Filters:   []domain.Constraint{{Field: "tier", Op: domain.Equal, Operand: strLiteral("platinum")}},
			Direction: domain.Maximize,
		},
	}
	for name, claim := range cases {
		t.Run(name, func(t *testing.T) {
			goals := &fakeGoals{}
			audits := &fakeAudits{}
			claude := &fakeClaude{intent: llm.GoalIntentResult{Track: llm.TrackVerify, Claim: claim}}

			if code := submitTabularGoal(t, claude, goals, audits).Code; code != http.StatusCreated {
				t.Fatalf("status = %d, want 201 -- an unconstructable claim never blocks registration", code)
			}
			if goals.inserted.Track != store.TrackVerify {
				t.Fatalf("track = %q, want verify", goals.inserted.Track)
			}
			if len(goals.inserted.Claim) != 0 {
				t.Fatalf("an unvalidated claim must not be persisted: %s", goals.inserted.Claim)
			}
			if goals.inserted.ClaimError == "" {
				t.Fatalf("the cannot-construct reason must be recorded on the goal")
			}
			if !hasAudit(audits, "goal_claim_construction_failed") {
				t.Fatalf("audit records = %+v", audits.records())
			}
		})
	}
}

// TestSubmitGoalClassificationFailsOpen: a classifier fault registers the goal on the
// explore track with a trail, rather than blocking registration. The classifier is an
// accelerator; a goal that cannot be classified is still worth running.
func TestSubmitGoalClassificationFailsOpen(t *testing.T) {
	goals := &fakeGoals{}
	audits := &fakeAudits{}
	claude := &fakeClaude{intentErr: errors.New("upstream down")}

	if code := submitTabularGoal(t, claude, goals, audits).Code; code != http.StatusCreated {
		t.Fatalf("status = %d, want 201 -- a classifier fault must not block registration", code)
	}
	if goals.inserted.Track != store.TrackExplore {
		t.Fatalf("track = %q, want the explore fallback", goals.inserted.Track)
	}
	if !hasAudit(audits, "goal_intent_classification_failed") {
		t.Fatalf("audit records = %+v", audits.records())
	}
}

// TestSubmitDocumentGoalIsExploreTrack: a document goal never runs classification --
// its objective is a set of fields to extract, not a segment to optimize -- so it must
// still persist a usable track rather than an empty one.
func TestSubmitDocumentGoalIsExploreTrack(t *testing.T) {
	goals := &fakeGoals{}
	rec := submitWithReview(t, documentIntakeServer(goals), map[string]string{})
	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201", rec.Code)
	}
	if goals.inserted.Track != "" {
		t.Fatalf("track = %q, want the store's own default to apply", goals.inserted.Track)
	}
}
