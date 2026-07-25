package graph

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/arborette/arborette/internal/domain"
	"github.com/neo4j/neo4j-go-driver/v5/neo4j"
)

// Node labels. The abstraction node is conceptually "Meta-Heuristic"; the graph
// uses the hyphen-free MetaHeuristic label so no Cypher needs backtick quoting,
// keeping every statement inside the Neptune-portable subset.
const (
	labelState         = "State"
	labelIntervention  = "Intervention"
	labelOutcome       = "Outcome"
	labelMetaHeuristic = "MetaHeuristic"
)

// Neo4jRepository is the Cypher-backed Repository implementation.
type Neo4jRepository struct {
	driver neo4j.DriverWithContext
}

// NewNeo4jRepository opens a driver against the given bolt URI.
func NewNeo4jRepository(ctx context.Context, uri, user, password string) (*Neo4jRepository, error) {
	driver, err := neo4j.NewDriverWithContext(uri, neo4j.BasicAuth(user, password, ""))
	if err != nil {
		return nil, fmt.Errorf("open neo4j driver: %w", err)
	}
	if err := driver.VerifyConnectivity(ctx); err != nil {
		return nil, fmt.Errorf("verify neo4j connectivity: %w", err)
	}
	return &Neo4jRepository{driver: driver}, nil
}

// Close releases the underlying driver.
func (r *Neo4jRepository) Close(ctx context.Context) error {
	return r.driver.Close(ctx)
}

func (r *Neo4jRepository) write(ctx context.Context, work func(tx neo4j.ManagedTransaction) (any, error)) (any, error) {
	session := r.driver.NewSession(ctx, neo4j.SessionConfig{AccessMode: neo4j.AccessModeWrite})
	defer session.Close(ctx)
	return session.ExecuteWrite(ctx, work)
}

// writeOp runs a write transaction, wrapping any failure with operation context.
func (r *Neo4jRepository) writeOp(ctx context.Context, op string, work func(tx neo4j.ManagedTransaction) (any, error)) error {
	if _, err := r.write(ctx, work); err != nil {
		return fmt.Errorf("%s: %w", op, err)
	}
	return nil
}

func (r *Neo4jRepository) read(ctx context.Context, work func(tx neo4j.ManagedTransaction) (any, error)) (any, error) {
	session := r.driver.NewSession(ctx, neo4j.SessionConfig{AccessMode: neo4j.AccessModeRead})
	defer session.Close(ctx)
	return session.ExecuteRead(ctx, work)
}

func (r *Neo4jRepository) CreateState(ctx context.Context, s domain.State) error {
	props, err := marshalProps(s.Properties)
	if err != nil {
		return err
	}
	return r.writeOp(ctx, "create state "+s.ID, func(tx neo4j.ManagedTransaction) (any, error) {
		return tx.Run(ctx,
			"MERGE (n:"+labelState+" {id: $id}) SET n.properties = $properties",
			map[string]any{"id": s.ID, "properties": props},
		)
	})
}

func (r *Neo4jRepository) CreateIntervention(ctx context.Context, i domain.Intervention) error {
	props, err := marshalProps(i.Properties)
	if err != nil {
		return err
	}
	return r.writeOp(ctx, "create intervention "+i.ID, func(tx neo4j.ManagedTransaction) (any, error) {
		return tx.Run(ctx,
			"MERGE (n:"+labelIntervention+" {id: $id}) SET n.type = $type, n.properties = $properties",
			map[string]any{"id": i.ID, "type": string(i.Type), "properties": props},
		)
	})
}

func (r *Neo4jRepository) CreateOutcome(ctx context.Context, o domain.Outcome) error {
	value, err := marshalProps(o.Value)
	if err != nil {
		return err
	}
	provenance, err := marshalProvenance(o.Provenance)
	if err != nil {
		return err
	}
	return r.writeOp(ctx, "create outcome "+o.ID, func(tx neo4j.ManagedTransaction) (any, error) {
		return tx.Run(ctx,
			"MERGE (n:"+labelOutcome+" {id: $id}) "+
				"SET n.verification_status = $status, n.value = $value, n.provenance = $provenance",
			map[string]any{
				"id":         o.ID,
				"status":     string(o.VerificationStatus),
				"value":      value,
				"provenance": provenance,
			},
		)
	})
}

