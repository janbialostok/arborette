package graph

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"

	"github.com/arborette/arborette/internal/domain"
	"github.com/neo4j/neo4j-go-driver/v5/neo4j"
)

// The DataColumn and CausalGraphMeta labels carry a synthesized deterministic id
// because InitSchema's id-unique constraint cannot enforce a composite key: the id
// is a SHA-256 over the composite parts (goal, datasource, name for a column; goal,
// datasource, version for meta), and the parts are stored as ordinary properties for
// querying. MERGE on {id} then keeps writes idempotent under the id-unique
// constraint while the composite parts remain honest, queryable properties.

func dataColumnID(goalID, datasourceRef, name string) string {
	return hashID(goalID, datasourceRef, name)
}

func causalMetaID(goalID, datasourceRef string, version int) string {
	return hashID(goalID, datasourceRef, strconv.Itoa(version))
}

func hashID(parts ...string) string {
	h := sha256.Sum256([]byte(strings.Join(parts, "\x00")))
	return hex.EncodeToString(h[:])
}

// UpsertDataColumns writes (or re-writes) the graph's DataColumn nodes for a
// discovery run, MERGEing each on its synthesized id so a crashed run's re-run is
// idempotent. It is written before the edges and the commit-marker meta.
func (r *Neo4jRepository) UpsertDataColumns(ctx context.Context, cols []domain.DataColumn) error {
	if len(cols) == 0 {
		return nil
	}
	rows := make([]map[string]any, 0, len(cols))
	for _, c := range cols {
		rows = append(rows, map[string]any{
			"id":            dataColumnID(c.GoalID, c.DatasourceRef, c.Name),
			"goalID":        c.GoalID,
			"datasourceRef": c.DatasourceRef,
			"name":          c.Name,
			"kind":          c.Kind,
		})
	}
	return r.writeOp(ctx, "upsert data columns", func(tx neo4j.ManagedTransaction) (any, error) {
		result, err := tx.Run(ctx,
			"UNWIND $rows AS row "+
				"MERGE (n:"+labelDataColumn+" {id: row.id}) "+
				"SET n.goal_id = row.goalID, n.datasource_ref = row.datasourceRef, n.name = row.name, n.kind = row.kind",
			map[string]any{"rows": rows},
		)
		if err != nil {
			return nil, err
		}
		_, err = result.Consume(ctx)
		return nil, err
	})
}

// UpsertCausalEdges writes the CAUSES edges for a discovery run. The relationship is
// MERGEd on the canonical undirected pair — the two column nodes (whose ids encode
// goal, datasource, and name) plus the version — with orientation carried as the
// `direction` property, never as the storage direction: a directed MERGE key would
// orphan edges when a re-run's non-deterministic LLM orientation flips a pair, since
// MERGE never deletes. With the undirected key, re-orientation is an idempotent
// property update.
func (r *Neo4jRepository) UpsertCausalEdges(ctx context.Context, edges []domain.CausalEdge) error {
	if len(edges) == 0 {
		return nil
	}
	rows := make([]map[string]any, 0, len(edges))
	for _, e := range edges {
		rows = append(rows, map[string]any{
			"aID":           dataColumnID(e.GoalID, e.DatasourceRef, e.ColA),
			"bID":           dataColumnID(e.GoalID, e.DatasourceRef, e.ColB),
			"version":       e.Version,
			"goalID":        e.GoalID,
			"datasourceRef": e.DatasourceRef,
			"colA":          e.ColA,
			"colB":          e.ColB,
			"direction":     string(e.Direction),
			"provenance":    string(e.Provenance),
			"confidence":    e.Confidence,
			"status":        string(e.Status),
		})
	}
	return r.writeOp(ctx, "upsert causal edges", func(tx neo4j.ManagedTransaction) (any, error) {
		result, err := tx.Run(ctx,
			"UNWIND $rows AS row "+
				"MATCH (a:"+labelDataColumn+" {id: row.aID}) "+
				"MATCH (b:"+labelDataColumn+" {id: row.bID}) "+
				"MERGE (a)-[e:"+domain.RelationCauses+" {version: row.version}]->(b) "+
				"SET e.goal_id = row.goalID, e.datasource_ref = row.datasourceRef, "+
				"e.col_a = row.colA, e.col_b = row.colB, e.direction = row.direction, "+
				"e.provenance = row.provenance, e.confidence = row.confidence, e.status = row.status",
			map[string]any{"rows": rows},
		)
		if err != nil {
			return nil, err
		}
		_, err = result.Consume(ctx)
		return nil, err
	})
}

