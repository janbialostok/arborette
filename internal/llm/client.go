// Package llm wraps the official Anthropic SDK behind the two structured-output
// calls the Orchestrator's Active Hypothesis Loop needs: translating an analyst
// goal into an Evaluation Matrix, and proposing candidate interventions for the
// hypothesis tree. It is the single contained seam where the codebase departs
// from the hand-rolled-HTTP convention (internal/embedding/ollama.go): the SDK
// is pure Go (no CGO), so it is safe under the CGO_ENABLED=0 orchestrator build,
// and structured outputs plus model-ID currency are materially more involved
// than a hand-rolled client would justify.
package llm

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/anthropics/anthropic-sdk-go"
	"github.com/anthropics/anthropic-sdk-go/option"

	"github.com/arborette/arborette/internal/domain"
)

// Sentinel errors the caller maps: a refusal or a truncated response is a
// wrapped error, never a silently mis-parsed body.
var (
	errRefused   = errors.New("claude declined the request")
	errMaxTokens = errors.New("claude response hit max_tokens before completing")
	errNoText    = errors.New("claude response carried no text block")
)

// maxTokens bounds each structured-output response. Kept well under the
// SDK/HTTP timeout ceiling so the small matrix/proposal bodies never stream.
const maxTokens = 16000

// messagesClient is the one SDK method this package calls, extracted so tests
// can inject a fake without a network round-trip or an API key.
type messagesClient interface {
	New(ctx context.Context, params anthropic.MessageNewParams, opts ...option.RequestOption) (*anthropic.Message, error)
}

// Client issues the Orchestrator's structured-output Claude calls against a
// configured model.
type Client struct {
	messages messagesClient
	model    anthropic.Model
}

// NewClient builds a client from primitive settings (infra-constructor
// convention): the API key authenticates the SDK, the model id is applied to
// every call so a cheaper structured-output model can back demos via config.
func NewClient(apiKey, model string) *Client {
	sdk := anthropic.NewClient(option.WithAPIKey(apiKey))
	return &Client{messages: &sdk.Messages, model: anthropic.Model(model)}
}

// GenerateEvaluationMatrix fits an analyst's plain-English goal to the data
// source's schema, resolving the objective to real columns and expressing it as
// a structured aggregation + value expression + direction per target. The
// response is constrained to the schema-aware matrix schema and unmarshalled
// straight into domain.EvaluationMatrix's json shape.
func (c *Client) GenerateEvaluationMatrix(ctx context.Context, goalText string, schema SandboxSchema) (domain.EvaluationMatrix, error) {
	user := "Analyst goal:\n" + goalText + "\n\nAvailable columns:\n" + columnSummary(schema)
	body, err := c.complete(ctx, anthropic.OutputConfigEffortHigh, evaluationMatrixSchema(schema),
		evaluationMatrixSystem, user)
	if err != nil {
		return domain.EvaluationMatrix{}, err
	}
	var matrix domain.EvaluationMatrix
	if err := json.Unmarshal([]byte(body), &matrix); err != nil {
		return domain.EvaluationMatrix{}, fmt.Errorf("parse evaluation matrix: %w", err)
	}
	return matrix, nil
}

