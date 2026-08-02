package orchestrator

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/arborette/arborette/internal/domain"
)

// windowBindingCols is the introspected schema parseWindowBindings resolves against:
// a categorical entity column, a numeric and a temporal orderable column, a
// non-orderable text column, and a case-colliding pair to exercise ambiguity.
func windowBindingCols() []columnDTO {
	return []columnDTO{
		{Name: "region", Type: "VARCHAR"},
		{Name: "order_value", Type: "DOUBLE"},
		{Name: "ts", Type: "TIMESTAMP"},
		{Name: "Dup", Type: "VARCHAR"},
		{Name: "dup", Type: "VARCHAR"},
	}
}

func windowBindingReq(entity, ts string) *http.Request {
	form := url.Values{}
	if entity != "" {
		form.Set("entity_key_column", entity)
	}
	if ts != "" {
		form.Set("time_column", ts)
	}
	// A non-nil Form makes FormValue read it directly rather than parse a body.
	return &http.Request{Form: form}
}

func TestParseWindowBindings(t *testing.T) {
	cols := windowBindingCols()

	t.Run("neither present is unbound, no error", func(t *testing.T) {
		e, ts, err := parseWindowBindings(windowBindingReq("", ""), cols)
		if err != nil || e != "" || ts != "" {
			t.Fatalf("unbound = (%q, %q, %v), want empty/empty/nil", e, ts, err)
		}
	})

	t.Run("only one binding is rejected", func(t *testing.T) {
		if _, _, err := parseWindowBindings(windowBindingReq("region", ""), cols); err == nil {
			t.Fatal("lone entity_key_column must be rejected")
		}
		if _, _, err := parseWindowBindings(windowBindingReq("", "order_value"), cols); err == nil {
			t.Fatal("lone time_column must be rejected")
		}
	})

	t.Run("valid bindings resolve to the actual column names", func(t *testing.T) {
		e, ts, err := parseWindowBindings(windowBindingReq("REGION", "order_value"), cols)
		if err != nil {
			t.Fatalf("valid bindings: %v", err)
		}
		if e != "region" || ts != "order_value" {
			t.Fatalf("bindings resolved to (%q, %q), want (region, order_value)", e, ts)
		}
	})

	t.Run("a temporal time column is orderable", func(t *testing.T) {
		if _, _, err := parseWindowBindings(windowBindingReq("region", "ts"), cols); err != nil {
			t.Fatalf("temporal time column must be orderable: %v", err)
		}
	})

	t.Run("unknown columns are rejected", func(t *testing.T) {
		if _, _, err := parseWindowBindings(windowBindingReq("ghost", "order_value"), cols); err == nil {
			t.Fatal("unknown entity column must be rejected")
		}
		if _, _, err := parseWindowBindings(windowBindingReq("region", "ghost"), cols); err == nil {
			t.Fatal("unknown time column must be rejected")
		}
	})

	t.Run("a case-colliding column is ambiguous, not a silent pick", func(t *testing.T) {
		if _, _, err := parseWindowBindings(windowBindingReq("dup", "order_value"), cols); err == nil {
			t.Fatal("ambiguous entity column must be rejected")
		}
	})

	t.Run("a non-orderable time column is rejected", func(t *testing.T) {
		if _, _, err := parseWindowBindings(windowBindingReq("order_value", "region"), cols); err == nil {
			t.Fatal("a VARCHAR time column is not orderable and must be rejected")
		}
	})
}

// postGoal drives a valid file-upload goal submission through the mux.
func postGoal(t *testing.T, srv *Server) *httptest.ResponseRecorder {
	t.Helper()
	body, ct := multipartBody(t, map[string]string{"goal": "grow revenue"}, "file", "data.csv", "revenue\n10\n")
	req := httptest.NewRequest(http.MethodPost, "/goals", body)
	req.Header.Set("Content-Type", ct)
	rec := httptest.NewRecorder()
	srv.Routes().ServeHTTP(rec, req)
	return rec
}

func fittedMatrix() domain.EvaluationMatrix {
	return domain.EvaluationMatrix{Targets: []domain.Target{{Field: "revenue", Direction: domain.Maximize, Aggregation: "avg"}}}
}

func revenueSchema() IntrospectResponse {
	return IntrospectResponse{Schema: schemaDTO{Columns: []columnDTO{{Name: "revenue", Type: "DOUBLE"}}}}
}

