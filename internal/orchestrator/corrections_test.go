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

	"github.com/arborette/arborette/internal/domain"
	"github.com/arborette/arborette/internal/graph"
	"github.com/arborette/arborette/internal/store"
)

func postCorrection(srv *Server, body string) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/goals/goal-1/causal-graph/corrections", strings.NewReader(body))
	srv.Routes().ServeHTTP(rec, req)
	return rec
}

// completedVerification is a terminal record whose adjustment set names the given
// column, so a correction on that column marks it stale.
func completedVerification(id, interventionID, adjustColumn string) store.CausalVerification {
	return store.CausalVerification{
		ID: id, GoalID: "goal-1", InterventionID: interventionID, GraphVersion: 1,
		Status: store.CausalStatusCausallyVerified, AdjustmentSet: []string{adjustColumn},
	}
}

// TestCausalCorrectionAppliesAndReverifies: a correction is served as a new graph
// version under the advisory lock, and the verifications it invalidated are marked
// stale and re-dispatched — exempt from the autonomous budget, because domain
// knowledge arriving must be actionable even after automation has spent it.
func TestCausalCorrectionAppliesAndReverifies(t *testing.T) {
	repo := &fakeRepo{}
	launcher := &fakeLauncher{}
	lock := &fakeGraphLock{acquired: true}
	verifications := &fakeCausalVerifications{records: []store.CausalVerification{
		completedVerification("v-1", "i-1", "Z"),
		completedVerification("v-2", "i-2", "unrelated"),
	}}
	audits := &fakeAudits{}
	srv := testServer{
		repo: repo, goals: &fakeGoals{get: verifyGoal()}, audits: audits,
		causalVerifications: verifications, graphLock: lock, verifierJobs: launcher,
	}.build()

	rec := postCorrection(srv, `{"op":"flip","from":"Z","to":"Y"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body)
	}

	var out struct {
		GraphVersion int `json:"graph_version"`
		Redispatched int `json:"redispatched"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if out.GraphVersion != 1 || out.Redispatched != 1 {
		t.Fatalf("response = %+v, want the new version and one re-dispatch", out)
	}
	if len(repo.corrections) != 1 || repo.corrections[0].Op != domain.CorrectionFlip {
		t.Fatalf("corrections = %+v", repo.corrections)
	}
	if lock.calls != 1 || lock.released != 1 {
		t.Fatalf("correction must run under the advisory lock (calls=%d released=%d)", lock.calls, lock.released)
	}
	if len(verifications.markedCols) != 2 {
		t.Fatalf("marked columns = %v, want both endpoints", verifications.markedCols)
	}
	dispatches := launcher.dispatches()
	if len(dispatches) != 1 || dispatches[0]["intervention_id"] != "i-1" {
		t.Fatalf("dispatches = %v, want only the stale record's finding", dispatches)
	}
	if dispatches[0]["budgeted"] != "false" {
		t.Fatalf("correction-triggered re-verification must be budget-exempt: %v", dispatches[0])
	}
	if !hasAudit(audits, "causal_graph_corrected") {
		t.Fatalf("audit records = %+v", audits.records())
	}
}

// TestCausalCorrectionCapsRedispatch: a correction that invalidates more
// verifications than the cap allows re-dispatches only up to it, so one edit cannot
// flood the Verifier.
func TestCausalCorrectionCapsRedispatch(t *testing.T) {
	records := make([]store.CausalVerification, 0, 5)
	for _, id := range []string{"a", "b", "c", "d", "e"} {
		records = append(records, completedVerification("v-"+id, "i-"+id, "Z"))
	}
	launcher := &fakeLauncher{}
	router := defaultRouter()
	router.StaleReverifyCap = 2
	srv := testServer{
		repo: &fakeRepo{}, goals: &fakeGoals{get: verifyGoal()},
		causalVerifications: &fakeCausalVerifications{records: records},
		verifierJobs:        launcher, router: &router,
	}.build()

	if code := postCorrection(srv, `{"op":"delete","from":"Z","to":"Y"}`).Code; code != http.StatusOK {
		t.Fatalf("status = %d, want 200", code)
	}
	if n := len(launcher.dispatches()); n != 2 {
		t.Fatalf("re-dispatched %d, want the cap of 2", n)
	}
}

// TestCausalCorrectionSkipsPendingVerifications: a pending record is not re-dispatched
// — it completes against the version it was dispatched at, and re-dispatching it now
// would coalesce onto that same in-flight record rather than test the corrected graph.
func TestCausalCorrectionSkipsPendingVerifications(t *testing.T) {
	pending := completedVerification("v-1", "i-1", "Z")
	pending.Status = store.CausalStatusPending
	launcher := &fakeLauncher{}
	srv := testServer{
		repo: &fakeRepo{}, goals: &fakeGoals{get: verifyGoal()},
		causalVerifications: &fakeCausalVerifications{records: []store.CausalVerification{pending}},
		verifierJobs:        launcher,
	}.build()

	if code := postCorrection(srv, `{"op":"flip","from":"Z","to":"Y"}`).Code; code != http.StatusOK {
		t.Fatalf("status = %d, want 200", code)
	}
	if n := len(launcher.dispatches()); n != 0 {
		t.Fatalf("re-dispatched %d in-flight verifications, want none", n)
	}
}

// TestCausalCorrectionContention: a correction that cannot take the (goal,
// data-source) lock answers 409 rather than interleaving with the holder. Two
// interleaved corrections would each copy forward the version they read, silently
// dropping one analyst's edit.
func TestCausalCorrectionContention(t *testing.T) {
	repo := &fakeRepo{}
	srv := testServer{
		repo: repo, goals: &fakeGoals{get: verifyGoal()},
		graphLock: &fakeGraphLock{acquired: false},
	}.build()
	srv.correctionLockWait = 0

	if code := postCorrection(srv, `{"op":"flip","from":"Z","to":"Y"}`).Code; code != http.StatusConflict {
		t.Fatalf("status = %d, want 409", code)
	}
	if len(repo.corrections) != 0 {
		t.Fatalf("a contended correction must not touch the graph")
	}
}

// TestCausalCorrectionValidation: an unusable correction is analyst-fixable, and a
// column the graph does not have is a 422 rather than a masked fault.
func TestCausalCorrectionValidation(t *testing.T) {
	cases := []struct {
		name string
		body string
		repo *fakeRepo
		want int
	}{
		{"unknown op", `{"op":"reverse","from":"Z","to":"Y"}`, &fakeRepo{}, http.StatusBadRequest},
		{"missing endpoint", `{"op":"flip","from":"Z"}`, &fakeRepo{}, http.StatusBadRequest},
		{"self edge", `{"op":"flip","from":"Z","to":"z"}`, &fakeRepo{}, http.StatusBadRequest},
		{"bad direction", `{"op":"flip","from":"Z","to":"Y","direction":"sideways"}`, &fakeRepo{}, http.StatusBadRequest},
		{"invalid body", `{`, &fakeRepo{}, http.StatusBadRequest},
		{"unknown column", `{"op":"flip","from":"Z","to":"Y"}`,
			&fakeRepo{correctErr: graph.ErrUnknownCausalColumn}, http.StatusUnprocessableEntity},
		{"no graph yet", `{"op":"flip","from":"Z","to":"Y"}`,
			&fakeRepo{correctErr: graph.ErrNotFound}, http.StatusNotFound},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			srv := testServer{repo: c.repo, goals: &fakeGoals{get: verifyGoal()}}.build()
			if code := postCorrection(srv, c.body).Code; code != c.want {
				t.Fatalf("status = %d, want %d", code, c.want)
			}
		})
	}
}