// RepairEvaluationMatrix re-fits the objective after the fitted matrix failed the
// Sandbox's dry-run validation: it re-generates the matrix with the prior fitted
// matrix and the Sandbox's exact validation error in the prompt, so the model can
// correct the specific incompatibility. One attempt only.
func (c *Client) RepairEvaluationMatrix(ctx context.Context, goalText string, schema SandboxSchema, prior domain.EvaluationMatrix, validationErr string) (domain.EvaluationMatrix, error) {
	priorJSON, err := json.Marshal(prior)
	if err != nil {
		return domain.EvaluationMatrix{}, fmt.Errorf("marshal prior matrix: %w", err)
	}
	user := "Analyst goal:\n" + goalText + "\n\nAvailable columns:\n" + columnSummary(schema) +
		"\nThis fitted objective failed to compile against the data source:\n" + string(priorJSON) +
		"\n\nThe sandbox rejected it with:\n" + validationErr +
		"\n\nReturn a corrected Evaluation Matrix whose objective compiles and measures."
	body, err := c.complete(ctx, anthropic.OutputConfigEffortHigh, evaluationMatrixSchema(schema),
		evaluationMatrixRepairSystem, user)
	if err != nil {
		return domain.EvaluationMatrix{}, err
	}
	var matrix domain.EvaluationMatrix
	if err := json.Unmarshal([]byte(body), &matrix); err != nil {
		return domain.EvaluationMatrix{}, fmt.Errorf("parse evaluation matrix: %w", err)
	}
	return matrix, nil
}

// ProposeInterventionTree proposes one node of the hypothesis tree: a set of
// candidate interventions that vary filters only, against the matrix-pinned
// objective and the parent's cumulative filter set. Candidate filters are
// constrained to the introspected columns.
func (c *Client) ProposeInterventionTree(ctx context.Context, goalText string, matrix domain.EvaluationMatrix, schema SandboxSchema, node TreeContext) (Proposal, error) {
	body, err := c.complete(ctx, anthropic.OutputConfigEffortXhigh, interventionTreeSchema(schema),
		interventionTreeSystem, treePrompt(goalText, matrix, schema, node))
	if err != nil {
		return Proposal{}, err
	}
	var wire proposalWire
	if err := json.Unmarshal([]byte(body), &wire); err != nil {
		return Proposal{}, fmt.Errorf("parse intervention proposal: %w", err)
	}
	proposal := Proposal{Candidates: make([]CandidateIntervention, 0, len(wire.Candidates))}
	for _, cand := range wire.Candidates {
		proposal.Candidates = append(proposal.Candidates, CandidateIntervention{Filters: cand.Filters})
	}
	return proposal, nil
}

// proposalWire mirrors the intervention-tree structured-output body: candidates
// carrying filters only.
type proposalWire struct {
	Candidates []struct {
		Filters []domain.Constraint `json:"filters"`
	} `json:"candidates"`
}

// complete issues one structured-output call: adaptive thinking (load-bearing on
// Opus 4.8 — an omitted thinking field runs with none), the requested effort,
// and the json_schema format. It maps a refusal or max_tokens stop reason to a
// sentinel and selects the text block rather than assuming Content[0], which a
// leading thinking block would displace.
func (c *Client) complete(ctx context.Context, effort anthropic.OutputConfigEffort, schema map[string]any, system, user string) (string, error) {
	adaptive := anthropic.ThinkingConfigAdaptiveParam{}
	resp, err := c.messages.New(ctx, anthropic.MessageNewParams{
		Model:     c.model,
		MaxTokens: maxTokens,
		Thinking:  anthropic.ThinkingConfigParamUnion{OfAdaptive: &adaptive},
		OutputConfig: anthropic.OutputConfigParam{
			Effort: effort,
			Format: anthropic.JSONOutputFormatParam{Schema: schema},
		},
		System: []anthropic.TextBlockParam{{Text: system}},
		Messages: []anthropic.MessageParam{
			anthropic.NewUserMessage(anthropic.NewTextBlock(user)),
		},
	})
	if err != nil {
		return "", fmt.Errorf("claude request: %w", err)
	}
	switch resp.StopReason {
	case anthropic.StopReasonRefusal:
		return "", errRefused
	case anthropic.StopReasonMaxTokens:
		return "", errMaxTokens
	}
	return textBlock(resp)
}

// textBlock returns the first text block's content, skipping any leading
// thinking block adaptive thinking may emit.
func textBlock(msg *anthropic.Message) (string, error) {
	for _, block := range msg.Content {
		if tb, ok := block.AsAny().(anthropic.TextBlock); ok {
			return tb.Text, nil
		}
	}
	return "", errNoText
}