// tooDeepMatrix fits an objective whose value expression nests one past the cap —
// the shape the plain-string output schema does not bound, caught by the
// registration depth guard before any sandbox Execute.
func tooDeepMatrix() domain.EvaluationMatrix {
	expr := domain.Expression{Kind: domain.ColumnRefKind, Column: "revenue"}
	for i := 0; i <= domain.MaxObjectiveExpressionDepth; i++ {
		inner := expr
		expr = domain.Expression{Kind: domain.CastKind, CastType: "DOUBLE", Operand: &inner}
	}
	return domain.EvaluationMatrix{Targets: []domain.Target{{Field: "revenue", Direction: domain.Maximize, Aggregation: "avg", Value: &expr}}}
}

func TestSubmitGoalIntrospectionFailureMapping(t *testing.T) {
	cases := []struct {
		name       string
		err        error
		wantStatus int
	}{
		{"unreadable source is 400", &SandboxError{Status: http.StatusBadRequest, Message: "unsupported data source"}, http.StatusBadRequest},
		{"missing source is 404", &SandboxError{Status: http.StatusNotFound, Message: "data source not found"}, http.StatusNotFound},
		{"sandbox fault is 502", &SandboxError{Status: http.StatusInternalServerError, Message: "internal error"}, http.StatusBadGateway},
		{"transport error is 502", errors.New("connection refused"), http.StatusBadGateway},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			claude := &fakeClaude{matrix: fittedMatrix()}
			sandbox := &fakeSandbox{introspectErr: c.err}
			srv := newTestServer(&fakeGoals{}, &fakeAudits{}, &fakeObjects{}, &fakeHeur{}, claude, sandbox)
			rec := postGoal(t, srv)
			if rec.Code != c.wantStatus {
				t.Fatalf("status = %d, want %d (body %q)", rec.Code, c.wantStatus, rec.Body.String())
			}
			// Matrix generation must not run when the precondition fails.
			if claude.gotSchema.Columns != nil {
				t.Fatalf("matrix generation ran despite an introspection failure")
			}
		})
	}
}

func TestSubmitGoalDryRunRepairThenSuccess(t *testing.T) {
	goals := &fakeGoals{}
	claude := &fakeClaude{matrix: fittedMatrix(), repair: fittedMatrix()}
	sandbox := &fakeSandbox{
		introspect: revenueSchema(),
		execErrs:   []error{&SandboxError{Status: http.StatusBadRequest, Message: "numeric aggregation over non-numeric column"}},
		execResps:  []ExecuteResponse{{}, {Value: map[string]any{"avg(revenue)": 10.0}}},
	}
	srv := newTestServer(goals, &fakeAudits{}, &fakeObjects{}, &fakeHeur{}, claude, sandbox)

	rec := postGoal(t, srv)
	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201 (body %q)", rec.Code, rec.Body.String())
	}
	if claude.repairCalls != 1 {
		t.Fatalf("expected exactly one repair, got %d", claude.repairCalls)
	}
	if goals.inserted == nil {
		t.Fatal("goal should be persisted after a repaired objective validates")
	}
}

// TestSubmitGoalRegistersRefBeforeSandbox proves the minted ref is registered with
// the same ref the goal persists -- the ordering the sandbox ref-scoping depends on,
// since the intake introspect/dry-run validate against the registry.
func TestSubmitGoalRegistersRefBeforeSandbox(t *testing.T) {
	goals := &fakeGoals{}
	claude := &fakeClaude{matrix: fittedMatrix()}
	sandbox := &fakeSandbox{introspect: revenueSchema(), execResps: []ExecuteResponse{{Value: map[string]any{"avg(revenue)": 10.0}}}}
	srv := newTestServer(goals, &fakeAudits{}, &fakeObjects{}, &fakeHeur{}, claude, sandbox)

	rec := postGoal(t, srv)
	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201 (body %q)", rec.Code, rec.Body.String())
	}
	if goals.inserted == nil || goals.registeredRef == "" || goals.registeredRef != goals.inserted.DataSourceRef {
		t.Fatalf("ref must be registered with the ref the goal persists: registered=%q goal=%+v", goals.registeredRef, goals.inserted)
	}
}

