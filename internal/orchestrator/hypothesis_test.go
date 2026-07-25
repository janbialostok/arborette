package orchestrator

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/arborette/arborette/internal/domain"
	"github.com/arborette/arborette/internal/llm"
	"github.com/arborette/arborette/internal/store"
)

func TestPinObjective(t *testing.T) {
	t.Run("pins the first target's aggregation, expression, and direction", func(t *testing.T) {
		matrix := domain.EvaluationMatrix{Targets: []domain.Target{
			{Field: "revenue", Direction: domain.Maximize, Aggregation: "sum"},
			{Field: "cost", Direction: domain.Minimize, Aggregation: "avg"},
		}}
		obj, err := pinObjective(matrix)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if obj.aggregation != "sum" || obj.direction != domain.Maximize {
			t.Fatalf("unexpected aggregation/direction: %+v", obj)
		}
		if obj.expr.Kind != domain.ColumnRefKind || obj.expr.Column != "revenue" {
			t.Fatalf("field-only target should degenerate to a bare ColumnRef: %+v", obj.expr)
		}
		if obj.label != "sum(revenue)" {
			t.Fatalf("label = %q, want sum(revenue)", obj.label)
		}
	})

	t.Run("explicit value expression is pinned and labeled", func(t *testing.T) {
		expr := domain.Expression{Kind: domain.ComparisonKind, Op: "=",
			Left:  &domain.Expression{Kind: domain.ColumnRefKind, Column: "Transported"},
			Right: &domain.Expression{Kind: domain.LiteralKind, Literal: &domain.LiteralValue{Bool: boolPtr(true)}},
		}
		matrix := domain.EvaluationMatrix{Targets: []domain.Target{
			{Direction: domain.Maximize, Aggregation: "avg", Value: &expr},
		}}
		obj, err := pinObjective(matrix)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if obj.expr.Kind != domain.ComparisonKind || obj.label != "avg(Transported = True)" {
			t.Fatalf("compound objective not pinned/labeled: %+v (label %q)", obj.expr, obj.label)
		}
	})

	t.Run("no targets is a terminal failure", func(t *testing.T) {
		if _, err := pinObjective(domain.EvaluationMatrix{}); !errors.Is(err, errNoObjective) {
			t.Fatalf("error = %v, want errNoObjective", err)
		}
	})

	t.Run("legacy field-only target with no aggregation is terminal", func(t *testing.T) {
		matrix := domain.EvaluationMatrix{Targets: []domain.Target{{Field: "revenue", Direction: domain.Maximize}}}
		if _, err := pinObjective(matrix); !errors.Is(err, errMissingAggregation) {
			t.Fatalf("error = %v, want errMissingAggregation", err)
		}
	})
}

func boolPtr(b bool) *bool { return &b }

func TestExecuteRequestFor(t *testing.T) {
	obj := objective{
		aggregation: "avg",
		expr:        domain.Expression{Kind: domain.ColumnRefKind, Column: "revenue"},
		label:       "avg(revenue)",
		direction:   domain.Maximize,
	}
	filters := []domain.Constraint{{Field: "region", Op: domain.GreaterThanOrEqual, Value: 1}}
	req := executeRequestFor(store.Goal{DataSourceRef: "ref"}, obj, filters)

	if req.ValueExpression == nil || req.ValueExpression.Column != "revenue" {
		t.Fatalf("value expression not carried: %+v", req.ValueExpression)
	}
	if req.ObjectiveLabel != "avg(revenue)" {
		t.Fatalf("objective label = %q, want avg(revenue)", req.ObjectiveLabel)
	}
	// The value expression carries the objective; Target.Field must stay empty so
	// the sandbox measures the expression, not a bare column.
	if req.Target.Field != "" {
		t.Fatalf("target field must be empty: %+v", req.Target)
	}
	if req.Aggregation != "avg" || req.Target.Direction != domain.Maximize || req.DataSourceRef != "ref" || len(req.Filters) != 1 {
		t.Fatalf("unexpected request: %+v", req)
	}
}

func TestObjectiveField(t *testing.T) {
	bare := objective{expr: domain.Expression{Kind: domain.ColumnRefKind, Column: "revenue"}}
	if objectiveField(bare) != "revenue" {
		t.Fatalf("bare ColumnRef field = %q, want revenue", objectiveField(bare))
	}
	// A compound expression has no single column, so the on-objective-field
	// constraint check is a no-op.
	compound := objective{expr: domain.Expression{Kind: domain.ComparisonKind}}
	if objectiveField(compound) != "" {
		t.Fatalf("compound objective field = %q, want empty", objectiveField(compound))
	}
}

