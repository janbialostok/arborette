// Package llm wraps the official Anthropic SDK behind the Claude calls the
// services need. Client issues the structured-output calls -- matrix fitting,
// document-field derivation, intervention proposal, extraction, and abstraction.
// ChatClient streams the analyst-facing agent preview over the beta Messages
// API.
//
// This is the single contained seam where the codebase departs from the
// hand-rolled-HTTP convention (internal/embedding/ollama.go): the SDK is pure Go
// (no CGO), so it is safe under the CGO_ENABLED=0 service builds, and structured
// outputs, streaming, and model-ID currency are materially more involved than a
// hand-rolled client would justify.
package llm

import (
	"bytes"
	"context"
	"encoding/base64"
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
	return decodeProposal(body)
}

// RepairInterventionTree re-proposes a node's candidates after some referenced
// filter columns were absent from the schema: it re-generates the proposal with
// the rejected candidates and the exact naming error in the prompt, so the model
// can re-propose using only real columns. The objective stays pinned from the
// matrix, never proposed here.
func (c *Client) RepairInterventionTree(ctx context.Context, goalText string, matrix domain.EvaluationMatrix, schema SandboxSchema, node TreeContext, prior Proposal, validationErr string) (Proposal, error) {
	user := treePrompt(goalText, matrix, schema, node) +
		"\nThese proposed candidates referenced columns absent from the schema:\n" + renderCandidates(prior.Candidates) +
		"\n\nThe rejection:\n" + validationErr +
		"\n\nRe-propose the candidates using only the listed columns, with their exact names."
	body, err := c.complete(ctx, anthropic.OutputConfigEffortXhigh, interventionTreeSchema(schema),
		interventionTreeRepairSystem, user)
	if err != nil {
		return Proposal{}, err
	}
	return decodeProposal(body)
}

// IntrospectDocumentFields derives the fields an analyst wants extracted from a
// document, grounded in the goal text and a sample of the document. The sandbox
// cannot do this (it holds no Claude client), so the document intake path calls
// it after the sandbox reports Kind: document plus a first-page sample. An empty
// result means no field matched the goal; the caller rejects that at intake.
func (c *Client) IntrospectDocumentFields(ctx context.Context, goalText, sample string) ([]domain.TargetField, error) {
	user := "Analyst goal:\n" + goalText + "\n\nDocument sample (first page):\n" + sample
	body, err := c.complete(ctx, anthropic.OutputConfigEffortHigh, documentFieldsSchema(),
		documentFieldsSystem, user)
	if err != nil {
		return nil, err
	}
	return decodeFields(body)
}

// Extract measures one document field: it sends the whole PDF as an inline
// base64 document block plus a field- and method-specific instruction, and
// returns the extracted value string plus the model's self-reported confidence in
// [0,1]. It deliberately does NOT request Citations — Citations and structured
// outputs are mutually exclusive on the Claude API — so the strict output schema
// keeps the value typed and provenance is computed separately by a deterministic
// text search over the source pages. Base64 for the MVP; the Files API is the
// scale path for large or repeated PDFs. method varies the extraction approach per
// competing sibling (the sub-tree breadth axis).
func (c *Client) Extract(ctx context.Context, pdf []byte, field domain.TargetField, method string) (string, float64, error) {
	doc := anthropic.NewDocumentBlock(anthropic.Base64PDFSourceParam{
		Data: base64.StdEncoding.EncodeToString(pdf),
	})
	body, err := c.completeWithContent(ctx, anthropic.OutputConfigEffortHigh, extractionSchema(),
		extractionSystem, []anthropic.ContentBlockParamUnion{doc, anthropic.NewTextBlock(extractionInstruction(field, method))})
	if err != nil {
		return "", 0, err
	}
	return decodeExtraction(body)
}

// documentFieldsWire mirrors the document-field structured-output body.
type documentFieldsWire struct {
	Fields []struct {
		Name        string `json:"name"`
		Description string `json:"description"`
	} `json:"fields"`
}