// TestCausalCorrectionInvalidatesWithResolvedColumns: the correction is accepted
// case-insensitively, so the columns the graph resolved -- not the analyst's spelling
// -- are what invalidate verifications. Their adjustment sets hold the graph's own
// casing and the overlap test is byte-exact, so the analyst's spelling would mark
// nothing stale while the correction reported success.
func TestCausalCorrectionInvalidatesWithResolvedColumns(t *testing.T) {
	repo := &fakeRepo{resolvedColumns: []string{"CryoSleep", "Transported"}}
	verifications := &fakeCausalVerifications{records: []store.CausalVerification{
		completedVerification("v-1", "i-1", "CryoSleep"),
	}}
	launcher := &fakeLauncher{}
	srv := testServer{
		repo: repo, goals: &fakeGoals{get: verifyGoal()},
		causalVerifications: verifications, verifierJobs: launcher,
	}.build()

	if code := postCorrection(srv, `{"op":"flip","from":"cryosleep","to":"transported"}`).Code; code != http.StatusOK {
		t.Fatalf("status = %d, want 200", code)
	}
	if len(verifications.markedCols) != 2 || verifications.markedCols[0] != "CryoSleep" {
		t.Fatalf("marked with %v, want the graph's own spelling", verifications.markedCols)
	}
	if n := len(launcher.dispatches()); n != 1 {
		t.Fatalf("re-dispatched %d, want the invalidated record", n)
	}
}

// TestCausalCorrectionRejectsUnmatchedEdge: a flip or delete naming a pair with no
// edge is analyst-fixable, not a silent success -- committing a version for it would
// report an edit that never happened.
func TestCausalCorrectionRejectsUnmatchedEdge(t *testing.T) {
	launcher := &fakeLauncher{}
	srv := testServer{
		repo:  &fakeRepo{correctErr: graph.ErrNoSuchCausalEdge},
		goals: &fakeGoals{get: verifyGoal()}, verifierJobs: launcher,
	}.build()

	if code := postCorrection(srv, `{"op":"flip","from":"Z","to":"Y"}`).Code; code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422", code)
	}
	if n := len(launcher.dispatches()); n != 0 {
		t.Fatalf("a rejected correction must invalidate nothing, got %d dispatches", n)
	}
}

