package sleepcycle

import (
	"context"
	"log"
	"slices"
	"strconv"
	"strings"

	"github.com/arborette/arborette/internal/datasource"
	"github.com/arborette/arborette/internal/domain"
	"github.com/arborette/arborette/internal/graph"
	"github.com/arborette/arborette/internal/objective"
	"github.com/arborette/arborette/internal/sandboxclient"
)

// atom is one distinct predicate the search conjoins. It carries provenance —
// the node ids of every eligible finding that contributed this predicate —
// because the abstraction stage links each Meta-Heuristic back to the component
// triplets its macro-segment was built from, and deduplicating predicates would
// otherwise discard exactly that.
type atom struct {
	key        string
	rank       int
	constraint domain.Constraint
	sourceIDs  []string
}

// Node is one candidate macro-segment: a set of atoms identified by canonical
// key, in ascending rank order, plus the conjoined filter they compile to.
//
// proposedBy names the Meta-Heuristic a policy adopted this conjunction from, and
// is empty for a candidate the search built by conjoining atoms. It is carried to
// the derived Intervention so a trace of the winner shows which prior knowledge
// produced it.
type Node struct {
	keys       []string
	ranks      []int
	filters    []domain.Constraint
	canonical  string
	proposedBy string
}

// Order is the node's conjunction order — how many atoms it conjoins.
func (n *Node) Order() int { return len(n.keys) }

// Measurement is one empirically measured candidate: the objective value, the
// matched row count backing it, and whether the measurement failed outright.
type Measurement struct {
	Value   float64
	Support int64
	Failed  bool
}

// SearchPolicy is the traversal strategy behind the search driver. The driver
// owns measurement, memoization, budget, and winner collection; the policy owns
// candidate generation and pruning and never talks to the sandbox.
//
// Seam contract, stated so Done and Select can never disagree: the driver is
// strictly sequential, so an Update always completes before the next Done or
// Select. The policy must therefore advance levels eagerly inside Update — the
// call that records a level's last outstanding candidate computes the frontier
// and generates the next level before returning. A policy that advanced lazily
// inside Select would let Done report false and the following Select find
// nothing. The driver defends against that anyway by treating an ok=false Select
// as equivalent to Done.
//
// Candidate invariant, binding on every implementation: no generated candidate
// conjoins two predicates sharing a (field, op) pair. Such a conjunction is either
// redundant (the tighter bound subsumes the looser) or empty (two equalities on
// one column), so measuring it spends budget on a segment that says nothing about
// interaction — which is the only thing this search exists to find. A policy that
// generates conjunctions atom by atom holds it with conflictsWithNode; one that
// adopts a whole conjunction from elsewhere holds it with duplicateFieldOp, before
// the candidate ever reaches Select.
type SearchPolicy interface {
	// Select returns the next unmeasured candidate; ok is false when none remain.
	Select() (node *Node, ok bool)
	// Update records a candidate's measurement and advances the level if it was
	// the last one outstanding.
	Update(node *Node, m Measurement)
	// Done reports that no candidates remain at any level.
	Done() bool
}

// Reasons a search run ended, logged so a short run is never ambiguous.
const (
	stopPolicyDone      = "policy_done"
	stopBudgetExhausted = "budget_exhausted"
	// stopPolicyExhausted is the seam-contract violation: the policy reported it
	// was not Done, then had nothing to select. The beam cannot reach it, but a
	// future policy could, and it must not be logged as a clean exhaustion.
	stopPolicyExhausted = "policy_exhausted_early"
	// stopContextDone is the run's deadline or cancellation reaching the search.
	stopContextDone = "context_done"
)

// measuredNode is one entry in the run-local table the winner gate reads.
type measuredNode struct {
	node        *Node
	measurement Measurement
}

// searchOutcome is what one search run produced: every node it measured and
// which bound ended the run. One entry is appended per distinct sandbox
// measurement, so len(measured) is the run's measurement count.
type searchOutcome struct {
	measured   []measuredNode
	stopReason string
}

// winners returns the measured nodes eligible to become macro-segments: order 2
// or more (the output is defined as conjunctions, so an order-1 node is only ever
// a route to one), successfully measured, and clearing the support floor. The
// caller's materially-better gate makes the final cut.
func (o searchOutcome) winners(minSupport int64) []measuredNode {
	var out []measuredNode
	for _, m := range o.measured {
		if m.node.Order() >= 2 && !belowFloor(m.measurement, minSupport) {
			out = append(out, m)
		}
	}
	return out
}

