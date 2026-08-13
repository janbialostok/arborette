-- Last-access recency for the dataset inventory ordering. The root URL serves
-- the inventory ordered by how recently each dataset was opened (detail GET);
-- NULL means "never accessed" and ranks below every accessed dataset. No grant
-- change: the orchestrator role already holds SELECT/UPDATE on datasets (0015).
ALTER TABLE datasets
    ADD COLUMN last_accessed_at timestamptz;