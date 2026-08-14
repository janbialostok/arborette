package store

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/pgvector/pgvector-go"
)

// EmbeddingStore upserts and searches Meta-Heuristic embeddings keyed by the
// application-assigned node UUID (the same id the graph keys on).
type EmbeddingStore struct {
	pool          *Pool
	distanceFloor float64
}

// NewEmbeddingStore wires the store to a pool. distanceFloor is the cosine
// distance beyond which a similarity hit is dropped rather than returned; a
// query far from the whole corpus returns empty instead of the corpus ranked by
// how distant it is. Validate it at startup with ValidateDistanceFloor so a bad
// env value fails once at boot rather than on every search.
func NewEmbeddingStore(pool *Pool, distanceFloor float64) *EmbeddingStore {
	return &EmbeddingStore{pool: pool, distanceFloor: distanceFloor}
}

// ValidateDistanceFloor rejects a floor outside the cosine-distance range
// (0, 2]. It is a standalone check called from each cmd startup beside
// ValidateEmbeddingDimension rather than an error return on the constructor, so
// the constructor signature stays mechanical.
func ValidateDistanceFloor(floor float64) error {
	if floor <= 0 || floor > 2 {
		return fmt.Errorf("embedding distance floor must be in (0, 2] (cosine distance), got %v", floor)
	}
	return nil
}

// ValidateEmbeddingDimension fails fast at startup when the configured embedding
// dimension disagrees with the meta_heuristic_embeddings.embedding column, so a
// mismatched EMBEDDING_DIMENSION surfaces as one clear error instead of opaque
// per-write failures. Changing the dimension is a migration + re-embed, never a
// runtime toggle; pgvector stores the column dimension in atttypmod.
func ValidateEmbeddingDimension(ctx context.Context, pool *Pool, expected int) error {
	var columnDim int
	err := pool.QueryRow(ctx,
		"SELECT atttypmod FROM pg_attribute "+
			"WHERE attrelid = 'meta_heuristic_embeddings'::regclass AND attname = 'embedding'",
	).Scan(&columnDim)
	if err != nil {
		return fmt.Errorf("read embedding column dimension: %w", err)
	}
	if columnDim != expected {
		return fmt.Errorf(
			"embedding dimension mismatch: config=%d but meta_heuristic_embeddings.embedding is vector(%d); "+
				"changing the dimension requires a matching migration and full re-embed, not a runtime toggle",
			expected, columnDim,
		)
	}
	return nil
}

// minVectorMajor and minVectorMinor are the lowest pgvector version whose
// hnsw.iterative_scan the goal-scoped search relies on to keep scanning past
// filtered-out candidates. It landed in 0.8.0.
const (
	minVectorMajor = 0
	minVectorMinor = 8
)

// ValidateVectorExtensionVersion fails fast at startup when the installed
// pgvector is older than the version whose hnsw.iterative_scan the goal/floor
// post-filters depend on, so a stale image surfaces as one clear boot error
// instead of every scoped search silently under-returning at runtime.
//
// The compare is on major.minor parsed as integers, never lexical: "0.10.0"
// sorts before "0.9.0" as strings, which would reject a version that in fact
// satisfies the floor.
func ValidateVectorExtensionVersion(ctx context.Context, pool *Pool) error {
	var version string
	err := pool.QueryRow(ctx,
		"SELECT extversion FROM pg_extension WHERE extname = 'vector'",
	).Scan(&version)
	if err != nil {
		return fmt.Errorf("read pgvector extension version: %w", err)
	}
	return checkVectorVersion(version)
}

// checkVectorVersion is the pure comparison behind ValidateVectorExtensionVersion,
// split out so the integer major.minor logic is unit-testable without a database.
func checkVectorVersion(version string) error {
	major, minor, err := parseMajorMinor(version)
	if err != nil {
		return fmt.Errorf("parse pgvector extension version %q: %w", version, err)
	}
	if major < minVectorMajor || (major == minVectorMajor && minor < minVectorMinor) {
		return fmt.Errorf(
			"pgvector %s is too old: goal-scoped retrieval needs hnsw.iterative_scan, added in %d.%d.0; "+
				"pin the image to a %d.%d.0-or-newer tag",
			version, minVectorMajor, minVectorMinor, minVectorMajor, minVectorMinor,
		)
	}
	return nil
}

