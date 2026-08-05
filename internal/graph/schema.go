package graph

import (
	"context"
	"fmt"

	"github.com/neo4j/neo4j-go-driver/v5/neo4j"
)

// InitSchema idempotently creates a per-label uniqueness constraint on the
// application-assigned id property. Consuming services call it at startup. Uses
// CREATE CONSTRAINT IF NOT EXISTS so re-running is a no-op; a future Neptune
// backend can implement it as a no-op outright, since Neptune has no schema
// constraints.
func (r *Neo4jRepository) InitSchema(ctx context.Context) error {
	labels := []string{labelState, labelIntervention, labelOutcome, labelMetaHeuristic, labelDataColumn, labelCausalGraphMeta}
	_, err := r.write(ctx, func(tx neo4j.ManagedTransaction) (any, error) {
		for _, label := range labels {
			constraint := fmt.Sprintf(
				"CREATE CONSTRAINT %s_id_unique IF NOT EXISTS FOR (n:%s) REQUIRE n.id IS UNIQUE",
				label, label,
			)
			if _, err := tx.Run(ctx, constraint, nil); err != nil {
				return nil, fmt.Errorf("create %s id constraint: %w", label, err)
			}
		}
		return nil, nil
	})
	return err
}