// UpsertCausalGraphMeta writes the CausalGraphMeta node — the commit marker written
// last, so GetCausalGraph reads a graph as present only once its meta exists. The
// tuning snapshot is stored as a JSON string (Neo4j stores only flat primitives);
// excluded columns ride as a native string list.
func (r *Neo4jRepository) UpsertCausalGraphMeta(ctx context.Context, meta domain.CausalGraphMeta) error {
	tuning, err := json.Marshal(meta.Tuning)
	if err != nil {
		return fmt.Errorf("marshal tuning: %w", err)
	}
	excluded := meta.ExcludedColumns
	if excluded == nil {
		excluded = []string{}
	}
	return r.writeOp(ctx, "upsert causal graph meta", func(tx neo4j.ManagedTransaction) (any, error) {
		return tx.Run(ctx,
			"MERGE (m:"+labelCausalGraphMeta+" {id: $id}) "+
				"SET m.goal_id = $goalID, m.datasource_ref = $datasourceRef, m.version = $version, "+
				"m.excluded_columns = $excluded, m.budget_truncated = $truncated, m.test_count = $testCount, "+
				"m.discovered_at = $discoveredAt, m.tuning = $tuning",
			map[string]any{
				"id":            causalMetaID(meta.GoalID, meta.DatasourceRef, meta.Version),
				"goalID":        meta.GoalID,
				"datasourceRef": meta.DatasourceRef,
				"version":       meta.Version,
				"excluded":      excluded,
				"truncated":     meta.BudgetTruncated,
				"testCount":     meta.TestCount,
				"discoveredAt":  meta.DiscoveredAt,
				"tuning":        string(tuning),
			},
		)
	})
}

// DeleteCausalGraphVersion clears any prior (torn or superseded) write of a version
// for a (goal, data-source) pair before a fresh discovery writes its own set: the
// version's meta, its CAUSES edges, and the pair's DataColumn nodes (detached). It is
// safe as pre-write cleanup because the caller has confirmed no committed meta
// exists, so nothing committed is lost.
func (r *Neo4jRepository) DeleteCausalGraphVersion(ctx context.Context, goalID, datasourceRef string, version int) error {
	return r.writeOp(ctx, "delete causal graph version", func(tx neo4j.ManagedTransaction) (any, error) {
		if _, err := tx.Run(ctx,
			"MATCH (m:"+labelCausalGraphMeta+" {goal_id: $goalID, datasource_ref: $datasourceRef, version: $version}) DELETE m",
			map[string]any{"goalID": goalID, "datasourceRef": datasourceRef, "version": version},
		); err != nil {
			return nil, err
		}
		result, err := tx.Run(ctx,
			"MATCH (c:"+labelDataColumn+" {goal_id: $goalID, datasource_ref: $datasourceRef}) DETACH DELETE c",
			map[string]any{"goalID": goalID, "datasourceRef": datasourceRef},
		)
		if err != nil {
			return nil, err
		}
		_, err = result.Consume(ctx)
		return nil, err
	})
}

// GetCausalGraph returns the newest committed graph for a (goal, data-source) pair
// and whether one exists. A graph is committed only when its CausalGraphMeta node is
// present, so a torn partial write (columns/edges written, meta not) reads as absent
// — the caller re-discovers over the same keys. The returned graph carries its
// columns, its edges (with direction/provenance/confidence/status), and the meta.
func (r *Neo4jRepository) GetCausalGraph(ctx context.Context, goalID, datasourceRef string) (domain.CausalGraph, bool, error) {
	res, err := r.read(ctx, func(tx neo4j.ManagedTransaction) (any, error) {
		metaRes, err := tx.Run(ctx,
			"MATCH (m:"+labelCausalGraphMeta+" {goal_id: $goalID, datasource_ref: $datasourceRef}) "+
				"RETURN m ORDER BY m.version DESC LIMIT 1",
			map[string]any{"goalID": goalID, "datasourceRef": datasourceRef},
		)
		if err != nil {
			return nil, err
		}
		metaRecs, err := metaRes.Collect(ctx)
		if err != nil {
			return nil, err
		}
		if len(metaRecs) == 0 {
			return nil, nil
		}
		metaNode, err := recordNode(metaRecs[0], "m")
		if err != nil {
			return nil, err
		}
		meta := causalGraphMetaFromNode(metaNode)

		colRes, err := tx.Run(ctx,
			"MATCH (c:"+labelDataColumn+" {goal_id: $goalID, datasource_ref: $datasourceRef}) RETURN c ORDER BY c.name",
			map[string]any{"goalID": goalID, "datasourceRef": datasourceRef},
		)
		if err != nil {
			return nil, err
		}
		colRecs, err := colRes.Collect(ctx)
		if err != nil {
			return nil, err
		}
		columns := make([]domain.DataColumn, 0, len(colRecs))
		for _, rec := range colRecs {
			node, err := recordNode(rec, "c")
			if err != nil {
				return nil, err
			}
			columns = append(columns, dataColumnFromNode(node))
		}

		edgeRes, err := tx.Run(ctx,
			"MATCH ()-[e:"+domain.RelationCauses+" {goal_id: $goalID, datasource_ref: $datasourceRef, version: $version}]->() "+
				"RETURN e ORDER BY e.col_a, e.col_b",
			map[string]any{"goalID": goalID, "datasourceRef": datasourceRef, "version": meta.Version},
		)
		if err != nil {
			return nil, err
		}
		edgeRecs, err := edgeRes.Collect(ctx)
		if err != nil {
			return nil, err
		}
		edges := make([]domain.CausalEdge, 0, len(edgeRecs))
		for _, rec := range edgeRecs {
			rel, err := recordRelationship(rec, "e")
			if err != nil {
				return nil, err
			}
			edges = append(edges, causalEdgeFromRelationship(rel))
		}

		return domain.CausalGraph{Columns: columns, Edges: edges, Meta: meta}, nil
	})
	if err != nil {
		return domain.CausalGraph{}, false, fmt.Errorf("get causal graph for %q: %w", goalID, err)
	}
	if res == nil {
		return domain.CausalGraph{}, false, nil
	}
	return res.(domain.CausalGraph), true, nil
}