// buildAtoms derives the search's atom index from the eligible findings: every
// distinct predicate any finding introduced, keyed canonically, ranked in a
// stable order, and carrying the ids of every finding that contributed it.
func buildAtoms(findings []graph.CausalTriplet) ([]atom, error) {
	byKey := map[string]*atom{}
	for _, f := range findings {
		filters, err := domain.DecodeConstraints(f.Intervention.Properties[domain.PropNewFilters])
		if err != nil {
			return nil, err
		}
		sources := []string{f.State.ID, f.Intervention.ID, f.Outcome.ID}
		for _, c := range filters {
			key, err := CanonicalFilters([]domain.Constraint{c})
			if err != nil {
				return nil, err
			}
			a, ok := byKey[key]
			if !ok {
				a = &atom{key: key, constraint: c}
				byKey[key] = a
			}
			a.sourceIDs = appendMissing(a.sourceIDs, sources...)
		}
	}

	atoms := make([]atom, 0, len(byKey))
	for _, a := range byKey {
		atoms = append(atoms, *a)
	}
	return rankAtoms(atoms), nil
}

// buildSchemaAtoms derives an atom vocabulary from the data source's own schema
// rather than from what Phase 1 happened to surface: one equality atom per
// distinct value of a low-cardinality column, and a pair of threshold atoms (at or
// below, above) per interior quantile cut of a numeric one. bins is the cut count
// the introspection was asked for; below 2 no threshold atoms are derived, which
// is also when the schema carries no cuts to derive them from.
//
// The atoms carry no source ids, deliberately. An enumerated predicate is not a
// measured finding, so attaching Phase-1 provenance to it would link a winner
// built from it to evidence that never existed; a winner built only from schema
// atoms is abstracted from its own derived triplet alone, which the provenance
// path already handles.
//
// Ranks are left unassigned: the caller merges this vocabulary with the
// findings-derived one and ranks the union, so ranking here would be overwritten.
func buildSchemaAtoms(schema sandboxclient.Schema, bins int) ([]atom, error) {
	var atoms []atom
	for _, col := range schema.Columns {
		predicates := valuePredicates(col)
		if bins >= 2 {
			predicates = append(predicates, thresholdPredicates(col)...)
		}
		for _, c := range predicates {
			key, err := CanonicalFilters([]domain.Constraint{c})
			if err != nil {
				return nil, err
			}
			atoms = append(atoms, atom{key: key, constraint: c})
		}
	}
	return atoms, nil
}

// valuePredicates enumerates one equality predicate per distinct value of a
// value-constrained column. The operand is typed from the column's own type, not
// from the value's spelling: introspection reports every value cast to text, and
// the sandbox rejects a filter whose operand type disagrees with its column, so an
// untyped predicate would compile-fail on every boolean and integer column.
func valuePredicates(col sandboxclient.Column) []domain.Constraint {
	out := make([]domain.Constraint, 0, len(col.DistinctValues))
	for _, v := range col.DistinctValues {
		operand := columnLiteral(col.Type, v)
		if operand == nil {
			continue
		}
		out = append(out, domain.Constraint{Field: col.Name, Op: domain.Equal, Operand: operand})
	}
	return out
}

// thresholdPredicates enumerates the two-sided threshold predicates a numeric
// column's quantile cuts define. Both sides are derived because the search ranks
// on directional delta and a segment above a cut and one at or below it are
// different populations, not complements of one ranking.
func thresholdPredicates(col sandboxclient.Column) []domain.Constraint {
	out := make([]domain.Constraint, 0, 2*len(col.QuantileCuts))
	for _, cut := range col.QuantileCuts {
		out = append(out,
			domain.Constraint{Field: col.Name, Op: domain.LessThanOrEqual, Value: cut},
			domain.Constraint{Field: col.Name, Op: domain.GreaterThan, Value: cut},
		)
	}
	return out
}

// columnLiteral types one introspected value against its column, returning nil for
// a value that does not parse as the column's type — a boolean column whose probe
// returned something other than true/false has no well-typed equality to offer, and
// inventing one would only fail at compile time.
func columnLiteral(colType, value string) *domain.LiteralValue {
	switch {
	case datasource.IsBooleanType(colType):
		b, err := strconv.ParseBool(value)
		if err != nil {
			return nil
		}
		return &domain.LiteralValue{Bool: &b}
	case datasource.IsNumericType(colType):
		n, err := strconv.ParseFloat(value, 64)
		if err != nil {
			return nil
		}
		return &domain.LiteralValue{Number: &n}
	default:
		v := value
		return &domain.LiteralValue{String: &v}
	}
}