func TestRunLoopSuccessKeysByObjectiveLabel(t *testing.T) {
	repo := &fakeRepo{}
	// One root candidate that does not improve, so the loop writes exactly one
	// triplet and stops without proposing children.
	claude := &fakeClaude{proposal: llm.Proposal{Candidates: []llm.CandidateIntervention{{Filters: nil}}}}
	sandbox := &fakeSandbox{
		introspect: IntrospectResponse{Schema: schemaDTO{Columns: []columnDTO{{Name: "revenue", Type: "DOUBLE"}}}},
		execResps: []ExecuteResponse{
			{Value: map[string]any{"avg(revenue)": 10.0}}, // root baseline
			{Value: map[string]any{"avg(revenue)": 8.0}},  // candidate (no improvement → stop)
		},
	}
	srv := newTestServerRepo(repo, &fakeGoals{}, &fakeAudits{}, &fakeObjects{}, &fakeHeur{}, claude, sandbox)

	goal := store.Goal{OptimizationFunctionID: "g1", DataSourceRef: "ref",
		EvaluationMatrix: domain.EvaluationMatrix{Targets: []domain.Target{{Field: "revenue", Direction: domain.Maximize, Aggregation: "avg"}}}}
	srv.runLoop(context.Background(), goal, "run-1")

	// The measured value is read and persisted under the rendered objective label
	// (the same key the execute request carries), not a bare column field.
	if len(repo.outcomes) != 1 {
		t.Fatalf("expected one outcome triplet, got %d", len(repo.outcomes))
	}
	if v, ok := repo.outcomes[0].Value["avg(revenue)"]; !ok || v != 8.0 {
		t.Fatalf("Outcome.Value must be keyed by the objective label: %+v", repo.outcomes[0].Value)
	}
	if len(repo.states) != 1 || repo.states[0].Properties["objective_label"] != "avg(revenue)" {
		t.Fatalf("state must carry objective_label: %+v", repo.states)
	}
	// The Phase-1 finding is observational; the PRODUCED edge records that provenance.
	if len(repo.produced) != 1 || repo.produced[0].EpistemicSource != domain.EpistemicObservational {
		t.Fatalf("PRODUCED edge must carry observational epistemic source: %+v", repo.produced)
	}
}

func TestRunLoopTripletPayloadCarriesSegmentAndDirection(t *testing.T) {
	repo := &fakeRepo{}
	claude := &fakeClaude{proposal: llm.Proposal{Candidates: []llm.CandidateIntervention{
		{Filters: []domain.Constraint{{Field: "CryoSleep", Op: domain.Equal, Operand: &domain.LiteralValue{Bool: boolPtr(true)}}}},
	}}}
	sandbox := &fakeSandbox{
		introspect: IntrospectResponse{Schema: schemaDTO{Columns: []columnDTO{
			{Name: "Transported", Type: "BOOLEAN"}, {Name: "CryoSleep", Type: "BOOLEAN"}}}},
		execResps: []ExecuteResponse{
			{Value: map[string]any{"avg(Transported = True)": 0.5}}, // root baseline
			{Value: map[string]any{"avg(Transported = True)": 0.4}}, // candidate (no improvement → stop)
		},
	}
	srv := newTestServerRepo(repo, &fakeGoals{}, &fakeAudits{}, &fakeObjects{}, &fakeHeur{}, claude, sandbox)

	valueExpr := domain.Expression{Kind: domain.ComparisonKind, Op: "=",
		Left:  &domain.Expression{Kind: domain.ColumnRefKind, Column: "Transported"},
		Right: &domain.Expression{Kind: domain.LiteralKind, Literal: &domain.LiteralValue{Bool: boolPtr(true)}}}
	goal := store.Goal{OptimizationFunctionID: "g1", DataSourceRef: "ref",
		EvaluationMatrix: domain.EvaluationMatrix{Targets: []domain.Target{
			{Field: "Transported", Direction: domain.Maximize, Aggregation: "avg", Value: &valueExpr}}}}

	// Subscribe before the run so the triplet event is captured; runLoop publishes and
	// Completes synchronously, which closes the channel and ends the range.
	_, ch, cancel := srv.hub.Subscribe("g1")
	defer cancel()
	srv.runLoop(context.Background(), goal, "run-1")

	var triplet map[string]any
	for ev := range ch {
		if ev.Type == "triplet" {
			triplet = ev.Payload
		}
	}
	if triplet == nil {
		t.Fatal("no triplet event was published")
	}
	if triplet["objective_label"] != "avg(Transported = True)" {
		t.Fatalf("triplet objective_label = %v, want avg(Transported = True)", triplet["objective_label"])
	}
	if triplet["direction"] != "maximize" {
		t.Fatalf("triplet direction = %v, want maximize", triplet["direction"])
	}
	filters, ok := triplet["filters"].([]string)
	if !ok || len(filters) != 1 || filters[0] != "CryoSleep = True" {
		t.Fatalf("triplet filters = %v, want [CryoSleep = True]", triplet["filters"])
	}
	newFilters, ok := triplet["new_filters"].([]string)
	if !ok || len(newFilters) != 1 || newFilters[0] != "CryoSleep = True" {
		t.Fatalf("triplet new_filters = %v, want [CryoSleep = True]", triplet["new_filters"])
	}
}

