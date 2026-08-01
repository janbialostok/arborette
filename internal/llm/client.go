// Package llm wraps LLM providers behind the structured-output calls the
// services need -- matrix fitting, document-field derivation, intervention
// proposal, extraction, and abstraction. Three backends are supported:
// Anthropic (the default), DeepInfra (OpenAI-compatible), and Ollama (local
// models). Switch between them via the LLM_PROVIDER environment variable --
// see config.LLMConfig. ChatClient streams the analyst-facing agent preview
// over the beta Messages API and is Anthropic-only.
//
// The Anthropic backend is the single contained seam where the codebase
// departs from the hand-rolled-HTTP convention (internal/embedding/ollama.go):
// the SDK is pure Go (no CGO), so it is safe under the CGO_ENABLED=0 service
// builds, and structured outputs, streaming, and model-ID currency are
// materially more involved than a hand-rolled client would justify. The
// DeepInfra and Ollama backends speak their REST APIs directly.
package llm

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/arborette/arborette/internal/config"
	"github.com/arborette/arborette/internal/domain"
)

var (
	errRefused   = errors.New("model declined the request")
	errMaxTokens = errors.New("model response hit max_tokens before completing")
	errNoText    = errors.New("model response carried no text block")

	_ Client = (*client)(nil)
)

const maxTokens = 16000

// Client is the LLM interface consumed by the orchestrator and sleep-cycle
// worker. Every method takes structured inputs and returns structured outputs
// using the shared prompt and response-decode machinery below.
type Client interface {
	GenerateEvaluationMatrix(ctx context.Context, goalText string, schema SandboxSchema) (domain.EvaluationMatrix, error)
	RepairEvaluationMatrix(ctx context.Context, goalText string, schema SandboxSchema, prior domain.EvaluationMatrix, validationErr string) (domain.EvaluationMatrix, error)
	ProposeInterventionTree(ctx context.Context, goalText string, matrix domain.EvaluationMatrix, schema SandboxSchema, node TreeContext) (Proposal, error)
	RepairInterventionTree(ctx context.Context, goalText string, matrix domain.EvaluationMatrix, schema SandboxSchema, node TreeContext, prior Proposal, validationErr string) (Proposal, error)
	IntrospectDocumentFields(ctx context.Context, goalText, sample string) ([]domain.TargetField, error)
	Extract(ctx context.Context, pdf []byte, field domain.TargetField, method string) (string, float64, error)
	AbstractMetaHeuristic(ctx context.Context, goalText string, seg MacroSegment) (Abstraction, error)
	RepairMetaHeuristic(ctx context.Context, goalText string, seg MacroSegment, prior Abstraction, validationErr string) (Abstraction, error)
}

// NewClient builds an LLM client from configuration. The LLM_PROVIDER env
// selects the backend — "anthropic" (default), "deepinfra", or "ollama".
func NewClient(cfg config.LLMConfig) (Client, error) {
	var b completer
	switch cfg.Provider {
	case "anthropic":
		var err error
		b, err = newAnthropicCompleter(cfg.Anthropic.APIKey, cfg.Anthropic.Model)
		if err != nil {
			return nil, err
		}
	case "deepinfra":
		var err error
		b, err = newDeepInfraCompleter(cfg.DeepInfra.APIKey, cfg.DeepInfra.Model, cfg.DeepInfra.BaseURL)
		if err != nil {
			return nil, err
		}
	case "ollama":
		var err error
		b, err = newOllamaCompleter(cfg.Ollama.Endpoint, cfg.Ollama.Model)
		if err != nil {
			return nil, err
		}
	default:
		return nil, fmt.Errorf("llm: unsupported provider %q", cfg.Provider)
	}
	return &client{backend: b}, nil
}

// completer is the pluggable API backend. Each LLM provider implements this
// interface with its own HTTP/SDK calls.
type completer interface {
	complete(ctx context.Context, system, user string, schema map[string]any) (string, error)
	completeWithPDF(ctx context.Context, system string, pdf []byte, instruction string, schema map[string]any) (string, error)
}

// client is the common implementation of Client that delegates the actual API
// call to a pluggable completer backend.
type client struct {
	backend completer
}