// parseMajorMinor splits a "major.minor[.patch]" version into its integer major
// and minor components. It ignores any patch/suffix beyond the minor.
func parseMajorMinor(version string) (int, int, error) {
	parts := strings.SplitN(version, ".", 3)
	if len(parts) < 2 {
		return 0, 0, fmt.Errorf("expected major.minor, got %q", version)
	}
	major, err := strconv.Atoi(parts[0])
	if err != nil {
		return 0, 0, fmt.Errorf("major: %w", err)
	}
	minor, err := strconv.Atoi(parts[1])
	if err != nil {
		return 0, 0, fmt.Errorf("minor: %w", err)
	}
	return major, minor, nil
}

// Upsert writes (or replaces) the embedding for a node under a goal. An empty
// goalID writes SQL NULL, mirroring the graph's nil-vs-"" discipline: a
// goal-less caller (the reconcile/resume of a legacy node) records no goal
// rather than a phantom one. The ON CONFLICT clause COALESCEs the incoming
// goal_id over the stored one, so a re-embed under a known goal repairs a NULL
// row while a goal-less re-embed can never re-blank a populated one.
func (e *EmbeddingStore) Upsert(ctx context.Context, nodeID, goalID string, embedding []float32) error {
	_, err := e.pool.Exec(ctx,
		"INSERT INTO meta_heuristic_embeddings (node_id, goal_id, embedding, updated_at) VALUES ($1, $2, $3, now()) "+
			"ON CONFLICT (node_id) DO UPDATE SET embedding = EXCLUDED.embedding, "+
			"goal_id = COALESCE(EXCLUDED.goal_id, meta_heuristic_embeddings.goal_id), updated_at = now()",
		nodeID, nullableUUID(goalID), pgvector.NewVector(embedding),
	)
	if err != nil {
		return fmt.Errorf("upsert embedding for %q: %w", nodeID, err)
	}
	return nil
}

// nullableUUID converts an empty id to a nil parameter so pgx writes SQL NULL:
// binding a Go "" against a uuid column fails at bind time, so an empty id must
// never reach the driver as a string.
func nullableUUID(id string) any {
	if id == "" {
		return nil
	}
	return id
}

// Delete removes the embedding for a node. Deleting a node_id that is not
// present is not an error: the desired end state is "no row", and it is reached
// whether this call or a concurrent one got there.
func (e *EmbeddingStore) Delete(ctx context.Context, nodeID string) error {
	_, err := e.pool.Exec(ctx, "DELETE FROM meta_heuristic_embeddings WHERE node_id = $1", nodeID)
	if err != nil {
		return fmt.Errorf("delete embedding for %q: %w", nodeID, err)
	}
	return nil
}

// DeleteByGoal removes every embedding row scoped to a goal. It is the objective
// delete's post-transaction cleanup: the goal's Meta-Heuristic nodes are gone
// from the graph first, so the rows exist only as goal-scoped residue. Deleting
// for a goal with no rows is not an error (a goal-less legacy heuristic keeps its
// NULL-goal row, which this never touches).
func (e *EmbeddingStore) DeleteByGoal(ctx context.Context, goalID string) error {
	_, err := e.pool.Exec(ctx, "DELETE FROM meta_heuristic_embeddings WHERE goal_id = $1", goalID)
	if err != nil {
		return fmt.Errorf("delete embeddings for goal %q: %w", goalID, err)
	}
	return nil
}