// TestRenderConstraintsEmpty pins the []-not-null contract the web
// TripletPayload.filters (string[]) depends on: a non-nil empty slice so an empty
// segment renders as [] rather than null (which would break filters.length/.map).
func TestRenderConstraintsEmpty(t *testing.T) {
	chips := renderConstraints(nil)
	if chips == nil {
		t.Fatal("renderConstraints(nil) must return a non-nil slice so it marshals to []")
	}
	b, err := json.Marshal(chips)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if string(b) != "[]" {
		t.Fatalf("empty chips marshaled to %s, want []", b)
	}
}

func TestRunLoopImprovingCandidateExpandsWithPinnedLabel(t *testing.T) {
	repo := &fakeRepo{}
	// Every proposal returns one candidate. The root candidate improves (12 > 10),
	// so it is expanded; its grandchild does not improve (11 < 12) and stops. That
	// bounds the run to two triplets and two proposals (root + one child).
	claude := &fakeClaude{proposal: llm.Proposal{Candidates: []llm.CandidateIntervention{{Filters: nil}}}}
	sandbox := &fakeSandbox{
		introspect: IntrospectResponse{Schema: schemaDTO{Columns: []columnDTO{{Name: "revenue", Type: "DOUBLE"}}}},
		execResps: []ExecuteResponse{
			{Value: map[string]any{"avg(revenue)": 10.0}}, // root baseline
			{Value: map[string]any{"avg(revenue)": 12.0}}, // candidate (improves → expand)
			{Value: map[string]any{"avg(revenue)": 11.0}}, // grandchild (no improvement → stop)
		},
	}
	srv := newTestServerRepo(repo, &fakeGoals{}, &fakeAudits{}, &fakeObjects{}, &fakeHeur{}, claude, sandbox)

	goal := store.Goal{OptimizationFunctionID: "g1", DataSourceRef: "ref",
		EvaluationMatrix: domain.EvaluationMatrix{Targets: []domain.Target{{Field: "revenue", Direction: domain.Maximize, Aggregation: "avg"}}}}
	srv.runLoop(context.Background(), goal, "run-1")

	if len(repo.outcomes) != 2 {
		t.Fatalf("expected two triplets (candidate + expanded child), got %d", len(repo.outcomes))
	}
	// The root proposal and one child proposal ran; the child carries the pinned
	// objective label and the parent's measured value.
	if len(claude.gotNodes) != 2 {
		t.Fatalf("expected root + one child proposal, got %d", len(claude.gotNodes))
	}
	child := claude.gotNodes[1]
	if child.IsRoot || child.ObjectiveLabel != "avg(revenue)" {
		t.Fatalf("child proposal must be non-root and carry the pinned label: %+v", child)
	}
	if child.PriorValue == nil || *child.PriorValue != 12.0 {
		t.Fatalf("child proposal must carry the parent's measured value (12), got %v", child.PriorValue)
	}
}

