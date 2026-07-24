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
	return decodeMatrix(body)
}

// RepairEvaluationMatrix re-fits the objective after the fitted matrix failed the
// Sandbox's dry-run validation: it re-generates the matrix with the prior fitted
// matrix and the Sandbox's exact validation error in the prompt, so the model can
// correct the specific incompatibility. One generation per call; the caller bounds
// how many times it retries.
func (c *Client) RepairEvaluationMatrix(ctx context.Context, goalText string, schema SandboxSchema, prior domain.EvaluationMatrix, validationErr string) (domain.EvaluationMatrix, error) {
	priorJSON, err := encodeMatrix(prior)
	if err != nil {
		return domain.EvaluationMatrix{}, fmt.Errorf("encode prior matrix: %w", err)
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
	return decodeMatrix(body)
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

// RepairInterventionTree re-proposes a node's candidates after some referenced
// filter columns were absent from the schema: it re-generates the proposal with
// the rejected candidates and the exact naming error in the prompt, so the model
// can re-propose using only real columns. The objective stays pinned from the
// matrix, never proposed here.
func (c *Client) RepairInterventionTree(ctx context.Context, goalText string, matrix domain.EvaluationMatrix, schema SandboxSchema, node TreeContext, prior Proposal, validationErr string) (Proposal, error) {
	priorJSON, err := json.Marshal(prior.Candidates)
	if err != nil {
		return Proposal{}, fmt.Errorf("marshal rejected candidates: %w", err)
	}
	user := treePrompt(goalText, matrix, schema, node) +
		"\nThese proposed candidates referenced columns absent from the schema:\n" + string(priorJSON) +
		"\n\nThe rejection:\n" + validationErr +
		"\n\nRe-propose the candidates using only the listed columns, with their exact names."
	body, err := c.complete(ctx, anthropic.OutputConfigEffortXhigh, interventionTreeSchema(schema),
		interventionTreeRepairSystem, user)
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

// evaluationMatrixWire mirrors the matrix structured-output body: each target's
// value expression is a plain string holding a JSON-encoded domain.Expression, not
// the AST object itself, because the output schema does not carry the recursive
// AST. decodeMatrix re-parses each string into the flat fat node.
type evaluationMatrixWire struct {
	Targets     []matrixTargetWire  `json:"targets"`
	Constraints []domain.Constraint `json:"constraints"`
}

// matrixTargetWire is one target in the matrix wire shape.
type matrixTargetWire struct {
	Direction   domain.TargetDirection `json:"direction"`
	Aggregation string                 `json:"aggregation"`
	Value       string                 `json:"value"`
}

// decodeMatrix decodes a matrix structured-output body into domain.EvaluationMatrix,
// re-parsing each target's JSON-string value into a domain.Expression. A malformed
// outer body or inner value string is a generation error (the caller maps it to a
// 502, matching the prior top-level parse-failure handling); the depth guard on the
// parsed AST is enforced downstream in the sandbox dry-run + repair loop.
func decodeMatrix(body string) (domain.EvaluationMatrix, error) {
	var wire evaluationMatrixWire
	if err := json.Unmarshal([]byte(body), &wire); err != nil {
		return domain.EvaluationMatrix{}, fmt.Errorf("parse evaluation matrix: %w", err)
	}
	targets := make([]domain.Target, 0, len(wire.Targets))
	for i, t := range wire.Targets {
		var expr domain.Expression
		if err := json.Unmarshal([]byte(t.Value), &expr); err != nil {
			return domain.EvaluationMatrix{}, fmt.Errorf("parse target %d value expression: %w", i, err)
		}
		targets = append(targets, domain.Target{Direction: t.Direction, Aggregation: t.Aggregation, Value: &expr})
	}
	return domain.EvaluationMatrix{Targets: targets, Constraints: wire.Constraints}, nil
}

// encodeMatrix re-encodes a matrix into the JSON-string-value wire body, the inverse
// of decodeMatrix, so the repair prompt shows the model the prior matrix in the exact
// form its output schema now demands (value as a JSON string, not a bare object). Each
// target's resolved value expression is marshaled back into the string field.
func encodeMatrix(matrix domain.EvaluationMatrix) ([]byte, error) {
	wire := evaluationMatrixWire{Constraints: matrix.Constraints}
	for i, t := range matrix.Targets {
		valueJSON, err := json.Marshal(t.ValueExpression())
		if err != nil {
			return nil, fmt.Errorf("marshal target %d value expression: %w", i, err)
		}
		wire.Targets = append(wire.Targets, matrixTargetWire{Direction: t.Direction, Aggregation: t.Aggregation, Value: string(valueJSON)})
	}
	return json.Marshal(wire)
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
	"comparison Transported = True). Reference ONLY columns present in the provided \"Available columns\" list, " +
	"and use each column's exact name as written there — a name absent from the list will not compile." +
	expressionShapeGuide

const evaluationMatrixRepairSystem = "You fit an analyst's optimization goal to a specific tabular data source. A " +
	"previously fitted objective failed to compile against the data. Given the prior fitted matrix and the exact " +
	"validation error, return a corrected Evaluation Matrix whose objective compiles and measures — resolve the " +
	"objective to the actual columns and types, and express it as an aggregation over a value expression with an " +
	"optimization direction. Reference ONLY columns present in the provided \"Available columns\" list, and use " +
	"each column's exact name as written there — a name absent from the list will not compile." +
	expressionShapeGuide

// expressionShapeGuide grounds the objective value expression, which the output
// schema carries as a plain string rather than a strict AST. It describes the
// domain.Expression JSON shape the model must emit inside that string, so the
// generated value round-trips through domain.Expression's json tags.
const expressionShapeGuide = ` Each target's "value" field MUST be a JSON string containing a JSON object that encodes the value expression — not a bare object. That object is a discriminated union on a "kind" field, one of: column_ref {"kind":"column_ref","column":<name>}; literal {"kind":"literal","literal":{"number"|"string"|"bool":<value>}}; cast {"kind":"cast","operand":<expr>,"cast_type":<type>}; comparison {"kind":"comparison","op":<op>,"left":<expr>,"right":<expr>}; arithmetic {"kind":"arithmetic","op":<op>,"left":<expr>,"right":<expr>}; case {"kind":"case","cases":[{"when":<expr>,"then":<expr>}],"else":<expr>}. Nested expressions are the same object shape. A plain numeric objective is a bare column_ref, e.g. "value":"{\"kind\":\"column_ref\",\"column\":\"revenue\"}". A boolean rate is avg over a comparison, e.g. "value":"{\"kind\":\"comparison\",\"op\":\"=\",\"left\":{\"kind\":\"column_ref\",\"column\":\"Transported\"},\"right\":{\"kind\":\"literal\",\"literal\":{\"bool\":true}}}". A bucketed rate is avg over a case, e.g. avg(CASE WHEN age > 18 THEN 1 ELSE 0 END) is a case whose one branch compares age > 18 then literal 1, else literal 0. Reference only columns from the "Available columns" list inside the expression.`

const interventionTreeSystem = "You propose candidate interventions for an empirical hypothesis tree over a " +
	"tabular data source. Each candidate is a set of hard-constraint filters that segments the data; the run " +
	"measures a single fixed objective aggregate over each segment. The objective is already fixed. Filter fields " +
	"must reference ONLY columns present in the provided \"Available columns\" list, using each column's exact " +
	"name as written there — a name absent from the list will be rejected. Candidates vary filters only — never " +
	"re-propose the objective."

const interventionTreeRepairSystem = "You propose candidate interventions for an empirical hypothesis tree over a " +
	"tabular data source. A previous proposal referenced filter columns absent from the schema. Given the rejected " +
	"candidates and the naming error, re-propose the candidates as hard-constraint filters that segment the data " +
	"toward the fixed objective. Filter fields must reference ONLY columns present in the provided \"Available " +
	"columns\" list, using each column's exact name as written there. Candidates vary filters only — never " +
	"re-propose the objective."

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
