// Package embedding defines the pluggable EmbeddingProvider seam and its
// default Ollama implementation. The interface distinguishes query from
// document embedding because nomic-embed-text is trained to require
// task-instruction prefixes and asymmetric retrieval degrades without them;
// baking the distinction in now freezes the seam so no consumer changes when a
// provider is swapped.
//
// Swapping providers (e.g. to Voyage AI) is NOT a live config toggle: different
// providers produce different-dimension, non-comparable vectors, so a switch
// requires re-embedding every stored Meta-Heuristic and re-dimensioning the
// pgvector column via migration.
package embedding

import "context"

// Provider embeds text for storage and retrieval. EmbedQuery embeds a
// search/operational-state string; EmbedDocument embeds text being stored.
type Provider interface {
	EmbedQuery(ctx context.Context, text string) ([]float32, error)
	EmbedDocument(ctx context.Context, text string) ([]float32, error)
	Dimensions() int
}