// decodeFields parses a document-field body into domain.TargetFields. A malformed
// body is a generation error, mapped by the caller like any other parse failure.
func decodeFields(body string) ([]domain.TargetField, error) {
	var wire documentFieldsWire
	if err := json.Unmarshal([]byte(body), &wire); err != nil {
		return nil, fmt.Errorf("parse document fields: %w", err)
	}
	fields := make([]domain.TargetField, 0, len(wire.Fields))
	for _, f := range wire.Fields {
		fields = append(fields, domain.TargetField{Name: f.Name, Description: f.Description})
	}
	return fields, nil
}

// extractionWire mirrors the extraction structured-output body: the value as a
// plain string plus a confidence.
type extractionWire struct {
	Value      string  `json:"value"`
	Confidence float64 `json:"confidence"`
}

// decodeExtraction parses an extraction body into the extracted value string and
// confidence. A malformed body is a generation error.
func decodeExtraction(body string) (string, float64, error) {
	var wire extractionWire
	if err := json.Unmarshal([]byte(body), &wire); err != nil {
		return "", 0, fmt.Errorf("parse extraction: %w", err)
	}
	return wire.Value, wire.Confidence, nil
}

// extractionInstruction assembles the per-node user message: the field to extract
// and, when set, the competing method that varies this sibling's approach.
func extractionInstruction(field domain.TargetField, method string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Extract this field from the attached document:\n- %s: %s\n", field.Name, field.Description)
	if method != "" {
		fmt.Fprintf(&b, "\nApproach: %s\n", method)
	}
	b.WriteString("\nReturn the extracted value verbatim from the document where possible, plus your confidence (0 to 1).")
	return b.String()
}

const documentFieldsSystem = "You identify the fields an analyst wants extracted from a document, given their " +
	"plain-English goal and a sample of the document's text. Return a small set of concrete, individually " +
	"extractable fields, each with a short name and a one-line description of what to extract. Ground the fields " +
	"in the goal and the sample — do not invent fields the goal does not ask for, and do not return a field the " +
	"goal clearly does not want."

const extractionSystem = "You extract a single requested field from a document. Return the field's value exactly as " +
	"it appears in the document (verbatim where possible, so it can be located in the source text), and a " +
	"confidence between 0 and 1 reflecting how certain you are the value is correct and complete. If the field is " +
	"absent from the document, return an empty value with a low confidence rather than guessing."

// proposalWire mirrors the intervention-tree structured-output body: candidates
// carrying filters only. Each filter's value is polymorphic (a bare scalar or a flat
// array), so it is captured as filterWire and decoded into a typed domain.Constraint.
type proposalWire struct {
	Candidates []struct {
		Filters []filterWire `json:"filters"`
	} `json:"candidates"`
}

// filterWire mirrors one intervention filter from the structured-output body: field,
// op, and a polymorphic value. domain.LiteralValue is a JSON object and cannot
// unmarshal a bare scalar, so the value is captured raw and token-sniffed in
// toConstraint — a distinct decode path from evaluationMatrixWire, which stays
// numeric.
type filterWire struct {
	Field string              `json:"field"`
	Op    domain.ConstraintOp `json:"op"`
	Value json.RawMessage     `json:"value"`
}

// decodeProposal parses an intervention-tree body and maps each candidate's filters
// into typed domain.Constraints. A malformed body or a type-mismatched filter value
// is a generation error, mapped by the caller like any other proposal parse failure.
func decodeProposal(body string) (Proposal, error) {
	var wire proposalWire
	if err := json.Unmarshal([]byte(body), &wire); err != nil {
		return Proposal{}, fmt.Errorf("parse intervention proposal: %w", err)
	}
	proposal := Proposal{Candidates: make([]CandidateIntervention, 0, len(wire.Candidates))}
	for _, cand := range wire.Candidates {
		filters := make([]domain.Constraint, 0, len(cand.Filters))
		for _, fw := range cand.Filters {
			f, err := fw.toConstraint()
			if err != nil {
				return Proposal{}, fmt.Errorf("parse intervention proposal: %w", err)
			}
			filters = append(filters, f)
		}
		proposal.Candidates = append(proposal.Candidates, CandidateIntervention{Filters: filters})
	}
	return proposal, nil
}

