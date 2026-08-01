// Package heuristics is the single string-in/matches-out query seam that both
// the Orchestrator's REST handler and the MCP Server's tool handler call.
// Callers pass a raw operational-state string and get matches back; they never
// touch the embedding provider for this path.
package heuristics

import (
	"context"
	"fmt"
	"log"
	"time"

	"github.com/arborette/arborette/internal/domain"
	"github.com/arborette/arborette/internal/embedding"
	"github.com/arborette/arborette/internal/graph"
	"github.com/arborette/arborette/internal/store"
)

// retireTimeout is the ceiling on how much cleanup latency a caller can be made
// to wait for: the deletes run before Query returns, so a slow pgvector is added
// response time.
const retireTimeout = 5 * time.Second

// Match is one semantic Meta-Heuristic result, ordered by similarity.
type Match struct {
	MetaHeuristic domain.MetaHeuristic
}

// The interfaces below are the narrow contracts the Service depends on, defined
// at the consumer so Query is unit-testable with fakes and no infra.
// cmd/orchestrator and cmd/mcpserver pass the concrete *store.EmbeddingStore and
// *graph.Neo4jRepository, which satisfy them.

// embeddingStore is the vector surface this seam needs: the scoped similarity
// search itself, plus the delete that retires a row the graph no longer backs.
type embeddingStore interface {
	SimilaritySearch(ctx context.Context, query []float32, k int, scope store.SearchScope) ([]string, error)
	Delete(ctx context.Context, nodeID string) error
}

// heuristicRepo hydrates a search's hits in one call rather than per id: a
// similarity search resolves every hit it returns, so a per-id read would put a
// graph round trip on each result of every query.
type heuristicRepo interface {
	GetMetaHeuristics(ctx context.Context, ids []string) ([]domain.MetaHeuristic, error)
	TraceCausalChain(ctx context.Context, metaHeuristicID string) ([]graph.CausalTriplet, error)
}

// Service answers get_optimized_heuristics and trace_causal_chain over the
// graph and vector stores.
type Service struct {
	provider   embedding.Provider
	embeddings embeddingStore
	repo       heuristicRepo
}

// NewService wires the query service from its three collaborators.
func NewService(provider embedding.Provider, embeddings embeddingStore, repo heuristicRepo) *Service {
	return &Service{provider: provider, embeddings: embeddings, repo: repo}
}

// Query embeds the operational-state string via EmbedQuery (the search_query
// path), runs a pgvector similarity search, and fetches the matched
// Meta-Heuristic nodes from the graph -- the read side of
// get_optimized_heuristics. Stored definitions are embedded via EmbedDocument
// when the Sleep Cycle writes them, keeping the query/document sides consistent.
//
// A hit whose node is gone comes back absent from the batch and is skipped
// rather than failed: one such row would otherwise blank out every search whose
// neighbourhood touches it. Only absence is read that way -- a failed fetch still
// propagates, because answering "no such heuristic" when the graph is merely
// unreachable would silently narrow the corpus.
func (s *Service) Query(ctx context.Context, stateString string, k int, scope store.SearchScope) ([]Match, error) {
	vec, err := s.provider.EmbedQuery(ctx, stateString)
	if err != nil {
		return nil, fmt.Errorf("embed query: %w", err)
	}
	ids, err := s.embeddings.SimilaritySearch(ctx, vec, k, scope)
	if err != nil {
		return nil, fmt.Errorf("similarity search: %w", err)
	}
	fetched, err := s.repo.GetMetaHeuristics(ctx, ids)
	if err != nil {
		return nil, fmt.Errorf("fetch meta-heuristics: %w", err)
	}
	byID := make(map[string]domain.MetaHeuristic, len(fetched))
	for _, mh := range fetched {
		byID[mh.ID] = mh
	}
	// Assembled by walking the search's ids, not the batch's own order: the hits
	// come back in cosine-distance rank order and Match is contractually ordered by
	// similarity, which iterating the graph's arbitrary result order would destroy.
	matches := make([]Match, 0, len(ids))
	var orphaned []string
	for _, id := range ids {
		mh, ok := byID[id]
		if !ok {
			orphaned = append(orphaned, id)
			continue
		}
		matches = append(matches, Match{MetaHeuristic: mh})
	}
	s.retireOrphans(ctx, orphaned, len(matches))
	return matches, nil
}

// retireOrphans deletes the pgvector rows behind similarity hits the graph has
// no node for, so the two stores reconverge on read. They are written
// separately and the graph can be reset on its own, which is how a row outlives
// its node.
//
// It repairs nothing unless some other hit in the same batch resolved. Per hit,
// "this node is gone" and "the whole graph is gone" are the same observation,
// and deleting on the second reading would empty a table only the sleep cycle
// repopulates -- reconcile re-embeds a missing row on its next run, but only for
// a node still in the graph, so a wrongly emptied table stays dark to every
// search until then. One live sibling is the cheap proof that the miss is really
// about this id.
//
// That proof is per-batch, not per-goal, and abstraction runs one goal at a
// time: a live hit from an already-abstracted goal will corroborate retiring a
// goal whose nodes have not been rebuilt yet. The embedding row carries a goal_id
// that could narrow the corroboration to the same goal, but this read path
// deliberately does not use it.
//
// Deletion is best-effort past that point. A search that found live heuristics
// is a good answer, and failing it because the cleanup failed would turn a
// self-healing read into the outage it exists to prevent.
func (s *Service) retireOrphans(ctx context.Context, orphaned []string, live int) {
	if len(orphaned) == 0 {
		return
	}
	if live == 0 {
		log.Printf("heuristics: retire %d orphaned embedding(s): skipped, no live match corroborates a populated graph", len(orphaned))
		return
	}
	// Cancellation is dropped deliberately: a client that hangs up mid-batch must
	// not leave a half-done repair for the next search to re-derive. WithoutCancel
	// rather than Background so request-scoped values survive.
	retireCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), retireTimeout)
	defer cancel()

	var retiredIDs []string
	var firstErr error
	for _, id := range orphaned {
		if err := s.embeddings.Delete(retireCtx, id); err != nil {
			if firstErr == nil {
				firstErr = fmt.Errorf("%q: %w", id, err)
			}
			continue
		}
		retiredIDs = append(retiredIDs, id)
	}
	// Naming every failure would hand an unauthenticated caller a log amplifier,
	// since one revoked grant fails every delete in the batch.
	if firstErr != nil {
		log.Printf("heuristics: retire %d orphaned embedding(s): %d retired %v, first failure %v",
			len(orphaned), len(retiredIDs), retiredIDs, firstErr)
		return
	}
	// Ids, not a count: this line is the only record of what an operator would
	// have to re-embed.
	log.Printf("heuristics: retired %d orphaned embedding(s) with no graph node: %v", len(retiredIDs), retiredIDs)
}

// Trace delegates to the repository's ABSTRACTED_FROM traversal for
// trace_causal_chain.
func (s *Service) Trace(ctx context.Context, metaHeuristicID string) ([]graph.CausalTriplet, error) {
	return s.repo.TraceCausalChain(ctx, metaHeuristicID)
}
