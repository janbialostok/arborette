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
	"github.com/arborette/arborette/internal/store"
)

// verifyGoal is a registered goal the router dispatches for.
func verifyGoal() store.Goal {
	return store.Goal{OptimizationFunctionID: "goal-1", DataSourceRef: "ref.csv", Track: store.TrackExplore}
}

func repoWithFinding(goalID, interventionID string) *fakeRepo {
	return &fakeRepo{interventionsByID: map[string]domain.Intervention{
		interventionID: {ID: interventionID, GoalID: goalID, Type: domain.InterventionQuery},
	}}
}

// TestVerifyFindingDispatches: an explicit verify launches the Verifier with the
// finding and is budget-exempt — an analyst request must never be refused because
// automation already spent the budget.
func TestVerifyFindingDispatches(t *testing.T) {
	launcher := &fakeLauncher{}
	audits := &fakeAudits{}
	srv := testServer{
		repo:         repoWithFinding("goal-1", "i-1"),
		goals:        &fakeGoals{get: verifyGoal()},
		audits:       audits,
		verifierJobs: launcher,
	}.build()

	rec := httptest.NewRecorder()
	srv.Routes().ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/goals/goal-1/findings/i-1/verify", nil))

	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202: %s", rec.Code, rec.Body)
	}
	if launcher.args["kind"] != verifierKindVerify || launcher.args["intervention_id"] != "i-1" ||
		launcher.args["datasource_ref"] != "ref.csv" {
		t.Fatalf("launcher args = %v", launcher.args)
	}
	if launcher.args["budgeted"] != "false" {
		t.Fatalf("budgeted = %q, want an exempt dispatch", launcher.args["budgeted"])
	}
	if len(audits.records()) != 1 || audits.records()[0].Action != "causal_verification_dispatched" {
		t.Fatalf("audit records = %+v", audits.records())
	}
}

// TestVerifyFindingNotFound: an unknown goal, an unknown finding, and a finding
// belonging to another goal are all 404 and dispatch nothing. The cross-goal case is
// the load-bearing one -- verifying it would adjust another goal's finding against
// this goal's causal graph and objective, producing a number about neither.
func TestVerifyFindingNotFound(t *testing.T) {
	cases := map[string]testServer{
		"unknown goal": {
			repo:  repoWithFinding("goal-1", "i-1"),
			goals: &fakeGoals{getErr: pgx.ErrNoRows},
		},
		"unknown finding": {
			repo:  &fakeRepo{interventionsByID: map[string]domain.Intervention{}},
			goals: &fakeGoals{get: verifyGoal()},
		},
		"another goal's finding": {
			repo:  repoWithFinding("other-goal", "i-1"),
			goals: &fakeGoals{get: verifyGoal()},
		},
	}
	for name, ts := range cases {
		t.Run(name, func(t *testing.T) {
			launcher := &fakeLauncher{}
			ts.verifierJobs = launcher
			rec := httptest.NewRecorder()
			ts.build().Routes().ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/goals/goal-1/findings/i-1/verify", nil))

			if rec.Code != http.StatusNotFound {
				t.Fatalf("status = %d, want 404", rec.Code)
			}
			if len(launcher.dispatches()) != 0 {
				t.Fatalf("dispatched despite a 404")
			}
		})
	}
}

// TestListCausalVerifications maps every field onto the snake_case wire shape, with
// the not-yet-measured effects reported as null rather than a misleading zero.
func TestListCausalVerifications(t *testing.T) {
	naive, adjusted, score := 0.4, 0.3, 0.9
	now := time.Now().UTC()
	records := []store.CausalVerification{
		{
			ID: "v-1", GoalID: "goal-1", InterventionID: "i-1", GraphVersion: 2,
			Status: store.CausalStatusCausallyVerified, NaiveEffect: &naive, AdjustedEffect: &adjusted,
			AdjustmentSet: []string{"Z"}, RefutationScore: &score, Confidence: &score,
			Stale: true, CreatedAt: now, UpdatedAt: now,
		},
		{ID: "v-2", GoalID: "goal-1", InterventionID: "i-2", GraphVersion: 2, Status: store.CausalStatusPending},
	}
	srv := testServer{
		goals:               &fakeGoals{get: verifyGoal()},
		causalVerifications: &fakeCausalVerifications{records: records},
	}.build()

	rec := httptest.NewRecorder()
	srv.Routes().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/goals/goal-1/causal-verifications", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body)
	}

	var out []causalVerificationDTO
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(out) != 2 {
		t.Fatalf("returned %d records, want 2", len(out))
	}
	first := out[0]
	if first.ID != "v-1" || first.InterventionID != "i-1" || first.GraphVersion != 2 ||
		first.Status != store.CausalStatusCausallyVerified || !first.Stale {
		t.Fatalf("record mapped wrong: %+v", first)
	}
	if first.NaiveEffect == nil || *first.NaiveEffect != naive ||
		first.AdjustedEffect == nil || *first.AdjustedEffect != adjusted ||
		first.RefutationScore == nil || first.Confidence == nil {
		t.Fatalf("measured effects lost: %+v", first)
	}
	if len(first.AdjustmentSet) != 1 || first.AdjustmentSet[0] != "Z" {
		t.Fatalf("adjustment set = %v", first.AdjustmentSet)
	}
	if pending := out[1]; pending.NaiveEffect != nil || pending.AdjustedEffect != nil {
		t.Fatalf("a pending record must report null effects, got %+v", pending)
	}
	// A nil adjustment set marshals as [] rather than null, so a consumer can iterate
	// it unconditionally.
	if out[1].AdjustmentSet == nil {
		t.Fatalf("nil adjustment set must marshal as an empty list")
	}
}

