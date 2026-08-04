// Package verifier owns Engine B's causal-discovery stage: it discovers a causal
// skeleton over a data source's columns by conditional-independence testing through
// the Sandbox Execution service, orients what statistics and Claude can, and
// persists the graph per (goal, data-source) behind a cross-process advisory-lock
// single-flight. It is CGO-free — it consumes internal/sandboxclient, never
// internal/sandbox.
package verifier

import (
	"context"
	"fmt"
	"log"
	"math/rand"
	"strings"
	"time"

	"github.com/arborette/arborette/internal/domain"
	"github.com/arborette/arborette/internal/graph"
	"github.com/arborette/arborette/internal/llm"
	"github.com/arborette/arborette/internal/sandboxclient"
	"github.com/arborette/arborette/internal/store"
)

// analyzer is the sandbox surface the Verifier drives: schema introspection (column
// types and low-cardinality values, the input to type routing and column selection),
// the two /analyze kinds, and /execute -- the claim reification needs the goal's
// unfiltered global baseline, and the /analyze effect kinds only ever answer with a
// segment and its complement. Backed by *sandboxclient.Client.
type analyzer interface {
	Introspect(ctx context.Context, req sandboxclient.IntrospectRequest) (sandboxclient.IntrospectResponse, error)
	Analyze(ctx context.Context, req sandboxclient.AnalyzeRequest) (sandboxclient.AnalyzeResponse, error)
	Execute(ctx context.Context, req sandboxclient.ExecuteRequest) (sandboxclient.ExecuteResponse, error)
}

// orienter is the one batched LLM orientation call (plus its repair sibling) the
// deterministic orientation escalates to. Backed by llm.Client.
type orienter interface {
	OrientCausalEdges(ctx context.Context, goalText string, columns []llm.ColumnSemantics, edges []llm.OrientEdge) ([]llm.OrientDecision, error)
	RepairOrientCausalEdges(ctx context.Context, goalText string, columns []llm.ColumnSemantics, edges []llm.OrientEdge, prior []llm.OrientDecision, validationErr string) ([]llm.OrientDecision, error)
}

// graphWriter is the graph surface every Verifier stage needs: discovery's committed
// read (for the single-flight recheck and the endpoint), version-scoped cleanup, the
// three upserts written columns→edges→meta (meta last, the commit marker), and the
// eligible-findings read; verification's finding read (GetIntervention, the
// segment-predicate source), the exempt causal-evidence read, and the two causal
// Outcome writers (confirming and retracting); plus the triplet writers a directly
// constructed claim is reified through, which are the same writers Phase 1 and the
// Sleep Cycle persist their findings with. Kept one interface rather than a
// second graph seam; *graph.Neo4jRepository satisfies the whole surface.
type graphWriter interface {
	GetCausalGraph(ctx context.Context, goalID, datasourceRef string) (domain.CausalGraph, bool, error)
	DeleteCausalGraphVersion(ctx context.Context, goalID, datasourceRef string, version int) error
	UpsertDataColumns(ctx context.Context, cols []domain.DataColumn) error
	UpsertCausalEdges(ctx context.Context, edges []domain.CausalEdge) error
	UpsertCausalGraphMeta(ctx context.Context, meta domain.CausalGraphMeta) error
	ListEligibleFindings(ctx context.Context, goalID string) ([]graph.CausalTriplet, error)
	GetIntervention(ctx context.Context, id string) (domain.Intervention, error)
	GetCausalEvidence(ctx context.Context, interventionID string) (graph.CausalEvidence, bool, error)
	WriteCausalVerification(ctx context.Context, interventionID string, version int, outcome domain.Outcome, edge domain.ProducedEdge) error
	SupersedePriorCausalOutcomes(ctx context.Context, interventionID string, version int) error
	CreateState(ctx context.Context, s domain.State) error
	CreateIntervention(ctx context.Context, i domain.Intervention) error
	CreateOutcome(ctx context.Context, o domain.Outcome) error
	CreatePreConditionFor(ctx context.Context, stateID, interventionID string) error
	CreateProduced(ctx context.Context, interventionID, outcomeID string, edge domain.ProducedEdge) error
}

// locker is the cross-process single-flight: a Postgres advisory lock on the (goal,
// data-source) key. Backed by *store.AdvisoryLock.
type locker interface {
	TryAcquireDiscoveryLock(ctx context.Context, goalID, datasourceRef string) (release func(), acquired bool, err error)
}

// goalReader supplies the goal's text and evaluation matrix (objective columns for
// selection, goal context for orientation). Backed by *store.GoalRegistry.
type goalReader interface {
	Get(ctx context.Context, optimizationFunctionID string) (store.Goal, error)
}

