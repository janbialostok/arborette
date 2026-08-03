package orchestrator

import (
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
	if len(audits.records) != 1 {
		t.Fatalf("expected one audit record, got %d", len(audits.records))
	}
	got := audits.records[0]
	if got.Action != "verification_outcome" || got.EventType != "causal_verification" || got.Actor != "analyst-test" {
		t.Fatalf("verification event not audited as expected: %+v", got)
	}
}
