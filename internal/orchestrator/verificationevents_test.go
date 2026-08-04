package orchestrator

import (
	"errors"
	"net/http"
	"testing"
)

func TestHandleVerificationEvent(t *testing.T) {
	audits := &fakeAudits{}
	srv := newTestServer(&fakeGoals{}, audits, &fakeObjects{}, &fakeHeur{}, &fakeClaude{}, &fakeSandbox{})

	if rec := serve(srv, http.MethodPost, "/internal/verification-events", "application/json", "{nope"); rec.Code != http.StatusBadRequest {
		t.Fatalf("bad json status = %d, want 400", rec.Code)
	}
	if rec := serve(srv, http.MethodPost, "/internal/verification-events", "application/json", `{"event":{"type":"x"}}`); rec.Code != http.StatusBadRequest {
		t.Fatalf("missing goal_id status = %d, want 400", rec.Code)
	}

	rec := serve(srv, http.MethodPost, "/internal/verification-events", "application/json",
		`{"goal_id":"g1","event":{"type":"verification_outcome","status":"causally_verified"}}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("valid event status = %d, want 201", rec.Code)
	}
	if len(audits.records()) != 1 {
		t.Fatalf("expected one audit record, got %d", len(audits.records()))
	}
	got := audits.records()[0]
	if got.Action != "verification_outcome" || got.EventType != "causal_verification" || got.Actor != "analyst-test" {
		t.Fatalf("verification event not audited as expected: %+v", got)
	}
}

// TestHandleVerificationEventRecordsClaimError: the Verifier's cannot-construct report
// is the one event that also changes durable goal state — registration validated the
// claim, and a re-validation failure at dispatch time has no run to fail, so the
// reason lands on the goal row. A write failure still delivers the event: the audit
// append is the durable trail regardless.
func TestHandleVerificationEventRecordsClaimError(t *testing.T) {
	const reason = "the claim names filter columns that are not in the data source: region"

	t.Run("records the reason", func(t *testing.T) {
		goals := &fakeGoals{}
		srv := newTestServer(goals, &fakeAudits{}, &fakeObjects{}, &fakeHeur{}, &fakeClaude{}, &fakeSandbox{})

		rec := serve(srv, http.MethodPost, "/internal/verification-events", "application/json",
			`{"goal_id":"g1","event":{"type":"claim_construction_failed","reason":"`+reason+`"}}`)
		if rec.Code != http.StatusCreated {
			t.Fatalf("status = %d, want 201", rec.Code)
		}
		if len(goals.claimErrors) != 1 || goals.claimErrors[0] != reason {
			t.Fatalf("claim errors = %v, want the reported reason", goals.claimErrors)
		}
	})

	t.Run("a write failure still delivers the event", func(t *testing.T) {
		audits := &fakeAudits{}
		goals := &fakeGoals{claimErrorErr: errors.New("db down")}
		srv := newTestServer(goals, audits, &fakeObjects{}, &fakeHeur{}, &fakeClaude{}, &fakeSandbox{})

		rec := serve(srv, http.MethodPost, "/internal/verification-events", "application/json",
			`{"goal_id":"g1","event":{"type":"claim_construction_failed","reason":"`+reason+`"}}`)
		if rec.Code != http.StatusCreated {
			t.Fatalf("status = %d, want 201 despite the goal-write failure", rec.Code)
		}
		if len(audits.records()) != 1 || audits.records()[0].Action != "claim_construction_failed" {
			t.Fatalf("audit records = %+v", audits.records())
		}
	})

	t.Run("other events leave the goal untouched", func(t *testing.T) {
		goals := &fakeGoals{}
		srv := newTestServer(goals, &fakeAudits{}, &fakeObjects{}, &fakeHeur{}, &fakeClaude{}, &fakeSandbox{})

		serve(srv, http.MethodPost, "/internal/verification-events", "application/json",
			`{"goal_id":"g1","event":{"type":"verification_outcome","status":"confounded"}}`)
		if len(goals.claimErrors) != 0 {
			t.Fatalf("an ordinary verification event must not write a claim error: %v", goals.claimErrors)
		}
	})
}
