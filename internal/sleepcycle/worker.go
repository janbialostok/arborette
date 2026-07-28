// Package sleepcycle is the Sleep-Cycle Worker: a manually-triggered batch job
// that searches the conjunction lattice of a goal's verified Phase-1 findings for
// macro-segments beating any single segment, writes each winner back as a full
// observational triplet, then selects the best segments the goal knows of —
// Phase-1 findings and derived winners alike — and abstracts each into a
// domain-agnostic Meta-Heuristic.
//
// It is a one-shot job, not a service: Run does the whole pass and returns.
package sleepcycle

import (
	"context"
	"errors"
	"fmt"
	"log"
	"math"
	"time"

	"github.com/google/uuid"

	"github.com/arborette/arborette/internal/domain"
	"github.com/arborette/arborette/internal/embedding"
	"github.com/arborette/arborette/internal/graph"
	"github.com/arborette/arborette/internal/llm"
	"github.com/arborette/arborette/internal/objective"
	"github.com/arborette/arborette/internal/sandboxclient"
	"github.com/arborette/arborette/internal/store"
)

// runTimeout bounds a whole Sleep-Cycle run. Every candidate costs one sequential
// sandbox call and every abstraction one Claude call, so without a deadline a
// wedged dependency would hang the job indefinitely. Sized to match the
// hypothesis loop's own bound.
const runTimeout = 30 * time.Minute

// auditWriteTimeout bounds the terminal audit write, which runs on a context
// detached from the run's own deadline so a timed-out run still records why.
const auditWriteTimeout = 5 * time.Second

// liftEpsilon floors the denominator of the relative-lift comparison so a
// baseline of exactly zero does not divide by zero.
const liftEpsilon = 1e-9

// errDocumentGoal rejects a document goal: it has no objective to pin, so there
// is nothing to optimize and no conjoinable segment to search.
var errDocumentGoal = errors.New("sleep cycle is not applicable to a document goal: it has no objective to optimize")

// Config is the search's tuning, held package-locally rather than as a
// config.SleepCycleConfig so internal/config stays a cmd/-only dependency
// (infra-constructor convention). cmd/sleepcycle translates one into the other.
type Config struct {
	MaxMeasurements int
	BeamWidth       int
	MaxOrder        int
	MinSupport      int
	MinLift         float64
	MaxPublications int
}

func (c Config) validate() error {
	switch {
	case c.MaxMeasurements < 1:
		return fmt.Errorf("max measurements must be at least 1, got %d", c.MaxMeasurements)
	case c.BeamWidth < 1:
		return fmt.Errorf("beam width must be at least 1, got %d", c.BeamWidth)
	case c.MaxOrder < 1:
		return fmt.Errorf("max order must be at least 1, got %d", c.MaxOrder)
	case c.MinSupport < 0:
		return fmt.Errorf("min support must not be negative, got %d", c.MinSupport)
	case c.MinLift < 0:
		return fmt.Errorf("min lift must not be negative, got %v", c.MinLift)
	case c.MaxPublications < 1:
		return fmt.Errorf("max publications must be at least 1, got %d", c.MaxPublications)
	}
	return nil
}

// The interfaces below are the narrow contracts the Worker depends on, defined
// at the consumer so the run is unit-testable with fakes and no infra.

// searchRepo is the graph surface the whole run needs: the search's input, the
// write-back, the abstraction write, and the embedding-pending lifecycle.
type searchRepo interface {
	ListEligibleFindings(ctx context.Context, goalID string) ([]graph.CausalTriplet, error)
	MarkStaleMetaHeuristics(ctx context.Context, goalID string) (int, error)
	CreateState(ctx context.Context, s domain.State) error
	CreateIntervention(ctx context.Context, i domain.Intervention) error
	CreateOutcome(ctx context.Context, o domain.Outcome) error
	CreatePreConditionFor(ctx context.Context, stateID, interventionID string) error
	CreateProduced(ctx context.Context, interventionID, outcomeID string, edge domain.ProducedEdge) error
	GetMetaHeuristic(ctx context.Context, id string) (domain.MetaHeuristic, error)
	CreateMetaHeuristic(ctx context.Context, mh domain.MetaHeuristic, abstractedFrom []string) error
	ClearEmbeddingPending(ctx context.Context, id string) error
	ListEmbeddingPending(ctx context.Context) ([]domain.MetaHeuristic, error)
}

