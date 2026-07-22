package llm

import (
	"context"
	"encoding/json"
	"errors"
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
	body := `{"targets":[{"field":"revenue","direction":"maximize"}],` +
		`"constraints":[{"field":"cost","op":"lte","value":100}]}`
	fake := &fakeMessages{resp: message(t, anthropic.StopReasonEndTurn, textBlockJSON(body))}
	c := &Client{messages: fake, model: "test-model"}

	matrix, err := c.GenerateEvaluationMatrix(context.Background(), "grow revenue without overspending")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(matrix.Targets) != 1 || matrix.Targets[0].Field != "revenue" || matrix.Targets[0].Direction != domain.Maximize {
		t.Fatalf("unexpected targets: %+v", matrix.Targets)
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
	body := `{"targets":[{"field":"latency","direction":"minimize"}],"constraints":[]}`
	fake := &fakeMessages{resp: message(t, anthropic.StopReasonEndTurn,
		thinkingBlockJSON("considering the goal"),
		textBlockJSON(body),
	)}
	c := &Client{messages: fake, model: "test-model"}

	matrix, err := c.GenerateEvaluationMatrix(context.Background(), "cut latency")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(matrix.Targets) != 1 || matrix.Targets[0].Direction != domain.Minimize {
		t.Fatalf("expected the text block to be selected past the thinking block: %+v", matrix)
	}
}

func TestProposeInterventionTreeRoot(t *testing.T) {
	body := `{"objective_aggregation":"sum","objective_field":"revenue",` +
		`"candidates":[{"filters":[{"field":"region","op":"gte","value":1}]},{"filters":[]}]}`
	fake := &fakeMessages{resp: message(t, anthropic.StopReasonEndTurn, textBlockJSON(body))}
	c := &Client{messages: fake, model: "test-model"}

	schema := SandboxSchema{Columns: []SandboxColumn{{Name: "revenue", Type: "DOUBLE"}, {Name: "region", Type: "BIGINT"}}}
	proposal, err := c.ProposeInterventionTree(context.Background(), "grow revenue",
		domain.EvaluationMatrix{Targets: []domain.Target{{Field: "revenue", Direction: domain.Maximize}}},
		schema, TreeContext{IsRoot: true, Breadth: 3})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if proposal.ObjectiveAggregation != "sum" || proposal.ObjectiveField != "revenue" {
		t.Fatalf("unexpected objective: %+v", proposal)
	}
	if len(proposal.Candidates) != 2 {
		t.Fatalf("expected 2 candidates, got %d", len(proposal.Candidates))
	}
	if len(proposal.Candidates[0].Filters) != 1 || proposal.Candidates[0].Filters[0].Field != "region" {
		t.Fatalf("unexpected first candidate filters: %+v", proposal.Candidates[0])
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
			if _, err := c.GenerateEvaluationMatrix(context.Background(), "goal"); !errors.Is(err, tc.want) {
				t.Fatalf("error = %v, want wrapped %v", err, tc.want)
			}
		})
	}
}

func TestGenerateEvaluationMatrixPropagatesRequestError(t *testing.T) {
	fake := &fakeMessages{err: errors.New("boom")}
	c := &Client{messages: fake, model: "test-model"}
	if _, err := c.GenerateEvaluationMatrix(context.Background(), "goal"); err == nil {
		t.Fatalf("expected an error when the request fails")
	}
}
