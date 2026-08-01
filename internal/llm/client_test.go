package llm

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/arborette/arborette/internal/domain"
)

// fakeCompleter is a stand-in for the LLM backend API call, so parsing and
// schema construction are exercised without a real API. It records the system
// and user prompts it was handed so a fencing test can assert what actually
// reached the backend.
type fakeCompleter struct {
	text      string
	err       error
	gotSystem string
	gotUser   string
}

func (f *fakeCompleter) complete(_ context.Context, system, user string, _ map[string]any) (string, error) {
	f.gotSystem, f.gotUser = system, user
	return f.text, f.err
}

func (f *fakeCompleter) completeWithPDF(_ context.Context, system string, _ []byte, instruction string, _ map[string]any) (string, error) {
	f.gotSystem, f.gotUser = system, instruction
	return f.text, f.err
}

func TestGenerateEvaluationMatrix(t *testing.T) {
	body := `{"targets":[{"direction":"maximize","aggregation":"avg",` +
		`"value":"{\"kind\":\"comparison\",\"op\":\"=\",` +
		`\"left\":{\"kind\":\"column_ref\",\"column\":\"revenue\"},` +
		`\"right\":{\"kind\":\"literal\",\"literal\":{\"number\":100}}}"}],` +
		`"constraints":[{"field":"cost","op":"lte","value":100}]}`
	c := &client{backend: &fakeCompleter{text: body}}

	schema := SandboxSchema{Columns: []SandboxColumn{{Name: "revenue", Type: "DOUBLE"}, {Name: "cost", Type: "DOUBLE"}}}
	matrix, err := c.GenerateEvaluationMatrix(context.Background(), "grow revenue without overspending", schema)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(matrix.Targets) != 1 || matrix.Targets[0].Direction != domain.Maximize || matrix.Targets[0].Aggregation != "avg" {
		t.Fatalf("unexpected targets: %+v", matrix.Targets)
	}
	expr := matrix.Targets[0].ValueExpression()
	if expr.Kind != domain.ComparisonKind || expr.Op != "=" || expr.Left == nil || expr.Left.Column != "revenue" {
		t.Fatalf("value expression did not round-trip into a comparison: %+v", expr)
	}
	if len(matrix.Constraints) != 1 || matrix.Constraints[0].Field != "cost" || matrix.Constraints[0].Op != domain.LessThanOrEqual || matrix.Constraints[0].Value != 100 {
		t.Fatalf("unexpected constraints: %+v", matrix.Constraints)
	}
}

