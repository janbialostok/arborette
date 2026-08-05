package llm

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
)

// AtomCritique is the critic's read of a data source's columns for one goal: the
// columns a search must not segment on, and an optional ranking of the ones most
// worth trying first.
//
// Excluded names the columns whose segments would be true but useless — an
// identifier that splits the data into singletons, a column recorded after the
// outcome, a restatement of the objective itself. Ranked is advisory: it orders
// the columns the critic expects to define a real segment, and an empty ranking
// simply means the critic offered no opinion.
type AtomCritique struct {
	ExcludedColumns []string
	RankedColumns   []string
	Rationale       string
}

func (c *client) CritiqueAtoms(ctx context.Context, goalText string, schema SandboxSchema) (AtomCritique, error) {
	fence := NewFence("COLUMN-CONTEXT")
	body, err := c.backend.complete(ctx, critiqueAtomsSystem+fence.Directive(),
		critiquePrompt(fence, goalText, schema), critiqueAtomsSchema())
	if err != nil {
		return AtomCritique{}, err
	}
	return decodeAtomCritique(body, schema)
}

type atomCritiqueWire struct {
	ExcludedColumns string `json:"excluded_columns"`
	RankedColumns   string `json:"ranked_columns"`
	Rationale       string `json:"rationale"`
}

// decodeAtomCritique parses one critique and grounds it to the real columns. A
// name the schema does not carry is dropped rather than rejected: the critique is
// advisory on both axes — an exclusion narrows a vocabulary the caller can search
// without, and a ranking only orders it — so failing the whole call over one
// hallucinated name would cost the goal every genuine exclusion alongside it.
//
// A column named on both lists is dropped from the ranking, since a column the
// search must not segment on cannot be one it should try first.
func decodeAtomCritique(body string, schema SandboxSchema) (AtomCritique, error) {
	var wire atomCritiqueWire
	if err := json.Unmarshal([]byte(body), &wire); err != nil {
		return AtomCritique{}, fmt.Errorf("parse atom critique: %w", err)
	}
	if len(wire.Rationale) > maxRationaleLen {
		return AtomCritique{}, errRationaleTooLong
	}
	excluded, err := decodeColumnList(wire.ExcludedColumns, schema)
	if err != nil {
		return AtomCritique{}, fmt.Errorf("parse excluded columns: %w", err)
	}
	ranked, err := decodeColumnList(wire.RankedColumns, schema)
	if err != nil {
		return AtomCritique{}, fmt.Errorf("parse ranked columns: %w", err)
	}
	critique := AtomCritique{
		ExcludedColumns: excluded,
		RankedColumns:   withoutColumns(ranked, excluded),
		Rationale:       stripControlChars(wire.Rationale),
	}
	return critique, nil
}

// decodeColumnList parses a JSON-encoded array of column names, keeping only those
// the schema really carries and resolving each to the schema's own spelling —
// every downstream consumer matches columns case-insensitively, so a critique that
// answered in a different case must still bind. Order is preserved (the ranking
// is meaningful) and duplicates are collapsed to the first occurrence. An empty
// string is no list, not a parse failure: both fields are optional.
func decodeColumnList(raw string, schema SandboxSchema) ([]string, error) {
	if strings.TrimSpace(raw) == "" {
		return nil, nil
	}
	var names []string
	if err := json.Unmarshal([]byte(raw), &names); err != nil {
		return nil, err
	}
	seen := map[string]bool{}
	var out []string
	for _, name := range names {
		actual, ok := schemaColumn(schema, name)
		if !ok || seen[strings.ToLower(actual)] {
			continue
		}
		seen[strings.ToLower(actual)] = true
		out = append(out, actual)
	}
	return out, nil
}

// schemaColumn resolves a name to the schema's own spelling under the
// case-insensitive matching the sandbox compiler and the domain grounding checks
// both use.
func schemaColumn(schema SandboxSchema, name string) (string, bool) {
	for _, c := range schema.Columns {
		if strings.EqualFold(c.Name, name) {
			return c.Name, true
		}
	}
	return "", false
}

func withoutColumns(names, remove []string) []string {
	if len(remove) == 0 {
		return names
	}
	drop := make(map[string]bool, len(remove))
	for _, r := range remove {
		drop[strings.ToLower(r)] = true
	}
	out := make([]string, 0, len(names))
	for _, n := range names {
		if !drop[strings.ToLower(n)] {
			out = append(out, n)
		}
	}
	return out
}

const critiqueAtomsSystem = "You review the columns of a tabular data source before an automated search segments the " +
	"data on them, given the analyst's optimization goal. Return two lists. \"excluded_columns\" names every column " +
	"the search must NOT segment on: unique or near-unique identifiers (row ids, keys, names) whose segments describe " +
	"single records rather than a population; columns recorded after or as a consequence of the outcome, so a segment " +
	"on them explains the outcome by restating it; and columns that are the objective itself or a direct " +
	"re-expression of it. \"ranked_columns\" is optional and advisory: the columns most likely to define a genuinely " +
	"useful segment for this goal, strongest first, and an empty list when you have no basis to prefer any. " +
	"Reference ONLY columns present in the provided \"Available columns\" list, using each column's exact name as " +
	"written there — a name absent from the list is ignored." + columnListShapeGuide

const columnListShapeGuide = ` Both "excluded_columns" and "ranked_columns" MUST be JSON strings containing a JSON array of column-name strings — not bare arrays. For example excluded_columns "[\"PassengerId\",\"Name\"]" and ranked_columns "[\"CryoSleep\",\"HomePlanet\"]". Return an empty string for a list you have nothing to put in.`

func critiquePrompt(fence Fence, goalText string, schema SandboxSchema) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Analyst goal:\n%s\n\n", fence.Wrap(goalText))
	fmt.Fprintf(&b, "Available columns:\n%s\n", fence.Wrap(columnSummary(schema)))
	b.WriteString("\nReview these columns for the search described above.\n")
	return b.String()
}