func (c *client) GenerateEvaluationMatrix(ctx context.Context, goalText string, schema SandboxSchema) (domain.EvaluationMatrix, error) {
	fence := NewFence("GOAL-CONTEXT")
	user := "Analyst goal:\n" + fence.Wrap(goalText) + "\n\nAvailable columns:\n" + fence.Wrap(columnSummary(schema))
	body, err := c.backend.complete(ctx, evaluationMatrixSystem+fence.Directive(), user, evaluationMatrixSchema(schema))
	if err != nil {
		return domain.EvaluationMatrix{}, err
	}
	return decodeMatrix(body)
}

func (c *client) RepairEvaluationMatrix(ctx context.Context, goalText string, schema SandboxSchema, prior domain.EvaluationMatrix, validationErr string) (domain.EvaluationMatrix, error) {
	priorJSON, err := encodeMatrix(prior)
	if err != nil {
		return domain.EvaluationMatrix{}, fmt.Errorf("encode prior matrix: %w", err)
	}
	fence := NewFence("GOAL-CONTEXT")
	user := "Analyst goal:\n" + fence.Wrap(goalText) + "\n\nAvailable columns:\n" + fence.Wrap(columnSummary(schema)) +
		"\nThis fitted objective failed to compile against the data source:\n" + fence.Wrap(string(priorJSON)) +
		"\n\nThe sandbox rejected it with:\n" + fence.Wrap(validationErr) +
		"\n\nReturn a corrected Evaluation Matrix whose objective compiles and measures."
	body, err := c.backend.complete(ctx, evaluationMatrixRepairSystem+fence.Directive(), user, evaluationMatrixSchema(schema))
	if err != nil {
		return domain.EvaluationMatrix{}, err
	}
	return decodeMatrix(body)
}

func (c *client) ProposeInterventionTree(ctx context.Context, goalText string, matrix domain.EvaluationMatrix, schema SandboxSchema, node TreeContext) (Proposal, error) {
	fence := NewFence("GOAL-CONTEXT")
	body, err := c.backend.complete(ctx, interventionTreeSystem+fence.Directive(),
		treePrompt(fence, goalText, matrix, schema, node), interventionTreeSchema(schema))
	if err != nil {
		return Proposal{}, err
	}
	return decodeProposal(body)
}

func (c *client) RepairInterventionTree(ctx context.Context, goalText string, matrix domain.EvaluationMatrix, schema SandboxSchema, node TreeContext, prior Proposal, validationErr string) (Proposal, error) {
	fence := NewFence("GOAL-CONTEXT")
	user := treePrompt(fence, goalText, matrix, schema, node) +
		"\nThese proposed candidates referenced columns absent from the schema:\n" + fence.Wrap(renderCandidates(prior.Candidates)) +
		"\n\nThe rejection:\n" + fence.Wrap(validationErr) +
		"\n\nRe-propose the candidates using only the listed columns, with their exact names."
	body, err := c.backend.complete(ctx, interventionTreeRepairSystem+fence.Directive(), user, interventionTreeSchema(schema))
	if err != nil {
		return Proposal{}, err
	}
	return decodeProposal(body)
}

func (c *client) IntrospectDocumentFields(ctx context.Context, goalText, sample string) ([]domain.TargetField, error) {
	fence := NewFence("DOC-CONTEXT")
	user := "Analyst goal:\n" + fence.Wrap(goalText) + "\n\nDocument sample (first page):\n" + fence.Wrap(sample)
	body, err := c.backend.complete(ctx, documentFieldsSystem+fence.Directive(), user, documentFieldsSchema())
	if err != nil {
		return nil, err
	}
	return decodeFields(body)
}

func (c *client) Extract(ctx context.Context, pdf []byte, field domain.TargetField, method string) (string, float64, error) {
	fence := NewFence("FIELD-CONTEXT")
	body, err := c.backend.completeWithPDF(ctx, extractionSystem+fence.Directive(), pdf,
		extractionInstruction(fence, field, method), extractionSchema())
	if err != nil {
		return "", 0, err
	}
	return decodeExtraction(body)
}

func (c *client) AbstractMetaHeuristic(ctx context.Context, goalText string, seg MacroSegment) (Abstraction, error) {
	fence := NewFence("SEGMENT-CONTEXT")
	body, err := c.backend.complete(ctx, metaHeuristicSystem+fence.Directive(),
		macroSegmentPrompt(fence, goalText, seg), metaHeuristicSchema())
	if err != nil {
		return Abstraction{}, err
	}
	return decodeAbstraction(body)
}