func (r *Neo4jRepository) GetState(ctx context.Context, id string) (domain.State, error) {
	node, err := r.getNode(ctx, labelState, id)
	if err != nil {
		return domain.State{}, err
	}
	props, err := unmarshalProps(node.Props["properties"])
	if err != nil {
		return domain.State{}, err
	}
	return domain.State{ID: id, Properties: props}, nil
}

func (r *Neo4jRepository) GetIntervention(ctx context.Context, id string) (domain.Intervention, error) {
	node, err := r.getNode(ctx, labelIntervention, id)
	if err != nil {
		return domain.Intervention{}, err
	}
	props, err := unmarshalProps(node.Props["properties"])
	if err != nil {
		return domain.Intervention{}, err
	}
	typ, _ := node.Props["type"].(string)
	return domain.Intervention{ID: id, Type: domain.InterventionType(typ), Properties: props}, nil
}

func (r *Neo4jRepository) GetOutcome(ctx context.Context, id string) (domain.Outcome, error) {
	node, err := r.getNode(ctx, labelOutcome, id)
	if err != nil {
		return domain.Outcome{}, err
	}
	return outcomeFromNode(id, node)
}

func (r *Neo4jRepository) GetMetaHeuristic(ctx context.Context, id string) (domain.MetaHeuristic, error) {
	node, err := r.getNode(ctx, labelMetaHeuristic, id)
	if err != nil {
		return domain.MetaHeuristic{}, err
	}
	return metaHeuristicFromNode(node), nil
}

func (r *Neo4jRepository) getNode(ctx context.Context, label, id string) (neo4j.Node, error) {
	res, err := r.read(ctx, func(tx neo4j.ManagedTransaction) (any, error) {
		result, err := tx.Run(ctx,
			"MATCH (n:"+label+" {id: $id}) RETURN n",
			map[string]any{"id": id},
		)
		if err != nil {
			return nil, err
		}
		rec, err := result.Single(ctx)
		if err != nil {
			return nil, err
		}
		return recordNode(rec, "n")
	})
	if err != nil {
		return neo4j.Node{}, fmt.Errorf("get %s %q: %w", label, id, err)
	}
	return res.(neo4j.Node), nil
}

func (r *Neo4jRepository) CreatePreConditionFor(ctx context.Context, stateID, interventionID string) error {
	return r.writeOp(ctx, "create pre_condition_for edge", func(tx neo4j.ManagedTransaction) (any, error) {
		return tx.Run(ctx,
			"MATCH (s:"+labelState+" {id: $stateID}) "+
				"MATCH (i:"+labelIntervention+" {id: $interventionID}) "+
				"MERGE (s)-[:"+domain.PreConditionFor+"]->(i)",
			map[string]any{"stateID": stateID, "interventionID": interventionID},
		)
	})
}

func (r *Neo4jRepository) CreateProduced(ctx context.Context, interventionID, outcomeID string, edge domain.ProducedEdge) error {
	src := edge.EpistemicSource
	if src == "" {
		src = domain.EpistemicObservational
	}
	return r.writeOp(ctx, "create produced edge", func(tx neo4j.ManagedTransaction) (any, error) {
		return tx.Run(ctx,
			"MATCH (i:"+labelIntervention+" {id: $interventionID}) "+
				"MATCH (o:"+labelOutcome+" {id: $outcomeID}) "+
				"MERGE (i)-[e:"+domain.Produced+"]->(o) "+
				"SET e.effect_size = $effectSize, e.confidence = $confidence, e.epistemic_source = $epistemicSource",
			map[string]any{
				"interventionID":  interventionID,
				"outcomeID":       outcomeID,
				"effectSize":      edge.EffectSize,
				"confidence":      edge.Confidence,
				"epistemicSource": string(src),
			},
		)
	})
}