func TestRunLoopTerminalOnMissingAggregation(t *testing.T) {
	audits := &fakeAudits{}
	sandbox := &fakeSandbox{}
	srv := newTestServer(&fakeGoals{}, audits, &fakeObjects{}, &fakeHeur{}, &fakeClaude{}, sandbox)

	goal := store.Goal{OptimizationFunctionID: "g1", DataSourceRef: "ref",
		EvaluationMatrix: domain.EvaluationMatrix{Targets: []domain.Target{{Field: "revenue", Direction: domain.Maximize}}}}
	srv.runLoop(context.Background(), goal, "run-1")

	// A legacy matrix fails at pin time — before any sandbox call.
	if sandbox.execCalls != 0 {
		t.Fatalf("expected no execute calls, got %d", sandbox.execCalls)
	}
	found := false
	for _, r := range audits.records {
		if r.Action == "hypothesis_branch_failure" {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected a hypothesis_branch_failure audit, got %+v", audits.records)
	}
}

func TestRunLoopRepairsInvalidColumnCandidate(t *testing.T) {
	repo := &fakeRepo{}
	// The root proposal references a column absent from the schema; the repair
	// returns a candidate on a real column. The invalid candidate must never reach
	// the sandbox — only the baseline and the repaired candidate execute.
	claude := &fakeClaude{
		proposal:   llm.Proposal{Candidates: []llm.CandidateIntervention{{Filters: []domain.Constraint{{Field: "unknown_col", Op: domain.GreaterThan, Value: 1}}}}},
		treeRepair: llm.Proposal{Candidates: []llm.CandidateIntervention{{Filters: []domain.Constraint{{Field: "revenue", Op: domain.GreaterThan, Value: 1}}}}},
	}
	sandbox := &fakeSandbox{
		introspect: IntrospectResponse{Schema: schemaDTO{Columns: []columnDTO{{Name: "revenue", Type: "DOUBLE"}}}},
		execResps: []ExecuteResponse{
			{Value: map[string]any{"avg(revenue)": 10.0}}, // root baseline
			{Value: map[string]any{"avg(revenue)": 8.0}},  // repaired candidate (no improvement → stop)
		},
	}
	srv := newTestServerRepo(repo, &fakeGoals{}, &fakeAudits{}, &fakeObjects{}, &fakeHeur{}, claude, sandbox)
	srv.runLoop(context.Background(), revenueGoal(), "run-1")

	if claude.treeRepairCalls != 1 {
		t.Fatalf("expected one intervention-tree repair, got %d", claude.treeRepairCalls)
	}
	// Only the invalid candidate is re-proposed — the accumulate-valid loop never
	// re-proposes candidates that already passed.
	if len(claude.treeRepairPrior) != 1 || claude.treeRepairPrior[0].Candidates[0].Filters[0].Field != "unknown_col" {
		t.Fatalf("repair must re-propose only the invalid candidate: %+v", claude.treeRepairPrior)
	}
	// Baseline + the one repaired candidate — the invalid candidate never executed.
	if sandbox.execCalls != 2 {
		t.Fatalf("expected exactly baseline + repaired-candidate executes, got %d", sandbox.execCalls)
	}
	if len(repo.outcomes) != 1 {
		t.Fatalf("expected one triplet from the repaired candidate, got %d", len(repo.outcomes))
	}
}

func TestRunLoopDropsUncorrectableCandidatesAndCompletes(t *testing.T) {
	runs := &fakeRuns{}
	audits := &fakeAudits{}
	// Both the proposal and every repair reference unknown columns, so after the
	// repair bound the node has no valid candidate. The run drops them, records a
	// branch failure, and still settles completed.
	claude := &fakeClaude{
		proposal:   llm.Proposal{Candidates: []llm.CandidateIntervention{{Filters: []domain.Constraint{{Field: "unknown_col", Op: domain.GreaterThan, Value: 1}}}}},
		treeRepair: llm.Proposal{Candidates: []llm.CandidateIntervention{{Filters: []domain.Constraint{{Field: "still_unknown", Op: domain.GreaterThan, Value: 1}}}}},
	}
	sandbox := &fakeSandbox{
		introspect: IntrospectResponse{Schema: schemaDTO{Columns: []columnDTO{{Name: "revenue", Type: "DOUBLE"}}}},
		execResps:  []ExecuteResponse{{Value: map[string]any{"avg(revenue)": 10.0}}}, // baseline only
	}
	srv := newTestServerRepo(&fakeRepo{}, &fakeGoals{}, audits, &fakeObjects{}, &fakeHeur{}, claude, sandbox)
	srv.runs = runs
	srv.runLoop(context.Background(), revenueGoal(), "run-1")

	if claude.treeRepairCalls != maxProposalRepairs {
		t.Fatalf("expected %d repair attempts before dropping, got %d", maxProposalRepairs, claude.treeRepairCalls)
	}
	found := false
	for _, r := range audits.records {
		if r.Action == "hypothesis_branch_failure" {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected a hypothesis_branch_failure audit for the dropped candidates: %+v", audits.records)
	}
	assertOneStatus(t, runs, store.RunCompleted, "")
}

func TestRunLoopPreservesInitiallyValidCandidatesAcrossRepair(t *testing.T) {
	repo := &fakeRepo{}
	// A mixed proposal: one candidate on a real column (valid), one on an unknown
	// column (invalid). Only the invalid one is re-proposed; the initially-valid one
	// is preserved (never re-proposed) and executes alongside the repaired candidate.
	claude := &fakeClaude{
		proposal: llm.Proposal{Candidates: []llm.CandidateIntervention{
			{Filters: []domain.Constraint{{Field: "revenue", Op: domain.GreaterThan, Value: 1}}},
			{Filters: []domain.Constraint{{Field: "unknown_col", Op: domain.GreaterThan, Value: 2}}},
		}},
		treeRepair: llm.Proposal{Candidates: []llm.CandidateIntervention{
			{Filters: []domain.Constraint{{Field: "revenue", Op: domain.LessThan, Value: 9}}},
		}},
	}
	sandbox := &fakeSandbox{
		introspect: IntrospectResponse{Schema: schemaDTO{Columns: []columnDTO{{Name: "revenue", Type: "DOUBLE"}}}},
		execResps: []ExecuteResponse{
			{Value: map[string]any{"avg(revenue)": 10.0}}, // root baseline
			{Value: map[string]any{"avg(revenue)": 8.0}},  // initially-valid candidate (no improvement → stop)
			{Value: map[string]any{"avg(revenue)": 7.0}},  // repaired candidate (no improvement → stop)
		},
	}
	srv := newTestServerRepo(repo, &fakeGoals{}, &fakeAudits{}, &fakeObjects{}, &fakeHeur{}, claude, sandbox)
	srv.runLoop(context.Background(), revenueGoal(), "run-1")

	if claude.treeRepairCalls != 1 {
		t.Fatalf("expected one repair, got %d", claude.treeRepairCalls)
	}
	// Only the invalid candidate is re-proposed — the valid one is never sent to repair.
	if len(claude.treeRepairPrior) != 1 || len(claude.treeRepairPrior[0].Candidates) != 1 ||
		claude.treeRepairPrior[0].Candidates[0].Filters[0].Field != "unknown_col" {
		t.Fatalf("repair must carry only the invalid candidate: %+v", claude.treeRepairPrior)
	}
	// Baseline + both surviving candidates (the initially-valid and the repaired) execute.
	if sandbox.execCalls != 3 {
		t.Fatalf("expected baseline + 2 candidate executes, got %d", sandbox.execCalls)
	}
	if len(repo.outcomes) != 2 {
		t.Fatalf("expected two triplets (valid + repaired candidate), got %d", len(repo.outcomes))
	}
}

func TestRunLoopRootRepairErrorIsNonTerminal(t *testing.T) {
	runs := &fakeRuns{}
	audits := &fakeAudits{}
	// The root proposal references an unknown column; the repair call fails with a
	// transport error. Best-effort salvage keeps the (here empty) accumulated valid
	// set and records a branch failure, so the run still completes rather than failing.
	claude := &fakeClaude{
		proposal:      llm.Proposal{Candidates: []llm.CandidateIntervention{{Filters: []domain.Constraint{{Field: "unknown_col", Op: domain.GreaterThan, Value: 1}}}}},
		treeRepairErr: errors.New("claude down"),
	}
	sandbox := &fakeSandbox{
		introspect: IntrospectResponse{Schema: schemaDTO{Columns: []columnDTO{{Name: "revenue", Type: "DOUBLE"}}}},
		execResps:  []ExecuteResponse{{Value: map[string]any{"avg(revenue)": 10.0}}}, // baseline only
	}
	srv := newTestServerRepo(&fakeRepo{}, &fakeGoals{}, audits, &fakeObjects{}, &fakeHeur{}, claude, sandbox)
	srv.runs = runs
	srv.runLoop(context.Background(), revenueGoal(), "run-1")

	if claude.treeRepairCalls != 1 {
		t.Fatalf("expected one repair attempt before the transport error, got %d", claude.treeRepairCalls)
	}
	found := false
	for _, r := range audits.records {
		if r.Action == "hypothesis_branch_failure" {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected a hypothesis_branch_failure audit for the failed repair: %+v", audits.records)
	}
	assertOneStatus(t, runs, store.RunCompleted, "")
}

func TestRunLoopRepairErrorSalvagesAlreadyValidCandidates(t *testing.T) {
	repo := &fakeRepo{}
	// A mixed proposal (one valid candidate, one invalid) whose repair call then fails
	// with a transport error. Best-effort salvage must keep the already-validated
	// candidate — it still executes despite the fault. If the salvage returned nil
	// instead of the accumulated valid set, only the baseline would execute.
	claude := &fakeClaude{
		proposal: llm.Proposal{Candidates: []llm.CandidateIntervention{
			{Filters: []domain.Constraint{{Field: "revenue", Op: domain.GreaterThan, Value: 1}}},
			{Filters: []domain.Constraint{{Field: "unknown_col", Op: domain.GreaterThan, Value: 2}}},
		}},
		treeRepairErr: errors.New("claude down"),
	}
	sandbox := &fakeSandbox{
		introspect: IntrospectResponse{Schema: schemaDTO{Columns: []columnDTO{{Name: "revenue", Type: "DOUBLE"}}}},
		execResps: []ExecuteResponse{
			{Value: map[string]any{"avg(revenue)": 10.0}}, // root baseline
			{Value: map[string]any{"avg(revenue)": 8.0}},  // preserved valid candidate (no improvement → stop)
		},
	}
	srv := newTestServerRepo(repo, &fakeGoals{}, &fakeAudits{}, &fakeObjects{}, &fakeHeur{}, claude, sandbox)
	srv.runLoop(context.Background(), revenueGoal(), "run-1")

	if claude.treeRepairCalls != 1 {
		t.Fatalf("expected one repair attempt, got %d", claude.treeRepairCalls)
	}
	// Baseline + the preserved valid candidate: salvage returned the accumulated valid
	// set, not nil.
	if sandbox.execCalls != 2 {
		t.Fatalf("expected baseline + preserved-candidate executes, got %d", sandbox.execCalls)
	}
	if len(repo.outcomes) != 1 {
		t.Fatalf("expected one triplet from the preserved valid candidate, got %d", len(repo.outcomes))
	}
}

func TestRunLoopRootProposalErrorIsTerminal(t *testing.T) {
	runs := &fakeRuns{}
	// A transport error from the initial proposal (not a repair) is terminal at the
	// root — it settles the run failed, distinct from the non-terminal repair-error
	// salvage above.
	claude := &fakeClaude{proposalErr: errors.New("claude down")}
	sandbox := &fakeSandbox{introspect: IntrospectResponse{Schema: schemaDTO{Columns: []columnDTO{{Name: "revenue", Type: "DOUBLE"}}}}}
	srv := newTestServerRepo(&fakeRepo{}, &fakeGoals{}, &fakeAudits{}, &fakeObjects{}, &fakeHeur{}, claude, sandbox)
	srv.runs = runs
	srv.runLoop(context.Background(), revenueGoal(), "run-1")

	if claude.treeRepairCalls != 0 {
		t.Fatalf("an initial-proposal error must not trigger repair, got %d", claude.treeRepairCalls)
	}
	// The proposal fault short-circuits before any sandbox baseline execute.
	if sandbox.execCalls != 0 {
		t.Fatalf("expected no execute calls on a terminal root proposal, got %d", sandbox.execCalls)
	}
	assertOneStatus(t, runs, store.RunFailed, "claude down")
}

func TestRunLoopChildRepairErrorIsNonTerminal(t *testing.T) {
	runs := &fakeRuns{}
	audits := &fakeAudits{}
	// The root proposes a valid candidate that improves and expands; the child
	// proposal references an unknown column and its repair fails with a transport
	// error. That is non-terminal — a branch failure — so the run still completes.
	childProposal := llm.Proposal{Candidates: []llm.CandidateIntervention{{Filters: []domain.Constraint{{Field: "unknown_col", Op: domain.GreaterThan, Value: 1}}}}}
	claude := &fakeClaude{
		proposal:      llm.Proposal{Candidates: []llm.CandidateIntervention{{Filters: []domain.Constraint{{Field: "revenue", Op: domain.GreaterThan, Value: 1}}}}},
		childProposal: &childProposal,
		treeRepairErr: errors.New("claude down"),
	}
	sandbox := &fakeSandbox{
		introspect: IntrospectResponse{Schema: schemaDTO{Columns: []columnDTO{{Name: "revenue", Type: "DOUBLE"}}}},
		execResps: []ExecuteResponse{
			{Value: map[string]any{"avg(revenue)": 10.0}}, // root baseline
			{Value: map[string]any{"avg(revenue)": 12.0}}, // valid candidate improves → expand
		},
	}
	srv := newTestServerRepo(&fakeRepo{}, &fakeGoals{}, audits, &fakeObjects{}, &fakeHeur{}, claude, sandbox)
	srv.runs = runs
	srv.runLoop(context.Background(), revenueGoal(), "run-1")

	if claude.treeRepairCalls != 1 {
		t.Fatalf("expected one child repair attempt, got %d", claude.treeRepairCalls)
	}
	found := false
	for _, r := range audits.records {
		if r.Action == "hypothesis_branch_failure" {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected a hypothesis_branch_failure audit for the failed child repair: %+v", audits.records)
	}
	assertOneStatus(t, runs, store.RunCompleted, "")
}

func TestRunLoopRepairMessageDedupsUnknownColumnsAcrossCandidates(t *testing.T) {
	// Two invalid candidates reference distinct unknown columns, one repeated in a
	// different case. splitByColumns collects the unknowns across candidates and dedups
	// case-insensitively in first-seen order; that deduped list is what reaches the
	// repair call as its validation error.
	claude := &fakeClaude{
		proposal: llm.Proposal{Candidates: []llm.CandidateIntervention{
			{Filters: []domain.Constraint{
				{Field: "Cabin", Op: domain.GreaterThan, Value: 1},
				{Field: "Age", Op: domain.GreaterThan, Value: 2},
			}},
			{Filters: []domain.Constraint{{Field: "cabin", Op: domain.LessThan, Value: 3}}},
		}},
		treeRepair: llm.Proposal{}, // no valid candidates back → loop resolves, run completes
	}
	sandbox := &fakeSandbox{
		introspect: IntrospectResponse{Schema: schemaDTO{Columns: []columnDTO{{Name: "revenue", Type: "DOUBLE"}}}},
		execResps:  []ExecuteResponse{{Value: map[string]any{"avg(revenue)": 10.0}}}, // baseline only
	}
	srv := newTestServerRepo(&fakeRepo{}, &fakeGoals{}, &fakeAudits{}, &fakeObjects{}, &fakeHeur{}, claude, sandbox)
	srv.runLoop(context.Background(), revenueGoal(), "run-1")

	if claude.treeRepairCalls != 1 {
		t.Fatalf("expected one repair call, got %d", claude.treeRepairCalls)
	}
	// The unknown columns reach the repair call, deduped case-insensitively (the second
	// candidate's "cabin" collapses into "Cabin") in first-seen order — a case-sensitive
	// dedup regression would append a third "cabin" and break the "Cabin, Age;" fragment.
	if msg := claude.treeRepairErrMsg[0]; !strings.Contains(msg, "schema: Cabin, Age;") {
		t.Fatalf("repair validation error must carry the deduped unknown columns, got %q", msg)
	}
}

// revenueGoal is the standard maximize-avg(revenue) goal the status-transition
// tests drive the loop with.
func revenueGoal() store.Goal {
	return store.Goal{OptimizationFunctionID: "g1", DataSourceRef: "ref",
		EvaluationMatrix: domain.EvaluationMatrix{Targets: []domain.Target{{Field: "revenue", Direction: domain.Maximize, Aggregation: "avg"}}}}
}

// panicSandbox panics on Introspect so a loop test can exercise the deferred
// recover path (a crash mid-run must settle failed, not propagate).
type panicSandbox struct{ msg string }

func (p panicSandbox) Introspect(context.Context, IntrospectRequest) (IntrospectResponse, error) {
	panic(p.msg)
}
func (p panicSandbox) Execute(context.Context, ExecuteRequest) (ExecuteResponse, error) {
	return ExecuteResponse{}, nil
}
func (p panicSandbox) DocumentText(context.Context, DocumentTextRequest) (DocumentTextResponse, error) {
	return DocumentTextResponse{}, nil
}

func assertOneStatus(t *testing.T, runs *fakeRuns, status store.RunStatus, reason string) {
	t.Helper()
	if len(runs.statusCalls) != 1 {
		t.Fatalf("expected one SetStatus call, got %d: %+v", len(runs.statusCalls), runs.statusCalls)
	}
	if c := runs.statusCalls[0]; c.status != status || c.reason != reason {
		t.Fatalf("SetStatus = (%q, %q), want (%q, %q)", c.status, c.reason, status, reason)
	}
}

func TestRunLoopStatusTransitions(t *testing.T) {
	revenueSchema := IntrospectResponse{Schema: schemaDTO{Columns: []columnDTO{{Name: "revenue", Type: "DOUBLE"}}}}

	t.Run("clean run settles completed", func(t *testing.T) {
		runs := &fakeRuns{}
		claude := &fakeClaude{proposal: llm.Proposal{Candidates: []llm.CandidateIntervention{{Filters: nil}}}}
		sandbox := &fakeSandbox{introspect: revenueSchema, execResps: []ExecuteResponse{
			{Value: map[string]any{"avg(revenue)": 10.0}}, // baseline
			{Value: map[string]any{"avg(revenue)": 8.0}},  // candidate: no improvement → stop
		}}
		srv := newTestServerRepo(&fakeRepo{}, &fakeGoals{}, &fakeAudits{}, &fakeObjects{}, &fakeHeur{}, claude, sandbox)
		srv.runs = runs
		srv.runLoop(context.Background(), revenueGoal(), "run-1")
		assertOneStatus(t, runs, store.RunCompleted, "")
	})

	t.Run("root failure settles failed with the real reason", func(t *testing.T) {
		runs := &fakeRuns{}
		sandbox := &fakeSandbox{introspectErr: &SandboxError{Status: http.StatusBadRequest, Message: "field not present in schema: X"}}
		srv := newTestServerRepo(&fakeRepo{}, &fakeGoals{}, &fakeAudits{}, &fakeObjects{}, &fakeHeur{}, &fakeClaude{}, sandbox)
		srv.runs = runs
		srv.runLoop(context.Background(), revenueGoal(), "run-1")
		assertOneStatus(t, runs, store.RunFailed, "field not present in schema: X")
	})

	t.Run("every candidate branch-failing still settles completed", func(t *testing.T) {
		runs := &fakeRuns{}
		claude := &fakeClaude{proposal: llm.Proposal{Candidates: []llm.CandidateIntervention{{Filters: nil}}}}
		sandbox := &fakeSandbox{introspect: revenueSchema,
			execResps: []ExecuteResponse{{Value: map[string]any{"avg(revenue)": 10.0}}}, // baseline only
			execErrs:  []error{nil, errors.New("candidate boom")},                       // candidate Execute fails (non-terminal)
		}
		srv := newTestServerRepo(&fakeRepo{}, &fakeGoals{}, &fakeAudits{}, &fakeObjects{}, &fakeHeur{}, claude, sandbox)
		srv.runs = runs
		srv.runLoop(context.Background(), revenueGoal(), "run-1")
		assertOneStatus(t, runs, store.RunCompleted, "")
	})

	t.Run("timeout after baseline settles failed via ctx promotion", func(t *testing.T) {
		runs := &fakeRuns{}
		claude := &fakeClaude{proposal: llm.Proposal{Candidates: []llm.CandidateIntervention{{Filters: nil}}}}
		sandbox := &fakeSandbox{introspect: revenueSchema,
			execResps: []ExecuteResponse{{Value: map[string]any{"avg(revenue)": 10.0}}},
			execErrs:  []error{nil, context.DeadlineExceeded},
		}
		srv := newTestServerRepo(&fakeRepo{}, &fakeGoals{}, &fakeAudits{}, &fakeObjects{}, &fakeHeur{}, claude, sandbox)
		srv.runs = runs
		ctx, cancel := context.WithCancel(context.Background())
		cancel() // loop ctx cancelled: a per-candidate failure is non-terminal, but the run did not complete
		srv.runLoop(ctx, revenueGoal(), "run-1")
		assertOneStatus(t, runs, store.RunFailed, context.Canceled.Error())
	})

	t.Run("terminal write runs on a live detached context", func(t *testing.T) {
		runs := &fakeRuns{}
		sandbox := &fakeSandbox{introspectErr: &SandboxError{Status: http.StatusBadRequest, Message: "boom"}}
		srv := newTestServerRepo(&fakeRepo{}, &fakeGoals{}, &fakeAudits{}, &fakeObjects{}, &fakeHeur{}, &fakeClaude{}, sandbox)
		srv.runs = runs
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		srv.runLoop(ctx, revenueGoal(), "run-1")
		if len(runs.statusCalls) != 1 {
			t.Fatalf("expected one SetStatus call, got %d", len(runs.statusCalls))
		}
		// The write must run on the detached writeCtx, not the cancelled loop ctx —
		// otherwise a regression reusing the loop ctx would surface here.
		if runs.statusCalls[0].ctxErr != nil {
			t.Fatalf("terminal write ran on a cancelled context: %v", runs.statusCalls[0].ctxErr)
		}
	})

	t.Run("a panic settles failed and is contained", func(t *testing.T) {
		runs := &fakeRuns{}
		srv := newTestServerRepo(&fakeRepo{}, &fakeGoals{}, &fakeAudits{}, &fakeObjects{}, &fakeHeur{}, &fakeClaude{}, panicSandbox{msg: "boom"})
		srv.runs = runs
		// Must not propagate — the deferred recover contains it; the test crashes
		// here if the recover regressed.
		srv.runLoop(context.Background(), revenueGoal(), "run-1")
		assertOneStatus(t, runs, store.RunFailed, "hypothesis loop panicked: boom")
	})
}

func TestImproves(t *testing.T) {
	if !improves(10, 12, domain.Maximize) {
		t.Fatal("maximize: larger value should improve")
	}
	if improves(10, 8, domain.Maximize) {
		t.Fatal("maximize: smaller value should not improve")
	}
	if !improves(10, 8, domain.Minimize) {
		t.Fatal("minimize: smaller value should improve")
	}
	if improves(10, 12, domain.Minimize) {
		t.Fatal("minimize: larger value should not improve")
	}
	if improves(10, 10, domain.Maximize) || improves(10, 10, domain.Minimize) {
		t.Fatal("equal value should not improve in either direction")
	}
}

func TestConstraintsSatisfied(t *testing.T) {
	constraints := []domain.Constraint{
		{Field: "revenue", Op: domain.GreaterThanOrEqual, Value: 100},
		{Field: "other", Op: domain.LessThan, Value: 1}, // on a different field — unverifiable, treated as satisfied
	}
	if !constraintsSatisfied(constraints, "revenue", 100) {
		t.Fatal("value at the gte boundary should satisfy")
	}
	if constraintsSatisfied(constraints, "revenue", 99) {
		t.Fatal("value below the gte boundary should violate")
	}
	// A constraint only on another field cannot be checked from the objective
	// aggregate and must not cause a false violation.
	if !constraintsSatisfied([]domain.Constraint{{Field: "other", Op: domain.LessThan, Value: 1}}, "revenue", 5) {
		t.Fatal("constraint on a non-objective field should be treated as satisfied")
	}
	// A non-numeric op on the objective field is unverifiable from a single aggregate
	// and falls through to satisfied — the switch has no default case.
	if !constraintsSatisfied([]domain.Constraint{{Field: "revenue", Op: domain.Equal, Operand: &domain.LiteralValue{Bool: boolPtr(true)}}}, "revenue", 5) {
		t.Fatal("a non-numeric-op constraint on the objective field should be treated as satisfied")
	}
	for _, tc := range []struct {
		op    domain.ConstraintOp
		value float64
		ok    bool
	}{
		{domain.LessThan, 4, true}, {domain.LessThan, 5, false},
		{domain.LessThanOrEqual, 5, true}, {domain.LessThanOrEqual, 6, false},
		{domain.GreaterThan, 6, true}, {domain.GreaterThan, 5, false},
		{domain.GreaterThanOrEqual, 5, true}, {domain.GreaterThanOrEqual, 4, false},
	} {
		got := constraintsSatisfied([]domain.Constraint{{Field: "f", Op: tc.op, Value: 5}}, "f", tc.value)
		if got != tc.ok {
			t.Fatalf("%s %v vs 5 = %v, want %v", tc.op, tc.value, got, tc.ok)
		}
	}
}

func TestNumericValue(t *testing.T) {
	if v, ok := numericValue(map[string]any{"f": 12.5}, "f"); !ok || v != 12.5 {
		t.Fatalf("float64 = (%v,%v), want (12.5,true)", v, ok)
	}
	if v, ok := numericValue(map[string]any{"f": json.Number("7")}, "f"); !ok || v != 7 {
		t.Fatalf("json.Number = (%v,%v), want (7,true)", v, ok)
	}
	if _, ok := numericValue(map[string]any{"f": nil}, "f"); ok {
		t.Fatal("nil value (empty aggregate) should be unusable")
	}
	if _, ok := numericValue(map[string]any{}, "f"); ok {
		t.Fatal("missing key should be unusable")
	}
	if _, ok := numericValue(map[string]any{"f": "12"}, "f"); ok {
		t.Fatal("string value should be unusable")
	}
}

func TestConcatFiltersDoesNotAliasParent(t *testing.T) {
	parent := []domain.Constraint{{Field: "region", Op: domain.GreaterThanOrEqual, Value: 1}}
	added := []domain.Constraint{{Field: "tier", Op: domain.LessThan, Value: 3}}

	out := concatFilters(parent, added)
	if len(out) != 2 {
		t.Fatalf("expected 2 filters, got %d", len(out))
	}
	// Mutating the result must not bleed into the parent's cumulative filters,
	// or sibling branches would cross-contaminate.
	out[0].Value = 999
	if parent[0].Value != 1 {
		t.Fatalf("parent filter mutated through the concatenated slice: %+v", parent[0])
	}
}
