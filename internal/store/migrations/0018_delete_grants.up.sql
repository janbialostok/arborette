-- Grants DELETE on the renamed dataset tables so the orchestrator can cascade-delete datasets.

GRANT DELETE ON dataset_registry TO arborette_orchestrator;
GRANT DELETE ON dataset_versions TO arborette_orchestrator;
GRANT DELETE ON visualizations TO arborette_orchestrator;
