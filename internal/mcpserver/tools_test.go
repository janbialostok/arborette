package mcpserver

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/arborette/arborette/internal/domain"
	"github.com/arborette/arborette/internal/graph"
	"github.com/arborette/arborette/internal/heuristics"
	"github.com/arborette/arborette/internal/orchestratorclient"
	"github.com/arborette/arborette/internal/store"
)

type fakeQuerier struct {
	matches  []heuristics.Match
	triplets []graph.CausalTriplet
	queryErr error
	traceErr error
	gotK     int
	gotScope store.SearchScope
}

func (f *fakeQuerier) Query(_ context.Context, _ string, k int, scope store.SearchScope) ([]heuristics.Match, error) {
	f.gotK = k
	f.gotScope = scope
	return f.matches, f.queryErr
}
func (f *fakeQuerier) Trace(_ context.Context, _ string) ([]graph.CausalTriplet, error) {
	return f.triplets, f.traceErr
}

type fakeSubmitter struct {
	optID         string
	err           error
	gotGoal       string
	gotImportPath string
}

func (f *fakeSubmitter) SubmitGoal(_ context.Context, goal, importPath string) (string, error) {
	f.gotGoal, f.gotImportPath = goal, importPath
	return f.optID, f.err
}

// connectTools registers the tools on a server backed by the given fakes and
// returns an in-memory client session wired to it.
func connectTools(t *testing.T, heur heuristicsQuerier, goals goalSubmitter) *mcp.ClientSession {
	t.Helper()
	ctx := context.Background()
	srv := mcp.NewServer(&mcp.Implementation{Name: "arborette-mcp", Version: "test"}, nil)
	RegisterTools(srv, heur, goals)

	t1, t2 := mcp.NewInMemoryTransports()
	if _, err := srv.Connect(ctx, t1, nil); err != nil {
		t.Fatalf("connect server: %v", err)
	}
	client := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "0"}, nil)
	cs, err := client.Connect(ctx, t2, nil)
	if err != nil {
		t.Fatalf("connect client: %v", err)
	}
	t.Cleanup(func() { cs.Close() })
	return cs
}

// decodeOutput unmarshals a tool's structured output from the JSON text content
// the SDK emits for a typed result.
func decodeOutput(t *testing.T, res *mcp.CallToolResult, v any) {
	t.Helper()
	if len(res.Content) == 0 {
		t.Fatalf("result has no content")
	}
	tc, ok := res.Content[0].(*mcp.TextContent)
	if !ok {
		t.Fatalf("content[0] is %T, want *mcp.TextContent", res.Content[0])
	}
	if err := json.Unmarshal([]byte(tc.Text), v); err != nil {
		t.Fatalf("decode output %q: %v", tc.Text, err)
	}
}

func TestGetOptimizedHeuristicsMapsAndClampsK(t *testing.T) {
	heur := &fakeQuerier{matches: []heuristics.Match{
		{MetaHeuristic: domain.MetaHeuristic{ID: "mh-1", Definition: "scale reads"}},
	}}
	cs := connectTools(t, heur, &fakeSubmitter{})

	res, err := cs.CallTool(context.Background(), &mcp.CallToolParams{
		Name:      "get_optimized_heuristics",
		Arguments: map[string]any{"operational_state": "hot shard", "k": 1000},
	})
	if err != nil {
		t.Fatalf("call tool: %v", err)
	}
	if res.IsError {
		t.Fatalf("unexpected tool error: %+v", res.Content)
	}
	if heur.gotK != maxSearchK {
		t.Fatalf("k = %d, want clamped to %d", heur.gotK, maxSearchK)
	}
	if !heur.gotScope.CrossGoal || heur.gotScope.GoalID != "" {
		t.Fatalf("scope = %+v, want cross-goal when goal_id is omitted", heur.gotScope)
	}
	var out getOptimizedHeuristicsOutput
	decodeOutput(t, res, &out)
	if len(out.Heuristics) != 1 || out.Heuristics[0].ID != "mh-1" || out.Heuristics[0].Definition != "scale reads" {
		t.Fatalf("unexpected heuristics: %+v", out.Heuristics)
	}
}

func TestGetOptimizedHeuristicsDefaultsKWhenOmitted(t *testing.T) {
	heur := &fakeQuerier{}
	cs := connectTools(t, heur, &fakeSubmitter{})

	res, err := cs.CallTool(context.Background(), &mcp.CallToolParams{
		Name:      "get_optimized_heuristics",
		Arguments: map[string]any{"operational_state": "hot shard"},
	})
	if err != nil {
		t.Fatalf("call tool (omitting optional k must succeed): %v", err)
	}
	if res.IsError {
		t.Fatalf("unexpected tool error: %+v", res.Content)
	}
	if heur.gotK != defaultSearchK {
		t.Fatalf("k = %d, want default %d", heur.gotK, defaultSearchK)
	}
}