const evaluationMatrixSystem = "You fit an analyst's plain-English optimization goal to a specific tabular data " +
	"source, given its columns and types. Return targets to maximize or minimize and any hard-constraint " +
	"boundaries the goal states. Resolve the objective to the actual columns — matching intent, not just names — " +
	"and express each target as an aggregation (count/sum/avg/min/max) over a value expression, with an " +
	"optimization direction. A plain numeric target is a bare column_ref value expression; a boolean or " +
	"categorical target is a cast or comparison indicator (e.g. a rate of transported passengers is avg over the " +
	"comparison Transported = True). Only reference columns present in the provided schema."

const evaluationMatrixRepairSystem = "You fit an analyst's optimization goal to a specific tabular data source. A " +
	"previously fitted objective failed to compile against the data. Given the prior fitted matrix and the exact " +
	"validation error, return a corrected Evaluation Matrix whose objective compiles and measures — resolve the " +
	"objective to the actual columns and types, and express it as an aggregation over a value expression with an " +
	"optimization direction. Only reference columns present in the provided schema."

const interventionTreeSystem = "You propose candidate interventions for an empirical hypothesis tree over a " +
	"tabular data source. Each candidate is a set of hard-constraint filters that segments the data; the run " +
	"measures a single fixed objective aggregate over each segment. The objective is already fixed. Only " +
	"reference columns that exist in the provided schema. Candidates vary filters only — never re-propose the " +
	"objective."

// treePrompt assembles the per-node user message: the goal, the matrix, the
// available columns, and — at deeper nodes — the pinned objective, the parent's
// cumulative filters, and the baseline value to improve against.
func treePrompt(goalText string, matrix domain.EvaluationMatrix, schema SandboxSchema, node TreeContext) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Analyst goal:\n%s\n\n", goalText)
	fmt.Fprintf(&b, "Evaluation Matrix:\n%s\n\n", matrixSummary(matrix))
	fmt.Fprintf(&b, "Available columns:\n%s\n\n", columnSummary(schema))
	if node.IsRoot {
		fmt.Fprintf(&b, "This is the root node. Propose up to %d candidate interventions (filters only) that "+
			"segment the data toward the objective.\n", node.Breadth)
		return b.String()
	}
	fmt.Fprintf(&b, "Fixed objective: %s, to %s.\n", node.ObjectiveLabel, node.Direction)
	fmt.Fprintf(&b, "Parent's effective filters (your proposals nest cumulatively on top of these):\n%s\n",
		filterSummary(node.ParentFilters))
	if node.PriorValue != nil {
		fmt.Fprintf(&b, "Parent's measured objective value (the baseline to improve on): %v\n", *node.PriorValue)
	}
	fmt.Fprintf(&b, "Propose up to %d refinement candidates (additional filters only) that should move the "+
		"objective in the desired direction versus the baseline.\n", node.Breadth)
	return b.String()
}

func matrixSummary(matrix domain.EvaluationMatrix) string {
	var b strings.Builder
	for _, t := range matrix.Targets {
		fmt.Fprintf(&b, "- %s %s\n", t.Direction, domain.RenderObjectiveLabel(t.Aggregation, t.ValueExpression()))
	}
	for _, c := range matrix.Constraints {
		fmt.Fprintf(&b, "- constraint: %s %s %v\n", c.Field, c.Op, c.Value)
	}
	return b.String()
}

func columnSummary(schema SandboxSchema) string {
	var b strings.Builder
	for _, c := range schema.Columns {
		fmt.Fprintf(&b, "- %s (%s)\n", c.Name, c.Type)
	}
	return b.String()
}

func filterSummary(filters []domain.Constraint) string {
	if len(filters) == 0 {
		return "(none)"
	}
	var b strings.Builder
	for _, f := range filters {
		fmt.Fprintf(&b, "- %s %s %v\n", f.Field, f.Op, f.Value)
	}
	return b.String()
}
