CREATE TABLE goal_registry (
    optimization_function_id uuid PRIMARY KEY,
    goal_text text NOT NULL,
    evaluation_matrix jsonb NOT NULL,
    datasource_ref text NOT NULL,
    created_at timestamptz NOT NULL DEFAULT now()
);