// TestSubmitGoalRegisterRefErrorIsInternalError proves a registry write failure is a
// masked 500 that never reaches the sandbox and never persists the goal.
func TestSubmitGoalRegisterRefErrorIsInternalError(t *testing.T) {
	goals := &fakeGoals{registerErr: errors.New("db down")}
	claude := &fakeClaude{matrix: fittedMatrix()}
	sandbox := &fakeSandbox{introspect: revenueSchema()}
	srv := newTestServer(goals, &fakeAudits{}, &fakeObjects{}, &fakeHeur{}, claude, sandbox)

	rec := postGoal(t, srv)
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500 (body %q)", rec.Code, rec.Body.String())
	}
	if sandbox.introspectCalls != 0 {
		t.Fatalf("sandbox introspected despite a failed ref registration (%d calls)", sandbox.introspectCalls)
	}
	if goals.inserted != nil {
		t.Fatal("goal must not persist when ref registration fails")
	}
}

func TestSubmitGoalRepairsExhaustedIsUnprocessable(t *testing.T) {
	goals := &fakeGoals{}
	claude := &fakeClaude{matrix: fittedMatrix(), repair: fittedMatrix()}
	// The initial dry-run plus one after each of the three repairs must all 400 for
	// the loop to exhaust still-failing — four scripted 400s (K repairs → K+1 dry-runs).
	sandbox := &fakeSandbox{
		introspect: revenueSchema(),
		execErrs: []error{
			&SandboxError{Status: http.StatusBadRequest, Message: "type-incompatible objective expression"},
			&SandboxError{Status: http.StatusBadRequest, Message: "type-incompatible objective expression"},
			&SandboxError{Status: http.StatusBadRequest, Message: "type-incompatible objective expression"},
			&SandboxError{Status: http.StatusBadRequest, Message: "type-incompatible objective expression"},
		},
	}
	srv := newTestServer(goals, &fakeAudits{}, &fakeObjects{}, &fakeHeur{}, claude, sandbox)

	rec := postGoal(t, srv)
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422 (body %q)", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "type-incompatible objective expression") {
		t.Fatalf("422 body should name what could not be resolved: %q", rec.Body.String())
	}
	if claude.repairCalls != maxObjectiveRepairs {
		t.Fatalf("expected %d repair attempts before giving up, got %d", maxObjectiveRepairs, claude.repairCalls)
	}
	if goals.inserted != nil {
		t.Fatal("an unfittable objective must not be persisted")
	}
}

func TestSubmitGoalRepairErrorIsBadGateway(t *testing.T) {
	goals := &fakeGoals{}
	claude := &fakeClaude{matrix: fittedMatrix(), repairErr: errors.New("claude down")}
	sandbox := &fakeSandbox{
		introspect: revenueSchema(),
		execErrs:   []error{&SandboxError{Status: http.StatusBadRequest, Message: "type-incompatible objective expression"}},
	}
	srv := newTestServer(goals, &fakeAudits{}, &fakeObjects{}, &fakeHeur{}, claude, sandbox)

	rec := postGoal(t, srv)
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502 (body %q)", rec.Code, rec.Body.String())
	}
	if claude.repairCalls != 1 {
		t.Fatalf("expected one repair attempt, got %d", claude.repairCalls)
	}
	if goals.inserted != nil {
		t.Fatal("a failed repair call must not persist the goal")
	}
}

func TestSubmitGoalTooDeepObjectiveRepairsThenSuccess(t *testing.T) {
	goals := &fakeGoals{}
	// The first fitted matrix nests past the depth cap, so the dry-run fails at the
	// depth guard (before any Execute) and is routed to repair; the repaired
	// within-cap matrix then measures and persists.
	claude := &fakeClaude{matrix: tooDeepMatrix(), repair: fittedMatrix()}
	sandbox := &fakeSandbox{
		introspect: revenueSchema(),
		execResps:  []ExecuteResponse{{Value: map[string]any{"avg(revenue)": 10.0}}},
	}
	srv := newTestServer(goals, &fakeAudits{}, &fakeObjects{}, &fakeHeur{}, claude, sandbox)

	rec := postGoal(t, srv)
	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201 (body %q)", rec.Code, rec.Body.String())
	}
	if claude.repairCalls != 1 {
		t.Fatalf("expected exactly one repair, got %d", claude.repairCalls)
	}
	// The depth guard short-circuits before the first Execute; only the post-repair
	// dry-run runs.
	if sandbox.execCalls != 1 {
		t.Fatalf("expected exactly one execute (post-repair dry-run), got %d", sandbox.execCalls)
	}
	if goals.inserted == nil {
		t.Fatal("goal should be persisted after a repaired objective validates")
	}
}