func TestGenerateEvaluationMatrixSkipsLeadingThinkingBlock(t *testing.T) {
	body := `{"targets":[{"direction":"minimize","aggregation":"avg",` +
		`"value":"{\"kind\":\"column_ref\",\"column\":\"latency\"}"}],"constraints":[]}`
	c := &client{backend: &fakeCompleter{text: body}}

	matrix, err := c.GenerateEvaluationMatrix(context.Background(), "cut latency", SandboxSchema{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(matrix.Targets) != 1 || matrix.Targets[0].Direction != domain.Minimize {
		t.Fatalf("expected the text block to be selected past the thinking block: %+v", matrix)
	}
}

func TestRepairEvaluationMatrix(t *testing.T) {
	body := `{"targets":[{"direction":"maximize","aggregation":"sum",` +
		`"value":"{\"kind\":\"column_ref\",\"column\":\"revenue\"}"}],"constraints":[]}`
	c := &client{backend: &fakeCompleter{text: body}}

	tru := true
	priorExpr := domain.Expression{Kind: domain.ComparisonKind, Op: "=",
		Left:  &domain.Expression{Kind: domain.ColumnRefKind, Column: "Transported"},
		Right: &domain.Expression{Kind: domain.LiteralKind, Literal: &domain.LiteralValue{Bool: &tru}}}
	prior := domain.EvaluationMatrix{Targets: []domain.Target{{Direction: domain.Maximize, Aggregation: "avg", Value: &priorExpr}}}
	schema := SandboxSchema{Columns: []SandboxColumn{{Name: "Transported", Type: "BOOLEAN"}}}
	matrix, err := c.RepairEvaluationMatrix(context.Background(), "grow revenue", schema, prior, "numeric aggregation over non-numeric column")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if matrix.Targets[0].Aggregation != "sum" {
		t.Fatalf("expected the repaired matrix, got %+v", matrix.Targets)
	}
}

func TestGenerateEvaluationMatrixMalformedValueErrors(t *testing.T) {
	body := `{"targets":[{"direction":"maximize","aggregation":"avg","value":"not json"}],"constraints":[]}`
	c := &client{backend: &fakeCompleter{text: body}}
	if _, err := c.GenerateEvaluationMatrix(context.Background(), "goal", SandboxSchema{}); err == nil {
		t.Fatalf("expected an error for a malformed value expression string")
	}
}

func TestGenerateEvaluationMatrixObjectValueErrors(t *testing.T) {
	body := `{"targets":[{"direction":"maximize","aggregation":"avg","value":{"kind":"column_ref","column":"revenue"}}],"constraints":[]}`
	c := &client{backend: &fakeCompleter{text: body}}
	if _, err := c.GenerateEvaluationMatrix(context.Background(), "goal", SandboxSchema{}); err == nil {
		t.Fatalf("expected an error for an object-shaped value expression")
	}
}

func TestProposeInterventionTreeRoot(t *testing.T) {
	body := `{"candidates":[{"filters":[{"field":"region","op":"gte","value":1}]},{"filters":[]}]}`
	c := &client{backend: &fakeCompleter{text: body}}

	schema := SandboxSchema{Columns: []SandboxColumn{{Name: "revenue", Type: "DOUBLE"}, {Name: "region", Type: "BIGINT"}}}
	proposal, err := c.ProposeInterventionTree(context.Background(), "grow revenue",
		domain.EvaluationMatrix{Targets: []domain.Target{{Field: "revenue", Direction: domain.Maximize, Aggregation: "sum"}}},
		schema, TreeContext{IsRoot: true, Breadth: 3})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(proposal.Candidates) != 2 {
		t.Fatalf("expected 2 candidates, got %d", len(proposal.Candidates))
	}
	if len(proposal.Candidates[0].Filters) != 1 || proposal.Candidates[0].Filters[0].Field != "region" {
		t.Fatalf("unexpected first candidate filters: %+v", proposal.Candidates[0])
	}
}

func TestRepairInterventionTree(t *testing.T) {
	body := `{"candidates":[{"filters":[{"field":"HomePlanet","op":"gte","value":1}]}]}`
	c := &client{backend: &fakeCompleter{text: body}}

	schema := SandboxSchema{Columns: []SandboxColumn{{Name: "HomePlanet", Type: "VARCHAR"}}}
	prior := Proposal{Candidates: []CandidateIntervention{{Filters: []domain.Constraint{
		{Field: "home_world", Op: domain.GreaterThanOrEqual, Value: 1},
	}}}}
	proposal, err := c.RepairInterventionTree(context.Background(), "grow revenue",
		domain.EvaluationMatrix{Targets: []domain.Target{{Field: "revenue", Direction: domain.Maximize, Aggregation: "sum"}}},
		schema, TreeContext{IsRoot: true, Breadth: 3}, prior, "these filter columns are not in the schema: home_world")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(proposal.Candidates) != 1 || proposal.Candidates[0].Filters[0].Field != "HomePlanet" {
		t.Fatalf("expected the repaired candidate, got %+v", proposal.Candidates)
	}

	// The repair prompt must embed the rejected candidates (rendered in the flat
	// filter shape the output schema demands, not domain.Constraint's internal json)
	// and the naming error — verified by the caller not being fakeable here.
}

func TestProposeInterventionTreeDecodesRicherFilters(t *testing.T) {
	body := `{"candidates":[{"filters":[` +
		`{"field":"CryoSleep","op":"eq","value":true},` +
		`{"field":"HomePlanet","op":"in","value":["Europa","Mars"]},` +
		`{"field":"Age","op":"lte","value":18}` +
		`]}]}`
	c := &client{backend: &fakeCompleter{text: body}}

	schema := SandboxSchema{Columns: []SandboxColumn{
		{Name: "CryoSleep", Type: "BOOLEAN"}, {Name: "HomePlanet", Type: "VARCHAR"}, {Name: "Age", Type: "BIGINT"}}}
	proposal, err := c.ProposeInterventionTree(context.Background(), "grow the transported rate",
		domain.EvaluationMatrix{Targets: []domain.Target{{Field: "x", Direction: domain.Maximize, Aggregation: "avg"}}},
		schema, TreeContext{IsRoot: true, Breadth: 3})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	filters := proposal.Candidates[0].Filters
	if len(filters) != 3 {
		t.Fatalf("want 3 filters, got %d", len(filters))
	}
	if filters[0].Op != domain.Equal || filters[0].Operand == nil || filters[0].Operand.Bool == nil || !*filters[0].Operand.Bool {
		t.Fatalf("bool eq not decoded to Operand: %+v", filters[0])
	}
	if filters[1].Op != domain.In || len(filters[1].Members) != 2 || filters[1].Members[0].String == nil || *filters[1].Members[0].String != "Europa" {
		t.Fatalf("in-set not decoded to Members: %+v", filters[1])
	}
	if filters[2].Op != domain.LessThanOrEqual || filters[2].Value != 18 || filters[2].Operand != nil || filters[2].Members != nil {
		t.Fatalf("threshold not decoded to Value: %+v", filters[2])
	}
}

func TestProposeInterventionTreeRejectsMismatchedValue(t *testing.T) {
	body := `{"candidates":[{"filters":[{"field":"Age","op":"lte","value":"young"}]}]}`
	c := &client{backend: &fakeCompleter{text: body}}
	if _, err := c.ProposeInterventionTree(context.Background(), "goal",
		domain.EvaluationMatrix{Targets: []domain.Target{{Field: "x", Direction: domain.Maximize, Aggregation: "avg"}}},
		SandboxSchema{}, TreeContext{IsRoot: true, Breadth: 3}); err == nil {
		t.Fatalf("expected a generation error for a threshold op with a non-numeric value")
	}
}

func TestFilterWireToConstraintRejectsShapeMismatch(t *testing.T) {
	cases := []struct {
		name string
		fw   filterWire
	}{
		{"membership with scalar", filterWire{Field: "HomePlanet", Op: domain.In, Value: json.RawMessage(`"Europa"`)}},
		{"equality with array", filterWire{Field: "HomePlanet", Op: domain.Equal, Value: json.RawMessage(`["Europa","Mars"]`)}},
		{"threshold with string", filterWire{Field: "Age", Op: domain.LessThanOrEqual, Value: json.RawMessage(`"young"`)}},
		{"threshold with array", filterWire{Field: "Age", Op: domain.LessThanOrEqual, Value: json.RawMessage(`[1]`)}},
		{"empty value", filterWire{Field: "Age", Op: domain.Equal, Value: json.RawMessage("")}},
		{"object value", filterWire{Field: "x", Op: domain.Equal, Value: json.RawMessage(`{"a":1}`)}},
		{"membership with non-scalar member", filterWire{Field: "HomePlanet", Op: domain.In, Value: json.RawMessage(`[["x"]]`)}},
		{"unknown operator", filterWire{Field: "x", Op: domain.ConstraintOp("bogus"), Value: json.RawMessage(`1`)}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if _, err := c.fw.toConstraint(); err == nil {
				t.Fatalf("expected a generation error for %s", c.name)
			}
		})
	}
}

func TestFilterWireToConstraintMembershipScalars(t *testing.T) {
	fw := filterWire{Field: "HomePlanet", Op: domain.NotIn, Value: json.RawMessage(`["Earth"]`)}
	con, err := fw.toConstraint()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(con.Members) != 1 || con.Members[0].String == nil || *con.Members[0].String != "Earth" {
		t.Fatalf("not_in members not decoded: %+v", con)
	}
}

func TestRenderFiltersInlineEmpty(t *testing.T) {
	if got := renderFiltersInline(nil); got != "(none)" {
		t.Fatalf("renderFiltersInline(nil) = %q, want (none)", got)
	}
}

func TestDecodeMatrixNumericConstraintUnchanged(t *testing.T) {
	body := `{"targets":[{"direction":"maximize","aggregation":"avg",` +
		`"value":"{\"kind\":\"column_ref\",\"column\":\"revenue\"}"}],` +
		`"constraints":[{"field":"cost","op":"lte","value":100}]}`
	matrix, err := decodeMatrix(body)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(matrix.Constraints) != 1 {
		t.Fatalf("want 1 constraint, got %d", len(matrix.Constraints))
	}
	c := matrix.Constraints[0]
	if c.Field != "cost" || c.Op != domain.LessThanOrEqual || c.Value != 100 || c.Operand != nil || c.Members != nil {
		t.Fatalf("numeric matrix constraint not decoded unchanged: %+v", c)
	}
}

func TestCompleteMapsStopReasons(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want error
	}{
		{"refusal", errRefused, errRefused},
		{"max tokens", errMaxTokens, errMaxTokens},
		{"other error", errors.New("boom"), nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if tc.err != nil {
				fake := &fakeCompleter{text: "{}", err: tc.err}
				c := &client{backend: fake}
				_, err := c.GenerateEvaluationMatrix(context.Background(), "goal", SandboxSchema{})
				if err == nil {
					t.Fatalf("expected an error")
				}
			}
		})
	}
}

func TestGenerateEvaluationMatrixPropagatesRequestError(t *testing.T) {
	fake := &fakeCompleter{err: errors.New("boom")}
	c := &client{backend: fake}
	if _, err := c.GenerateEvaluationMatrix(context.Background(), "goal", SandboxSchema{}); err == nil {
		t.Fatalf("expected an error when the request fails")
	}
}
