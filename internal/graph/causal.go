package graph

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
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

// GetCausalEvidence returns the latest non-superseded causal_inferred PRODUCED edge
// for an intervention and whether one exists. It is the dedicated causal-evidence
// read path — deliberately exempt from the observational-only collection filter — so
// a consumer that weights causal knowledge reaches the backdoor-adjusted effect and
// its refutation confidence. Ordering by graph_version DESC picks the most recent
// verification; superseded edges (retracted by a newer version or a non-confirming
// re-verification) are excluded by construction.
func (r *Neo4jRepository) GetCausalEvidence(ctx context.Context, interventionID string) (CausalEvidence, bool, error) {
	res, err := r.read(ctx, func(tx neo4j.ManagedTransaction) (any, error) {
		result, err := tx.Run(ctx,
			"MATCH (i:"+labelIntervention+" {id: $id})-[e:"+domain.Produced+"]->(:"+labelOutcome+") "+
				"WHERE e.epistemic_source = $source AND coalesce(e.superseded, false) = false "+
				"RETURN e ORDER BY e.graph_version DESC LIMIT 1",
			map[string]any{"id": interventionID, "source": string(domain.EpistemicCausalInferred)},
		)
		if err != nil {
			return nil, err
		}
		recs, err := result.Collect(ctx)
		if err != nil {
			return nil, err
		}
		if len(recs) == 0 {
			return nil, nil
		}
		rel, err := recordRelationship(recs[0], "e")
		if err != nil {
			return nil, err
		}
		return causalEvidenceFromRelationship(interventionID, rel), nil
	})
	if err != nil {
		return CausalEvidence{}, false, fmt.Errorf("get causal evidence for %q: %w", interventionID, err)
	}
	if res == nil {
		return CausalEvidence{}, false, nil
	}
	return res.(CausalEvidence), true, nil
}

// CausalEvidenceForHeuristics returns the strongest live causal evidence behind each
// of the given Meta-Heuristics, keyed by heuristic id and absent for those with none.
// It walks each heuristic's ABSTRACTED_FROM links to the Interventions it generalized
// and reads their non-superseded causal_inferred edges, so a consumer can say which
// served heuristics rest on a verified effect and how strongly.
//
// It is one query rather than a lookup per traced intervention: a search returns up
// to the caller's top-k heuristics, each abstracted from several triplets, and a
// per-intervention read would put that product of round trips on every search. Like
// the single-intervention lookup, it is exempt from the observational-only collection
// filter by design -- it reads nothing but causal edges.
func (r *Neo4jRepository) CausalEvidenceForHeuristics(ctx context.Context, metaHeuristicIDs []string) (map[string]CausalEvidence, error) {
	if len(metaHeuristicIDs) == 0 {
		return map[string]CausalEvidence{}, nil
	}
	res, err := r.read(ctx, func(tx neo4j.ManagedTransaction) (any, error) {
		result, err := tx.Run(ctx,
			"MATCH (m:"+labelMetaHeuristic+")-[:"+domain.AbstractedFrom+"]->(i:"+labelIntervention+")"+
				"-[e:"+domain.Produced+"]->(:"+labelOutcome+") "+
				"WHERE m.id IN $ids AND e.epistemic_source = $source AND coalesce(e.superseded, false) = false "+
				"RETURN m.id AS mid, i.id AS iid, e",
			map[string]any{"ids": metaHeuristicIDs, "source": string(domain.EpistemicCausalInferred)},
		)
		if err != nil {
			return nil, err
		}
		recs, err := result.Collect(ctx)
		if err != nil {
			return nil, err
		}
		out := make(map[string]CausalEvidence, len(recs))
		for _, rec := range recs {
			mid, _ := rec.Get("mid")
			heuristicID, ok := mid.(string)
			if !ok {
				continue
			}
			rel, err := recordRelationship(rec, "e")
			if err != nil {
				return nil, err
			}
			iid, _ := rec.Get("iid")
			interventionID, _ := iid.(string)
			evidence := causalEvidenceFromRelationship(interventionID, rel)
			// A heuristic generalizes several findings; the strongest surviving one is
			// what it is served with, so a single weakly-confirmed component cannot
			// understate evidence the others carry.
			if prior, seen := out[heuristicID]; seen && prior.Confidence >= evidence.Confidence {
				continue
			}
			out[heuristicID] = evidence
		}
		return out, nil
	})
	if err != nil {
		return nil, fmt.Errorf("get causal evidence for heuristics: %w", err)
	}
	return res.(map[string]CausalEvidence), nil
}

