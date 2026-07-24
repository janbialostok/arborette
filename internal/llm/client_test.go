package llm

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/anthropics/anthropic-sdk-go"
	"github.com/anthropics/anthropic-sdk-go/option"

	"github.com/arborette/arborette/internal/domain"
)

// fakeMessages is a stand-in for the one SDK call, so parsing and schema
// construction are exercised without a network round-trip or an API key.
type fakeMessages struct {
	resp *anthropic.Message
	err  error
	got  anthropic.MessageNewParams
}

func (f *fakeMessages) New(_ context.Context, params anthropic.MessageNewParams, _ ...option.RequestOption) (*anthropic.Message, error) {
	f.got = params
	if f.err != nil {
		return nil, f.err
	}
	return f.resp, nil
}

// message builds a canned response by round-tripping JSON, so each content
// block's raw form is populated — AsText/AsThinking read from that raw, not from
// the struct fields.
func message(t *testing.T, stop anthropic.StopReason, blocks ...map[string]any) *anthropic.Message {
	t.Helper()
	raw, err := json.Marshal(map[string]any{
		"id": "msg_test", "type": "message", "role": "assistant", "model": "test-model",
		"stop_reason": string(stop), "content": blocks,
	})
	if err != nil {
		t.Fatalf("marshal canned message: %v", err)
	}
	var msg anthropic.Message
	if err := json.Unmarshal(raw, &msg); err != nil {
		t.Fatalf("unmarshal canned message: %v", err)
	}
	return &msg
}

func textBlockJSON(body string) map[string]any { return map[string]any{"type": "text", "text": body} }
func thinkingBlockJSON(text string) map[string]any {
	return map[string]any{"type": "thinking", "thinking": text}
}

func TestGenerateEvaluationMatrix(t *testing.T) {
	// A boolean/categorical objective is fitted as an aggregation over a comparison
	// indicator; the output schema carries the value as a JSON string, which must
	// re-parse into the AST.
	body := `{"targets":[{"direction":"maximize","aggregation":"avg",` +
		`"value":"{\"kind\":\"comparison\",\"op\":\"=\",` +
		`\"left\":{\"kind\":\"column_ref\",\"column\":\"revenue\"},` +
		`\"right\":{\"kind\":\"literal\",\"literal\":{\"number\":100}}}"}],` +
		`"constraints":[{"field":"cost","op":"lte","value":100}]}`
	fake := &fakeMessages{resp: message(t, anthropic.StopReasonEndTurn, textBlockJSON(body))}
	c := &Client{messages: fake, model: "test-model"}

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
	// Adaptive thinking and a json_schema format are load-bearing on the request.
	if fake.got.Thinking.OfAdaptive == nil {
		t.Fatalf("expected adaptive thinking on the request")
	}
	if fake.got.OutputConfig.Format.Schema == nil {
		t.Fatalf("expected a json_schema output format on the request")
	}
}

func TestGenerateEvaluationMatrixSkipsLeadingThinkingBlock(t *testing.T) {
	body := `{"targets":[{"direction":"minimize","aggregation":"avg",` +
		`"value":"{\"kind\":\"column_ref\",\"column\":\"latency\"}"}],"constraints":[]}`
	fake := &fakeMessages{resp: message(t, anthropic.StopReasonEndTurn,
		thinkingBlockJSON("considering the goal"),
		textBlockJSON(body),
	)}
	c := &Client{messages: fake, model: "test-model"}

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
	fake := &fakeMessages{resp: message(t, anthropic.StopReasonEndTurn, textBlockJSON(body))}
	c := &Client{messages: fake, model: "test-model"}

	// A composite prior objective (a boolean-rate comparison indicator) is the
	// representative repair input: decodeMatrix always hands RepairEvaluationMatrix a
	// full value AST, so encodeMatrix must re-encode a nested expression, not just
	// a degenerate column_ref, back into the JSON-string value field.
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
	// The repair prompt embeds the prior fitted matrix re-encoded through the wire:
	// its aggregation is present and its composite value expression is shown as a
	// JSON string (matching the output schema the model must now satisfy).
	user := fake.got.Messages[0].Content[0].OfText.Text
	if !strings.Contains(user, `"aggregation":"avg"`) {
		t.Fatalf("repair prompt did not embed the prior matrix: %q", user)
	}
	if !strings.Contains(user, `\"kind\":\"comparison\"`) || !strings.Contains(user, `\"column\":\"Transported\"`) {
		t.Fatalf("repair prompt did not embed the prior composite value as a JSON string: %q", user)
	}
	if !strings.Contains(user, "numeric aggregation over non-numeric column") {
		t.Fatalf("repair prompt did not embed the validation error: %q", user)
	}
}