type sandboxExecutor interface {
	Introspect(ctx context.Context, req sandboxclient.IntrospectRequest) (sandboxclient.IntrospectResponse, error)
	Execute(ctx context.Context, req sandboxclient.ExecuteRequest) (sandboxclient.ExecuteResponse, error)
}

type claudeClient interface {
	AbstractMetaHeuristic(ctx context.Context, goalText string, seg llm.MacroSegment) (llm.Abstraction, error)
	RepairMetaHeuristic(ctx context.Context, goalText string, seg llm.MacroSegment, prior llm.Abstraction, validationErr string) (llm.Abstraction, error)
}

type embeddingStore interface {
	Upsert(ctx context.Context, nodeID string, embedding []float32) error
}

type goalStore interface {
	Get(ctx context.Context, optimizationFunctionID string) (store.Goal, error)
}

type auditAppender interface {
	Append(ctx context.Context, action, eventType string, detail map[string]any) error
}

// Worker runs one Sleep Cycle per invocation.
type Worker struct {
	repo       searchRepo
	sandbox    sandboxExecutor
	claude     claudeClient
	provider   embedding.Provider
	embeddings embeddingStore
	goals      goalStore
	audits     auditAppender
	cfg        Config
}

// NewWorker wires the worker from its collaborators, validating the search
// tuning up front so a bad env value fails at startup rather than mid-search.
func NewWorker(
	repo searchRepo,
	sandbox sandboxExecutor,
	claude claudeClient,
	provider embedding.Provider,
	embeddings embeddingStore,
	goals goalStore,
	audits auditAppender,
	cfg Config,
) (*Worker, error) {
	if err := cfg.validate(); err != nil {
		return nil, err
	}
	return &Worker{
		repo:       repo,
		sandbox:    sandbox,
		claude:     claude,
		provider:   provider,
		embeddings: embeddings,
		goals:      goals,
		audits:     audits,
		cfg:        cfg,
	}, nil
}

// searchTarget is the run-invariant addressing the search and write-back share.
type searchTarget struct {
	goalID        string
	dataSourceRef string
	namespace     uuid.UUID
}

