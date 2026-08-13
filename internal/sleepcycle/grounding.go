package sleepcycle

import (
	"context"
	"errors"
	"fmt"
	"log"
	"strings"

	"github.com/arborette/arborette/internal/domain"
	"github.com/arborette/arborette/internal/llm"
	"github.com/arborette/arborette/internal/sandboxclient"
	"github.com/arborette/arborette/internal/store"
)

// maxProposalsPerHeuristic bounds how many conjunctions one retrieved heuristic may
// contribute. The same-data-source path recovers a conjunction per Intervention the
// heuristic was abstracted from, and that set grows with the Phase-1 tree behind it,
// so without a bound one strong retrieval hit could crowd out both the other hits
// and the goal's own predicates — the root expands every move before it descends,
// so that spends the budget before the search conjoins anything of its own. Three
// keeps a heuristic represented while leaving the retrieval's breadth intact; the
// three kept are the first the graph returns, not the strongest, since a source
// conjunction carries no ranking the retrieval can compare across heuristics.
const maxProposalsPerHeuristic = 3

// maxProposalAttempts bounds how many of a heuristic's conjunctions are validated
// at all. Every rejection is audited, and the same-data-source path routinely
// offers order-1 conjunctions that validation must refuse, so bounding only the
// kept proposals would leave the audit volume unbounded on exactly the heuristics
// that contribute nothing.
const maxProposalAttempts = 3 * maxProposalsPerHeuristic

// groundedProposal is one conjunction a previously abstracted Meta-Heuristic
// proposes for this goal: the predicates expressed in this data source's own
// columns, the heuristic they came from, and how close that heuristic sat to the
// goal in the corpus. keys are the per-predicate canonical keys in filters order,
// and canonical is the key of the set — precomputed here so the policy reifies the
// proposal without re-deriving what the validation already had to compute.
type groundedProposal struct {
	filters     []domain.Constraint
	keys        []string
	canonical   string
	heuristicID string
	prior       float64
}

// groundProposals turns the corpus into candidate conjunctions for this goal:
// retrieve the heuristics nearest the goal, recover each one's segment in this data
// source's terms, and keep only those that survive deterministic validation.
//
// The retrieval is cross-goal by default, because that is the whole point — a
// heuristic abstracted from this goal's own findings describes a segment the atom
// search already reaches, so a goal-scoped retrieval would leave the reuse path
// inert on exactly the sparse-findings goal it exists for. The kill switch narrows
// it back to one goal when an operator needs that.
//
// Every failure degrades to fewer proposals, never to an aborted run: the search
// runs on atoms alone if retrieval, hydration, or every single grounding call
// fails.
func (w *Worker) groundProposals(ctx context.Context, target searchTarget, goalText string, schema sandboxclient.Schema) []groundedProposal {
	vec, err := w.provider.EmbedQuery(ctx, goalText)
	if err != nil {
		w.groundingFailure(ctx, target.goalID, "", fmt.Errorf("embed goal: %w", err))
		return nil
	}
	scope := store.SearchScope{CrossGoal: true}
	if !w.cfg.CrossGoalGrounding {
		scope = store.ScopeFromGoalID(target.goalID)
	}
	refs, err := w.embeddings.SimilaritySearchScored(ctx, vec, w.cfg.RetrievalK, scope)
	if err != nil {
		w.groundingFailure(ctx, target.goalID, "", fmt.Errorf("retrieve heuristics: %w", err))
		return nil
	}
	if len(refs) == 0 {
		return nil
	}

	ids := make([]string, 0, len(refs))
	for _, ref := range refs {
		ids = append(ids, ref.NodeID)
	}
	fetched, err := w.repo.GetMetaHeuristics(ctx, ids)
	if err != nil {
		w.groundingFailure(ctx, target.goalID, "", fmt.Errorf("load heuristics: %w", err))
		return nil
	}
	byID := make(map[string]domain.MetaHeuristic, len(fetched))
	for _, mh := range fetched {
		byID[mh.ID] = mh
	}

	var proposals []groundedProposal
	// Walked in retrieval order, not the graph's: the distance is what ranks a
	// proposal, and the batch read carries the graph's own arbitrary ordering.
	for _, ref := range refs {
		mh, ok := byID[ref.NodeID]
		if !ok || mh.Stale {
			continue
		}
		conjunctions, err := w.instantiate(ctx, target, schema, mh)
		if err != nil {
			// A heuristic with no counterpart here is the expected outcome of reaching
			// across datasets, so it is recorded as a dropped proposal rather than as a
			// broken dependency — the distinction an operator reads to tell "the corpus
			// does not transfer" from "the model is unreachable".
			if errors.Is(err, llm.ErrNoGroundedFilters) {
				w.proposalDropped(ctx, target.goalID, mh.ID, err)
				continue
			}
			w.groundingFailure(ctx, target.goalID, mh.ID, err)
			continue
		}
		kept, attempted := 0, 0
		for _, filters := range conjunctions {
			if attempted >= maxProposalAttempts {
				w.proposalsTruncated(ctx, target.goalID, mh.ID, len(conjunctions), kept)
				break
			}
			attempted++
			if kept >= maxProposalsPerHeuristic {
				w.proposalsTruncated(ctx, target.goalID, mh.ID, len(conjunctions), kept)
				break
			}
			prop, err := w.validateProposal(filters, mh.ID, similarityPrior(ref.Distance), schema)
			if err != nil {
				w.proposalDropped(ctx, target.goalID, mh.ID, err)
				continue
			}
			proposals = append(proposals, prop)
			kept++
		}
	}
	if len(proposals) > 0 {
		w.report(ctx, "sleepcycle_grounded_proposals", "job", map[string]any{
			"optimization_function_id": target.goalID,
			"retrieved":                len(refs),
			"proposals":                len(proposals),
			"cross_goal":               w.cfg.CrossGoalGrounding,
		})
	}
	return proposals
}