// mergeAtoms unions the findings-derived vocabulary with the schema-derived one,
// dropping every schema atom on an excluded column and ranking the result. It also
// returns how many enumerated predicates the exclusions removed, so a caller
// reporting that number reports the filtering that actually happened.
//
// The exclusion applies to the schema-derived side only: a findings-derived atom
// is a predicate the hypothesis loop already measured against this objective, so
// it is evidence, and a critic's opinion about a column does not retract evidence.
// Findings-derived provenance also wins a key collision, since the two carry the
// same predicate but only one carries the source ids that link a winner back to
// the triplets it generalizes.
func mergeAtoms(findingAtoms, schemaAtoms []atom, excluded []string) ([]atom, int) {
	drop := make(map[string]bool, len(excluded))
	for _, col := range excluded {
		drop[strings.ToLower(col)] = true
	}
	merged := slices.Clone(findingAtoms)
	byKey := make(map[string]bool, len(findingAtoms))
	for _, a := range findingAtoms {
		byKey[a.key] = true
	}
	dropped := 0
	for _, a := range schemaAtoms {
		if drop[strings.ToLower(a.constraint.Field)] {
			dropped++
			continue
		}
		if byKey[a.key] {
			continue
		}
		byKey[a.key] = true
		merged = append(merged, a)
	}
	return rankAtoms(merged), dropped
}

// rankAtoms sorts an atom set by canonical key and numbers it, so rank is a pure
// function of the set and a re-run walks the lattice in the same order.
func rankAtoms(atoms []atom) []atom {
	slices.SortFunc(atoms, func(a, b atom) int { return strings.Compare(a.key, b.key) })
	for i := range atoms {
		atoms[i].rank = i
	}
	return atoms
}

// conflictsWithNode reports whether conjoining an atom onto a node would breach the
// policy contract's candidate invariant: two predicates on the same (field, op)
// pair. Fields are compared case-insensitively, mirroring the sandbox compiler's
// own column resolution, so two spellings of one column still collide.
func conflictsWithNode(n *Node, a atom) bool {
	for _, f := range n.filters {
		if f.Op == a.constraint.Op && strings.EqualFold(f.Field, a.constraint.Field) {
			return true
		}
	}
	return false
}

// duplicateFieldOp reports whether a whole conjunction already breaches the same
// invariant. It is the check for a candidate adopted intact rather than built atom
// by atom, which never passes through conflictsWithNode.
func duplicateFieldOp(filters []domain.Constraint) bool {
	seen := make(map[string]bool, len(filters))
	for _, f := range filters {
		key := strings.ToLower(f.Field) + "\x00" + string(f.Op)
		if seen[key] {
			return true
		}
		seen[key] = true
	}
	return false
}

// belowFloor is the one reading of a measurement the support floor turns on: a
// failed measurement has unknown support, which counts as insufficient, and a
// successful one is judged against the floor. It has a name because the pruning
// rules and the winner gate must agree on it — a divergence there would prune what
// the gate would have accepted, or write back what the search thought starved.
func belowFloor(m Measurement, minSupport int64) bool {
	return m.Failed || m.Support < minSupport
}

// prunedBySubset applies the Apriori rule against measured evidence: a candidate is
// unmeasurable-by-implication when any of its subsets came back below the support
// floor or failed, since conjoining cannot grow support.
//
// Two subset families are checked, and both are load-bearing. Each single predicate,
// because a starved predicate can never appear in any winner however the candidate
// was reached — neither policy generates conjunctions in an order that guarantees a
// starved predicate's pair was measured first, so the (k−1) family alone leaks at
// order 3 and above. And each (k−1)-subset, because a pair can fall below the floor
// while both its predicates clear it: {1,2,3} reached through a frequent {1,2} still
// contains {1,3}, and must be dropped when that was measured below floor.
//
// Only *measured* evidence prunes — an unmeasured subset (never generated, or
// dropped by width) says nothing about support, and letting it prune would disguise
// a policy's own pruning as support-pruning. Both policies share it so the rule
// cannot drift.
func prunedBySubset(keys []string, memo map[string]Measurement, minSupport int64) bool {
	for _, key := range keys {
		if m, measured := memo[key]; measured && belowFloor(m, minSupport) {
			return true
		}
	}
	for skip := range keys {
		subset := make([]string, 0, len(keys)-1)
		for i, k := range keys {
			if i != skip {
				subset = append(subset, k)
			}
		}
		m, measured := memo[canonicalKeyOf(subset)]
		if measured && belowFloor(m, minSupport) {
			return true
		}
	}
	return false
}