// Run executes one Sleep Cycle for a goal, in order: settle any prior run's
// leftovers (staleness sweep, embedding resume), then introspect, pin the
// objective and measure the global baseline, load the eligible findings, search
// the lattice, write back the winners, select what to publish, and abstract each
// selection.
//
// Neither search outcome ends the run: the lattice finding nothing to write back
// says nothing about whether the goal has segments worth publishing. See
// materiallyBetter for why the two gates must stay separate.
//
// Failure disposition is deliberate and differs by stage. The sweep and the
// resume pass are non-terminal: neither is a prerequisite for producing valid
// macro-segments, and the resume pass may not even concern this goal. Loading the
// goal, introspection, pinning, the baseline, and the eligible-finding read are
// terminal — each is a hard input with nothing meaningful to degrade to. Per
// candidate, per macro-segment, and per publication, failures are isolated and
// the run continues.
func (w *Worker) Run(ctx context.Context, goalID string) (err error) {
	ctx, cancel := context.WithTimeout(ctx, runTimeout)
	defer cancel()

	winners, measurements, published := 0, 0, 0
	searchSkipped := ""
	defer func() {
		// A panic unwinds with err still nil; recover so a crashed run reports
		// failed rather than success, and so one run's panic cannot escape the job.
		if r := recover(); r != nil {
			err = fmt.Errorf("sleep cycle panicked: %v", r)
		} else if err == nil && ctx.Err() != nil {
			err = ctx.Err()
		}
		// Detached from the run's own deadline so the terminal audit lands even
		// when the run ended because that context expired.
		auditCtx, auditCancel := context.WithTimeout(context.Background(), auditWriteTimeout)
		defer auditCancel()
		if err != nil {
			w.report(auditCtx, "sleepcycle_run_failed", "job", map[string]any{
				"optimization_function_id": goalID,
				"error":                    err.Error(),
			})
			return
		}
		detail := map[string]any{
			"optimization_function_id": goalID,
			"winners":                  winners,
			"measurements":             measurements,
			"published":                published,
		}
		// Only present when the search never ran, so a run reporting zero
		// measurements is never ambiguous between "searched and found nothing" and
		// "had nothing to search".
		if searchSkipped != "" {
			detail["search_skipped"] = searchSkipped
		}
		w.report(auditCtx, "sleepcycle_run_complete", "job", detail)
	}()

	goal, err := w.goals.Get(ctx, goalID)
	if err != nil {
		return fmt.Errorf("load goal: %w", err)
	}
	if goal.IsDocument() {
		return errDocumentGoal
	}

	w.sweepStale(ctx, goalID)
	w.resumeEmbeddings(ctx)

	introspect, err := w.sandbox.Introspect(ctx, sandboxclient.IntrospectRequest{DataSourceRef: goal.DataSourceRef})
	if err != nil {
		return fmt.Errorf("introspect: %w", err)
	}
	columns := columnNames(introspect.Schema)

	obj, err := objective.Pin(goal.EvaluationMatrix)
	if err != nil {
		return fmt.Errorf("pin objective: %w", err)
	}
	target := searchTarget{
		goalID:        goalID,
		dataSourceRef: goal.DataSourceRef,
		namespace:     goalNamespace(goalID),
	}
	baseline, err := w.measureBaseline(ctx, target, obj)
	if err != nil {
		return err
	}

	findings, err := w.repo.ListEligibleFindings(ctx, goalID)
	if err != nil {
		return fmt.Errorf("list eligible findings: %w", err)
	}
	bestSingle := bestSingleSegment(findings, obj, int64(w.cfg.MinSupport))

	atoms, err := buildAtoms(findings)
	if err != nil {
		return fmt.Errorf("build atoms: %w", err)
	}
	// No conjunction is formable either when there are fewer than two distinct
	// predicates or when the order cap forbids conjoining at all. The order clause
	// is load-bearing: with plenty of atoms and MaxOrder 1 the atom count alone
	// would pass while no conjunction can exist.
	var written []winner
	if len(atoms) < 2 || w.cfg.MaxOrder < 2 {
		searchSkipped = "no conjunction formable"
	} else {
		policy := newBeamPolicy(atoms, w.cfg, baseline, obj.Direction)
		outcome := w.runSearch(ctx, target, obj, policy)
		measurements = len(outcome.measured)
		if segments := w.materiallyBetter(outcome, bestSingle, obj); len(segments) > 0 {
			written = w.writeWinners(ctx, target, obj, baseline, segments)
			winners = len(written)
		}
	}

	cands := append(w.candidatesFromFindings(ctx, goalID, findings, obj, baseline),
		w.candidatesFromWinners(written, atoms, obj, baseline)...)
	selected := selectPublications(cands, w.cfg.MaxPublications)
	if len(selected) == 0 {
		w.noAbstraction(ctx, goalID, "no candidate cleared publication selection", len(atoms), bestSingle)
		return nil
	}

	published = w.abstractAll(ctx, target, obj, goal.GoalText, baseline, columns, selected)
	return nil
}

// measureBaseline measures the objective with no filters — the goal's global
// baseline, the reference every derived triplet's effect size is relative to.
//
// This and introspection are terminal on failure, matching the hypothesis loop's
// treatment of the same two calls: without a baseline there is no effect size to
// write and no delta to rank a frontier by.
func (w *Worker) measureBaseline(ctx context.Context, target searchTarget, obj objective.Objective) (float64, error) {
	resp, err := w.sandbox.Execute(ctx, objective.ExecuteRequestFor(target.dataSourceRef, obj, nil))
	if err != nil {
		return 0, fmt.Errorf("measure global baseline: %w", err)
	}
	baseline, ok := objective.NumericValue(resp.Value, obj.Label)
	if !ok {
		return 0, fmt.Errorf("measure global baseline: %w", objective.ErrNonNumericValue)
	}
	return baseline, nil
}

// bestSingleSegment is S*: the best absolute objective value any eligible finding
// achieved. It is a strictly harder bar than "best single predicate" and needs no
// depth special-casing — a deep Phase-1 finding counts too. Findings are held to
// the same support floor as the candidates S* gates, so a bar set by evidence the
// gate itself would reject (including legacy outcomes with no recorded support,
// which read as 0) is impossible. The result is nil when no eligible finding
// yields a value at or above the floor: S* is simply undefined, so nothing can
// clear materiallyBetter and the search writes nothing back.
func bestSingleSegment(findings []graph.CausalTriplet, obj objective.Objective, minSupport int64) *float64 {
	var best *float64
	for _, f := range findings {
		// Zero is excluded unconditionally, not just when a positive floor is set:
		// the package treats an unrecorded row count as no evidence anywhere else
		// (measure fails a zero-row segment, publication skips one), and a floor of 0
		// must not be the one configuration where a legacy finding sets the bar that
		// decides what gets written back.
		if f.Outcome.Support <= 0 || f.Outcome.Support < minSupport {
			continue
		}
		v, ok := objective.NumericValue(f.Outcome.Value, obj.Label)
		if !ok {
			continue
		}
		if best == nil || objective.Improves(*best, v, obj.Direction) {
			best = &v
		}
	}
	return best
}

