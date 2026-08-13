-- Creates time-series support tables for datasets.

CREATE TABLE dataset_versions (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    dataset_id UUID NOT NULL REFERENCES dataset_registry(optimization_function_id) ON DELETE CASCADE,
    version_number INTEGER NOT NULL,
    data_snapshot JSONB NOT NULL DEFAULT '{}',
    created_at TIMESTAMP WITH TIME ZONE NOT NULL DEFAULT NOW(),
    UNIQUE(dataset_id, version_number)
);

CREATE INDEX idx_dataset_versions_dataset ON dataset_versions(dataset_id);
CREATE INDEX idx_dataset_versions_version ON dataset_versions(dataset_id, version_number DESC);
CREATE INDEX idx_dataset_versions_created ON dataset_versions(created_at);