// TestVerifyFindingSurfacesCollaboratorFailures: a Verifier that is down is a 502
// (the answer an operator reads during an outage, distinct from the 404 an unknown
// finding gets), and a graph failure that is not "not found" is a masked 500.
func TestVerifyFindingSurfacesCollaboratorFailures(t *testing.T) {
	t.Run("verifier unavailable", func(t *testing.T) {
		srv := testServer{
			repo:         repoWithFinding("goal-1", "i-1"),
			goals:        &fakeGoals{get: verifyGoal()},
			verifierJobs: &fakeLauncher{err: errLaunch},
		}.build()

		rec := httptest.NewRecorder()
		srv.Routes().ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/goals/goal-1/findings/i-1/verify", nil))
		if rec.Code != http.StatusBadGateway {
			t.Fatalf("status = %d, want 502", rec.Code)
		}
	})

	t.Run("graph fault is masked", func(t *testing.T) {
		srv := testServer{
			repo:  &fakeRepo{interventionErr: errors.New("neo4j: connection refused")},
			goals: &fakeGoals{get: verifyGoal()},
		}.build()

		rec := httptest.NewRecorder()
		srv.Routes().ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/goals/goal-1/findings/i-1/verify", nil))
		if rec.Code != http.StatusInternalServerError {
			t.Fatalf("status = %d, want 500", rec.Code)
		}
		if strings.Contains(rec.Body.String(), "neo4j") {
			t.Fatalf("graph internals leaked to the caller: %s", rec.Body)
		}
	})

	t.Run("listing fault is masked", func(t *testing.T) {
		srv := testServer{
			goals:               &fakeGoals{get: verifyGoal()},
			causalVerifications: &fakeCausalVerifications{listErr: errors.New("pgx: connection refused")},
		}.build()

		rec := httptest.NewRecorder()
		srv.Routes().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/goals/goal-1/causal-verifications", nil))
		if rec.Code != http.StatusInternalServerError {
			t.Fatalf("status = %d, want 500", rec.Code)
		}
		if strings.Contains(rec.Body.String(), "pgx") {
			t.Fatalf("store internals leaked to the caller: %s", rec.Body)
		}
	})
}

// TestVerifyFindingRecordsTheDispatchAfterAHangup: the dispatch is irreversible once
// the launcher returns, so a client that hangs up while it is in flight must not cost
// the record of it — a verification would then be running and consuming the in-flight
// cap with nothing attributing it to anyone.
func TestVerifyFindingRecordsTheDispatchAfterAHangup(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	launcher := &fakeLauncher{onLaunch: cancel}
	audits := &fakeAudits{}
	srv := testServer{
		repo:         repoWithFinding("goal-1", "i-1"),
		goals:        &fakeGoals{get: verifyGoal()},
		audits:       audits,
		verifierJobs: launcher,
	}.build()

	rec := httptest.NewRecorder()
	srv.Routes().ServeHTTP(rec, httptest.NewRequest(
		http.MethodPost, "/goals/goal-1/findings/i-1/verify", nil).WithContext(ctx))

	if len(launcher.dispatches()) != 1 {
		t.Fatalf("dispatches = %v, want the verification launched", launcher.dispatches())
	}
	if len(audits.records()) != 1 || audits.records()[0].Action != "causal_verification_dispatched" {
		t.Fatalf("audit records = %+v, want the dispatch recorded despite the hangup", audits.records())
	}
}