// materiallyBetter is the final cut on the search's winner candidates: the
// macro-segment must beat the best single segment in the objective's direction by
// at least MinLift, relative. The comparison is of absolute objective scores, not
// effect sizes. With no eligible findings S* is undefined, so nothing can clear
// the bar and nothing is written back.
//
// It measures the *search's* marginal value and therefore gates write-back only.
// Publication is selected separately, over the union of Phase-1 findings and the
// winners this gate passed: a bar phrased relative to S* gets harder to clear the
// better Phase 1 did, which must not decide whether the goal publishes anything.
func (w *Worker) materiallyBetter(outcome searchOutcome, bestSingle *float64, obj objective.Objective) []measuredNode {
	if bestSingle == nil {
		return nil
	}
	var segments []measuredNode
	for _, m := range outcome.winners(int64(w.cfg.MinSupport)) {
		if !objective.Improves(*bestSingle, m.measurement.Value, obj.Direction) {
			continue
		}
		lift := math.Abs(m.measurement.Value-*bestSingle) / math.Max(math.Abs(*bestSingle), liftEpsilon)
		if lift >= w.cfg.MinLift {
			segments = append(segments, m)
		}
	}
	return segments
}

// sweepStale flags Meta-Heuristics whose supporting evidence an analyst has since
// rejected. It marks only — whether get_optimized_heuristics filters stale
// heuristics is a read-side decision that belongs with the HITL work.
//
// Non-terminal: a cosmetic marking pass is not a prerequisite for producing valid
// macro-segments, so a transient graph hiccup must not kill the whole run.
func (w *Worker) sweepStale(ctx context.Context, goalID string) {
	marked, err := w.repo.MarkStaleMetaHeuristics(ctx, goalID)
	if err != nil {
		log.Printf("sleepcycle: staleness sweep for %q: %v", goalID, err)
		w.report(ctx, "sleepcycle_staleness_sweep_failure", "failure", map[string]any{
			"optimization_function_id": goalID,
			"error":                    err.Error(),
		})
		return
	}
	w.report(ctx, "sleepcycle_staleness_sweep", "job", map[string]any{
		"optimization_function_id": goalID,
		"marked":                   marked,
	})
}

// noAbstraction records that the run produced no Meta-Heuristic, with the search
// inputs that explain it. A degenerate run is a successful run, not a failure.
//
// The reason string names the stage that came up empty; which flavour of
// degenerate it was is read off the run-complete audit, whose search_skipped,
// measurements, and winners together separate a search that never ran from one
// that ran and found nothing material.
//
// best_single is recorded as nil when there were no eligible findings: S* is
// undefined over an empty set, and a fabricated 0 would be indistinguishable from
// a legitimately-measured 0.
func (w *Worker) noAbstraction(ctx context.Context, goalID, reason string, atoms int, bestSingle *float64) {
	detail := map[string]any{
		"optimization_function_id": goalID,
		"reason":                   reason,
		"atoms":                    atoms,
		"max_order":                w.cfg.MaxOrder,
		// A typed nil *float64 in an any is not == nil, so assign through the pointer.
		"best_single": nil,
	}
	if bestSingle != nil {
		detail["best_single"] = *bestSingle
	}
	log.Printf("sleepcycle: no abstraction: %s (atoms=%d)", reason, atoms)
	w.report(ctx, "sleepcycle_no_abstraction", "job", detail)
}

// report appends an audit record, logging and swallowing any failure: an audit
// hiccup must never take down the work it describes.
func (w *Worker) report(ctx context.Context, action, eventType string, detail map[string]any) {
	if err := w.audits.Append(ctx, action, eventType, detail); err != nil {
		log.Printf("sleepcycle: append audit: %v", err)
	}
}

func columnNames(schema sandboxclient.Schema) []string {
	names := make([]string, 0, len(schema.Columns))
	for _, c := range schema.Columns {
		names = append(names, c.Name)
	}
	return names
}