// toConstraint maps a filterWire into a typed domain.Constraint by sniffing the raw
// JSON token of value: a JSON array feeds Members (in/not_in); a bare number/string/
// bool scalar feeds Value (numeric thresholds) or Operand (eq/neq). A value whose
// JSON shape does not match the operator class is a generation error.
func (f filterWire) toConstraint() (domain.Constraint, error) {
	c := domain.Constraint{Field: f.Field, Op: f.Op}
	trimmed := bytes.TrimSpace(f.Value)
	if len(trimmed) == 0 {
		return domain.Constraint{}, fmt.Errorf("filter %q: empty value", f.Field)
	}
	isArray := trimmed[0] == '['
	switch {
	case c.IsMembershipOp():
		if !isArray {
			return domain.Constraint{}, fmt.Errorf("filter %q: %s requires a set value", f.Field, f.Op)
		}
		members, err := scalarLiterals(trimmed)
		if err != nil {
			return domain.Constraint{}, fmt.Errorf("filter %q: %w", f.Field, err)
		}
		c.Members = members
	case c.IsNumericThresholdOp():
		lit, err := scalarLiteral(trimmed)
		if err != nil {
			return domain.Constraint{}, fmt.Errorf("filter %q: %w", f.Field, err)
		}
		if lit.Number == nil {
			return domain.Constraint{}, fmt.Errorf("filter %q: %s requires a numeric value", f.Field, f.Op)
		}
		c.Value = *lit.Number
	case c.IsEqualityOp():
		lit, err := scalarLiteral(trimmed)
		if err != nil {
			return domain.Constraint{}, fmt.Errorf("filter %q: %w", f.Field, err)
		}
		c.Operand = &lit
	default:
		return domain.Constraint{}, fmt.Errorf("filter %q: unsupported operator %q", f.Field, f.Op)
	}
	return c, nil
}

// scalarLiteral parses one JSON scalar (number, string, or boolean) into a typed
// domain.LiteralValue; any other JSON shape (array, object, null) is an error.
func scalarLiteral(raw json.RawMessage) (domain.LiteralValue, error) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		return domain.LiteralValue{}, fmt.Errorf("parse scalar value: %w", err)
	}
	switch n := v.(type) {
	case json.Number:
		f, err := n.Float64()
		if err != nil {
			return domain.LiteralValue{}, fmt.Errorf("parse numeric value: %w", err)
		}
		return domain.LiteralValue{Number: &f}, nil
	case string:
		s := n
		return domain.LiteralValue{String: &s}, nil
	case bool:
		b := n
		return domain.LiteralValue{Bool: &b}, nil
	default:
		return domain.LiteralValue{}, fmt.Errorf("value is not a scalar")
	}
}