func causalEvidenceFromRelationship(interventionID string, rel neo4j.Relationship) CausalEvidence {
	effect, _ := rel.Props["effect_size"].(float64)
	confidence, _ := rel.Props["confidence"].(float64)
	version, _ := rel.Props["graph_version"].(int64)
	return CausalEvidence{
		InterventionID: interventionID,
		EffectSize:     effect,
		Confidence:     confidence,
		GraphVersion:   int(version),
	}
}

func dataColumnID(goalID, datasourceRef, name string) string {
	return hashID(goalID, datasourceRef, name)
}

func causalMetaID(goalID, datasourceRef string, version int) string {
	return hashID(goalID, datasourceRef, strconv.Itoa(version))
}

// causalOutcomeID is the deterministic id of the causal Outcome a verification writes,
// keyed on (interventionID, graph version) so a re-run at the same version MERGEs over
// the same node idempotently while a newer version allocates a distinct one.
func causalOutcomeID(interventionID string, version int) string {
	return hashID(interventionID, strconv.Itoa(version))
}

func hashID(parts ...string) string {
	h := sha256.Sum256([]byte(strings.Join(parts, "\x00")))
	return hex.EncodeToString(h[:])
}

// WriteCausalVerification records a confirmed verification additively in one write:
// it MERGEs the causal Outcome (verified, the adjusted effect in its value) on the
// deterministic (interventionID, version) id, MERGEs the causal_inferred PRODUCED
// edge from the finding's Intervention carrying the adjusted effect, the
// refutation-derived confidence, and the graph version, and in the same transaction
// supersedes every prior (older-version) causal_inferred edge for that intervention.
// Observational edges are never touched (the supersession clause filters on
// epistemic_source). The MERGEs make a re-run at the same version idempotent — one
// Outcome, one edge.
func (r *Neo4jRepository) WriteCausalVerification(ctx context.Context, interventionID string, version int, outcome domain.Outcome, edge domain.ProducedEdge) error {
	value, err := marshalProps(outcome.Value)
	if err != nil {
		return err
	}
	oid := causalOutcomeID(interventionID, version)
	return r.writeOp(ctx, "write causal verification "+interventionID, func(tx neo4j.ManagedTransaction) (any, error) {
		if _, err := tx.Run(ctx,
			"MERGE (o:"+labelOutcome+" {id: $oid}) "+
				"SET o.goal_id = $goalID, o.verification_status = $status, o.value = $value",
			map[string]any{"oid": oid, "goalID": outcome.GoalID, "status": string(outcome.VerificationStatus), "value": value},
		); err != nil {
			return nil, err
		}
		if _, err := tx.Run(ctx,
			"MATCH (i:"+labelIntervention+" {id: $interventionID}) "+
				"MATCH (o:"+labelOutcome+" {id: $oid}) "+
				"MERGE (i)-[e:"+domain.Produced+"]->(o) "+
				"SET e.effect_size = $effectSize, e.confidence = $confidence, "+
				"e.epistemic_source = $source, e.superseded = false, e.graph_version = $version",
			map[string]any{
				"interventionID": interventionID,
				"oid":            oid,
				"effectSize":     edge.EffectSize,
				"confidence":     edge.Confidence,
				"source":         string(domain.EpistemicCausalInferred),
				"version":        version,
			},
		); err != nil {
			return nil, err
		}
		result, err := tx.Run(ctx,
			"MATCH (i:"+labelIntervention+" {id: $interventionID})-[pe:"+domain.Produced+"]->(:"+labelOutcome+") "+
				"WHERE pe.epistemic_source = $source AND coalesce(pe.superseded, false) = false AND pe.graph_version < $version "+
				"SET pe.superseded = true",
			map[string]any{"interventionID": interventionID, "source": string(domain.EpistemicCausalInferred), "version": version},
		)
		if err != nil {
			return nil, err
		}
		_, err = result.Consume(ctx)
		return nil, err
	})
}

