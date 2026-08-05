package domain

// RelationCauses is the graph edge relation the discovered causal graph writes
// between two DataColumn nodes. It is a separate axis from the PRODUCED edge's
// EpistemicSource: CAUSES records column-to-column structure, PRODUCED records a
// measured finding's provenance.
const RelationCauses = "CAUSES"

// EdgeProvenance records how a causal edge's direction was established. It is
// distinct from EpistemicSource (which is reserved for the causal_inferred
// verification records of a later stage): provenance is about who oriented the
// edge — the statistics, Claude's domain prior, or an analyst correction. analyst
// is written by the correction flow of a later shell but belongs in the enum now so
// re-discovery never overwrites an analyst edge.
type EdgeProvenance string

const (
	ProvenanceStatistical EdgeProvenance = "statistical"
	ProvenanceLLMPrior    EdgeProvenance = "llm_prior"
	ProvenanceAnalyst     EdgeProvenance = "analyst"
)

// EdgeStatus is the discovery disposition of a column pair. tested edges are the
// only ones eligible for orientation; unknown (a test errored or timed out) and
// budget_capped (the sweep hit its test budget before reaching this pair) are both
// fail-closed — the edge is retained in the skeleton for adjacency but is fixed
// undirected, so effects whose identifiability depends on it report not-identifiable
// rather than being silently directed.
type EdgeStatus string

const (
	EdgeTested       EdgeStatus = "tested"
	EdgeUnknown      EdgeStatus = "unknown"
	EdgeBudgetCapped EdgeStatus = "budget_capped"
)

// EdgeDirection is the orientation of a CAUSES edge relative to its canonical
// (colA < colB) column pair: a_to_b means colA causes colB, b_to_a the reverse,
// undirected means the statistics and Claude left it unoriented, unknown means the
// pair is fail-closed (an unknown/budget_capped status). Storing direction as a
// property of an undirected-keyed edge (rather than in the relationship direction)
// keeps re-orientation an idempotent property update — see UpsertCausalEdges.
type EdgeDirection string

const (
	DirectionAToB       EdgeDirection = "a_to_b"
	DirectionBToA       EdgeDirection = "b_to_a"
	DirectionUndirected EdgeDirection = "undirected"
	DirectionUnknown    EdgeDirection = "unknown"
)

// EdgeCorrectionOp is what an analyst's correction does to the discovered graph:
// re-orient an edge the statistics produced, remove one the data suggested but the
// analyst knows is not there, or add a known edge (typically a confounder) the tests
// missed.
type EdgeCorrectionOp string

const (
	CorrectionFlip   EdgeCorrectionOp = "flip"
	CorrectionDelete EdgeCorrectionOp = "delete"
	CorrectionAdd    EdgeCorrectionOp = "add"
)

// EdgeCorrection is one analyst edit to a goal's causal graph, naming the edge by its
// two columns. From and To are read as cause→effect for flip and add, so an analyst
// states the correction the way they think about it rather than in the canonical
// pair's terms; Direction overrides that when set, which is how an edge is returned
// to undirected. Delete ignores both.
type EdgeCorrection struct {
	Op        EdgeCorrectionOp
	From      string
	To        string
	Direction EdgeDirection
}

// CorrectedDirection is the direction a correction asserts: the explicit one when the
// analyst gave it, otherwise the one From→To encodes over the canonical pair.
func (c EdgeCorrection) CorrectedDirection() EdgeDirection {
	if c.Direction != "" {
		return c.Direction
	}
	return DirectionForCause(c.From, c.To)
}

// DataColumn is a node in the discovered causal graph: one column of a (goal,
// datasource) pair. ID is the synthesized deterministic id the graph MERGEs on
// (the id-unique constraint cannot enforce a composite key); the composite parts
// ride as ordinary properties. Kind records whether discovery treated the column as
// categorical or numeric, so the read endpoint can render it.
type DataColumn struct {
	ID            string
	GoalID        string
	DatasourceRef string
	Name          string
	Kind          string
}

// CausalEdge is one discovered edge between two columns, keyed on the canonical
// undirected pair (ColA < ColB lexicographically) with orientation carried as the
// Direction property. Provenance/Confidence/Status describe how and how well the
// edge was oriented; Version scopes it to a discovery run.
type CausalEdge struct {
	GoalID        string
	DatasourceRef string
	Version       int
	ColA          string
	ColB          string
	Direction     EdgeDirection
	Provenance    EdgeProvenance
	Confidence    float64
	Status        EdgeStatus
}

// CausalGraphMeta is the commit marker and summary of one discovery run for a
// (goal, datasource) pair. It is written last (columns and edges first), so a
// reader treats a graph as present only once its meta exists — a torn partial write
// reads as absent. ExcludedColumns records the deterministic column-cap selection;
// BudgetTruncated and TestCount record whether the sweep stopped early; the tuning
// snapshot pins the knobs the run used.
type CausalGraphMeta struct {
	ID              string
	GoalID          string
	DatasourceRef   string
	Version         int
	ExcludedColumns []string
	BudgetTruncated bool
	TestCount       int
	DiscoveredAt    string
	Tuning          map[string]any
}

// CausalGraph is the read projection of a committed graph: its columns, edges, and
// meta, returned by the graph repository and mapped to the orchestrator's DTO.
type CausalGraph struct {
	Columns []DataColumn
	Edges   []CausalEdge
	Meta    CausalGraphMeta
}

// CanonicalColumnPair orders two column names lexicographically, the one spelling
// every CausalEdge key and every undirected-pair comparison uses.
func CanonicalColumnPair(a, b string) (colA, colB string) {
	if a <= b {
		return a, b
	}
	return b, a
}

// DirectionForCause returns the edge Direction that encodes cause→effect over the
// edge's canonical pair: a_to_b when the cause sorts first, b_to_a otherwise.
func DirectionForCause(cause, effect string) EdgeDirection {
	colA, _ := CanonicalColumnPair(cause, effect)
	if colA == cause {
		return DirectionAToB
	}
	return DirectionBToA
}