func scalarLiterals(raw json.RawMessage) ([]domain.LiteralValue, error) {
	var items []json.RawMessage
	if err := json.Unmarshal(raw, &items); err != nil {
		return nil, fmt.Errorf("parse set value: %w", err)
	}
	lits := make([]domain.LiteralValue, 0, len(items))
	for _, it := range items {
		lit, err := scalarLiteral(it)
		if err != nil {
			return nil, err
		}
		lits = append(lits, lit)
	}
	return lits, nil
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

// complete issues one text-only structured-output call, delegating to
// completeWithContent with a single user text block.
func (c *Client) complete(ctx context.Context, effort anthropic.OutputConfigEffort, schema map[string]any, system, user string) (string, error) {
	return c.completeWithContent(ctx, effort, schema, system, []anthropic.ContentBlockParamUnion{anthropic.NewTextBlock(user)})
}

// completeWithContent issues one structured-output call over an arbitrary user
// content block list: adaptive thinking (load-bearing on Opus 4.8 — an omitted
// thinking field runs with none), the requested effort, and the json_schema
// format. It maps a refusal or max_tokens stop reason to a sentinel and selects
// the text block rather than assuming Content[0], which a leading thinking block
// would displace.
func (c *Client) completeWithContent(ctx context.Context, effort anthropic.OutputConfigEffort, schema map[string]any, system string, content []anthropic.ContentBlockParamUnion) (string, error) {
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
			anthropic.NewUserMessage(content...),
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
	"tabular data source. Each candidate is a set of filters that segments the data; the run " +
	"measures a single fixed objective aggregate over each segment. The objective is already fixed. Filter fields " +
	"must reference ONLY columns present in the provided \"Available columns\" list, using each column's exact " +
	"name as written there — a name absent from the list will be rejected. Candidates vary filters only — never " +
	"re-propose the objective." + filterShapeGuide

const interventionTreeRepairSystem = "You propose candidate interventions for an empirical hypothesis tree over a " +
	"tabular data source. A previous proposal referenced filter columns absent from the schema. Given the rejected " +
	"candidates and the naming error, re-propose the candidates as filters that segment the data " +
	"toward the fixed objective. Filter fields must reference ONLY columns present in the provided \"Available " +
	"columns\" list, using each column's exact name as written there. Candidates vary filters only — never " +
	"re-propose the objective." + filterShapeGuide

// filterShapeGuide grounds the intervention filter shape by column type, so the model
// emits an operator and value that match the column and compile in the sandbox. It is
// appended to the intervention-tree system prompts, à la expressionShapeGuide.
const filterShapeGuide = ` Each filter is {"field":<column>,"op":<operator>,"value":<value>}. Choose the operator and value by the column's type: a numeric column uses "lt"/"lte"/"gt"/"gte" with a number (e.g. {"field":"Age","op":"lte","value":18}); a boolean column uses "eq"/"neq" with true or false (e.g. {"field":"CryoSleep","op":"eq","value":true}); a categorical/string column uses "eq"/"neq" with a string, or "in"/"not_in" with an array of strings (e.g. {"field":"HomePlanet","op":"in","value":["Europa","Mars"]}). The value is a bare JSON scalar for eq/neq and the threshold ops, and a JSON array for in/not_in — never a quoted-JSON string. Use only an operator and value type that match the column's type.`

// treePrompt assembles the per-node user message: the goal, the matrix, the
// available columns, and — at deeper nodes — the pinned objective, the parent's
// cumulative filters, and the baseline value to improve against.
func treePrompt(goalText string, matrix domain.EvaluationMatrix, schema SandboxSchema, node TreeContext) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Analyst goal:\n%s\n\n", goalText)
	fmt.Fprintf(&b, "Evaluation Matrix:\n%s\n\n", MatrixSummary(matrix))
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

// MatrixSummary renders an Evaluation Matrix as the readable target/constraint
// lines Claude is shown. Exported so every prompt describing an objective renders
// it identically; two renderings that drift would describe the same run
// differently to the same model.
func MatrixSummary(matrix domain.EvaluationMatrix) string {
	var b strings.Builder
	for _, t := range matrix.Targets {
		fmt.Fprintf(&b, "- %s %s\n", t.Direction, domain.RenderObjectiveLabel(t.Aggregation, t.ValueExpression()))
	}
	for _, c := range matrix.Constraints {
		fmt.Fprintf(&b, "- constraint: %s\n", domain.RenderConstraint(c))
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
		fmt.Fprintf(&b, "- %s\n", domain.RenderConstraint(f))
	}
	return b.String()
}

// renderCandidates renders rejected candidates as readable filter chips for the
// repair prompt, matching the flat filter shape the output schema demands rather than
// domain.Constraint's internal json (value:0, operand:{...}).
func renderCandidates(candidates []CandidateIntervention) string {
	var b strings.Builder
	for i, cand := range candidates {
		fmt.Fprintf(&b, "- candidate %d: %s\n", i+1, renderFiltersInline(cand.Filters))
	}
	return b.String()
}

func renderFiltersInline(filters []domain.Constraint) string {
	if len(filters) == 0 {
		return "(none)"
	}
	parts := make([]string, 0, len(filters))
	for _, f := range filters {
		parts = append(parts, domain.RenderConstraint(f))
	}
	return strings.Join(parts, ", ")
}