// TestCausalCorrectionSurvivesStoreFailures: marking stale or listing can fail without
// failing the correction -- the analyst's edit is already durable, and rejecting it
// because the Verifier's store is unreachable would lose the domain knowledge.
func TestCausalCorrectionSurvivesStoreFailures(t *testing.T) {
	launcher := &fakeLauncher{}
	srv := testServer{
		repo: &fakeRepo{}, goals: &fakeGoals{get: verifyGoal()},
		causalVerifications: &fakeCausalVerifications{markErr: errors.New("pgx: connection refused")},
		verifierJobs:        launcher,
	}.build()

	rec := postCorrection(srv, `{"op":"flip","from":"Z","to":"Y"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 -- the correction itself is durable", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), `"redispatched":0`) {
		t.Fatalf("body = %s, want redispatched 0", rec.Body)
	}
	if n := len(launcher.dispatches()); n != 0 {
		t.Fatalf("dispatched %d despite the marking failure", n)
	}
}

// TestCausalCorrectionLockFailure: an advisory-lock error is a masked fault, and the
// correction is not attempted -- an unserialized read-modify-write could drop an
// analyst's edit.
func TestCausalCorrectionLockFailure(t *testing.T) {
	repo := &fakeRepo{}
	srv := testServer{
		repo: repo, goals: &fakeGoals{get: verifyGoal()},
		graphLock: &fakeGraphLock{err: errors.New("pgx: connection refused")},
	}.build()

	if code := postCorrection(srv, `{"op":"flip","from":"Z","to":"Y"}`).Code; code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", code)
	}
	if len(repo.corrections) != 0 {
		t.Fatalf("the correction must not run without the lock")
	}
}

// TestCausalCorrectionRetriesTheLock: the lock is polled, not tried once — a
// correction arriving while a discovery sweep briefly holds it should succeed on a
// later poll rather than answering 409.
func TestCausalCorrectionRetriesTheLock(t *testing.T) {
	lock := &fakeGraphLock{acquireAfter: 3}
	repo := &fakeRepo{}
	srv := testServer{repo: repo, goals: &fakeGoals{get: verifyGoal()}, graphLock: lock}.build()
	srv.correctionLockPoll = time.Millisecond

	if code := postCorrection(srv, `{"op":"flip","from":"Z","to":"Y"}`).Code; code != http.StatusOK {
		t.Fatalf("status = %d, want 200 after the lock frees up", code)
	}
	if lock.calls < 3 {
		t.Fatalf("lock tried %d times, want the poll loop to retry", lock.calls)
	}
	if len(repo.corrections) != 1 {
		t.Fatalf("the correction must run once the lock is acquired")
	}
}

// TestCausalCorrectionSurvivesAClientHangup: everything after the graph write commits
// runs on a detached context. The endpoint takes no credential, so the audit record is
// the only attribution this class of write has, and the re-verification is the whole
// point of accepting the correction — neither may be lost because the analyst closed
// the tab while the fan-out was in flight.
func TestCausalCorrectionSurvivesAClientHangup(t *testing.T) {
	repo := &fakeRepo{}
	launcher := &fakeLauncher{}
	verifications := &fakeCausalVerifications{records: []store.CausalVerification{
		completedVerification("v-1", "i-1", "Z"),
	}}
	audits := &fakeAudits{}
	srv := testServer{
		repo: repo, goals: &fakeGoals{get: verifyGoal()}, audits: audits,
		causalVerifications: verifications, graphLock: &fakeGraphLock{acquired: true},
		verifierJobs: launcher,
	}.build()

	// A request whose context is already dead, as it is once the client hangs up.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/goals/goal-1/causal-graph/corrections",
		strings.NewReader(`{"op":"flip","from":"Z","to":"Y"}`)).WithContext(ctx)
	srv.Routes().ServeHTTP(rec, req)

	if len(repo.corrections) != 1 {
		t.Fatalf("corrections = %+v, want the graph write to have committed", repo.corrections)
	}
	if verifications.markedCalls != 1 {
		t.Fatalf("markedCalls = %d, want the invalidated verifications flagged", verifications.markedCalls)
	}
	if got := launcher.dispatches(); len(got) != 1 {
		t.Fatalf("dispatches = %v, want the stale verification re-dispatched", got)
	}
	if !hasAudit(audits, "causal_graph_corrected") {
		t.Fatalf("audit records = %+v, want the correction recorded", audits.records())
	}
}