// instantiate recovers the concrete conjunctions a heuristic stands for in this
// data source, by whichever of the two routes its provenance supports.
//
// A heuristic abstracted from this very data source needs no model call at all: its
// own source conjunctions are already written in these columns, and the persisted
// term map confirms the definition really is the abstraction of them. Anything else
// — another data source, or a node abstracted before the terms were persisted —
// goes through the grounding call, which is the only way to map bracketed terms
// onto columns it has never seen.
func (w *Worker) instantiate(ctx context.Context, target searchTarget, schema sandboxclient.Schema, mh domain.MetaHeuristic) ([][]domain.Constraint, error) {
	if w.sameDataSource(target, mh) {
		conjunctions, err := w.repo.AbstractionSourceFilters(ctx, mh.ID)
		if err != nil {
			return nil, fmt.Errorf("load source conjunctions: %w", err)
		}
		if len(conjunctions) > 0 {
			return conjunctions, nil
		}
	}
	filters, err := w.claude.GroundHeuristic(ctx, mh.Definition, ontologyTermsForLLM(mh.OntologyTerms), llmSchema(schema))
	if err != nil {
		return nil, fmt.Errorf("ground heuristic: %w", err)
	}
	return [][]domain.Constraint{filters}, nil
}

// sameDataSource reports whether a heuristic's own segments are already expressed
// in the target's columns: it was abstracted from this data source, and its term
// map resolves every bracketed term its definition names. Both halves are required
// — an origin match with no resolvable terms means the node predates the persisted
// mapping, and its definition cannot be confirmed to describe those segments.
func (w *Worker) sameDataSource(target searchTarget, mh domain.MetaHeuristic) bool {
	if mh.OriginDataSourceRef == "" || mh.OriginDataSourceRef != target.dataSourceRef || len(mh.OntologyTerms) == 0 {
		return false
	}
	for _, term := range bracketedTerms(mh.Definition) {
		if !resolvesTerm(mh.OntologyTerms, term) {
			return false
		}
	}
	return true
}

// bracketedTerms reads the ontology terms a definition names, which are exactly its
// bracketed spans — the abstraction contract requires every variable to be written
// that way, so an unbracketed word is prose, not a term.
func bracketedTerms(definition string) []string {
	var terms []string
	rest := definition
	for {
		open := strings.Index(rest, "[")
		if open < 0 {
			return terms
		}
		rest = rest[open+1:]
		end := strings.Index(rest, "]")
		if end < 0 {
			return terms
		}
		if term := strings.TrimSpace(rest[:end]); term != "" {
			terms = append(terms, term)
		}
		rest = rest[end+1:]
	}
}

func resolvesTerm(terms []domain.OntologyTerm, name string) bool {
	for _, t := range terms {
		if strings.EqualFold(strings.Trim(t.Ontological, "[]"), name) {
			return true
		}
	}
	return false
}

// validateProposal is the gate every adopted conjunction passes before it can be
// measured. A proposal naming a column or value this data source does not have, or
// breaching the policy contract's (field, op) invariant, or falling outside the
// conjunction orders this search forms, is dropped whole — never repaired. A
// repaired proposal is no longer the heuristic's suggestion, and measuring one
// would attribute a segment this code invented to knowledge that did not propose it.
func (w *Worker) validateProposal(filters []domain.Constraint, heuristicID string, prior float64, schema sandboxclient.Schema) (groundedProposal, error) {
	if len(filters) < 2 || len(filters) > w.cfg.MaxOrder {
		return groundedProposal{}, fmt.Errorf("conjunction of %d predicates is outside the searchable order range [2, %d]",
			len(filters), w.cfg.MaxOrder)
	}
	if unknown := domain.UnknownFilterColumns(filters, columnNames(schema)); len(unknown) > 0 {
		return groundedProposal{}, fmt.Errorf("names unknown column(s): %s", strings.Join(unknown, ", "))
	}
	if unknown := domain.UnknownFilterValues(filters, distinctValues(schema)); len(unknown) > 0 {
		return groundedProposal{}, fmt.Errorf("names unknown value(s): %s", strings.Join(unknown, ", "))
	}
	if duplicateFieldOp(filters) {
		return groundedProposal{}, fmt.Errorf("conjoins two predicates on one field and operator")
	}

	keys := make([]string, 0, len(filters))
	for _, f := range filters {
		key, err := CanonicalFilters([]domain.Constraint{f})
		if err != nil {
			return groundedProposal{}, err
		}
		keys = append(keys, key)
	}
	return groundedProposal{
		filters:     filters,
		keys:        keys,
		canonical:   canonicalKeyOf(keys),
		heuristicID: heuristicID,
		prior:       prior,
	}, nil
}