// SearchScope selects a SimilaritySearch's visibility. Exactly one mode is set:
// a goal-scoped search (GoalID non-empty) returns only that goal's rows; a
// cross-goal search (CrossGoal true) returns every row including the NULL-goal
// legacy corpus. Setting both or neither is a caller error, not a silent default
// -- the browsing surface picks cross-goal deliberately, and grounding picks a
// goal deliberately, so neither should fall through to the other.
//
// Two optional access fields ride on the mode:
//
//   - UserID narrows a cross-goal search to rows whose goal's dataset the
//     acting user can access (owner or share grant). Legacy NULL-goal rows are
//     returned only when the user is CaretakerID -- the only account that may
//     read the pre-dataset corpus. An empty UserID leaves the corpus unscoped
//     (the system/boot path: MCP reads, sleep-cycle).
//   - DatasetID selects rows whose goal belongs to a dataset, plus the legacy
//     NULL-goal corpus. It is the sleep-cycle grounding mode: the worker scopes
//     its knowledge-reuse read to the run's own dataset plus system-owned rows
//     so it never retrieves across an ownership boundary. It is exclusive with
//     the goal/cross-goal mode pair.
type SearchScope struct {
	GoalID      string
	CrossGoal   bool
	UserID      string
	CaretakerID string
	DatasetID   string
}

// ScopeFromGoalID is the boundary translation an external caller uses: a
// non-empty goal narrows the search to that goal, an empty one browses the whole
// corpus cross-goal. It is the single place the "absent goal means cross-goal"
// policy lives, so the orchestrator and MCP surfaces cannot drift on it -- which
// is why it also trims here: a whitespace-only goal_id from either surface must
// map to cross-goal identically, not to a whitespace goal that fails the uuid
// bind on one surface and browses on the other.
func ScopeFromGoalID(goalID string) SearchScope {
	if strings.TrimSpace(goalID) == "" {
		return SearchScope{CrossGoal: true}
	}
	return SearchScope{GoalID: strings.TrimSpace(goalID)}
}

func (s SearchScope) validate() error {
	switch {
	case s.DatasetID != "":
		if s.GoalID != "" || s.CrossGoal {
			return fmt.Errorf("search scope has a dataset plus a goal/cross-goal mode; dataset mode is exclusive")
		}
		return nil
	case s.CrossGoal && s.GoalID != "":
		return fmt.Errorf("search scope has both a goal and cross-goal set; exactly one mode is allowed")
	case !s.CrossGoal && s.GoalID == "":
		return fmt.Errorf("search scope has neither a goal nor cross-goal set; exactly one mode is required")
	}
	return nil
}

// SimilaritySearch returns up to the k nearest node_ids to the query vector by
// cosine distance, filtered by the scope and the store's distance floor. It is the
// id-only projection of SimilaritySearchScored, which holds the query, its
// transaction, and the constraints both entry points enforce.
func (e *EmbeddingStore) SimilaritySearch(ctx context.Context, query []float32, k int, scope SearchScope) ([]string, error) {
	refs, err := e.SimilaritySearchScored(ctx, query, k, scope)
	if err != nil {
		return nil, err
	}
	if refs == nil {
		return nil, nil
	}
	ids := make([]string, 0, len(refs))
	for _, ref := range refs {
		ids = append(ids, ref.NodeID)
	}
	return ids, nil
}

// ScoredRef is one similarity hit with the cosine distance that ranked it, for a
// caller that weights hits rather than only ordering them. Distance is in [0, 2]
// (the <=> operator's range), so 0 is an exact match.
type ScoredRef struct {
	NodeID   string
	Distance float64
}

