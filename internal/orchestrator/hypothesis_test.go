package orchestrator

import (
	"context"
	"encoding/json"
	"errors"
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
	srv.runLoop(context.Background(), goal)

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
	srv.runLoop(context.Background(), goal)

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
	srv.runLoop(context.Background(), goal)

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