func TestGetOptimizedHeuristicsScopesToGoal(t *testing.T) {
	heur := &fakeQuerier{}
	cs := connectTools(t, heur, &fakeSubmitter{})

	res, err := cs.CallTool(context.Background(), &mcp.CallToolParams{
		Name:      "get_optimized_heuristics",
		Arguments: map[string]any{"operational_state": "hot shard", "goal_id": "goal-7"},
	})
	if err != nil {
		t.Fatalf("call tool: %v", err)
	}
	if res.IsError {
		t.Fatalf("unexpected tool error: %+v", res.Content)
	}
	if heur.gotScope.CrossGoal || heur.gotScope.GoalID != "goal-7" {
		t.Fatalf("scope = %+v, want goal-scoped to goal-7", heur.gotScope)
	}
}

func TestGetOptimizedHeuristicsMissingRequiredArg(t *testing.T) {
	cs := connectTools(t, &fakeQuerier{}, &fakeSubmitter{})

	// The SDK validates arguments against the input schema before the handler
	// runs, so an omitted required field surfaces as a call error, not IsError.
	_, err := cs.CallTool(context.Background(), &mcp.CallToolParams{
		Name:      "get_optimized_heuristics",
		Arguments: map[string]any{},
	})
	if err == nil {
		t.Fatalf("expected a schema-validation error for the missing operational_state")
	}
}

func TestGetOptimizedHeuristicsMasksCollaboratorError(t *testing.T) {
	heur := &fakeQuerier{queryErr: errors.New("pgvector: connection refused")}
	cs := connectTools(t, heur, &fakeSubmitter{})

	res, err := cs.CallTool(context.Background(), &mcp.CallToolParams{
		Name:      "get_optimized_heuristics",
		Arguments: map[string]any{"operational_state": "hot shard"},
	})
	if err != nil {
		t.Fatalf("a collaborator failure must be an IsError result, not a call error: %v", err)
	}
	if !res.IsError {
		t.Fatalf("expected IsError result")
	}
	tc, ok := res.Content[0].(*mcp.TextContent)
	if !ok || tc.Text != "internal error" {
		t.Fatalf("error content = %+v, want masked \"internal error\"", res.Content[0])
	}
}

func TestGetOptimizedHeuristicsRejectsEmptyState(t *testing.T) {
	heur := &fakeQuerier{}
	cs := connectTools(t, heur, &fakeSubmitter{})

	// A present-but-blank operational_state passes the required-field schema
	// check, so it reaches the handler's semantic guard, which returns an
	// IsError result without calling the collaborator.
	res, err := cs.CallTool(context.Background(), &mcp.CallToolParams{
		Name:      "get_optimized_heuristics",
		Arguments: map[string]any{"operational_state": "   "},
	})
	if err != nil {
		t.Fatalf("call tool: %v", err)
	}
	if !res.IsError {
		t.Fatalf("expected IsError result for a blank operational_state")
	}
	tc, ok := res.Content[0].(*mcp.TextContent)
	if !ok || tc.Text != "operational_state must not be empty" {
		t.Fatalf("error content = %+v, want the empty-state message", res.Content[0])
	}
	if heur.gotK != 0 {
		t.Fatalf("collaborator must not be called for a blank state (gotK=%d)", heur.gotK)
	}
}

func TestTraceCausalChainMapsTriplets(t *testing.T) {
	heur := &fakeQuerier{triplets: []graph.CausalTriplet{{
		State:        domain.State{ID: "s-1", Properties: map[string]any{"value": 10.0}},
		Intervention: domain.Intervention{ID: "i-1", Type: domain.InterventionQuery},
		Outcome:      domain.Outcome{ID: "o-1", VerificationStatus: domain.VerificationVerified},
	}}}
	cs := connectTools(t, heur, &fakeSubmitter{})

	res, err := cs.CallTool(context.Background(), &mcp.CallToolParams{
		Name:      "trace_causal_chain",
		Arguments: map[string]any{"meta_heuristic_id": "mh-1"},
	})
	if err != nil {
		t.Fatalf("call tool: %v", err)
	}
	if res.IsError {
		t.Fatalf("unexpected tool error: %+v", res.Content)
	}
	var out traceCausalChainOutput
	decodeOutput(t, res, &out)
	if len(out.Triplets) != 1 {
		t.Fatalf("triplets = %+v, want 1", out.Triplets)
	}
	tr := out.Triplets[0]
	if tr.Intervention.Type != "query" || tr.Outcome.VerificationStatus != "verified" {
		t.Fatalf("unexpected snake_case mapping: %+v", tr)
	}
	// Nil maps on the domain objects must be emitted as {} (valid against the
	// inferred type:object output schema), not null.
	if tr.Intervention.Properties == nil || tr.Outcome.Value == nil {
		t.Fatalf("nil maps must be coerced to empty objects: %+v", tr)
	}
}