// SimilaritySearchScored returns up to the k nearest node_ids to the query vector
// by cosine distance (the <=> operator, matching the hnsw vector_cosine_ops index),
// each with the distance that ranked it, filtered by the scope and the store's
// distance floor. It carries the distance because a consumer that weights knowledge
// by how close it is (a search prior, a confidence) cannot recover it from an
// ordered id list, and re-reading it per hit would cost a second scan of the same
// neighbourhood.
//
// The scope's goal predicate and the floor are applied as post-filters over the
// hnsw candidate neighbourhood, so a goal-scoped query over a corpus dominated
// by other goals could return fewer than k rows unless the scan continues past
// filtered-out candidates. The search therefore runs inside a transaction that
// first sets hnsw.iterative_scan = relaxed_order (added in pgvector 0.8.0, which
// ValidateVectorExtensionVersion pins at startup). The result is min(k, all rows
// of that scope within the floor) up to hnsw.max_scan_tuples (default 20000
// tuples scanned) -- exhaustive at expected corpus scale; revisit that GUC with
// the floor calibration if the corpus ever approaches the bound.
func (e *EmbeddingStore) SimilaritySearchScored(ctx context.Context, query []float32, k int, scope SearchScope) ([]ScoredRef, error) {
	if err := scope.validate(); err != nil {
		return nil, err
	}

	tx, err := e.pool.Begin(ctx)
	if err != nil {
		return nil, fmt.Errorf("similarity search: begin: %w", err)
	}
	defer tx.Rollback(ctx)

	if _, err := tx.Exec(ctx, "SET LOCAL hnsw.iterative_scan = relaxed_order"); err != nil {
		return nil, fmt.Errorf("similarity search: enable iterative scan: %w", err)
	}

	vec := pgvector.NewVector(query)
	var rows pgx.Rows
	// The goal_id column is uuid, so a goal-scoped mode binds the id and adds the
	// predicate while cross-goal mode omits it entirely: binding a Go "" against a
	// uuid parameter fails at bind time regardless of a runtime OR short-circuit,
	// so the two modes cannot share one OR-ed clause.
	switch {
	case scope.DatasetID != "":
		// Sleep-cycle grounding mode: the run's own dataset plus the legacy
		// NULL-goal system corpus. The LEFT JOIN is how the NULL-goal rows are
		// reached at all: they have no goal_registry row, so the dataset filter
		// falls back to the explicit OR on goal_id IS NULL.
		rows, err = tx.Query(ctx,
			"SELECT mh.node_id, mh.embedding <=> $1 AS distance FROM meta_heuristic_embeddings mh "+
				"LEFT JOIN goal_registry g ON g.optimization_function_id = mh.goal_id "+
				"WHERE (mh.embedding <=> $1) <= $2 "+
				"AND (mh.goal_id IS NULL OR g.dataset_id = $3) "+
				"ORDER BY mh.embedding <=> $1 LIMIT $4",
			vec, e.distanceFloor, scope.DatasetID, k,
		)
	case scope.CrossGoal && scope.UserID != "":
		// User-scoped browse: a row is reachable when its goal's dataset is
		// accessible to the user (owner or share), and the legacy NULL-goal
		// corpus belongs to the admin caretaker alone.
		rows, err = tx.Query(ctx,
			"SELECT mh.node_id, mh.embedding <=> $1 AS distance FROM meta_heuristic_embeddings mh "+
				"WHERE (mh.embedding <=> $1) <= $2 AND ("+
				"(mh.goal_id IS NOT NULL AND EXISTS ("+
				"SELECT 1 FROM goal_registry g JOIN datasets d ON d.id = g.dataset_id "+
				"WHERE g.optimization_function_id = mh.goal_id "+
				"AND (d.owner_id = $3::uuid OR EXISTS ("+
				"SELECT 1 FROM dataset_shares sh WHERE sh.dataset_id = d.id AND sh.user_id = $3::uuid)))) "+
				"OR (mh.goal_id IS NULL AND $3::uuid = $4::uuid)) "+
				"ORDER BY mh.embedding <=> $1 LIMIT $5",
			vec, e.distanceFloor, scope.UserID, nullableUUID(scope.CaretakerID), k,
		)
	case scope.CrossGoal:
		rows, err = tx.Query(ctx,
			"SELECT node_id, embedding <=> $1 AS distance FROM meta_heuristic_embeddings "+
				"WHERE (embedding <=> $1) <= $2 ORDER BY embedding <=> $1 LIMIT $3",
			vec, e.distanceFloor, k,
		)
	case scope.UserID != "":
		// Goal-scoped with an access gate: even a caller that already verified
		// the goal's dataset access at the handler re-checks it here, so a
		// direct store caller cannot enumerate a stranger's goal rows.
		rows, err = tx.Query(ctx,
			"SELECT mh.node_id, mh.embedding <=> $1 AS distance FROM meta_heuristic_embeddings mh "+
				"WHERE (mh.embedding <=> $1) <= $2 AND mh.goal_id = $3 AND EXISTS ("+
				"SELECT 1 FROM goal_registry g JOIN datasets d ON d.id = g.dataset_id "+
				"WHERE g.optimization_function_id = mh.goal_id "+
				"AND (d.owner_id = $4::uuid OR EXISTS ("+
				"SELECT 1 FROM dataset_shares sh WHERE sh.dataset_id = d.id AND sh.user_id = $4::uuid))) "+
				"ORDER BY mh.embedding <=> $1 LIMIT $5",
			vec, e.distanceFloor, scope.GoalID, scope.UserID, k,
		)
	default:
		rows, err = tx.Query(ctx,
			"SELECT node_id, embedding <=> $1 AS distance FROM meta_heuristic_embeddings "+
				"WHERE (embedding <=> $1) <= $2 AND goal_id = $3 ORDER BY embedding <=> $1 LIMIT $4",
			vec, e.distanceFloor, scope.GoalID, k,
		)
	}
	if err != nil {
		return nil, fmt.Errorf("similarity search: %w", err)
	}
	defer rows.Close()

	var refs []ScoredRef
	for rows.Next() {
		var ref ScoredRef
		if err := rows.Scan(&ref.NodeID, &ref.Distance); err != nil {
			return nil, fmt.Errorf("scan similarity row: %w", err)
		}
		refs = append(refs, ref)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate similarity rows: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, fmt.Errorf("similarity search: commit: %w", err)
	}
	return refs, nil
}