// similarityPrior maps a cosine distance onto a prior in [0, 1]: the operator's
// distance is in [0, 2], so an exact match priors at 1 and the opposite end at 0.
func similarityPrior(distance float64) float64 {
	prior := 1 - distance/2
	if prior < 0 {
		return 0
	}
	return prior
}

// distinctValues indexes the schema's value sets by column, the shape the shared
// value-grounding check reads.
func distinctValues(schema sandboxclient.Schema) map[string][]string {
	values := make(map[string][]string, len(schema.Columns))
	for _, c := range schema.Columns {
		if len(c.DistinctValues) > 0 {
			values[c.Name] = c.DistinctValues
		}
	}
	return values
}

// llmSchema converts the introspected schema into the dependency-free shape the llm
// package prompts against, so the grounding call is shown the same columns and
// value sets the validation checks against.
func llmSchema(schema sandboxclient.Schema) llm.SandboxSchema {
	cols := make([]llm.SandboxColumn, 0, len(schema.Columns))
	for _, c := range schema.Columns {
		cols = append(cols, llm.SandboxColumn{Name: c.Name, Type: c.Type, DistinctValues: c.DistinctValues})
	}
	return llm.SandboxSchema{Columns: cols}
}

func ontologyTermsForLLM(terms []domain.OntologyTerm) []llm.OntologyTerm {
	out := make([]llm.OntologyTerm, 0, len(terms))
	for _, t := range terms {
		out = append(out, llm.OntologyTerm{Concrete: t.Concrete, Ontological: t.Ontological})
	}
	return out
}

func ontologyTermsFromLLM(terms []llm.OntologyTerm) []domain.OntologyTerm {
	out := make([]domain.OntologyTerm, 0, len(terms))
	for _, t := range terms {
		out = append(out, domain.OntologyTerm{Concrete: t.Concrete, Ontological: t.Ontological})
	}
	return out
}

// groundingFailure records a heuristic that could not be turned into a proposal at
// all — retrieval, hydration, or the grounding call itself failed. It is separate
// from a dropped proposal so an operator can tell a broken dependency from
// knowledge that simply does not transfer.
func (w *Worker) groundingFailure(ctx context.Context, goalID, mhID string, err error) {
	log.Printf("sleepcycle: ground heuristic for %q: %v", goalID, err)
	detail := map[string]any{
		"optimization_function_id": goalID,
		"meta_heuristic_id":        nil,
		"error":                    err.Error(),
	}
	if mhID != "" {
		detail["meta_heuristic_id"] = mhID
	}
	w.report(ctx, "sleepcycle_grounding_failure", "failure", detail)
}

// proposalsTruncated records that a heuristic offered more conjunctions than one
// retrieval hit may walk. It is reported rather than left silent because a
// truncated set reads exactly like a heuristic that only had that much to offer —
// which is also why kept is the count this walk actually produced rather than the
// cap: the attempt bound stops a heuristic whose conjunctions were all refused, and
// reporting the cap there would claim contributions it never made.
func (w *Worker) proposalsTruncated(ctx context.Context, goalID, mhID string, offered, kept int) {
	w.report(ctx, "sleepcycle_proposals_truncated", "job", map[string]any{
		"optimization_function_id": goalID,
		"meta_heuristic_id":        mhID,
		"offered":                  offered,
		"kept":                     kept,
	})
}

// proposalDropped records a grounded conjunction the validation refused. It is a
// job event, not a failure: knowledge that does not express itself in this data
// source's columns is the expected outcome of reaching across datasets, and the
// record is what makes the drop visible rather than silent.
func (w *Worker) proposalDropped(ctx context.Context, goalID, mhID string, err error) {
	log.Printf("sleepcycle: dropped grounded proposal from %q: %v", mhID, err)
	w.report(ctx, "sleepcycle_proposal_dropped", "job", map[string]any{
		"optimization_function_id": goalID,
		"meta_heuristic_id":        mhID,
		"reason":                   err.Error(),
	})
}
