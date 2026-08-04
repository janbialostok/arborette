// Package mcpserver is the read-side interface for downstream AI agents: it
// exposes the accumulated Meta-Heuristic knowledge (get_optimized_heuristics,
// trace_causal_chain), goal registration (submit_analyst_goal), and on-demand
// causal verification of a finding (verify_finding) as MCP tools over Streamable
// HTTP. Everything served carries its epistemic label, so an agent can weight
// causally verified knowledge above correlation. The read tools sit on the shared
// heuristics query seam; the server never mutates state directly, proxying both
// mutations to the Orchestrator's REST surface.
package mcpserver

import (
	"context"
	"errors"
	"log"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/arborette/arborette/internal/domain"
	"github.com/arborette/arborette/internal/graph"
	"github.com/arborette/arborette/internal/heuristics"
	"github.com/arborette/arborette/internal/orchestratorclient"
	"github.com/arborette/arborette/internal/store"
)

// The registered tool names, exported because the chat connector's allowlist is
// pinned against them.
const (
	ToolGetOptimizedHeuristics = "get_optimized_heuristics"
	ToolTraceCausalChain       = "trace_causal_chain"
	ToolSubmitAnalystGoal      = "submit_analyst_goal"
	ToolVerifyFinding          = "verify_finding"
)

// defaultSearchK is the similarity-search result count used when a call omits or
// malforms k; maxSearchK clamps how large a top-k a caller can request. These
// mirror the Orchestrator's REST handler so both read surfaces behave alike.
const (
	defaultSearchK = 10
	maxSearchK     = 100
)

// The interfaces below are the narrow contracts the tool handlers depend on,
// defined at the consumer so the handlers are unit-testable with fakes.
// *heuristics.Service satisfies heuristicsQuerier, *graph.Neo4jRepository satisfies
// evidenceReader, and *orchestratorclient.Client satisfies orchestratorProxy.

type heuristicsQuerier interface {
	Query(ctx context.Context, stateString string, k int, scope store.SearchScope) ([]heuristics.Match, error)
	Trace(ctx context.Context, metaHeuristicID string) ([]graph.CausalTriplet, error)
}

// evidenceReader is the causal-evidence lookup that labels served heuristics. It is a
// separate seam from the query service because it is a distinct read path -- causal
// edges only, exempt from the observational-only collection filter the search itself
// runs under.
type evidenceReader interface {
	CausalEvidenceForHeuristics(ctx context.Context, metaHeuristicIDs []string) (map[string]graph.CausalEvidence, error)
}

// orchestratorProxy is the Orchestrator's REST surface this server never writes
// directly: goal registration and the explicit verify affordance both proxy to it.
type orchestratorProxy interface {
	SubmitGoal(ctx context.Context, goal, importPath string) (string, error)
	VerifyFinding(ctx context.Context, goalID, findingID string) error
}

// The tool output DTOs below select and snake_case the fields downstream agents
// need. heuristics.Match wraps domain.MetaHeuristic and graph.CausalTriplet
// wraps domain.State/Intervention/Outcome -- none carry json tags. The SDK
// infers each tool's output schema from these structs, and every output DTO must
// infer to a JSON object (a bare slice infers to type:array and panics AddTool),
// so list results are wrapped in an object field.

// heuristicMatchDTO carries the epistemic label with the heuristic so a downstream
// agent can weight causal knowledge above correlation. RefutationConfidence is set
// only where causal evidence exists, and Caveat rides with it: an inferred effect is
// conditional on the discovered causal model and its no-latent-confounder assumption,
// so it is never served as established causation.
type heuristicMatchDTO struct {
	ID                   string   `json:"id"`
	Definition           string   `json:"definition"`
	EpistemicSource      string   `json:"epistemic_source"`
	RefutationConfidence *float64 `json:"refutation_confidence,omitempty"`
	Caveat               string   `json:"caveat,omitempty"`
}

type stateDTO struct {
	ID         string         `json:"id"`
	Properties map[string]any `json:"properties"`
}

type interventionDTO struct {
	ID         string         `json:"id"`
	Type       string         `json:"type"`
	Properties map[string]any `json:"properties"`
}

type outcomeDTO struct {
	ID                 string         `json:"id"`
	VerificationStatus string         `json:"verification_status"`
	Value              map[string]any `json:"value"`
}

// tripletDTO labels each edge a trace returns. A trace deliberately shows both the
// observational finding and any causal Outcome attached to it, so the label is what
// distinguishes them, and an edge with no recorded value reads as observational --
// the forward-compatibility rule every consumer of this field follows.
type tripletDTO struct {
	State           stateDTO        `json:"state"`
	Intervention    interventionDTO `json:"intervention"`
	Outcome         outcomeDTO      `json:"outcome"`
	EpistemicSource string          `json:"epistemic_source"`
	Caveat          string          `json:"caveat,omitempty"`
}

