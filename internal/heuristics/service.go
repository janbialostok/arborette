// Package heuristics is the single string-in/matches-out query seam that both
// the Orchestrator's REST handler and the MCP Server's tool handler call.
// Callers pass a raw operational-state string and get matches back; they never
// touch the embedding provider for this path.
package heuristics

import (
	"context"
	"fmt"

	"github.com/arborette/arborette/internal/domain"
	"github.com/arborette/arborette/internal/embedding"
	"github.com/arborette/arborette/internal/graph"
	"github.com/arborette/arborette/internal/store"
)

// Match is one semantic Meta-Heuristic result, ordered by similarity.
type Match struct {
	MetaHeuristic domain.MetaHeuristic
}

// Service answers get_optimized_heuristics and trace_causal_chain over the
// graph and vector stores.
type Service struct {
	provider   embedding.Provider
	embeddings *store.EmbeddingStore
	repo       graph.Repository
}

// NewService wires the query service from its three collaborators.
func NewService(provider embedding.Provider, embeddings *store.EmbeddingStore, repo graph.Repository) *Service {
	return &Service{provider: provider, embeddings: embeddings, repo: repo}
}

// Query embeds the operational-state string via EmbedQuery (the search_query
// path), runs a pgvector similarity search, and fetches the matched
// Meta-Heuristic nodes from the graph -- the read side of
// get_optimized_heuristics. Stored definitions are embedded via EmbedDocument
// when the Sleep Cycle writes them, keeping the query/document sides consistent.
func (s *Service) Query(ctx context.Context, stateString string, k int) ([]Match, error) {
	vec, err := s.provider.EmbedQuery(ctx, stateString)
	if err != nil {
		return nil, fmt.Errorf("embed query: %w", err)
	}
	ids, err := s.embeddings.SimilaritySearch(ctx, vec, k)
	if err != nil {
		return nil, fmt.Errorf("similarity search: %w", err)
	}
	matches := make([]Match, 0, len(ids))
	for _, id := range ids {
		mh, err := s.repo.GetMetaHeuristic(ctx, id)
		if err != nil {
			return nil, fmt.Errorf("fetch meta-heuristic %q: %w", id, err)
		}
		matches = append(matches, Match{MetaHeuristic: mh})
	}
	return matches, nil
}

// Trace delegates to the repository's ABSTRACTED_FROM traversal for
// trace_causal_chain.
func (s *Service) Trace(ctx context.Context, metaHeuristicID string) ([]graph.CausalTriplet, error) {
	return s.repo.TraceCausalChain(ctx, metaHeuristicID)
}