func appendMissing(dst []string, ids ...string) []string {
	for _, id := range ids {
		if id == "" || slices.Contains(dst, id) {
			continue
		}
		dst = append(dst, id)
	}
	return dst
}

// runSearch drives the policy: select a candidate, measure it against the
// sandbox, record it, hand the measurement back.
//
// A run ends when the policy reports Done (the common case — the reachable
// lattice is exhausted) or the measurement budget runs out, which stops the
// search immediately, mid-level, with winners drawn from whatever was measured.
// Mirroring the hypothesis loop's per-branch discipline, a failed measurement
// never aborts the run.
func (w *Worker) runSearch(ctx context.Context, goal searchTarget, obj objective.Objective, policy SearchPolicy) searchOutcome {
	outcome := searchOutcome{stopReason: stopPolicyDone}
	// Memoized by canonical filter so the budget counts distinct measurements and
	// a policy that re-selects a measured node costs nothing. The beam never
	// re-selects by construction; this is an invariant guard, not a hot path.
	memo := map[string]Measurement{}

	for !policy.Done() {
		// The budget only advances on a distinct measurement, and a memoized
		// iteration does no I/O, so a policy that re-selects without ever
		// finishing would otherwise spin past both the budget and the run
		// deadline. Checking the context makes the loop bounded for any policy.
		if ctx.Err() != nil {
			outcome.stopReason = stopContextDone
			break
		}
		if len(outcome.measured) >= w.cfg.MaxMeasurements {
			outcome.stopReason = stopBudgetExhausted
			break
		}
		node, ok := policy.Select()
		if !ok {
			outcome.stopReason = stopPolicyExhausted
			break
		}

		m, seen := memo[node.canonical]
		if !seen {
			m = w.measure(ctx, goal, obj, node)
			memo[node.canonical] = m
			outcome.measured = append(outcome.measured, measuredNode{node: node, measurement: m})
		}
		policy.Update(node, m)
	}

	log.Printf("sleepcycle: search for %q ended (%s) after %d measurements",
		goal.datasetID, outcome.stopReason, len(outcome.measured))
	return outcome
}

// measure runs one candidate's conjoined filter through the sandbox, reading the
// objective and its support out of the same response. An Execute error, or a
// value that is not numeric, is recorded as a failed measurement: audited,
// memoized, budget-consuming, and treated by the policy as below the support
// floor, so the node is never expanded and never wins.
func (w *Worker) measure(ctx context.Context, goal searchTarget, obj objective.Objective, node *Node) Measurement {
	req := objective.ExecuteRequestFor(goal.dataSourceRef, obj, node.filters)
	req.IncludeRowCount = true

	resp, err := w.sandbox.Execute(ctx, req)
	if err != nil {
		w.measurementFailure(ctx, goal.datasetID, node, err.Error())
		return Measurement{Failed: true}
	}
	// The support count decides how to read the rest of the response, so it comes
	// first. A missing count means the sandbox did not answer the question asked,
	// which is a fault.
	support, counted := sandboxclient.RowCount(resp)
	if !counted {
		w.measurementFailure(ctx, goal.datasetID, node, "sandbox returned no row count for a counted measurement")
		return Measurement{Failed: true}
	}
	// A count of zero means the conjunction matched nothing: a routine search
	// result, not a fault, so it is logged rather than audited. It still counts as
	// failed, and that is checked here rather than only alongside an unreadable
	// objective, because whether an empty segment yields a value at all depends on
	// the aggregation — avg/sum/min/max scan as SQL NULL, but count returns 0. A
	// count objective would otherwise produce a real-looking zero backed by no
	// rows, which at a support floor of zero ranks (first, under Minimize) and can
	// be written back and abstracted as the best known segment.
	if support == 0 {
		log.Printf("sleepcycle: segment matched zero rows for %q: %s", goal.datasetID, node.canonical)
		return Measurement{Failed: true}
	}
	value, ok := objective.NumericValue(resp.Value, obj.Label)
	if !ok {
		w.measurementFailure(ctx, goal.datasetID, node, objective.ErrNonNumericValue.Error())
		return Measurement{Failed: true}
	}
	return Measurement{Value: value, Support: support}
}

func (w *Worker) measurementFailure(ctx context.Context, goalID string, node *Node, cause string) {
	log.Printf("sleepcycle: measurement failed for %q: %s", goalID, cause)
	w.report(ctx, "sleepcycle_measurement_failure", "failure", map[string]any{
		"optimization_function_id": goalID,
		"canonical_filter":         node.canonical,
		"error":                    cause,
	})
}
