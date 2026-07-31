package orchestrator

import (
	"log"
	"net/http"
	"strconv"

	"github.com/arborette/arborette/internal/graph"
	"github.com/arborette/arborette/internal/service"
)

// defaultSearchK is the similarity-search result count used when the request
// omits or malforms k; maxSearchK clamps how large a top-k a caller can request.
const (
	defaultSearchK = 10
	maxSearchK     = 100
)

// The response DTOs below select and snake_case the fields the web UI needs.
// heuristics.Match wraps domain.MetaHeuristic and graph.CausalTriplet wraps
// domain.State/Intervention/Outcome — none carry json tags, so marshaling them
// directly would emit PascalCase keys and leak internal fields.

type heuristicMatchDTO struct {
	ID         string `json:"id"`
	Definition string `json:"definition"`
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

type tripletDTO struct {
	State        stateDTO        `json:"state"`
	Intervention interventionDTO `json:"intervention"`
	Outcome      outcomeDTO      `json:"outcome"`
}

// handleHeuristicSearch backs the web UI's heuristic browser: it runs the shared
// embedding-similarity query and returns the mapped matches.
func (s *Server) handleHeuristicSearch(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query().Get("q")
	if q == "" {
		service.WriteErr(w, http.StatusBadRequest, "q is required")
		return
	}
	k := defaultSearchK
	if raw := r.URL.Query().Get("k"); raw != "" {
		if n, err := strconv.Atoi(raw); err == nil && n > 0 {
			k = n
			if k > maxSearchK {
				k = maxSearchK
			}
		}
	}

	matches, err := s.heur.Query(r.Context(), q, k)
	if err != nil {
		log.Printf("orchestrator: heuristic search: %v", err)
		service.WriteErr(w, http.StatusInternalServerError, "internal error")
		return
	}
	out := make([]heuristicMatchDTO, 0, len(matches))
	for _, m := range matches {
		out = append(out, heuristicMatchDTO{ID: m.MetaHeuristic.ID, Definition: m.MetaHeuristic.Definition})
	}
	service.WriteJSON(w, http.StatusOK, out)
}

// handleHeuristicTrace backs the trace view: it walks the causal chain behind a
// Meta-Heuristic and returns the mapped triplets.
func (s *Server) handleHeuristicTrace(w http.ResponseWriter, r *http.Request) {
	triplets, err := s.heur.Trace(r.Context(), r.PathValue("id"))
	if err != nil {
		log.Printf("orchestrator: heuristic trace: %v", err)
		service.WriteErr(w, http.StatusInternalServerError, "internal error")
		return
	}
	out := make([]tripletDTO, 0, len(triplets))
	for _, t := range triplets {
		out = append(out, toTripletDTO(t))
	}
	service.WriteJSON(w, http.StatusOK, out)
}

func toTripletDTO(t graph.CausalTriplet) tripletDTO {
	return tripletDTO{
		State: stateDTO{ID: t.State.ID, Properties: t.State.Properties},
		Intervention: interventionDTO{
			ID:         t.Intervention.ID,
			Type:       string(t.Intervention.Type),
			Properties: t.Intervention.Properties,
		},
		Outcome: outcomeDTO{
			ID:                 t.Outcome.ID,
			VerificationStatus: string(t.Outcome.VerificationStatus),
			Value:              t.Outcome.Value,
		},
	}
}