// auditClient reports Verifier lifecycle events through the orchestrator's
// authenticated internal API: discovery events go through Append (audit only), while
// PublishVerification delivers a verification transition to both the goal's SSE
// channel and the audit trail in one authenticated call. Backed by
// *orchestratorclient.Client.
type auditClient interface {
	Append(ctx context.Context, action, eventType string, detail map[string]any) error
	PublishVerification(ctx context.Context, goalID string, event map[string]any) error
}

// verificationStore is the leased-record surface VerifyOne drives: the charge-at-
// accept dispatch, the mid-run lease renewal, and the guarded terminal write. Backed
// by *store.CausalVerifications. Reaping and staleness marking run on the concrete
// store outside the per-verification path.
type verificationStore interface {
	DispatchAccept(ctx context.Context, rec store.CausalVerification) (store.CausalVerification, bool, error)
	Heartbeat(ctx context.Context, id string) error
	Complete(ctx context.Context, id, status string, naive, adjusted *float64, adjustmentSet []string, refutationScore, confidence *float64) (bool, error)
}

// Worker runs causal discovery for one (goal, data-source) pair end to end. It is
// wired once and reentrant across runs (the sleepcycle worker convention), so serve
// mode dispatches many runs against one Worker.
type Worker struct {
	analyzer       analyzer
	graph          graphWriter
	orienter       orienter
	locker         locker
	goals          goalReader
	audit          auditClient
	verifications  verificationStore
	cfg            Config
	orientMax      int
	heartbeatEvery time.Duration
	pollMin        time.Duration
	pollMax        time.Duration
	waitBudget     time.Duration
}

// NewWorker wires the Worker from its narrow seams and configuration (infra-
// constructor convention). orientMaxRepairs bounds the LLM orientation repair loop;
// heartbeatEvery paces the verification lease renewal. verifications may be nil for a
// discovery-only Worker (the one-shot discovery job); the verification path requires
// it.
func NewWorker(a analyzer, g graphWriter, o orienter, lk locker, goals goalReader, audit auditClient, verifications verificationStore, cfg Config, orientMaxRepairs int, heartbeatEvery time.Duration) *Worker {
	return &Worker{
		analyzer:       a,
		graph:          g,
		orienter:       o,
		locker:         lk,
		goals:          goals,
		audit:          audit,
		verifications:  verifications,
		cfg:            cfg,
		orientMax:      orientMaxRepairs,
		heartbeatEvery: heartbeatEvery,
		pollMin:        2 * time.Second,
		pollMax:        5 * time.Second,
		waitBudget:     30 * time.Minute,
	}
}

// discoveryVersion is the version an initial discovery always writes. It is fixed so
// a crashed run's re-run MERGEs over the same (goal, data-source, 1) keys
// idempotently rather than allocating a second partial graph. Higher versions belong
// to analyst corrections alone, which is what makes an analyst's edits durable: a
// re-run of discovery can only ever rewrite version 1, while the corrected graph is
// served from above it.
const discoveryVersion = 1

// RunDiscovery ensures a committed causal graph exists for the (goal, data-source)
// pair, discovering it on first need behind the cross-process advisory-lock
// single-flight. It is the runner the serve surface and job mode both call. The
// waiter loop never holds a pooled connection while blocking: each attempt tries the
// lock momentarily, and a contender that loses polls for the committed graph with
// jittered backoff, returning the moment it appears.
func (w *Worker) RunDiscovery(ctx context.Context, goalID, datasourceRef string) error {
	deadline := time.Now().Add(w.waitBudget)
	for {
		release, acquired, err := w.locker.TryAcquireDiscoveryLock(ctx, goalID, datasourceRef)
		if err != nil {
			return fmt.Errorf("acquire discovery lock: %w", err)
		}
		if acquired {
			defer release()
			// Re-check under the lock: another holder may have committed a graph while we
			// were contending, in which case there is nothing to discover.
			if _, ok, err := w.graph.GetCausalGraph(ctx, goalID, datasourceRef); err != nil {
				return err
			} else if ok {
				return nil
			}
			return w.discover(ctx, goalID, datasourceRef)
		}

		// Lost the lock: poll for the committed graph rather than block on the lock,
		// so a fan-out of contenders does not pin one pool connection each.
		if _, ok, err := w.graph.GetCausalGraph(ctx, goalID, datasourceRef); err != nil {
			return err
		} else if ok {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("timed out waiting for concurrent discovery of goal %q", goalID)
		}
		if err := w.sleepBackoff(ctx); err != nil {
			return err
		}
	}
}

