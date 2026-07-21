-- node_id holds the application-assigned node UUID -- the same id property the
-- graph keys on, NOT Neo4j's internal element id. Never join this on an
-- internal id. Dimensioned to nomic-embed-text's 768-d output.
CREATE TABLE meta_heuristic_embeddings (
    node_id uuid PRIMARY KEY,
    embedding vector(768) NOT NULL,
    updated_at timestamptz NOT NULL DEFAULT now()
);

-- hnsw builds incrementally and needs no training step, so it is correct
-- against an empty table. Do NOT use ivfflat here: its list centroids are
-- trained from rows present at build time and give degraded recall when built
-- on zero rows.
CREATE INDEX meta_heuristic_embeddings_hnsw
    ON meta_heuristic_embeddings
    USING hnsw (embedding vector_cosine_ops);
