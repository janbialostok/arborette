package llm

import (
	"context"
	"errors"
	"testing"

	"github.com/arborette/arborette/internal/domain"
)

func groundSchema() SandboxSchema {
	return SandboxSchema{Columns: []SandboxColumn{
		{Name: "HomePlanet", Type: "VARCHAR", DistinctValues: []string{"Earth", "Mars"}},
		{Name: "Age", Type: "DOUBLE"},
	}}
}

// TestGroundHeuristicDecodesTheFilterString pins the JSON-string filter round trip:
// the conjunction rides as a plain string to stay inside the grammar ceiling, so the
// polymorphic value has to survive the extra encoding layer with its type intact.
func TestGroundHeuristicDecodesTheFilterString(t *testing.T) {
	body := `{"filters":"[{\"field\":\"HomePlanet\",\"op\":\"eq\",\"value\":\"Mars\"},` +
		`{\"field\":\"Age\",\"op\":\"lte\",\"value\":18}]","rationale":"population centre and youth"}`
	c := &client{backend: &fakeCompleter{text: body}}

	filters, err := c.GroundHeuristic(context.Background(), "[Primary Population Center] raises [System Output]", nil, groundSchema())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(filters) != 2 {
		t.Fatalf("filters = %+v, want the two-predicate conjunction", filters)
	}
	if filters[0].Operand == nil || filters[0].Operand.String == nil || *filters[0].Operand.String != "Mars" {
		t.Fatalf("string operand did not survive the encoding layer: %+v", filters[0])
	}
	if filters[1].Op != domain.LessThanOrEqual || filters[1].Value != 18 {
		t.Fatalf("numeric threshold did not survive the encoding layer: %+v", filters[1])
	}
}

// TestGroundHeuristicReportsANonTransferringHeuristic: an empty filter string is the
// model declining to invent a segment, which the caller must be able to tell from a
// malformed body — one drops a proposal, the other is a fault worth auditing.
func TestGroundHeuristicReportsANonTransferringHeuristic(t *testing.T) {
	for name, body := range map[string]string{
		"empty string": `{"filters":"","rationale":"no counterpart here"}`,
		"empty array":  `{"filters":"[]","rationale":"no counterpart here"}`,
	} {
		t.Run(name, func(t *testing.T) {
			c := &client{backend: &fakeCompleter{text: body}}
			_, err := c.GroundHeuristic(context.Background(), "[Term] raises [Output]", nil, groundSchema())
			if !errors.Is(err, ErrNoGroundedFilters) {
				t.Fatalf("err = %v, want the non-transferring sentinel", err)
			}
		})
	}
}

// TestGroundHeuristicRejectsAMalformedConjunction: the filters field is a free
// string, so a body that parses as JSON can still carry a predicate no compiler
// would accept. It must fail here rather than reach the sandbox.
func TestGroundHeuristicRejectsAMalformedConjunction(t *testing.T) {
	body := `{"filters":"[{\"field\":\"Age\",\"op\":\"lte\",\"value\":\"young\"}]","rationale":""}`
	c := &client{backend: &fakeCompleter{text: body}}

	if _, err := c.GroundHeuristic(context.Background(), "[Term] raises [Output]", nil, groundSchema()); err == nil {
		t.Fatal("a numeric threshold with a string operand must be rejected")
	}
}

// TestGroundHeuristicSchemaStaysStatic: filter columns and values are dataset-sized,
// so the conjunction must not be a schema'd array the grammar has to compile.
func TestGroundHeuristicSchemaStaysStatic(t *testing.T) {
	properties, _ := groundHeuristicSchema()["properties"].(map[string]any)
	prop, _ := properties["filters"].(map[string]any)
	if prop["type"] != "string" {
		t.Fatalf("filters must be a plain string, got %v", prop)
	}
}