func dataColumnFromNode(node neo4j.Node) domain.DataColumn {
	return domain.DataColumn{
		ID:            stringProp(node.Props["id"]),
		GoalID:        stringProp(node.Props["goal_id"]),
		DatasourceRef: stringProp(node.Props["datasource_ref"]),
		Name:          stringProp(node.Props["name"]),
		Kind:          stringProp(node.Props["kind"]),
	}
}

func causalEdgeFromRelationship(rel neo4j.Relationship) domain.CausalEdge {
	version, _ := rel.Props["version"].(int64)
	confidence, _ := rel.Props["confidence"].(float64)
	return domain.CausalEdge{
		GoalID:        stringProp(rel.Props["goal_id"]),
		DatasourceRef: stringProp(rel.Props["datasource_ref"]),
		Version:       int(version),
		ColA:          stringProp(rel.Props["col_a"]),
		ColB:          stringProp(rel.Props["col_b"]),
		Direction:     domain.EdgeDirection(stringProp(rel.Props["direction"])),
		Provenance:    domain.EdgeProvenance(stringProp(rel.Props["provenance"])),
		Confidence:    confidence,
		Status:        domain.EdgeStatus(stringProp(rel.Props["status"])),
	}
}

func causalGraphMetaFromNode(node neo4j.Node) domain.CausalGraphMeta {
	version, _ := node.Props["version"].(int64)
	testCount, _ := node.Props["test_count"].(int64)
	truncated, _ := node.Props["budget_truncated"].(bool)
	var tuning map[string]any
	if raw := stringProp(node.Props["tuning"]); raw != "" {
		_ = json.Unmarshal([]byte(raw), &tuning)
	}
	return domain.CausalGraphMeta{
		ID:              stringProp(node.Props["id"]),
		GoalID:          stringProp(node.Props["goal_id"]),
		DatasourceRef:   stringProp(node.Props["datasource_ref"]),
		Version:         int(version),
		ExcludedColumns: stringList(node.Props["excluded_columns"]),
		BudgetTruncated: truncated,
		TestCount:       int(testCount),
		DiscoveredAt:    stringProp(node.Props["discovered_at"]),
		Tuning:          tuning,
	}
}

// recordRelationship extracts a relationship value from a record field.
func recordRelationship(rec *neo4j.Record, key string) (neo4j.Relationship, error) {
	v, ok := rec.Get(key)
	if !ok {
		return neo4j.Relationship{}, fmt.Errorf("record has no field %q", key)
	}
	rel, ok := v.(neo4j.Relationship)
	if !ok {
		return neo4j.Relationship{}, fmt.Errorf("field %q is not a relationship", key)
	}
	return rel, nil
}

// stringList coerces a Neo4j list property (returned as []any) into a string slice.
func stringList(v any) []string {
	raw, ok := v.([]any)
	if !ok {
		return nil
	}
	out := make([]string, 0, len(raw))
	for _, item := range raw {
		if s, ok := item.(string); ok {
			out = append(out, s)
		}
	}
	return out
}
