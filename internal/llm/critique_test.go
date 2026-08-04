package llm

import (
	"context"
	"slices"
	"strings"
	"testing"
)

func critiqueSchema() SandboxSchema {
	return SandboxSchema{Columns: []SandboxColumn{
		{Name: "PassengerId", Type: "VARCHAR"},
		{Name: "HomePlanet", Type: "VARCHAR", DistinctValues: []string{"Earth", "Mars"}},
		{Name: "CryoSleep", Type: "BOOLEAN", DistinctValues: []string{"true", "false"}},
	}}
}

// TestCritiqueAtomsGroundsToRealColumns pins the deterministic half of a free-string
// output: the two lists are prompt-grounded, so a name the schema does not carry has
// to be dropped here or it would silently narrow (or misrank) the search vocabulary
// against a column that does not exist.
func TestCritiqueAtomsGroundsToRealColumns(t *testing.T) {
	body := `{"excluded_columns":"[\"PassengerId\",\"NoSuchColumn\"]",` +
		`"ranked_columns":"[\"cryosleep\",\"Invented\",\"HomePlanet\"]","rationale":"ids split into singletons"}`
	fake := &fakeCompleter{text: body}
	c := &client{backend: fake}

	critique, err := c.CritiqueAtoms(context.Background(), "grow the transport rate", critiqueSchema())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !slices.Equal(critique.ExcludedColumns, []string{"PassengerId"}) {
		t.Fatalf("excluded = %v, want only the real column", critique.ExcludedColumns)
	}
	// Resolved to the schema's own spelling, since every downstream consumer matches
	// columns case-insensitively but records them verbatim.
	if !slices.Equal(critique.RankedColumns, []string{"CryoSleep", "HomePlanet"}) {
		t.Fatalf("ranked = %v, want the real columns in the critic's order and spelling", critique.RankedColumns)
	}
}

// TestCritiqueAtomsDropsExcludedFromRanking: a column the search must not segment on
// cannot also be one it should try first, and a critique naming it on both lists
// would otherwise prior an atom that was never in the vocabulary.
func TestCritiqueAtomsDropsExcludedFromRanking(t *testing.T) {
	body := `{"excluded_columns":"[\"PassengerId\"]","ranked_columns":"[\"PassengerId\",\"CryoSleep\"]","rationale":""}`
	c := &client{backend: &fakeCompleter{text: body}}

	critique, err := c.CritiqueAtoms(context.Background(), "goal", critiqueSchema())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !slices.Equal(critique.RankedColumns, []string{"CryoSleep"}) {
		t.Fatalf("ranked = %v, want the excluded column dropped", critique.RankedColumns)
	}
}

// TestCritiqueAtomsAcceptsAnEmptyCritique: both lists are optional, and an empty
// string is "no opinion", not a malformed body — the search is meant to degrade to
// an unfiltered, unranked vocabulary rather than fail.
func TestCritiqueAtomsAcceptsAnEmptyCritique(t *testing.T) {
	c := &client{backend: &fakeCompleter{text: `{"excluded_columns":"","ranked_columns":"","rationale":""}`}}

	critique, err := c.CritiqueAtoms(context.Background(), "goal", critiqueSchema())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(critique.ExcludedColumns) != 0 || len(critique.RankedColumns) != 0 {
		t.Fatalf("empty lists must decode to nothing, got %+v", critique)
	}
}

// TestCritiqueAtomsRejectsAnOverlongRationale guards the one free-string field the
// schema does not bound, matching how the classifier treats its own.
func TestCritiqueAtomsRejectsAnOverlongRationale(t *testing.T) {
	body := `{"excluded_columns":"","ranked_columns":"","rationale":"` + strings.Repeat("x", maxRationaleLen+1) + `"}`
	c := &client{backend: &fakeCompleter{text: body}}

	if _, err := c.CritiqueAtoms(context.Background(), "goal", critiqueSchema()); err == nil {
		t.Fatal("an overlong rationale must be rejected")
	}
}

// TestCritiqueAtomsSchemaStaysStatic: the column set is dataset-sized, so it must
// ride as a plain string rather than an enum the grammar has to compile.
func TestCritiqueAtomsSchemaStaysStatic(t *testing.T) {
	schema := critiqueAtomsSchema()
	properties, _ := schema["properties"].(map[string]any)
	for _, field := range []string{"excluded_columns", "ranked_columns"} {
		prop, _ := properties[field].(map[string]any)
		if prop["type"] != "string" {
			t.Fatalf("%s must be a plain string, got %v", field, prop)
		}
		if _, hasEnum := prop["enum"]; hasEnum {
			t.Fatalf("%s must not carry a dataset-sized enum: %v", field, prop)
		}
	}
}

// TestCritiqueAtomsCollapsesDuplicateColumns: a critic that names one column twice
// would otherwise weight it twice in the derived priors, and under a different case
// the two spellings would not even look like the same column.
func TestCritiqueAtomsCollapsesDuplicateColumns(t *testing.T) {
	body := `{"excluded_columns":"[\"PassengerId\",\"passengerid\"]",` +
		`"ranked_columns":"[\"CryoSleep\",\"cryosleep\",\"HomePlanet\"]","rationale":""}`
	c := &client{backend: &fakeCompleter{text: body}}

	critique, err := c.CritiqueAtoms(context.Background(), "goal", critiqueSchema())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !slices.Equal(critique.ExcludedColumns, []string{"PassengerId"}) {
		t.Fatalf("excluded = %v, want one entry per column", critique.ExcludedColumns)
	}
	if !slices.Equal(critique.RankedColumns, []string{"CryoSleep", "HomePlanet"}) {
		t.Fatalf("ranked = %v, want one entry per column in first-seen order", critique.RankedColumns)
	}
}

// TestCritiqueAtomsRejectsAMalformedList: the lists are free strings the schema does
// not bound, so a body that parses as JSON can still carry something that is not a
// column list. It fails here rather than silently reading as no opinion.
func TestCritiqueAtomsRejectsAMalformedList(t *testing.T) {
	for name, body := range map[string]string{
		"not an array":     `{"excluded_columns":"PassengerId","ranked_columns":"","rationale":""}`,
		"not strings":      `{"excluded_columns":"[1,2]","ranked_columns":"","rationale":""}`,
		"malformed ranked": `{"excluded_columns":"","ranked_columns":"{\"a\":1}","rationale":""}`,
	} {
		t.Run(name, func(t *testing.T) {
			c := &client{backend: &fakeCompleter{text: body}}
			if _, err := c.CritiqueAtoms(context.Background(), "goal", critiqueSchema()); err == nil {
				t.Fatal("a malformed column list must be rejected")
			}
		})
	}
}
