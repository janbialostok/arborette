package llm

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/arborette/arborette/internal/domain"
)

// ErrNoGroundedFilters reports a heuristic the model could not express against the
// target's columns. It is a normal answer, not a fault: a heuristic abstracted from
// one dataset need not have a counterpart in another, and the caller drops the
// proposal rather than searching a filter it invented to fill the gap.
var ErrNoGroundedFilters = errors.New("heuristic does not ground to this data source")

// GroundHeuristic re-instantiates an abstract heuristic against a concrete data
// source: given the bracketed definition, the ontology terms it was written in,
// and the target's columns and value sets, it returns the filter conjunction that
// expresses the same relationship here.
//
// The definition and its terms are prior model output and the columns are
// dataset-derived, so all three are fenced as data. The returned filters are
// grounded to real columns and values by the caller's deterministic check — the
// output schema leaves them a free string, and a proposal naming a column that
// does not exist is dropped, never repaired.
func (c *client) GroundHeuristic(ctx context.Context, definition string, terms []OntologyTerm, schema SandboxSchema) ([]domain.Constraint, error) {
	fence := NewFence("HEURISTIC-CONTEXT")
	body, err := c.backend.complete(ctx, groundHeuristicSystem+fence.Directive(),
		groundPrompt(fence, definition, terms, schema), groundHeuristicSchema())
	if err != nil {
		return nil, err
	}
	return decodeGroundedFilters(body)
}

type groundedHeuristicWire struct {
	Filters   string `json:"filters"`
	Rationale string `json:"rationale"`
}

// decodeGroundedFilters parses one grounding answer. An empty filter string is the
// model declining to transfer the heuristic, which is reported as
// ErrNoGroundedFilters so the caller can tell it from a malformed body.
func decodeGroundedFilters(body string) ([]domain.Constraint, error) {
	var wire groundedHeuristicWire
	if err := json.Unmarshal([]byte(body), &wire); err != nil {
		return nil, fmt.Errorf("parse grounded heuristic: %w", err)
	}
	if strings.TrimSpace(wire.Filters) == "" {
		return nil, ErrNoGroundedFilters
	}
	filters, err := decodeFilterConjunction(wire.Filters)
	if err != nil {
		return nil, fmt.Errorf("parse grounded heuristic: %w", err)
	}
	if len(filters) == 0 {
		return nil, ErrNoGroundedFilters
	}
	return filters, nil
}

const groundHeuristicSystem = "You re-express a generalized, dataset-agnostic heuristic as a concrete segment of a " +
	"specific tabular data source. The heuristic's definition references only bracketed structural ontology terms; " +
	"you are given the term mapping it was abstracted under and the columns of the new data source. Map each " +
	"bracketed term onto the column of THIS data source that plays the same structural role — matching what the " +
	"column means, not what it is named — and return the filter conjunction that selects the segment the heuristic " +
	"describes. Reference ONLY columns present in the provided \"Available columns\" list, using each column's exact " +
	"name as written there, and when a column lists a value set use only values drawn verbatim from it — a filter " +
	"naming a column or value that is not there will be rejected. Return an empty \"filters\" string when the " +
	"heuristic has no counterpart in this data source: a segment invented to fill the gap is worse than none." +
	filterShapeGuide + ` The "filters" field MUST be a JSON string containing a JSON array of filter objects — not a bare array — and the array is read as a conjunction (every filter must hold).`

func groundPrompt(fence Fence, definition string, terms []OntologyTerm, schema SandboxSchema) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Heuristic to ground:\n%s\n\n", fence.Wrap(definition))
	fmt.Fprintf(&b, "Ontology terms it was abstracted under:\n%s\n\n", fence.Wrap(termSummary(terms)))
	fmt.Fprintf(&b, "Available columns:\n%s\n", fence.Wrap(columnSummary(schema)))
	b.WriteString("\nReturn the filter conjunction selecting this heuristic's segment in this data source.\n")
	return b.String()
}

// termSummary renders the abstraction's term mapping as the readable pairs the
// grounding prompt is shown. The concrete side names another dataset's columns, so
// it is untrusted text and the caller renders this inside a fence.
func termSummary(terms []OntologyTerm) string {
	if len(terms) == 0 {
		return "(none recorded)"
	}
	var b strings.Builder
	for _, t := range terms {
		fmt.Fprintf(&b, "- %s was abstracted from a column meaning %q\n", t.Ontological, t.Concrete)
	}
	return b.String()
}