func (c *client) RepairMetaHeuristic(ctx context.Context, goalText string, seg MacroSegment, prior Abstraction, validationErr string) (Abstraction, error) {
	fence := NewFence("SEGMENT-CONTEXT")
	user := macroSegmentPrompt(fence, goalText, seg) +
		"\nThis definition still referenced concrete, dataset-bound terms:\n" + fence.Wrap(prior.Definition) +
		"\n\nThe rejection:\n" + fence.Wrap(validationErr) +
		"\n\nRewrite the definition so every variable is a bracketed ontology term and no raw column name survives."
	body, err := c.backend.complete(ctx, metaHeuristicRepairSystem+fence.Directive(), user, metaHeuristicSchema())
	if err != nil {
		return Abstraction{}, err
	}
	return decodeAbstraction(body)
}

// ----- Shared response-wire types and decoders -----

type documentFieldsWire struct {
	Fields []struct {
		Name        string `json:"name"`
		Description string `json:"description"`
	} `json:"fields"`
}

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

type extractionWire struct {
	Value      string  `json:"value"`
	Confidence float64 `json:"confidence"`
}

func decodeExtraction(body string) (string, float64, error) {
	var wire extractionWire
	if err := json.Unmarshal([]byte(body), &wire); err != nil {
		return "", 0, fmt.Errorf("parse extraction: %w", err)
	}
	return wire.Value, wire.Confidence, nil
}

func extractionInstruction(fence Fence, field domain.TargetField, method string) string {
	var b strings.Builder
	b.WriteString("Extract this field from the attached document:\n")
	b.WriteString(fence.Wrap(fmt.Sprintf("- %s: %s", field.Name, field.Description)))
	b.WriteString("\n")
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

type proposalWire struct {
	Candidates []struct {
		Filters []filterWire `json:"filters"`
	} `json:"candidates"`
}

type filterWire struct {
	Field string              `json:"field"`
	Op    domain.ConstraintOp `json:"op"`
	Value json.RawMessage     `json:"value"`
}

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

type evaluationMatrixWire struct {
	Targets     []matrixTargetWire  `json:"targets"`
	Constraints []domain.Constraint `json:"constraints"`
}

type matrixTargetWire struct {
	Direction   domain.TargetDirection `json:"direction"`
	Aggregation string                 `json:"aggregation"`
	Value       string                 `json:"value"`
}

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

// ----- Prompt system constants and helpers -----

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

const filterShapeGuide = ` Each filter is {"field":<column>,"op":<operator>,"value":<value>}. Choose the operator and value by the column's type: a numeric column uses "lt"/"lte"/"gt"/"gte" with a number (e.g. {"field":"Age","op":"lte","value":18}); a boolean column uses "eq"/"neq" with true or false (e.g. {"field":"CryoSleep","op":"eq","value":true}); a categorical/string column uses "eq"/"neq" with a string, or "in"/"not_in" with an array of strings (e.g. {"field":"HomePlanet","op":"in","value":["Europa","Mars"]}). The value is a bare JSON scalar for eq/neq and the threshold ops, and a JSON array for in/not_in — never a quoted-JSON string. Use only an operator and value type that match the column's type.`

func treePrompt(fence Fence, goalText string, matrix domain.EvaluationMatrix, schema SandboxSchema, node TreeContext) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Analyst goal:\n%s\n\n", fence.Wrap(goalText))
	fmt.Fprintf(&b, "Evaluation Matrix:\n%s\n\n", fence.Wrap(MatrixSummary(matrix)))
	fmt.Fprintf(&b, "Available columns:\n%s\n\n", fence.Wrap(columnSummary(schema)))
	if node.IsRoot {
		fmt.Fprintf(&b, "This is the root node. Propose up to %d candidate interventions (filters only) that "+
			"segment the data toward the objective.\n", node.Breadth)
		return b.String()
	}
	fmt.Fprintf(&b, "Fixed objective (to %s):\n%s\n", node.Direction, fence.Wrap(node.ObjectiveLabel))
	fmt.Fprintf(&b, "Parent's effective filters (your proposals nest cumulatively on top of these):\n%s\n",
		fence.Wrap(filterSummary(node.ParentFilters)))
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