// SupersedePriorCausalOutcomes retracts an intervention's causal evidence without
// writing a new edge — the standalone path for a non-confirming verification
// (confounded / not_identifiable / unsupported_objective): it marks every
// non-superseded causal_inferred edge at or below the given version superseded, so a
// result that invalidates a prior claim removes it from serving. Observational edges
// are never touched.
func (r *Neo4jRepository) SupersedePriorCausalOutcomes(ctx context.Context, interventionID string, version int) error {
	return r.writeOp(ctx, "supersede causal outcomes "+interventionID, func(tx neo4j.ManagedTransaction) (any, error) {
		result, err := tx.Run(ctx,
			"MATCH (i:"+labelIntervention+" {id: $interventionID})-[pe:"+domain.Produced+"]->(:"+labelOutcome+") "+
				"WHERE pe.epistemic_source = $source AND coalesce(pe.superseded, false) = false AND pe.graph_version <= $version "+
				"SET pe.superseded = true",
			map[string]any{"interventionID": interventionID, "source": string(domain.EpistemicCausalInferred), "version": version},
		)
		if err != nil {
			return nil, err
		}
		_, err = result.Consume(ctx)
		return nil, err
	})
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

// ErrUnknownCausalColumn reports a correction naming a column the goal's discovered
// graph does not have, and ErrNoSuchCausalEdge a flip or delete naming a pair the
// graph has no edge between. Callers map both to an analyst-fixable status rather
// than a fault: the columns and edges are the ones discovery selected, and naming
// another is a correctable mistake, not a broken graph.
//
// Reporting the second is what keeps a correction honest. A flip that matched
// nothing would otherwise commit a new version and answer 200, telling the analyst
// their domain knowledge was applied when nothing changed.
var (
	ErrUnknownCausalColumn = errors.New("column is not in the goal's causal graph")
	ErrNoSuchCausalEdge    = errors.New("the goal's causal graph has no edge between these columns")
)

// CorrectCausalEdge applies one analyst correction and serves it as a new graph
// version, returning that version.
//
// It copies the whole edge set forward rather than editing one edge in place, because
// CAUSES edges are version-keyed and GetCausalGraph reads them at exactly the meta's
// version: bumping the meta alone would strand every uncorrected edge at the old
// version and leave verification adjusting against an empty graph. So the corrected
// set is written whole at version+1, with the meta committed last — a crash mid-copy
// leaves the previous version served, the same commit-marker invariant discovery
// relies on.
//
// A deleted edge is durable by omission, with no tombstone: discovery early-returns
// whenever a committed meta exists and only ever writes its initial version, so
// nothing can re-add the edge at the version being served. Analyst provenance is
// durable the same way — the copy preserves each edge's provenance, so a later
// correction carries an earlier analyst's edits forward with it.
//
// The caller serializes concurrent corrections for a (goal, data-source) pair: this
// is a read-modify-write, and two interleaved ones would each copy forward the
// version they read, silently dropping one analyst's edit.
//
// It returns the new version and the graph's own spelling of the two corrected
// columns, which is what the caller must use to invalidate verifications — the
// analyst's spelling would not match the stored adjustment sets.
func (r *Neo4jRepository) CorrectCausalEdge(ctx context.Context, goalID, datasourceRef string, correction domain.EdgeCorrection) (int, []string, error) {
	current, ok, err := r.GetCausalGraph(ctx, goalID, datasourceRef)
	if err != nil {
		return 0, nil, err
	}
	if !ok {
		return 0, nil, ErrNotFound
	}
	// Resolve the analyst's spelling to the graph's own before anything downstream
	// touches it. A column name reaches three case-sensitive consumers — the SHA-256
	// DataColumn id an edge write MATCHes on, the lexicographic pair ordering, and
	// the caller's jsonb adjustment-set overlap — so accepting a name
	// case-insensitively and then passing it through raw would write an edge nothing
	// binds, compare against a transposed pair, and invalidate nothing, all while
	// reporting success.
	known := map[string]string{}
	for _, c := range current.Columns {
		known[strings.ToLower(c.Name)] = c.Name
	}
	resolved := make([]string, 0, 2)
	for _, name := range []string{correction.From, correction.To} {
		actual, ok := known[strings.ToLower(name)]
		if !ok {
			return 0, nil, fmt.Errorf("%q: %w", name, ErrUnknownCausalColumn)
		}
		resolved = append(resolved, actual)
	}
	correction.From, correction.To = resolved[0], resolved[1]

	version := current.Meta.Version + 1
	edges, matched := applyCorrection(current.Edges, correction, domain.CausalEdge{
		GoalID: goalID, DatasourceRef: datasourceRef, Version: version,
	})
	// A flip or delete that matched no edge changes nothing, so committing a version
	// for it would report an edit that never happened. An add is the one op that is
	// meaningful without a match.
	if !matched && correction.Op != domain.CorrectionAdd {
		return 0, nil, fmt.Errorf("%q and %q: %w", correction.From, correction.To, ErrNoSuchCausalEdge)
	}
	// Clear any torn write at the target version first. The upserts MERGE, so a
	// crashed earlier attempt that wrote an edge this correction now omits would
	// survive and be served the moment the meta commits — the one way a deleted edge
	// could come back. Safe as pre-write cleanup for the same reason discovery's is:
	// the served meta is still at the previous version, so nothing committed is lost.
	if err := r.deleteCausalEdgesAtVersion(ctx, goalID, datasourceRef, version); err != nil {
		return 0, nil, err
	}
	if err := r.UpsertCausalEdges(ctx, edges); err != nil {
		return 0, nil, err
	}
	meta := current.Meta
	meta.Version = version
	if err := r.UpsertCausalGraphMeta(ctx, meta); err != nil {
		return 0, nil, err
	}
	return version, resolved, nil
}

// applyCorrection returns the corrected edge set at the new version — every edge
// copied forward with its own provenance intact, the corrected one re-oriented or
// omitted, and an added edge appended — plus whether the correction matched an
// existing edge. An analyst-corrected edge is stamped analyst/tested at full
// confidence: the analyst is asserting it, so it must not stay fail-closed and leave
// the effect unidentifiable.
//
// The correction's columns must already carry the graph's own spelling, since the
// canonical pair is ordered by byte comparison and the appended edge's columns become
// a MERGE key.
func applyCorrection(current []domain.CausalEdge, correction domain.EdgeCorrection, added domain.CausalEdge) ([]domain.CausalEdge, bool) {
	colA, colB := domain.CanonicalColumnPair(correction.From, correction.To)
	edges := make([]domain.CausalEdge, 0, len(current)+1)
	matched := false
	for _, e := range current {
		e.Version = added.Version
		if e.ColA != colA || e.ColB != colB {
			edges = append(edges, e)
			continue
		}
		matched = true
		if correction.Op == domain.CorrectionDelete {
			continue
		}
		edges = append(edges, analystEdge(e, correction))
	}
	// An add naming a pair the skeleton never produced has nothing to copy forward,
	// so the edge is appended; an add on a pair that already exists is a
	// re-orientation of it, which the loop above already applied.
	if !matched && correction.Op == domain.CorrectionAdd {
		added.ColA, added.ColB = colA, colB
		edges = append(edges, analystEdge(added, correction))
	}
	return edges, matched
}

// deleteCausalEdgesAtVersion removes a (goal, data-source) pair's CAUSES edges at one
// version, leaving its DataColumn nodes and every other version intact. It is the
// edge-only counterpart to DeleteCausalGraphVersion, which detaches the columns and
// so would take every version's edges with them.
func (r *Neo4jRepository) deleteCausalEdgesAtVersion(ctx context.Context, goalID, datasourceRef string, version int) error {
	return r.writeOp(ctx, "delete causal edges at version", func(tx neo4j.ManagedTransaction) (any, error) {
		result, err := tx.Run(ctx,
			"MATCH ()-[e:"+domain.RelationCauses+" {goal_id: $goalID, datasource_ref: $datasourceRef, version: $version}]->() DELETE e",
			map[string]any{"goalID": goalID, "datasourceRef": datasourceRef, "version": version},
		)
		if err != nil {
			return nil, err
		}
		_, err = result.Consume(ctx)
		return nil, err
	})
}

// analystEdge stamps one edge with the analyst's correction.
func analystEdge(e domain.CausalEdge, correction domain.EdgeCorrection) domain.CausalEdge {
	e.Direction = correction.CorrectedDirection()
	e.Provenance = domain.ProvenanceAnalyst
	e.Status = domain.EdgeTested
	e.Confidence = 1.0
	return e
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
