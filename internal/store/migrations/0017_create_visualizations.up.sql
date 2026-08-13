CREATE TABLE visualizations (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    insight_id UUID NOT NULL REFERENCES insight_embeddings(node_id) ON DELETE CASCADE,
    type VARCHAR(50) NOT NULL CHECK (type IN ('line_chart', 'bar_chart', 'trend_graph', 'calendar_view', 'comparison_chart')),
    title VARCHAR(255),
    config JSONB NOT NULL DEFAULT '{}',
    created_at TIMESTAMP WITH TIME ZONE NOT NULL DEFAULT NOW(),
    updated_at TIMESTAMP WITH TIME ZONE NOT NULL DEFAULT NOW()
);

CREATE INDEX idx_visualizations_insight ON visualizations(insight_id);
CREATE INDEX idx_visualizations_type ON visualizations(type);
