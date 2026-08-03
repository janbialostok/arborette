package orchestrator

import (
	"log"
	"net/http"

	"github.com/arborette/arborette/internal/domain"
	"github.com/arborette/arborette/internal/service"
)

// The DTOs below select and snake_case the discovered causal graph for the web UI.
// domain.CausalGraph and its parts carry no json tags, so marshaling them directly
// would emit PascalCase keys and leak internal ids.

type causalColumnDTO struct {
	Name string `json:"name"`
	Kind string `json:"kind"`
}

type causalEdgeDTO struct {
	ColA       string  `json:"col_a"`
	ColB       string  `json:"col_b"`
	Direction  string  `json:"direction"`
	Provenance string  `json:"provenance"`
	Confidence float64 `json:"confidence"`
	Status     string  `json:"status"`
}

type causalMetaDTO struct {
	Version         int      `json:"version"`
	ExcludedColumns []string `json:"excluded_columns"`
	BudgetTruncated bool     `json:"budget_truncated"`
	TestCount       int      `json:"test_count"`
	DiscoveredAt    string   `json:"discovered_at"`
}

type causalGraphDTO struct {
	Columns []causalColumnDTO `json:"columns"`
	Edges   []causalEdgeDTO   `json:"edges"`
	Meta    causalMetaDTO     `json:"meta"`
}

// handleCausalGraph serves the discovered causal graph for a goal's data source. It
// resolves the goal's immutable data_source_ref from the registry, fetches the
// committed graph from the graph repo, and maps it to a DTO carrying per-edge
// provenance/confidence/status and the meta (excluded columns, version). A goal with
// no committed graph yet returns 404 — the same shape a discovery that only wrote a
// torn partial (no meta) produces, since GetCausalGraph reports absent until meta
// commits.
func (s *Server) handleCausalGraph(w http.ResponseWriter, r *http.Request) {
	goal, ok := s.lookupGoal(r.Context(), w, r.PathValue("id"))
	if !ok {
		return
	}

	graph, found, err := s.repo.GetCausalGraph(r.Context(), goal.OptimizationFunctionID, goal.DataSourceRef)
	if err != nil {
		log.Printf("orchestrator: get causal graph: %v", err)
		service.WriteErr(w, http.StatusInternalServerError, "internal error")
		return
	}
	if !found {
		service.WriteErr(w, http.StatusNotFound, "no causal graph discovered for this goal yet")
		return
	}
	service.WriteJSON(w, http.StatusOK, toCausalGraphDTO(graph))
}

func toCausalGraphDTO(g domain.CausalGraph) causalGraphDTO {
	columns := make([]causalColumnDTO, 0, len(g.Columns))
	for _, c := range g.Columns {
		columns = append(columns, causalColumnDTO{Name: c.Name, Kind: c.Kind})
	}
	edges := make([]causalEdgeDTO, 0, len(g.Edges))
	for _, e := range g.Edges {
		edges = append(edges, causalEdgeDTO{
			ColA:       e.ColA,
			ColB:       e.ColB,
			Direction:  string(e.Direction),
			Provenance: string(e.Provenance),
			Confidence: e.Confidence,
			Status:     string(e.Status),
		})
	}
	excluded := g.Meta.ExcludedColumns
	if excluded == nil {
		excluded = []string{}
	}
	return causalGraphDTO{
		Columns: columns,
		Edges:   edges,
		Meta: causalMetaDTO{
			Version:         g.Meta.Version,
			ExcludedColumns: excluded,
			BudgetTruncated: g.Meta.BudgetTruncated,
			TestCount:       g.Meta.TestCount,
			DiscoveredAt:    g.Meta.DiscoveredAt,
		},
	}
}