func TestSubmitGoalAlwaysTooDeepIsUnprocessable(t *testing.T) {
	goals := &fakeGoals{}
	// Both the initial and every repaired matrix nest past the cap, so the depth
	// guard exhausts the repair loop and the goal is 422 — never a sandbox fault,
	// and never dispatched to Execute.
	claude := &fakeClaude{matrix: tooDeepMatrix(), repair: tooDeepMatrix()}
	sandbox := &fakeSandbox{introspect: revenueSchema()}
	srv := newTestServer(goals, &fakeAudits{}, &fakeObjects{}, &fakeHeur{}, claude, sandbox)

	rec := postGoal(t, srv)
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422 (body %q)", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "nests too deeply") {
		t.Fatalf("422 body should name the depth failure: %q", rec.Body.String())
	}
	if claude.repairCalls != maxObjectiveRepairs {
		t.Fatalf("expected %d repair attempts before giving up, got %d", maxObjectiveRepairs, claude.repairCalls)
	}
	if sandbox.execCalls != 0 {
		t.Fatalf("a too-deep objective must never reach the sandbox, got %d executes", sandbox.execCalls)
	}
	if goals.inserted != nil {
		t.Fatal("an unfittable objective must not be persisted")
	}
}

func TestSubmitGoalMissingAggregationRepairs(t *testing.T) {
	goals := &fakeGoals{}
	// The first fitted matrix lacks an aggregation, so the dry-run fails at pin
	// time (before any Execute) and is routed to repair, not a fault.
	noAgg := domain.EvaluationMatrix{Targets: []domain.Target{{Field: "revenue", Direction: domain.Maximize}}}
	claude := &fakeClaude{matrix: noAgg, repair: fittedMatrix()}
	sandbox := &fakeSandbox{
		introspect: revenueSchema(),
		execResps:  []ExecuteResponse{{Value: map[string]any{"avg(revenue)": 10.0}}},
	}
	srv := newTestServer(goals, &fakeAudits{}, &fakeObjects{}, &fakeHeur{}, claude, sandbox)

	rec := postGoal(t, srv)
	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201 (body %q)", rec.Code, rec.Body.String())
	}
	if claude.repairCalls != 1 {
		t.Fatalf("expected exactly one repair, got %d", claude.repairCalls)
	}
	// The pin failure short-circuits before the first Execute; only the post-repair
	// dry-run runs.
	if sandbox.execCalls != 1 {
		t.Fatalf("expected exactly one execute (post-repair dry-run), got %d", sandbox.execCalls)
	}
	if goals.inserted == nil {
		t.Fatal("goal should be persisted after a repaired objective validates")
	}
}

func TestSubmitGoalPostRepairFaultIsBadGateway(t *testing.T) {
	goals := &fakeGoals{}
	claude := &fakeClaude{matrix: fittedMatrix(), repair: fittedMatrix()}
	sandbox := &fakeSandbox{
		introspect: revenueSchema(),
		execErrs: []error{
			&SandboxError{Status: http.StatusBadRequest, Message: "type-incompatible objective expression"},
			&SandboxError{Status: http.StatusInternalServerError, Message: "internal error"},
		},
	}
	srv := newTestServer(goals, &fakeAudits{}, &fakeObjects{}, &fakeHeur{}, claude, sandbox)

	rec := postGoal(t, srv)
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502 (body %q)", rec.Code, rec.Body.String())
	}
	if claude.repairCalls != 1 {
		t.Fatalf("expected one repair attempt, got %d", claude.repairCalls)
	}
	if goals.inserted != nil {
		t.Fatal("a post-repair sandbox fault must not persist the goal")
	}
}

func TestSubmitGoalDryRunSandboxFaultDoesNotRepair(t *testing.T) {
	goals := &fakeGoals{}
	claude := &fakeClaude{matrix: fittedMatrix(), repair: fittedMatrix()}
	sandbox := &fakeSandbox{
		introspect: revenueSchema(),
		execErrs:   []error{&SandboxError{Status: http.StatusInternalServerError, Message: "internal error"}},
	}
	srv := newTestServer(goals, &fakeAudits{}, &fakeObjects{}, &fakeHeur{}, claude, sandbox)

	rec := postGoal(t, srv)
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502 (body %q)", rec.Code, rec.Body.String())
	}
	if claude.repairCalls != 0 {
		t.Fatalf("a sandbox fault must not trigger repair, got %d calls", claude.repairCalls)
	}
	if goals.inserted != nil {
		t.Fatal("a sandbox fault must not persist the goal")
	}
}