func TestGenerateEvaluationMatrixMalformedValueErrors(t *testing.T) {
	// A value string that is not valid JSON fails decodeMatrix's inner unmarshal;
	// the caller maps this to a 502, matching the prior top-level parse-failure path.
	body := `{"targets":[{"direction":"maximize","aggregation":"avg","value":"not json"}],"constraints":[]}`
	fake := &fakeMessages{resp: message(t, anthropic.StopReasonEndTurn, textBlockJSON(body))}
	c := &Client{messages: fake, model: "test-model"}
	if _, err := c.GenerateEvaluationMatrix(context.Background(), "goal", SandboxSchema{}); err == nil {
		t.Fatalf("expected an error for a malformed value expression string")
	}
}

func TestGenerateEvaluationMatrixObjectValueErrors(t *testing.T) {
	// The pre-migration output shape emitted value as a bare object; against the
	// string-typed wire it fails the outer unmarshal and surfaces as a generation
	// error (mapped to 502), never a silently empty expression.
	body := `{"targets":[{"direction":"maximize","aggregation":"avg","value":{"kind":"column_ref","column":"revenue"}}],"constraints":[]}`
	fake := &fakeMessages{resp: message(t, anthropic.StopReasonEndTurn, textBlockJSON(body))}
	c := &Client{messages: fake, model: "test-model"}
	if _, err := c.GenerateEvaluationMatrix(context.Background(), "goal", SandboxSchema{}); err == nil {
		t.Fatalf("expected an error for an object-shaped value expression")
	}
}

func TestProposeInterventionTreeRoot(t *testing.T) {
	body := `{"candidates":[{"filters":[{"field":"region","op":"gte","value":1}]},{"filters":[]}]}`
	fake := &fakeMessages{resp: message(t, anthropic.StopReasonEndTurn, textBlockJSON(body))}
	c := &Client{messages: fake, model: "test-model"}

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
	fake := &fakeMessages{resp: message(t, anthropic.StopReasonEndTurn, textBlockJSON(body))}
	c := &Client{messages: fake, model: "test-model"}

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
	// and the naming error.
	user := fake.got.Messages[0].Content[0].OfText.Text
	if !strings.Contains(user, "home_world >= 1") {
		t.Fatalf("repair prompt did not embed the rejected candidates as rendered chips: %q", user)
	}
	if !strings.Contains(user, "these filter columns are not in the schema: home_world") {
		t.Fatalf("repair prompt did not embed the naming error: %q", user)
	}
}

// TestProposeInterventionTreeDecodesRicherFilters proves the filterWire decode maps a
// proposal mixing eq/in/threshold filters into typed domain.Constraints by sniffing
// the value's JSON token, and that the intervention system prompt grounds the shape.
func TestProposeInterventionTreeDecodesRicherFilters(t *testing.T) {
	body := `{"candidates":[{"filters":[` +
		`{"field":"CryoSleep","op":"eq","value":true},` +
		`{"field":"HomePlanet","op":"in","value":["Europa","Mars"]},` +
		`{"field":"Age","op":"lte","value":18}` +
		`]}]}`
	fake := &fakeMessages{resp: message(t, anthropic.StopReasonEndTurn, textBlockJSON(body))}
	c := &Client{messages: fake, model: "test-model"}

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
	// bool scalar token → Operand.
	if filters[0].Op != domain.Equal || filters[0].Operand == nil || filters[0].Operand.Bool == nil || !*filters[0].Operand.Bool {
		t.Fatalf("bool eq not decoded to Operand: %+v", filters[0])
	}
	// array token → Members.
	if filters[1].Op != domain.In || len(filters[1].Members) != 2 || filters[1].Members[0].String == nil || *filters[1].Members[0].String != "Europa" {
		t.Fatalf("in-set not decoded to Members: %+v", filters[1])
	}
	// number scalar token under a threshold op → Value.
	if filters[2].Op != domain.LessThanOrEqual || filters[2].Value != 18 || filters[2].Operand != nil || filters[2].Members != nil {
		t.Fatalf("threshold not decoded to Value: %+v", filters[2])
	}
	// The intervention system prompt grounds the filter shape (filterShapeGuide).
	if !strings.Contains(fake.got.System[0].Text, "not_in") {
		t.Fatalf("intervention system prompt missing filterShapeGuide: %q", fake.got.System[0].Text)
	}
}