// getOptimizedHeuristicsInput carries the operational-state string and an
// optional result count. k is tagged omitempty so the SDK's pre-handler schema
// validation does not reject callers that omit it.
type getOptimizedHeuristicsInput struct {
	OperationalState string `json:"operational_state" jsonschema:"the operational-state description to find optimized heuristics for"`
	K                int    `json:"k,omitempty" jsonschema:"maximum number of heuristics to return (clamped to a server maximum)"`
	GoalID           string `json:"goal_id,omitempty" jsonschema:"optional optimization function id to scope the search to one goal's heuristics; omit to search all accumulated heuristics"`
}

type getOptimizedHeuristicsOutput struct {
	Heuristics []heuristicMatchDTO `json:"heuristics"`
}

type traceCausalChainInput struct {
	MetaHeuristicID string `json:"meta_heuristic_id" jsonschema:"the Meta-Heuristic id to trace back to its supporting causal evidence"`
}

type traceCausalChainOutput struct {
	Triplets []tripletDTO `json:"triplets"`
}

type submitAnalystGoalInput struct {
	Goal       string `json:"goal" jsonschema:"the analyst goal to register for optimization"`
	ImportPath string `json:"import_path" jsonschema:"path to the data source on the orchestrator's read-only import mount"`
}

type submitAnalystGoalOutput struct {
	OptimizationFunctionID string `json:"optimization_function_id"`
}

type verifyFindingInput struct {
	GoalID    string `json:"goal_id" jsonschema:"the optimization function id the finding belongs to"`
	FindingID string `json:"finding_id" jsonschema:"the finding's intervention id, as returned by trace_causal_chain"`
}

// verifyFindingOutput reports the dispatch, not the verdict: verification runs for
// minutes, so the tool answers as soon as the request is accepted and the agent polls
// the existing read tools for the result -- the same async shape submit_analyst_goal
// has.
type verifyFindingOutput struct {
	Dispatched bool   `json:"dispatched"`
	Detail     string `json:"detail"`
}

// tools binds the tool handlers to their collaborators. Handler methods match
// the SDK's mcp.ToolHandlerFor[In, Out] signature so mcp.AddTool infers each
// tool's input/output schema directly from the DTO types.
type tools struct {
	heur     heuristicsQuerier
	evidence evidenceReader
	orch     orchestratorProxy
}

// RegisterTools registers the MCP tools on the server. It runs at startup,
// so a schema-inference violation (e.g. a non-object output) panics here rather
// than at first call.
func RegisterTools(s *mcp.Server, heur heuristicsQuerier, evidence evidenceReader, orch orchestratorProxy) {
	t := &tools{heur: heur, evidence: evidence, orch: orch}
	mcp.AddTool(s, &mcp.Tool{
		Name:        ToolGetOptimizedHeuristics,
		Description: "Return the Meta-Heuristics most relevant to an operational-state description, ranked by semantic similarity.",
	}, t.getOptimizedHeuristics)
	mcp.AddTool(s, &mcp.Tool{
		Name:        ToolTraceCausalChain,
		Description: "Trace a Meta-Heuristic back to the State/Intervention/Outcome triplets that support it.",
	}, t.traceCausalChain)
	mcp.AddTool(s, &mcp.Tool{
		Name:        ToolSubmitAnalystGoal,
		Description: "Register an analyst optimization goal against a data source on the import mount, proxied to the orchestrator.",
	}, t.submitAnalystGoal)
	mcp.AddTool(s, &mcp.Tool{
		Name: ToolVerifyFinding,
		Description: "Request causal verification of one observational finding: its effect is re-computed adjusted for " +
			"the confounders in the discovered causal graph and stress-tested. Dispatch is asynchronous; poll " +
			"trace_causal_chain for the verified effect.",
	}, t.verifyFinding)
}

// getOptimizedHeuristics and traceCausalChain mask genuine store failures: the
// SDK copies a returned error's text verbatim into the analyst-visible result,
// so store internals must never leak. submitAnalystGoal surfaces only the
// Orchestrator's own response message (an OrchestratorError, analyst-safe) and
// masks everything else -- transport and marshal errors reference the internal
// service address, so they get the same masking as the read tools.

