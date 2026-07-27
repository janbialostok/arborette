package sleepcycle

import (
	"context"
	"log"
	"slices"
	"strings"

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
type Node struct {
	keys      []string
	ranks     []int
	filters   []domain.Constraint
	canonical string
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
		if m.node.Order() >= 2 && !m.measurement.Failed && m.measurement.Support >= minSupport {
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
		filters, err := decodeConstraints(f.Intervention.Properties[domain.PropNewFilters])
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
	slices.SortFunc(atoms, func(a, b atom) int { return strings.Compare(a.key, b.key) })
	for i := range atoms {
		atoms[i].rank = i
	}
	return atoms, nil
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
		goal.goalID, outcome.stopReason, len(outcome.measured))
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
		w.measurementFailure(ctx, goal.goalID, node, err.Error())
		return Measurement{Failed: true}
	}
	// The support count decides how to read the rest of the response, so it comes
	// first. A missing count means the sandbox did not answer the question asked,
	// which is a fault.
	support, counted := sandboxclient.RowCount(resp)
	if !counted {
		w.measurementFailure(ctx, goal.goalID, node, "sandbox returned no row count for a counted measurement")
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
		log.Printf("sleepcycle: segment matched zero rows for %q: %s", goal.goalID, node.canonical)
		return Measurement{Failed: true}
	}
	value, ok := objective.NumericValue(resp.Value, obj.Label)
	if !ok {
		w.measurementFailure(ctx, goal.goalID, node, objective.ErrNonNumericValue.Error())
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