// NodeRef is one embedding row's identity and goal scope, the reconcile pass's
// view of the pgvector side.
type NodeRef struct {
	NodeID string
	GoalID string
}

// ListNodeRefs returns every embedding row's node_id and goal_id, so the
// reconcile pass can diff the pgvector side against the graph. The COALESCE is
// load-bearing: pgx errors scanning a SQL NULL into a plain string, and one
// legacy NULL-goal row -- exactly the population the goal-repair arm exists to
// heal -- would otherwise abort the whole scan.
func (e *EmbeddingStore) ListNodeRefs(ctx context.Context) ([]NodeRef, error) {
	rows, err := e.pool.Query(ctx,
		"SELECT node_id, COALESCE(goal_id::text, '') FROM meta_heuristic_embeddings",
	)
	if err != nil {
		return nil, fmt.Errorf("list node refs: %w", err)
	}
	defer rows.Close()

	var refs []NodeRef
	for rows.Next() {
		var ref NodeRef
		if err := rows.Scan(&ref.NodeID, &ref.GoalID); err != nil {
			return nil, fmt.Errorf("scan node ref: %w", err)
		}
		refs = append(refs, ref)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate node refs: %w", err)
	}
	return refs, nil
}

// SetGoalID repairs a NULL-goal row's scope without re-embedding: the vector is
// unchanged, only the provenance the earlier write could not recover. It touches
// only a row whose goal_id is still NULL, so a populated scope is never
// overwritten.
func (e *EmbeddingStore) SetGoalID(ctx context.Context, nodeID, goalID string) error {
	_, err := e.pool.Exec(ctx,
		"UPDATE meta_heuristic_embeddings SET goal_id = $2 WHERE node_id = $1 AND goal_id IS NULL",
		nodeID, nullableUUID(goalID),
	)
	if err != nil {
		return fmt.Errorf("set goal_id for %q: %w", nodeID, err)
	}
	return nil
}