// sleepBackoff waits a jittered interval in [pollMin, pollMax], returning early if
// the context is cancelled.
func (w *Worker) sleepBackoff(ctx context.Context) error {
	jitter := w.pollMax - w.pollMin
	d := w.pollMin
	if jitter > 0 {
		d += time.Duration(rand.Int63n(int64(jitter)))
	}
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

// discover runs the full pipeline under the held lock: introspect the schema, build
// the discovery variable set, run the CI-test sweep, orient (colliders + Meek + the
// batched LLM call), and persist the graph behind the commit protocol. Audit events
// bracket the run.
func (w *Worker) discover(ctx context.Context, goalID, datasourceRef string) error {
	goal, err := w.goals.Get(ctx, goalID)
	if err != nil {
		return fmt.Errorf("get goal %q: %w", goalID, err)
	}

	introspect, err := w.analyzer.Introspect(ctx, sandboxclient.IntrospectRequest{DataSourceRef: datasourceRef})
	if err != nil {
		return fmt.Errorf("introspect %q: %w", datasourceRef, err)
	}
	cols := toColumns(introspect.Schema.Columns)
	priority := selectionPriority(goal, w.findingColumns(ctx, goalID))

	w.reportAudit(ctx, "causal_discovery_started", map[string]any{
		"optimization_function_id": goalID,
		"data_source_ref":          datasourceRef,
		"column_count":             len(cols),
	})
	log.Printf("verifier: discovery start goal=%q columns=%d", goalID, len(cols))

	res, err := Discover(ctx, w.analyzer, datasourceRef, w.cfg, cols, priority)
	if err != nil {
		return fmt.Errorf("discover: %w", err)
	}
	orientations := orientEdges(ctx, res, w.orienter, goal.GoalText, res.Columns, w.orientMax)

	if err := w.persist(ctx, goalID, datasourceRef, res, orientations); err != nil {
		return fmt.Errorf("persist graph: %w", err)
	}

	unknown := 0
	for _, e := range orientations {
		if e.Status != domain.EdgeTested {
			unknown++
		}
	}
	w.reportAudit(ctx, "causal_discovery_complete", map[string]any{
		"optimization_function_id": goalID,
		"data_source_ref":          datasourceRef,
		"edge_count":               len(orientations),
		"test_count":               res.TestCount,
		"unknown_pairs":            unknown,
		"truncated":                res.Truncated,
	})
	log.Printf("verifier: discovery complete goal=%q edges=%d tests=%d unknown=%d", goalID, len(orientations), res.TestCount, unknown)
	return nil
}

// reportAudit appends one audit event and logs a swallowed failure rather than
// discarding it: an audit-write failure (an INTERNAL_AUTH_TOKEN mismatch answering
// 401, a transport error) must leave a trace, since the discovery would otherwise
// report success while its audit trail — including the terminal record carrying the
// run's results — silently vanishes. The audit event type is always the short
// causal_discovery category; action is the specific event.
func (w *Worker) reportAudit(ctx context.Context, action string, detail map[string]any) {
	if err := w.audit.Append(ctx, action, "causal_discovery", detail); err != nil {
		log.Printf("verifier: append audit %q: %v", action, err)
	}
}

// persist writes the discovered graph behind the commit protocol: it first clears
// any torn version-1 write (safe because no committed meta exists — the caller
// rechecked), then writes columns and edges, and writes CausalGraphMeta last as the
// commit marker, so GetCausalGraph reads the graph as present only once the whole
// write lands.
func (w *Worker) persist(ctx context.Context, goalID, datasourceRef string, res *DiscoveryResult, orientations []edgeOrientation) error {
	if err := w.graph.DeleteCausalGraphVersion(ctx, goalID, datasourceRef, discoveryVersion); err != nil {
		return err
	}

	columns := make([]domain.DataColumn, 0, len(res.Columns))
	for _, c := range res.Columns {
		kind := "categorical"
		if c.Numeric {
			kind = "numeric"
		}
		columns = append(columns, domain.DataColumn{
			GoalID:        goalID,
			DatasourceRef: datasourceRef,
			Name:          c.Name,
			Kind:          kind,
		})
	}
	if err := w.graph.UpsertDataColumns(ctx, columns); err != nil {
		return err
	}

	edges := make([]domain.CausalEdge, 0, len(orientations))
	for _, e := range orientations {
		edges = append(edges, domain.CausalEdge{
			GoalID:        goalID,
			DatasourceRef: datasourceRef,
			Version:       discoveryVersion,
			ColA:          e.Pair.A,
			ColB:          e.Pair.B,
			Direction:     e.Direction,
			Provenance:    e.Provenance,
			Confidence:    e.Confidence,
			Status:        e.Status,
		})
	}
	if err := w.graph.UpsertCausalEdges(ctx, edges); err != nil {
		return err
	}

	return w.graph.UpsertCausalGraphMeta(ctx, domain.CausalGraphMeta{
		GoalID:          goalID,
		DatasourceRef:   datasourceRef,
		Version:         discoveryVersion,
		ExcludedColumns: res.Excluded,
		BudgetTruncated: res.Truncated,
		TestCount:       res.TestCount,
		DiscoveredAt:    time.Now().UTC().Format(time.RFC3339),
		Tuning: map[string]any{
			"alpha":        w.cfg.Alpha,
			"fdr":          w.cfg.FDR,
			"max_cond_set": w.cfg.MaxCondSet,
			"bins":         w.cfg.Bins,
			"column_cap":   w.cfg.ColumnCap,
			"max_tests":    w.cfg.MaxTests,
		},
	})
}

// findingColumns extracts the columns named in the goal's eligible findings'
// intervention filters, a selection-priority input. A read failure is non-fatal —
// selection only matters over the column cap — so it logs and yields nothing.
func (w *Worker) findingColumns(ctx context.Context, goalID string) []string {
	triplets, err := w.graph.ListEligibleFindings(ctx, goalID)
	if err != nil {
		log.Printf("verifier: list eligible findings for %q: %v", goalID, err)
		return nil
	}
	var cols []string
	for _, t := range triplets {
		cols = append(cols, filterColumns(t.Intervention.Properties)...)
	}
	return cols
}

// toColumns maps introspected columns to the discovery variable model: a numeric
// column is a numeric endpoint/conditioner; a low-cardinality column carries its
// distinct values (for categorical routing and the orientation prompt).
func toColumns(cols []sandboxclient.Column) []Column {
	out := make([]Column, 0, len(cols))
	for _, c := range cols {
		out = append(out, Column{
			Name:    c.Name,
			Numeric: isNumericType(c.Type),
			Samples: c.DistinctValues,
		})
	}
	return out
}

// isNumericType mirrors the sandbox compiler's numeric classification across the CGO
// firewall (the sandbox package cannot be imported here), so a column routes to the
// numeric test path exactly as it would compile.
func isNumericType(t string) bool {
	u := strings.ToUpper(strings.TrimSpace(t))
	switch u {
	case "TINYINT", "SMALLINT", "INTEGER", "BIGINT", "HUGEINT",
		"UTINYINT", "USMALLINT", "UINTEGER", "UBIGINT", "UHUGEINT",
		"FLOAT", "DOUBLE":
		return true
	}
	return strings.HasPrefix(u, "DECIMAL")
}

// selectionPriority is the column-cap priority list: the objective columns (from the
// goal's evaluation matrix) followed by finding-referenced columns, deduplicated in
// first-seen order.
func selectionPriority(goal store.Goal, findingCols []string) []string {
	seen := map[string]bool{}
	var out []string
	add := func(name string) {
		key := strings.ToLower(name)
		if name == "" || seen[key] {
			return
		}
		seen[key] = true
		out = append(out, name)
	}
	for _, t := range goal.EvaluationMatrix.Targets {
		for _, col := range expressionColumns(t.ValueExpression()) {
			add(col)
		}
	}
	for _, c := range goal.EvaluationMatrix.Constraints {
		add(c.Field)
	}
	for _, c := range findingCols {
		add(c)
	}
	return out
}

// expressionColumns collects the column names an objective value expression
// references, walking the AST.
func expressionColumns(e domain.Expression) []string {
	switch e.Kind {
	case domain.ColumnRefKind:
		return []string{e.Column}
	case domain.CastKind:
		return childColumns(e.Operand)
	case domain.ComparisonKind, domain.ArithmeticKind:
		return append(childColumns(e.Left), childColumns(e.Right)...)
	case domain.CaseKind:
		var cols []string
		for _, br := range e.Cases {
			cols = append(cols, childColumns(br.When)...)
			cols = append(cols, childColumns(br.Then)...)
		}
		return append(cols, childColumns(e.Else)...)
	case domain.LagKind, domain.TrailingAggregateKind:
		return childColumns(e.Inner)
	default:
		return nil
	}
}

func childColumns(e *domain.Expression) []string {
	if e == nil {
		return nil
	}
	return expressionColumns(*e)
}

// filterColumns pulls the filter column names out of an intervention's stored
// effective/new-filters property, tolerating the []any-of-maps shape JSON decoding
// produces. It is best-effort: an unrecognized shape yields nothing.
func filterColumns(props map[string]any) []string {
	var cols []string
	for _, key := range []string{domain.PropEffectiveFilters, domain.PropNewFilters} {
		raw, ok := props[key].([]any)
		if !ok {
			continue
		}
		for _, item := range raw {
			m, ok := item.(map[string]any)
			if !ok {
				continue
			}
			if field, ok := m["field"].(string); ok {
				cols = append(cols, field)
			}
		}
	}
	return cols
}