func (r *Neo4jRepository) CreateMetaHeuristic(ctx context.Context, mh domain.MetaHeuristic, abstractedFrom []string) error {
	return r.writeOp(ctx, "create meta-heuristic "+mh.ID, func(tx neo4j.ManagedTransaction) (any, error) {
		// Validate first: every referenced id must resolve to exactly one node.
		// A bare `WHERE n.id IN $ids` would silently drop nonexistent ids, so we
		// count matches per requested id and reject anything that is not exactly
		// one. On any failure we return an error, rolling back the whole tx so
		// neither the node nor a partial ABSTRACTED_FROM edge is left behind.
		if len(abstractedFrom) > 0 {
			check, err := tx.Run(ctx,
				"UNWIND $ids AS wantId "+
					"OPTIONAL MATCH (n {id: wantId}) "+
					"WITH wantId, count(n) AS c WHERE c <> 1 "+
					"RETURN wantId AS missing",
				map[string]any{"ids": abstractedFrom},
			)
			if err != nil {
				return nil, err
			}
			bad, err := check.Collect(ctx)
			if err != nil {
				return nil, err
			}
			if len(bad) > 0 {
				return nil, fmt.Errorf("abstractedFrom references do not each resolve to exactly one node: %d invalid", len(bad))
			}
		}

		result, err := tx.Run(ctx,
			"CREATE (m:"+labelMetaHeuristic+" {id: $id, definition: $definition, embedding_pending: true}) "+
				"WITH m UNWIND $ids AS targetId "+
				"MATCH (t {id: targetId}) "+
				"CREATE (m)-[:"+domain.AbstractedFrom+"]->(t) "+
				"RETURN count(*) AS edges",
			map[string]any{"id": mh.ID, "definition": mh.Definition, "ids": abstractedFrom},
		)
		if err != nil {
			return nil, err
		}
		// Consume the result so the write is applied within this tx.
		_, err = result.Consume(ctx)
		return nil, err
	})
}

func (r *Neo4jRepository) ClearEmbeddingPending(ctx context.Context, id string) error {
	return r.writeOp(ctx, "clear embedding_pending "+id, func(tx neo4j.ManagedTransaction) (any, error) {
		return tx.Run(ctx,
			"MATCH (m:"+labelMetaHeuristic+" {id: $id}) SET m.embedding_pending = false",
			map[string]any{"id": id},
		)
	})
}

func (r *Neo4jRepository) ListEmbeddingPending(ctx context.Context) ([]domain.MetaHeuristic, error) {
	res, err := r.read(ctx, func(tx neo4j.ManagedTransaction) (any, error) {
		result, err := tx.Run(ctx,
			"MATCH (m:"+labelMetaHeuristic+" {embedding_pending: true}) RETURN m",
			nil,
		)
		if err != nil {
			return nil, err
		}
		recs, err := result.Collect(ctx)
		if err != nil {
			return nil, err
		}
		out := make([]domain.MetaHeuristic, 0, len(recs))
		for _, rec := range recs {
			node, err := recordNode(rec, "m")
			if err != nil {
				return nil, err
			}
			out = append(out, metaHeuristicFromNode(node))
		}
		return out, nil
	})
	if err != nil {
		return nil, fmt.Errorf("list embedding-pending meta-heuristics: %w", err)
	}
	return res.([]domain.MetaHeuristic), nil
}

func (r *Neo4jRepository) UpdateOutcomeVerification(ctx context.Context, outcomeID string, status domain.VerificationStatus, confidence float64) error {
	return r.writeOp(ctx, "update outcome verification "+outcomeID, func(tx neo4j.ManagedTransaction) (any, error) {
		return tx.Run(ctx,
			"MATCH (i:"+labelIntervention+")-[e:"+domain.Produced+"]->(o:"+labelOutcome+" {id: $id}) "+
				"SET o.verification_status = $status, e.confidence = $confidence",
			map[string]any{"id": outcomeID, "status": string(status), "confidence": confidence},
		)
	})
}

func (r *Neo4jRepository) TraceCausalChain(ctx context.Context, metaHeuristicID string) ([]CausalTriplet, error) {
	res, err := r.read(ctx, func(tx neo4j.ManagedTransaction) (any, error) {
		result, err := tx.Run(ctx,
			"MATCH (m:"+labelMetaHeuristic+" {id: $id})-[:"+domain.AbstractedFrom+"]->(t) "+
				"MATCH (s:"+labelState+")-[:"+domain.PreConditionFor+"]->(i:"+labelIntervention+")-[:"+domain.Produced+"]->(o:"+labelOutcome+") "+
				"WHERE t = s OR t = i OR t = o "+
				"RETURN DISTINCT s, i, o",
			map[string]any{"id": metaHeuristicID},
		)
		if err != nil {
			return nil, err
		}
		recs, err := result.Collect(ctx)
		if err != nil {
			return nil, err
		}
		triplets := make([]CausalTriplet, 0, len(recs))
		for _, rec := range recs {
			triplet, err := tripletFromRecord(rec)
			if err != nil {
				return nil, err
			}
			triplets = append(triplets, triplet)
		}
		return triplets, nil
	})
	if err != nil {
		return nil, fmt.Errorf("trace causal chain %q: %w", metaHeuristicID, err)
	}
	return res.([]CausalTriplet), nil
}

