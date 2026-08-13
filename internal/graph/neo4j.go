package graph

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/arborette/arborette/internal/domain"
	"github.com/neo4j/neo4j-go-driver/v5/neo4j"
)

// ErrNotFound reports that a node with the requested id does not exist, as
// opposed to the read itself failing. Callers match it with errors.Is to tell an
// absent node from an unreachable database.
var ErrNotFound = errors.New("node not found")

// Node labels. The abstraction node is conceptually "Meta-Heuristic"; the graph
// uses the hyphen-free MetaHeuristic label so no Cypher needs backtick quoting,
// keeping every statement inside the Neptune-portable subset.
const (
	labelState           = "State"
	labelIntervention    = "Intervention"
	labelOutcome         = "Outcome"
	labelMetaHeuristic   = "MetaHeuristic"
	labelDataColumn      = "DataColumn"
	labelCausalGraphMeta = "CausalGraphMeta"
)

// goal_id and sleep_derived are deliberately written as top-level node
// properties rather than entries in the marshalProps JSON blob: the
// eligible-finding query MATCHes on them, and the Neptune-portable Cypher subset
// this package is restricted to cannot filter inside a JSON string property.

// Neo4jRepository is the driver-holding, Cypher-backed implementation of this
// package's graph access.
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
			"MERGE (n:"+labelState+" {id: $id}) SET n.goal_id = $goalID, n.properties = $properties",
			map[string]any{"id": s.ID, "goalID": s.GoalID, "properties": props},
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
			"MERGE (n:"+labelIntervention+" {id: $id}) "+
				"SET n.goal_id = $goalID, n.type = $type, n.sleep_derived = $sleepDerived, n.properties = $properties",
			map[string]any{
				"id":           i.ID,
				"goalID":       i.GoalID,
				"type":         string(i.Type),
				"sleepDerived": i.SleepDerived,
				"properties":   props,
			},
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
	// An unrecorded support (0) is stored as null — SET removes the property —
	// so it stays absent, mirroring the nil-provenance omission.
	var support any
	if o.Support > 0 {
		support = o.Support
	}
	return r.writeOp(ctx, "create outcome "+o.ID, func(tx neo4j.ManagedTransaction) (any, error) {
		return tx.Run(ctx,
			"MERGE (n:"+labelOutcome+" {id: $id}) "+
				"SET n.goal_id = $goalID, n.verification_status = $status, n.value = $value, n."+domain.PropSupport+" = $support, n.provenance = $provenance",
			map[string]any{
				"id":         o.ID,
				"goalID":     o.GoalID,
				"status":     string(o.VerificationStatus),
				"value":      value,
				"support":    support,
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
	return stateFromNode(id, node)
}

func (r *Neo4jRepository) GetIntervention(ctx context.Context, id string) (domain.Intervention, error) {
	node, err := r.getNode(ctx, labelIntervention, id)
	if err != nil {
		return domain.Intervention{}, err
	}
	return interventionFromNode(id, node)
}

func (r *Neo4jRepository) GetOutcome(ctx context.Context, id string) (domain.Outcome, error) {
	node, err := r.getNode(ctx, labelOutcome, id)
	if err != nil {
		return domain.Outcome{}, err
	}
	return outcomeFromNode(id, node)
}

// GetMetaHeuristic fetches one Meta-Heuristic, yielding ErrNotFound for an
// unknown id. Hydrating a set of ids is GetMetaHeuristics, which is one round trip.
func (r *Neo4jRepository) GetMetaHeuristic(ctx context.Context, id string) (domain.MetaHeuristic, error) {
	node, err := r.getNode(ctx, labelMetaHeuristic, id)
	if err != nil {
		return domain.MetaHeuristic{}, err
	}
	return metaHeuristicFromNode(node), nil
}

// GetMetaHeuristics fetches a batch of Meta-Heuristics in one round trip, the
// hydration step behind a similarity search. Ids with no node are simply absent
// from the result rather than an error -- a search whose neighbourhood touches a
// retired node must still answer -- and the result carries Neo4j's own ordering,
// so a caller that needs the ids' order re-keys by id.
func (r *Neo4jRepository) GetMetaHeuristics(ctx context.Context, ids []string) ([]domain.MetaHeuristic, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	res, err := r.read(ctx, func(tx neo4j.ManagedTransaction) (any, error) {
		result, err := tx.Run(ctx,
			"MATCH (n:"+labelMetaHeuristic+") WHERE n.id IN $ids RETURN n",
			map[string]any{"ids": ids},
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
			node, err := recordNode(rec, "n")
			if err != nil {
				return nil, err
			}
			out = append(out, metaHeuristicFromNode(node))
		}
		return out, nil
	})
	if err != nil {
		return nil, fmt.Errorf("get %d MetaHeuristics: %w", len(ids), err)
	}
	return res.([]domain.MetaHeuristic), nil
}

// getNode fetches one node by label and application-assigned id, wrapping a miss
// in ErrNotFound.
//
// The zero/duplicate branch is decided by collecting rows rather than by
// inspecting the driver's error: Result.Single reports both "no records" and
// "more than one record" as the same *UsageError type, whose only field is a
// message string, and neither IsNeo4jError nor IsUsageError separates them.
// Matching on that message would be undocumented, brittle API surface.
func (r *Neo4jRepository) getNode(ctx context.Context, label, id string) (neo4j.Node, error) {
	res, err := r.read(ctx, func(tx neo4j.ManagedTransaction) (any, error) {
		result, err := tx.Run(ctx,
			"MATCH (n:"+label+" {id: $id}) RETURN n",
			map[string]any{"id": id},
		)
		if err != nil {
			return nil, err
		}
		recs, err := result.Collect(ctx)
		if err != nil {
			return nil, err
		}
		switch len(recs) {
		case 0:
			// A miss is returned as a nil value, not an error: the driver treats any
			// error out of a transaction function as "the client wants to roll back"
			// and skips the commit, so signalling the normal first-run case that way
			// would cost a RESET round-trip on every lookup that finds nothing.
			return nil, nil
		case 1:
			return recordNode(recs[0], "n")
		default:
			return nil, fmt.Errorf("id resolves to %d nodes", len(recs))
		}
	})
	if err != nil {
		return neo4j.Node{}, fmt.Errorf("get %s %q: %w", label, id, err)
	}
	if res == nil {
		return neo4j.Node{}, fmt.Errorf("get %s %q: %w", label, id, ErrNotFound)
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

// CreateMetaHeuristic writes the node, its ABSTRACTED_FROM edges, and
// embedding_pending:true within one transaction. It first validates that every
// abstractedFrom id resolves to exactly one existing node, erroring (and
// committing nothing) on any missing or duplicate reference, so a Meta-Heuristic
// is never abstracted from incomplete evidence.
func (r *Neo4jRepository) CreateMetaHeuristic(ctx context.Context, mh domain.MetaHeuristic, abstractedFrom []string) error {
	terms, err := marshalOntologyTerms(mh.OntologyTerms)
	if err != nil {
		return err
	}
	return r.writeOp(ctx, "create meta-heuristic "+mh.ID, func(tx neo4j.ManagedTransaction) (any, error) {
		// A bare `WHERE n.id IN $ids` would silently drop nonexistent ids, so
		// match counts are checked per requested id.
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

		// MERGE, not CREATE: a re-run after a mid-write crash must re-write the same
		// node rather than fail the uniqueness constraint. ON MATCH deliberately
		// leaves embedding_pending alone -- resurrecting a cleared flag would make a
		// finished abstraction look unprocessed and re-embed it forever.
		//
		// goal_id is set on create unconditionally, but the match-side heal is a
		// standalone FOREACH rather than an ON MATCH SET item: FOREACH cannot be
		// embedded in ON MATCH SET (which takes only set-items), and it must run once
		// before the per-edge UNWIND, not per edge. Guarding it on a non-empty goalID
		// is what lets a legacy node heal on re-abstraction while a goal-less caller
		// can never re-blank a node that already carries a goal.
		//
		// The abstraction provenance heals on the same guarded-FOREACH terms, for the
		// same reason: the re-link branch re-issues this write carrying only the
		// definition it read back, so an unguarded ON MATCH SET would blank the terms
		// and origin every time a later run widened an existing heuristic's evidence.
		// ontology_terms rides as a JSON string because Neo4j properties cannot nest.
		result, err := tx.Run(ctx,
			"MERGE (m:"+labelMetaHeuristic+" {id: $id}) "+
				"ON CREATE SET m.definition = $definition, m.embedding_pending = true, m.goal_id = $goalID, "+
				"m.ontology_terms = $terms, m.origin_goal_id = $originGoalID, m.origin_datasource_ref = $originRef "+
				"ON MATCH SET m.definition = $definition "+
				"FOREACH (_ IN CASE WHEN $goalID <> '' THEN [1] ELSE [] END | SET m.goal_id = $goalID) "+
				"FOREACH (_ IN CASE WHEN $terms <> '' THEN [1] ELSE [] END | SET m.ontology_terms = $terms) "+
				"FOREACH (_ IN CASE WHEN $originGoalID <> '' THEN [1] ELSE [] END | SET m.origin_goal_id = $originGoalID) "+
				"FOREACH (_ IN CASE WHEN $originRef <> '' THEN [1] ELSE [] END | SET m.origin_datasource_ref = $originRef) "+
				"WITH m UNWIND $ids AS targetId "+
				"MATCH (t {id: targetId}) "+
				"MERGE (m)-[:"+domain.AbstractedFrom+"]->(t) "+
				"RETURN count(*) AS edges",
			map[string]any{
				"id":           mh.ID,
				"definition":   mh.Definition,
				"goalID":       mh.GoalID,
				"terms":        terms,
				"originGoalID": mh.OriginGoalID,
				"originRef":    mh.OriginDataSourceRef,
				"ids":          abstractedFrom,
			},
		)
		if err != nil {
			return nil, err
		}
		// Consume the result so the write is applied within this tx.
		_, err = result.Consume(ctx)
		return nil, err
	})
}

// ClearEmbeddingPending marks a Meta-Heuristic's embedding as written to
// pgvector.
func (r *Neo4jRepository) ClearEmbeddingPending(ctx context.Context, id string) error {
	return r.writeOp(ctx, "clear embedding_pending "+id, func(tx neo4j.ManagedTransaction) (any, error) {
		return tx.Run(ctx,
			"MATCH (m:"+labelMetaHeuristic+" {id: $id}) SET m.embedding_pending = false",
			map[string]any{"id": id},
		)
	})
}

// DeleteMetaHeuristic removes a Meta-Heuristic node and its abstraction edges in
// both directions via DETACH DELETE: another heuristic abstracted from this one
// keeps its node but loses the meta_abstracted_from edge, so no dangling
// reference survives into a later query. The node's embedding row is the
// caller's to remove (the orchestrator deletes it from pgvector alongside).
// Removing an id that matches nothing is not an error: the desired end state --
// no node with that id -- already holds.
func (r *Neo4jRepository) DeleteMetaHeuristic(ctx context.Context, id string) error {
	return r.writeOp(ctx, "delete meta-heuristic "+id, func(tx neo4j.ManagedTransaction) (any, error) {
		result, err := tx.Run(ctx,
			"MATCH (m:"+labelMetaHeuristic+" {id: $id}) DETACH DELETE m "+
				"RETURN count(*) AS removed",
			map[string]any{"id": id},
		)
		if err != nil {
			return nil, err
		}
		// Consume the result so the write is applied within this tx.
		_, err = result.Consume(ctx)
		return nil, err
	})
}

// ListMetaHeuristics returns every Meta-Heuristic node, the graph side of the
// reconcile diff. It does not filter on the embedding-pending flag: reconcile
// needs the whole set so it can find both nodes whose embedding row is entirely
// missing and nodes whose row exists but lost its goal scope. Each node carries
// its EmbeddingPending flag, so a caller wanting only the crash-left-behind
// subset filters the result itself.
func (r *Neo4jRepository) ListMetaHeuristics(ctx context.Context) ([]domain.MetaHeuristic, error) {
	res, err := r.read(ctx, func(tx neo4j.ManagedTransaction) (any, error) {
		result, err := tx.Run(ctx,
			"MATCH (m:"+labelMetaHeuristic+") RETURN m",
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
		return nil, fmt.Errorf("list meta-heuristics: %w", err)
	}
	return res.([]domain.MetaHeuristic), nil
}

// ListEligibleFindings returns the Sleep-Cycle search's input set for one goal:
// complete State→Intervention→Outcome paths that are not sleep-derived (a prior
// run's macro-segments are search outputs, not atomic inputs) and whose outcome
// status is search-eligible (domain.SearchEligibleStatuses).
func (r *Neo4jRepository) ListEligibleFindings(ctx context.Context, goalID string) ([]CausalTriplet, error) {
	statuses := make([]string, 0, len(domain.SearchEligibleStatuses))
	for _, s := range domain.SearchEligibleStatuses {
		statuses = append(statuses, string(s))
	}
	res, err := r.read(ctx, func(tx neo4j.ManagedTransaction) (any, error) {
		result, err := tx.Run(ctx,
			"MATCH (s:"+labelState+")-[:"+domain.PreConditionFor+"]->(i:"+labelIntervention+")-[e:"+domain.Produced+"]->(o:"+labelOutcome+") "+
				"WHERE i.goal_id = $goalID "+
				"AND (i.sleep_derived IS NULL OR i.sleep_derived = false) "+
				"AND o.verification_status IN $statuses "+
				"AND coalesce(e.epistemic_source, 'observational') = 'observational' "+
				"RETURN s, i, o, e",
			map[string]any{"goalID": goalID, "statuses": statuses},
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
		return nil, fmt.Errorf("list eligible findings for %q: %w", goalID, err)
	}
	return res.([]CausalTriplet), nil
}

// MarkStaleMetaHeuristics flags every Meta-Heuristic for a goal whose
// ABSTRACTED_FROM components include a rejected Outcome, returning how many were
// marked. It triggers on rejected only -- deliberately not corrected, which
// domain.SearchEligibleStatuses admits, so a corrected-triggering sweep would
// flag the worker's own fresh output on the very next run.
func (r *Neo4jRepository) MarkStaleMetaHeuristics(ctx context.Context, goalID string) (int, error) {
	res, err := r.write(ctx, func(tx neo4j.ManagedTransaction) (any, error) {
		// Recompute the flag rather than only ever setting it, so the sweep is
		// idempotent and self-correcting: an analyst who corrects a rejected
		// outcome makes that evidence eligible again, and a set-only sweep would
		// leave the heuristic suppressed forever with no writer to clear it.
		result, err := tx.Run(ctx,
			"MATCH (m:"+labelMetaHeuristic+")-[:"+domain.AbstractedFrom+"]->(o:"+labelOutcome+") "+
				"WHERE o.goal_id = $goalID "+
				"WITH m, sum(CASE WHEN o.verification_status = $rejected THEN 1 ELSE 0 END) AS rejectedComponents "+
				"SET m.stale = rejectedComponents > 0 "+
				"RETURN count(CASE WHEN rejectedComponents > 0 THEN 1 END) AS marked",
			map[string]any{"goalID": goalID, "rejected": string(domain.VerificationRejected)},
		)
		if err != nil {
			return nil, err
		}
		rec, err := result.Single(ctx)
		if err != nil {
			return nil, err
		}
		marked, _ := rec.Get("marked")
		count, _ := marked.(int64)
		return int(count), nil
	})
	if err != nil {
		return 0, fmt.Errorf("mark stale meta-heuristics for %q: %w", goalID, err)
	}
	return res.(int), nil
}

// UpdateOutcomeVerification applies an HITL resolution to an Outcome and its
// inbound PRODUCED-edge confidence. CorrectOutcome is its correction counterpart.
func (r *Neo4jRepository) UpdateOutcomeVerification(ctx context.Context, outcomeID string, status domain.VerificationStatus, confidence float64) error {
	return r.writeOp(ctx, "update outcome verification "+outcomeID, func(tx neo4j.ManagedTransaction) (any, error) {
		return tx.Run(ctx,
			"MATCH (i:"+labelIntervention+")-[e:"+domain.Produced+"]->(o:"+labelOutcome+" {id: $id}) "+
				"WHERE coalesce(e.epistemic_source, 'observational') = 'observational' "+
				"SET o.verification_status = $status, e.confidence = $confidence",
			map[string]any{"id": outcomeID, "status": string(status), "confidence": confidence},
		)
	})
}

// CorrectOutcome applies an analyst's correction in one statement: the
// replacement value, the locator recomputed against it, the resolution status,
// and the inbound edge's confidence. All four move together so a failure cannot
// leave a corrected value sitting under an unreviewed status.
func (r *Neo4jRepository) CorrectOutcome(ctx context.Context, outcomeID string, value map[string]any, provenance *domain.ProvenanceLocator, status domain.VerificationStatus, confidence float64) error {
	marshalled, err := marshalProps(value)
	if err != nil {
		return err
	}
	locator, err := marshalProvenance(provenance)
	if err != nil {
		return err
	}
	return r.writeOp(ctx, "correct outcome "+outcomeID, func(tx neo4j.ManagedTransaction) (any, error) {
		return tx.Run(ctx,
			"MATCH (i:"+labelIntervention+")-[e:"+domain.Produced+"]->(o:"+labelOutcome+" {id: $id}) "+
				"WHERE coalesce(e.epistemic_source, 'observational') = 'observational' "+
				"SET o.value = $value, o.provenance = $provenance, o.verification_status = $status, e.confidence = $confidence",
			map[string]any{
				"id":         outcomeID,
				"value":      marshalled,
				"provenance": locator,
				"status":     string(status),
				"confidence": confidence,
			},
		)
	})
}

// ListExtractionOutcomes returns every extract-type outcome for a goal, each
// joined to its producing intervention and PRODUCED edge.
func (r *Neo4jRepository) ListExtractionOutcomes(ctx context.Context, goalID string) ([]ExtractionOutcome, error) {
	res, err := r.read(ctx, func(tx neo4j.ManagedTransaction) (any, error) {
		result, err := tx.Run(ctx,
			"MATCH (i:"+labelIntervention+" {goal_id: $goalID, type: $type})-[e:"+domain.Produced+"]->(o:"+labelOutcome+") "+
				"WHERE coalesce(e.epistemic_source, 'observational') = 'observational' "+
				"RETURN o, i, e.confidence AS confidence",
			map[string]any{"goalID": goalID, "type": string(domain.InterventionExtract)},
		)
		if err != nil {
			return nil, err
		}
		recs, err := result.Collect(ctx)
		if err != nil {
			return nil, err
		}
		out := make([]ExtractionOutcome, 0, len(recs))
		for _, rec := range recs {
			outcome, err := extractionOutcomeFromRecord(rec)
			if err != nil {
				return nil, err
			}
			out = append(out, outcome)
		}
		return out, nil
	})
	if err != nil {
		return nil, fmt.Errorf("list extraction outcomes for %q: %w", goalID, err)
	}
	return res.([]ExtractionOutcome), nil
}

// GetExtractionOutcome returns one extract-type outcome by id, joined to its
// producing intervention and PRODUCED edge. It yields ErrNotFound both for an
// unknown id and for a query-type outcome, which is verified by construction and
// so is never human-resolvable.
func (r *Neo4jRepository) GetExtractionOutcome(ctx context.Context, outcomeID string) (ExtractionOutcome, error) {
	res, err := r.read(ctx, func(tx neo4j.ManagedTransaction) (any, error) {
		result, err := tx.Run(ctx,
			"MATCH (i:"+labelIntervention+" {type: $type})-[e:"+domain.Produced+"]->(o:"+labelOutcome+" {id: $id}) "+
				"WHERE coalesce(e.epistemic_source, 'observational') = 'observational' "+
				"RETURN o, i, e.confidence AS confidence",
			map[string]any{"id": outcomeID, "type": string(domain.InterventionExtract)},
		)
		if err != nil {
			return nil, err
		}
		recs, err := result.Collect(ctx)
		if err != nil {
			return nil, err
		}
		// A miss returns nil rather than an error, for the same reason getNode
		// does: an error out of a transaction function costs a rollback round-trip
		// on what is a normal not-found answer.
		switch len(recs) {
		case 0:
			return nil, nil
		case 1:
			return extractionOutcomeFromRecord(recs[0])
		default:
			return nil, fmt.Errorf("id resolves to %d extraction outcomes", len(recs))
		}
	})
	if err != nil {
		return ExtractionOutcome{}, fmt.Errorf("get extraction outcome %q: %w", outcomeID, err)
	}
	if res == nil {
		return ExtractionOutcome{}, fmt.Errorf("get extraction outcome %q: %w", outcomeID, ErrNotFound)
	}
	return res.(ExtractionOutcome), nil
}

func extractionOutcomeFromRecord(rec *neo4j.Record) (ExtractionOutcome, error) {
	oNode, err := recordNode(rec, "o")
	if err != nil {
		return ExtractionOutcome{}, err
	}
	iNode, err := recordNode(rec, "i")
	if err != nil {
		return ExtractionOutcome{}, err
	}
	outcome, err := outcomeFromNode(stringProp(oNode.Props["id"]), oNode)
	if err != nil {
		return ExtractionOutcome{}, err
	}
	intervention, err := interventionFromNode(stringProp(iNode.Props["id"]), iNode)
	if err != nil {
		return ExtractionOutcome{}, err
	}
	confidence, _ := rec.Get("confidence")
	weight, _ := confidence.(float64)
	field, _ := intervention.Properties["field"].(string)
	method, _ := intervention.Properties["method"].(string)
	return ExtractionOutcome{
		OutcomeID:          outcome.ID,
		GoalID:             outcome.GoalID,
		Field:              field,
		Method:             method,
		Value:              outcome.Value,
		Provenance:         outcome.Provenance,
		VerificationStatus: outcome.VerificationStatus,
		Confidence:         weight,
	}, nil
}

// TraceCausalChain walks ABSTRACTED_FROM from a Meta-Heuristic back to the
// State/Intervention/Outcome triplet(s) that support it. Unlike the collection reads,
// it intentionally returns both observational and causal_inferred edges, each triplet
// labeled with its edge's epistemic_source so the caller can distinguish them.
func (r *Neo4jRepository) TraceCausalChain(ctx context.Context, metaHeuristicID string) ([]CausalTriplet, error) {
	res, err := r.read(ctx, func(tx neo4j.ManagedTransaction) (any, error) {
		result, err := tx.Run(ctx,
			"MATCH (m:"+labelMetaHeuristic+" {id: $id})-[:"+domain.AbstractedFrom+"]->(t) "+
				"MATCH (s:"+labelState+")-[:"+domain.PreConditionFor+"]->(i:"+labelIntervention+")-[e:"+domain.Produced+"]->(o:"+labelOutcome+") "+
				"WHERE t = s OR t = i OR t = o "+
				"RETURN DISTINCT s, i, o, e",
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

// AbstractionSourceFilters returns the filter conjunctions of the Interventions a
// Meta-Heuristic was abstracted from — the concrete segments its definition
// generalizes. It is the same-dataset re-instantiation path's input: when the
// heuristic's origin data source matches the target's, its own source
// conjunctions are already expressed in that dataset's columns, so no grounding
// call is needed to recover them.
//
// Only the Intervention hop is walked (the ABSTRACTED_FROM set also carries State
// and Outcome ids), and a conjunction that fails to decode is skipped rather than
// failing the read: one malformed property must not cost the caller every other
// conjunction the heuristic offers.
func (r *Neo4jRepository) AbstractionSourceFilters(ctx context.Context, metaHeuristicID string) ([][]domain.Constraint, error) {
	res, err := r.read(ctx, func(tx neo4j.ManagedTransaction) (any, error) {
		result, err := tx.Run(ctx,
			"MATCH (m:"+labelMetaHeuristic+" {id: $id})-[:"+domain.AbstractedFrom+"]->(i:"+labelIntervention+") "+
				"RETURN i ORDER BY i.id",
			map[string]any{"id": metaHeuristicID},
		)
		if err != nil {
			return nil, err
		}
		recs, err := result.Collect(ctx)
		if err != nil {
			return nil, err
		}
		out := make([][]domain.Constraint, 0, len(recs))
		for _, rec := range recs {
			node, err := recordNode(rec, "i")
			if err != nil {
				return nil, err
			}
			intervention, err := interventionFromNode(stringProp(node.Props["id"]), node)
			if err != nil {
				continue
			}
			filters, err := domain.DecodeConstraints(intervention.Properties[domain.PropNewFilters])
			if err != nil || len(filters) == 0 {
				continue
			}
			out = append(out, filters)
		}
		return out, nil
	})
	if err != nil {
		return nil, fmt.Errorf("abstraction source filters for %q: %w", metaHeuristicID, err)
	}
	return res.([][]domain.Constraint), nil
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
	state, err := stateFromNode(stringProp(sNode.Props["id"]), sNode)
	if err != nil {
		return CausalTriplet{}, err
	}
	intervention, err := interventionFromNode(stringProp(iNode.Props["id"]), iNode)
	if err != nil {
		return CausalTriplet{}, err
	}
	outcome, err := outcomeFromNode(stringProp(oNode.Props["id"]), oNode)
	if err != nil {
		return CausalTriplet{}, err
	}
	// The producing PRODUCED edge is present in every triplet query (RETURN ... e), so
	// its epistemic_source labels the triplet. An edge with no recorded value defaults
	// to observational, preserving the forward-compatibility rule.
	source := domain.EpistemicObservational
	if rel, err := recordRelationship(rec, "e"); err == nil {
		if s := stringProp(rel.Props["epistemic_source"]); s != "" {
			source = domain.EpistemicSource(s)
		}
	}
	return CausalTriplet{State: state, Intervention: intervention, Outcome: outcome, EpistemicSource: source}, nil
}

func stateFromNode(id string, node neo4j.Node) (domain.State, error) {
	props, err := unmarshalProps(node.Props["properties"])
	if err != nil {
		return domain.State{}, err
	}
	return domain.State{ID: id, GoalID: stringProp(node.Props["goal_id"]), Properties: props}, nil
}

func interventionFromNode(id string, node neo4j.Node) (domain.Intervention, error) {
	props, err := unmarshalProps(node.Props["properties"])
	if err != nil {
		return domain.Intervention{}, err
	}
	typ, _ := node.Props["type"].(string)
	sleepDerived, _ := node.Props["sleep_derived"].(bool)
	return domain.Intervention{
		ID:           id,
		GoalID:       stringProp(node.Props["goal_id"]),
		Type:         domain.InterventionType(typ),
		SleepDerived: sleepDerived,
		Properties:   props,
	}, nil
}

// metaHeuristicFromNode decodes one Meta-Heuristic node. Undecodable ontology
// terms read as none rather than failing the read: the terms are a reuse
// optimization (their absence routes the heuristic through grounding), so a
// malformed property must not blank out a search whose neighbourhood touches it.
func metaHeuristicFromNode(node neo4j.Node) domain.MetaHeuristic {
	def, _ := node.Props["definition"].(string)
	pending, _ := node.Props["embedding_pending"].(bool)
	stale, _ := node.Props["stale"].(bool)
	return domain.MetaHeuristic{
		ID:                  stringProp(node.Props["id"]),
		Definition:          def,
		GoalID:              stringProp(node.Props["goal_id"]),
		EmbeddingPending:    pending,
		Stale:               stale,
		OntologyTerms:       unmarshalOntologyTerms(node.Props["ontology_terms"]),
		OriginGoalID:        stringProp(node.Props["origin_goal_id"]),
		OriginDataSourceRef: stringProp(node.Props["origin_datasource_ref"]),
	}
}

// marshalOntologyTerms serializes the term map to the JSON string the node
// property holds. An empty set marshals to "" rather than "[]" so the write's
// guarded FOREACH reads it as "this caller carries no terms" and leaves any
// persisted ones alone.
func marshalOntologyTerms(terms []domain.OntologyTerm) (string, error) {
	if len(terms) == 0 {
		return "", nil
	}
	b, err := json.Marshal(terms)
	if err != nil {
		return "", fmt.Errorf("marshal ontology terms: %w", err)
	}
	return string(b), nil
}

func unmarshalOntologyTerms(v any) []domain.OntologyTerm {
	s, ok := v.(string)
	if !ok || s == "" {
		return nil
	}
	var terms []domain.OntologyTerm
	if err := json.Unmarshal([]byte(s), &terms); err != nil {
		return nil
	}
	return terms
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
	support, _ := node.Props[domain.PropSupport].(int64)
	return domain.Outcome{
		ID:                 id,
		GoalID:             stringProp(node.Props["goal_id"]),
		VerificationStatus: domain.VerificationStatus(status),
		Value:              value,
		Support:            support,
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
