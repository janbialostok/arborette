package store

import (
	"context"
	"fmt"

	"github.com/pgvector/pgvector-go"
)

// EmbeddingStore upserts and searches Meta-Heuristic embeddings keyed by the
// application-assigned node UUID (the same id the graph keys on).
type EmbeddingStore struct {
	pool *Pool
}

// NewEmbeddingStore wires the store to a pool.
func NewEmbeddingStore(pool *Pool) *EmbeddingStore {
	return &EmbeddingStore{pool: pool}
}

// ValidateEmbeddingDimension fails fast at startup when the configured embedding
// dimension disagrees with the meta_heuristic_embeddings.embedding column, so a
// mismatched EMBEDDING_DIMENSION surfaces as one clear error instead of opaque
// per-write failures. Changing the dimension is a migration + re-embed, never a
// runtime toggle; pgvector stores the column dimension in atttypmod.
func ValidateEmbeddingDimension(ctx context.Context, pool *Pool, expected int) error {
	var columnDim int
	err := pool.QueryRow(ctx,
		"SELECT atttypmod FROM pg_attribute "+
			"WHERE attrelid = 'meta_heuristic_embeddings'::regclass AND attname = 'embedding'",
	).Scan(&columnDim)
	if err != nil {
		return fmt.Errorf("read embedding column dimension: %w", err)
	}
	if columnDim != expected {
		return fmt.Errorf(
			"embedding dimension mismatch: config=%d but meta_heuristic_embeddings.embedding is vector(%d); "+
				"changing the dimension requires a matching migration and full re-embed, not a runtime toggle",
			expected, columnDim,
		)
	}
	return nil
}

// Upsert writes (or replaces) the embedding for a node.
func (e *EmbeddingStore) Upsert(ctx context.Context, nodeID string, embedding []float32) error {
	_, err := e.pool.Exec(ctx,
		"INSERT INTO meta_heuristic_embeddings (node_id, embedding, updated_at) VALUES ($1, $2, now()) "+
			"ON CONFLICT (node_id) DO UPDATE SET embedding = EXCLUDED.embedding, updated_at = now()",
		nodeID, pgvector.NewVector(embedding),
	)
	if err != nil {
		return fmt.Errorf("upsert embedding for %q: %w", nodeID, err)
	}
	return nil
}

// Delete removes the embedding for a node. Deleting a node_id that is not
// present is not an error: the desired end state is "no row", and it is reached
// whether this call or a concurrent one got there.
func (e *EmbeddingStore) Delete(ctx context.Context, nodeID string) error {
	_, err := e.pool.Exec(ctx, "DELETE FROM meta_heuristic_embeddings WHERE node_id = $1", nodeID)
	if err != nil {
		return fmt.Errorf("delete embedding for %q: %w", nodeID, err)
	}
	return nil
}

// SimilaritySearch returns the k nearest node_ids to the query vector by cosine
// distance (the <=> operator, matching the hnsw vector_cosine_ops index).
func (e *EmbeddingStore) SimilaritySearch(ctx context.Context, query []float32, k int) ([]string, error) {
	rows, err := e.pool.Query(ctx,
		"SELECT node_id FROM meta_heuristic_embeddings ORDER BY embedding <=> $1 LIMIT $2",
		pgvector.NewVector(query), k,
	)
	if err != nil {
		return nil, fmt.Errorf("similarity search: %w", err)
	}
	defer rows.Close()

	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("scan similarity row: %w", err)
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate similarity rows: %w", err)
	}
	return ids, nil
}