func tripletFromRecord(rec *neo4j.Record) (CausalTriplet, error) {
	sNode, err := recordNode(rec, "s")
	if err != nil {
		return CausalTriplet{}, err
	}
	iNode, err := recordNode(rec, "i")
	if err != nil {
		return CausalTriplet{}, err
	}
	oNode, err := recordNode(rec, "o")
	if err != nil {
		return CausalTriplet{}, err
	}
	sProps, err := unmarshalProps(sNode.Props["properties"])
	if err != nil {
		return CausalTriplet{}, err
	}
	iProps, err := unmarshalProps(iNode.Props["properties"])
	if err != nil {
		return CausalTriplet{}, err
	}
	outcome, err := outcomeFromNode(stringProp(oNode.Props["id"]), oNode)
	if err != nil {
		return CausalTriplet{}, err
	}
	iType, _ := iNode.Props["type"].(string)
	return CausalTriplet{
		State:        domain.State{ID: stringProp(sNode.Props["id"]), Properties: sProps},
		Intervention: domain.Intervention{ID: stringProp(iNode.Props["id"]), Type: domain.InterventionType(iType), Properties: iProps},
		Outcome:      outcome,
	}, nil
}

func metaHeuristicFromNode(node neo4j.Node) domain.MetaHeuristic {
	def, _ := node.Props["definition"].(string)
	pending, _ := node.Props["embedding_pending"].(bool)
	return domain.MetaHeuristic{
		ID:               stringProp(node.Props["id"]),
		Definition:       def,
		EmbeddingPending: pending,
	}
}

func outcomeFromNode(id string, node neo4j.Node) (domain.Outcome, error) {
	value, err := unmarshalProps(node.Props["value"])
	if err != nil {
		return domain.Outcome{}, err
	}
	provenance, err := unmarshalProvenance(node.Props["provenance"])
	if err != nil {
		return domain.Outcome{}, err
	}
	status, _ := node.Props["verification_status"].(string)
	return domain.Outcome{
		ID:                 id,
		VerificationStatus: domain.VerificationStatus(status),
		Value:              value,
		Provenance:         provenance,
	}, nil
}

func recordNode(rec *neo4j.Record, key string) (neo4j.Node, error) {
	v, ok := rec.Get(key)
	if !ok {
		return neo4j.Node{}, fmt.Errorf("record has no field %q", key)
	}
	node, ok := v.(neo4j.Node)
	if !ok {
		return neo4j.Node{}, fmt.Errorf("field %q is not a node", key)
	}
	return node, nil
}

func stringProp(v any) string {
	s, _ := v.(string)
	return s
}

// marshalProps serializes a property map to a JSON string so nested structures
// round-trip through Neo4j (which stores only flat primitive properties). An
// empty map is stored as "{}".
func marshalProps(props map[string]any) (string, error) {
	if props == nil {
		props = map[string]any{}
	}
	b, err := json.Marshal(props)
	if err != nil {
		return "", fmt.Errorf("marshal properties: %w", err)
	}
	return string(b), nil
}

func unmarshalProps(v any) (map[string]any, error) {
	s, ok := v.(string)
	if !ok || s == "" {
		return map[string]any{}, nil
	}
	var props map[string]any
	if err := json.Unmarshal([]byte(s), &props); err != nil {
		return nil, fmt.Errorf("unmarshal properties: %w", err)
	}
	return props, nil
}

func marshalProvenance(p *domain.ProvenanceLocator) (any, error) {
	if p == nil {
		return nil, nil
	}
	b, err := json.Marshal(p)
	if err != nil {
		return nil, fmt.Errorf("marshal provenance: %w", err)
	}
	return string(b), nil
}

func unmarshalProvenance(v any) (*domain.ProvenanceLocator, error) {
	s, ok := v.(string)
	if !ok || s == "" {
		return nil, nil
	}
	var p domain.ProvenanceLocator
	if err := json.Unmarshal([]byte(s), &p); err != nil {
		return nil, fmt.Errorf("unmarshal provenance: %w", err)
	}
	return &p, nil
}