func (t *tools) getOptimizedHeuristics(ctx context.Context, _ *mcp.CallToolRequest, in getOptimizedHeuristicsInput) (*mcp.CallToolResult, getOptimizedHeuristicsOutput, error) {
	state := strings.TrimSpace(in.OperationalState)
	if state == "" {
		return nil, getOptimizedHeuristicsOutput{}, errors.New("operational_state must not be empty")
	}
	k := in.K
	if k <= 0 {
		k = defaultSearchK
	}
	if k > maxSearchK {
		k = maxSearchK
	}

	// Absent goal_id searches the whole accumulated corpus (including NULL-goal
	// legacy rows); a present one scopes to that goal's heuristics. ScopeFromGoalID
	// trims, so a whitespace-only id maps to cross-goal identically here and at the
	// orchestrator surface.
	scope := store.ScopeFromGoalID(in.GoalID)

	matches, err := t.heur.Query(ctx, state, k, scope)
	if err != nil {
		log.Printf("mcpserver: get_optimized_heuristics: %v", err)
		return nil, getOptimizedHeuristicsOutput{}, errors.New("internal error")
	}

	ids := make([]string, 0, len(matches))
	for _, m := range matches {
		ids = append(ids, m.MetaHeuristic.ID)
	}
	// The evidence lookup only labels the results, so a failure degrades the answer
	// rather than replacing it: serving the heuristics unlabelled (which reads as
	// observational, the safe default) beats serving nothing.
	evidence, err := t.evidence.CausalEvidenceForHeuristics(ctx, ids)
	if err != nil {
		log.Printf("mcpserver: get_optimized_heuristics evidence: %v", err)
		evidence = nil
	}

	out := getOptimizedHeuristicsOutput{Heuristics: make([]heuristicMatchDTO, 0, len(matches))}
	for _, m := range matches {
		dto := heuristicMatchDTO{
			ID:              m.MetaHeuristic.ID,
			Definition:      m.MetaHeuristic.Definition,
			EpistemicSource: string(domain.EpistemicObservational),
		}
		if ev, ok := evidence[m.MetaHeuristic.ID]; ok {
			confidence := ev.Confidence
			dto.EpistemicSource = string(domain.EpistemicCausalInferred)
			dto.RefutationConfidence = &confidence
			dto.Caveat = domain.CausalInferredCaveat
		}
		out.Heuristics = append(out.Heuristics, dto)
	}
	return nil, out, nil
}

func (t *tools) traceCausalChain(ctx context.Context, _ *mcp.CallToolRequest, in traceCausalChainInput) (*mcp.CallToolResult, traceCausalChainOutput, error) {
	triplets, err := t.heur.Trace(ctx, in.MetaHeuristicID)
	if err != nil {
		log.Printf("mcpserver: trace_causal_chain: %v", err)
		return nil, traceCausalChainOutput{}, errors.New("internal error")
	}
	out := traceCausalChainOutput{Triplets: make([]tripletDTO, 0, len(triplets))}
	for _, tr := range triplets {
		out.Triplets = append(out.Triplets, toTripletDTO(tr))
	}
	return nil, out, nil
}

func (t *tools) submitAnalystGoal(ctx context.Context, _ *mcp.CallToolRequest, in submitAnalystGoalInput) (*mcp.CallToolResult, submitAnalystGoalOutput, error) {
	optID, err := t.orch.SubmitGoal(ctx, in.Goal, in.ImportPath)
	if err != nil {
		var oerr *orchestratorclient.OrchestratorError
		if errors.As(err, &oerr) {
			return nil, submitAnalystGoalOutput{}, oerr
		}
		log.Printf("mcpserver: submit_analyst_goal: %v", err)
		return nil, submitAnalystGoalOutput{}, errors.New("internal error")
	}
	return nil, submitAnalystGoalOutput{OptimizationFunctionID: optID}, nil
}

func (t *tools) verifyFinding(ctx context.Context, _ *mcp.CallToolRequest, in verifyFindingInput) (*mcp.CallToolResult, verifyFindingOutput, error) {
	if err := t.orch.VerifyFinding(ctx, in.GoalID, in.FindingID); err != nil {
		var oerr *orchestratorclient.OrchestratorError
		if errors.As(err, &oerr) {
			return nil, verifyFindingOutput{}, oerr
		}
		log.Printf("mcpserver: verify_finding: %v", err)
		return nil, verifyFindingOutput{}, errors.New("internal error")
	}
	return nil, verifyFindingOutput{
		Dispatched: true,
		Detail:     "verification dispatched; poll trace_causal_chain for the verified effect",
	}, nil
}

// toTripletDTO maps a causal triplet to its wire shape. Nil property/value maps
// are coerced to empty objects: the SDK validates tool output against the
// inferred type:object schema, which a JSON null would fail.
func toTripletDTO(t graph.CausalTriplet) tripletDTO {
	source := t.EpistemicSource
	if source == "" {
		source = domain.EpistemicObservational
	}
	dto := tripletDTO{
		State: stateDTO{ID: t.State.ID, Properties: nonNilMap(t.State.Properties)},
		Intervention: interventionDTO{
			ID:         t.Intervention.ID,
			Type:       string(t.Intervention.Type),
			Properties: nonNilMap(t.Intervention.Properties),
		},
		Outcome: outcomeDTO{
			ID:                 t.Outcome.ID,
			VerificationStatus: string(t.Outcome.VerificationStatus),
			Value:              nonNilMap(t.Outcome.Value),
		},
		EpistemicSource: string(source),
	}
	if source == domain.EpistemicCausalInferred {
		dto.Caveat = domain.CausalInferredCaveat
	}
	return dto
}

func nonNilMap(m map[string]any) map[string]any {
	if m == nil {
		return map[string]any{}
	}
	return m
}