// TestProposeInterventionTreeRejectsMismatchedValue proves a value whose JSON token
// does not match the operator class is a generation error, mapped like any other
// proposal parse failure.
func TestProposeInterventionTreeRejectsMismatchedValue(t *testing.T) {
	body := `{"candidates":[{"filters":[{"field":"Age","op":"lte","value":"young"}]}]}`
	fake := &fakeMessages{resp: message(t, anthropic.StopReasonEndTurn, textBlockJSON(body))}
	c := &Client{messages: fake, model: "test-model"}
	if _, err := c.ProposeInterventionTree(context.Background(), "goal",
		domain.EvaluationMatrix{Targets: []domain.Target{{Field: "x", Direction: domain.Maximize, Aggregation: "avg"}}},
		SandboxSchema{}, TreeContext{IsRoot: true, Breadth: 3}); err == nil {
		t.Fatalf("expected a generation error for a threshold op with a non-numeric value")
	}
}

// TestFilterWireToConstraintRejectsShapeMismatch pins the op/value-shape agreement
// that toConstraint alone enforces: the loose filterItem schema accepts a scalar or
// an array for any op, so a membership op with a scalar, an equality op with an
// array, a threshold with a non-number, an empty value, or a non-scalar shape must
// each become a generation error rather than a mis-typed constraint.
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

// TestFilterWireToConstraintMembershipScalars confirms an array value decodes into
// typed Members preserving element types.
func TestFilterWireToConstraintMembershipScalars(t *testing.T) {
	fw := filterWire{Field: "HomePlanet", Op: domain.NotIn, Value: json.RawMessage(`["Earth"]`)}
	c, err := fw.toConstraint()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(c.Members) != 1 || c.Members[0].String == nil || *c.Members[0].String != "Earth" {
		t.Fatalf("not_in members not decoded: %+v", c)
	}
}

func TestRenderFiltersInlineEmpty(t *testing.T) {
	if got := renderFiltersInline(nil); got != "(none)" {
		t.Fatalf("renderFiltersInline(nil) = %q, want (none)", got)
	}
}

// TestDecodeMatrixNumericConstraintUnchanged guards that the matrix hard constraints
// stay numeric-only end to end: a numeric constraint decodes into a bare
// {field, op, value} with no operand/members.
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
		stop anthropic.StopReason
		want error
	}{
		{"refusal", anthropic.StopReasonRefusal, errRefused},
		{"max tokens", anthropic.StopReasonMaxTokens, errMaxTokens},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fake := &fakeMessages{resp: message(t, tc.stop)}
			c := &Client{messages: fake, model: "test-model"}
			if _, err := c.GenerateEvaluationMatrix(context.Background(), "goal", SandboxSchema{}); !errors.Is(err, tc.want) {
				t.Fatalf("error = %v, want wrapped %v", err, tc.want)
			}
		})
	}
}

func TestGenerateEvaluationMatrixPropagatesRequestError(t *testing.T) {
	fake := &fakeMessages{err: errors.New("boom")}
	c := &Client{messages: fake, model: "test-model"}
	if _, err := c.GenerateEvaluationMatrix(context.Background(), "goal", SandboxSchema{}); err == nil {
		t.Fatalf("expected an error when the request fails")
	}
}