func TestTraceCausalChainMasksCollaboratorError(t *testing.T) {
	heur := &fakeQuerier{traceErr: errors.New("neo4j: connection reset")}
	cs := connectTools(t, heur, &fakeSubmitter{})

	res, err := cs.CallTool(context.Background(), &mcp.CallToolParams{
		Name:      "trace_causal_chain",
		Arguments: map[string]any{"meta_heuristic_id": "mh-1"},
	})
	if err != nil {
		t.Fatalf("a collaborator failure must be an IsError result, not a call error: %v", err)
	}
	if !res.IsError {
		t.Fatalf("expected IsError result")
	}
	tc, ok := res.Content[0].(*mcp.TextContent)
	if !ok || tc.Text != "internal error" {
		t.Fatalf("error content = %+v, want masked \"internal error\"", res.Content[0])
	}
}

func TestSubmitAnalystGoalReturnsID(t *testing.T) {
	goals := &fakeSubmitter{optID: "opt-42"}
	cs := connectTools(t, &fakeQuerier{}, goals)

	res, err := cs.CallTool(context.Background(), &mcp.CallToolParams{
		Name:      "submit_analyst_goal",
		Arguments: map[string]any{"goal": "grow revenue", "import_path": "data.csv"},
	})
	if err != nil {
		t.Fatalf("call tool: %v", err)
	}
	if res.IsError {
		t.Fatalf("unexpected tool error: %+v", res.Content)
	}
	var out submitAnalystGoalOutput
	decodeOutput(t, res, &out)
	if out.OptimizationFunctionID != "opt-42" {
		t.Fatalf("optimization_function_id = %q, want opt-42", out.OptimizationFunctionID)
	}
	if goals.gotGoal != "grow revenue" || goals.gotImportPath != "data.csv" {
		t.Fatalf("submitter received goal:%q import_path:%q", goals.gotGoal, goals.gotImportPath)
	}
}

func TestSubmitAnalystGoalSurfacesOrchestratorMessage(t *testing.T) {
	goals := &fakeSubmitter{err: &orchestratorclient.OrchestratorError{
		Status:  http.StatusBadRequest,
		Message: "import_path escapes the import directory",
	}}
	cs := connectTools(t, &fakeQuerier{}, goals)

	res, err := cs.CallTool(context.Background(), &mcp.CallToolParams{
		Name:      "submit_analyst_goal",
		Arguments: map[string]any{"goal": "grow revenue", "import_path": "../secret.csv"},
	})
	if err != nil {
		t.Fatalf("call tool: %v", err)
	}
	if !res.IsError {
		t.Fatalf("expected IsError result")
	}
	tc, ok := res.Content[0].(*mcp.TextContent)
	if !ok || tc.Text != "import_path escapes the import directory" {
		t.Fatalf("error content = %+v, want the orchestrator's message surfaced verbatim", res.Content[0])
	}
}

func TestSubmitAnalystGoalMasksNonOrchestratorError(t *testing.T) {
	// A transport error references the internal orchestrator address, so it must
	// be masked rather than surfaced -- unlike an OrchestratorError.
	goals := &fakeSubmitter{err: errors.New(`call orchestrator /goals: Post "http://orchestrator:8080/goals": dial tcp: connection refused`)}
	cs := connectTools(t, &fakeQuerier{}, goals)

	res, err := cs.CallTool(context.Background(), &mcp.CallToolParams{
		Name:      "submit_analyst_goal",
		Arguments: map[string]any{"goal": "grow revenue", "import_path": "data.csv"},
	})
	if err != nil {
		t.Fatalf("call tool: %v", err)
	}
	if !res.IsError {
		t.Fatalf("expected IsError result")
	}
	tc, ok := res.Content[0].(*mcp.TextContent)
	if !ok || tc.Text != "internal error" {
		t.Fatalf("error content = %+v, want masked \"internal error\" (no internal address leak)", res.Content[0])
	}
}
