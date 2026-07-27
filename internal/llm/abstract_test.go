package llm

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/anthropics/anthropic-sdk-go"

	"github.com/arborette/arborette/internal/domain"
)

func testSegment() MacroSegment {
	return MacroSegment{
		ObjectiveLabel: "avg(Transported = True)",
		Direction:      domain.Maximize,
		Filters: []domain.Constraint{
			{Field: "HomePlanet", Op: domain.Equal, Operand: &domain.LiteralValue{String: ptr("Mars")}},
			{Field: "CryoSleep", Op: domain.Equal, Operand: &domain.LiteralValue{Bool: ptr(true)}},
		},
		Baseline: 0.5,
		Value:    0.82,
	}
}

func ptr[T any](v T) *T { return &v }

func TestAbstractMetaHeuristic(t *testing.T) {
	body := `{"definition":"[Primary Population Center] combined with [Dormancy State] raises [System Output]",` +
		`"ontology_terms":[{"concrete":"HomePlanet","ontological":"[Primary Population Center]"},` +
		`{"concrete":"CryoSleep","ontological":"[Dormancy State]"}]}`
	fake := &fakeMessages{resp: message(t, anthropic.StopReasonEndTurn, textBlockJSON(body))}
	c := &Client{messages: fake, model: "test-model"}

	abstraction, err := c.AbstractMetaHeuristic(context.Background(), "grow the transported rate", testSegment())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(abstraction.Definition, "[Primary Population Center]") {
		t.Fatalf("definition did not decode: %q", abstraction.Definition)
	}
	if len(abstraction.OntologyTerms) != 2 {
		t.Fatalf("expected 2 ontology terms, got %d", len(abstraction.OntologyTerms))
	}
	if abstraction.OntologyTerms[0].Concrete != "HomePlanet" || abstraction.OntologyTerms[0].Ontological != "[Primary Population Center]" {
		t.Fatalf("ontology term did not decode: %+v", abstraction.OntologyTerms[0])
	}

	// The strict output schema and adaptive thinking are load-bearing on the request.
	if fake.got.OutputConfig.Format.Schema == nil {
		t.Fatal("expected a json_schema output format on the request")
	}
	if fake.got.Thinking.OfAdaptive == nil {
		t.Fatal("expected adaptive thinking on the request")
	}
	// The prompt renders the segment through the shared constraint renderer, so
	// the prompt and the graph describe the same segment in the same spelling.
	user := fake.got.Messages[0].Content[0].OfText.Text
	if !strings.Contains(user, "HomePlanet = Mars") || !strings.Contains(user, "CryoSleep = True") {
		t.Fatalf("prompt did not render the conjoined filter: %q", user)
	}
	if !strings.Contains(user, "avg(Transported = True)") {
		t.Fatalf("prompt did not carry the objective label: %q", user)
	}
}

func TestRepairMetaHeuristicEmbedsThePriorAndTheLeak(t *testing.T) {
	body := `{"definition":"[Primary Population Center] raises [System Output]","ontology_terms":[]}`
	fake := &fakeMessages{resp: message(t, anthropic.StopReasonEndTurn, textBlockJSON(body))}
	c := &Client{messages: fake, model: "test-model"}

	prior := Abstraction{Definition: "HomePlanet drives Transported"}
	_, err := c.RepairMetaHeuristic(context.Background(), "grow the transported rate", testSegment(), prior,
		"these dataset column names are still present in the definition: HomePlanet")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	user := fake.got.Messages[0].Content[0].OfText.Text
	if !strings.Contains(user, "HomePlanet drives Transported") {
		t.Fatalf("repair prompt did not embed the prior definition: %q", user)
	}
	if !strings.Contains(user, "still present in the definition: HomePlanet") {
		t.Fatalf("repair prompt did not embed the leak: %q", user)
	}
}

func TestAbstractMetaHeuristicPropagatesRequestError(t *testing.T) {
	fake := &fakeMessages{err: errors.New("boom")}
	c := &Client{messages: fake, model: "test-model"}
	if _, err := c.AbstractMetaHeuristic(context.Background(), "goal", testSegment()); err == nil {
		t.Fatal("expected an error when the request fails")
	}
}

// TestLeakedConcreteTerms is the deterministic guard that grounds "actually
// generalized": the output schema leaves the definition a free string, so nothing
// else checks it.
func TestLeakedConcreteTerms(t *testing.T) {
	columns := []string{"HomePlanet", "CryoSleep", "Transported"}

	clean := "[Primary Population Center] combined with [Dormancy State] raises [System Output]"
	if leaked := LeakedConcreteTerms(clean, columns); len(leaked) != 0 {
		t.Fatalf("a fully abstracted definition must not leak: %v", leaked)
	}

	// Case-insensitive, matching the sandbox compiler's own column resolution.
	leaked := LeakedConcreteTerms("segments where homeplanet is Mars raise Transported", columns)
	if len(leaked) != 2 {
		t.Fatalf("expected both columns flagged, got %v", leaked)
	}
	if leaked[0] != "HomePlanet" || leaked[1] != "Transported" {
		t.Fatalf("leaks must be reported in schema order with their real names, got %v", leaked)
	}

	// Deduplicated, so one repeated column is reported once.
	if leaked := LeakedConcreteTerms("HomePlanet and HomePlanet", columns); len(leaked) != 1 {
		t.Fatalf("a repeated column must be reported once, got %v", leaked)
	}
	if leaked := LeakedConcreteTerms("anything", nil); len(leaked) != 0 {
		t.Fatalf("no columns means nothing can leak, got %v", leaked)
	}
}

// TestLeakedConcreteTermsMatchesWholeWords is the false-positive guard. Short
// column names are common English fragments, and this check fails closed: a
// spurious leak burns a repair call and then drops the macro-segment entirely, so
// a substring match would silently produce no heuristics at all on a schema with
// a column like Age or Spa.
func TestLeakedConcreteTermsMatchesWholeWords(t *testing.T) {
	columns := []string{"Age", "Spa", "Name", "VIP", "new"}

	clean := []string{
		"the average [System Output] across the [Primary Population Center]",
		"usage rises for a sparse [Dormancy State] population",
		"namely, the percentage engaged in [Transit]",
		"newly acquired members of the [Cohort]",
	}
	for _, def := range clean {
		if leaked := LeakedConcreteTerms(def, columns); len(leaked) != 0 {
			t.Fatalf("%q contains no whole-word column name, but reported %v", def, leaked)
		}
	}

	// A genuine leak still fires, including when punctuation or brackets abut it.
	for _, def := range []string{
		"segments where Age > 30 raise output",
		// The embedded "age" inside "average" comes first; the scan must keep
		// looking rather than concluding the definition is clean.
		"the average [System Output] rises when Age exceeds thirty",
		"the [Age] bracket drives it",
		"grouped by Spa, then by VIP",
		"filtered to new customers",
	} {
		if leaked := LeakedConcreteTerms(def, columns); len(leaked) == 0 {
			t.Fatalf("%q names a real column and must be flagged", def)
		}
	}
}

// TestLeakedConcreteTermsAnchorsOnlyWordEdges pins the conditional half of the
// boundary rule. A CSV header can legally start or end with punctuation, and for
// those columns an unconditional boundary check fails OPEN: the adjacent
// character is not a word rune either, so a genuine leak reads as clean.
func TestLeakedConcreteTermsAnchorsOnlyWordEdges(t *testing.T) {
	columns := []string{"% Change", "#Orders", "(net)"}

	for _, def := range []string{
		"a 5% Change in throughput",
		"tracked as 12#Orders per cycle",
		"the total(net) across the segment",
	} {
		if leaked := LeakedConcreteTerms(def, columns); len(leaked) == 0 {
			t.Fatalf("%q names a real column abutted by a digit or letter and must be flagged", def)
		}
	}
}

// TestLeakedConcreteTermsTreatsUnderscoreAsAWordRune is the snake_case
// false-positive guard. Underscore-joined ontology terms are a natural thing for
// the model to emit, and a column name embedded in one is not a leak -- but a
// spurious leak fails closed and drops the macro-segment entirely.
func TestLeakedConcreteTermsTreatsUnderscoreAsAWordRune(t *testing.T) {
	columns := []string{"output", "age", "id"}

	for _, def := range []string{
		"[Primary_Output] rises with [Dormancy_State]",
		"the [Passenger_Age_Band] drives it",
		"keyed by [System_id]",
	} {
		if leaked := LeakedConcreteTerms(def, columns); len(leaked) != 0 {
			t.Fatalf("%q embeds the column inside a snake_case token, but reported %v", def, leaked)
		}
	}

	// The column standing alone is still a leak.
	if leaked := LeakedConcreteTerms("segments where output is high", columns); len(leaked) != 1 {
		t.Fatalf("a bare column name must still be flagged, got %v", leaked)
	}
}

// TestDecodeAbstractionRejectsAnEmptyDefinition: an empty definition passes the
// leak check vacuously and would be persisted and embedded permanently, since the
// deterministic id plus a cleared embedding flag make every later run skip it.
func TestDecodeAbstractionRejectsAnEmptyDefinition(t *testing.T) {
	for _, body := range []string{
		`{"definition":"","ontology_terms":[]}`,
		`{"definition":"   \n","ontology_terms":[]}`,
	} {
		if _, err := decodeAbstraction(body); err == nil {
			t.Fatalf("an empty definition must not decode successfully: %s", body)
		}
	}
}

func TestMetaHeuristicSchemaIsStatic(t *testing.T) {
	schema := metaHeuristicSchema()
	props, _ := schema["properties"].(map[string]any)
	if _, ok := props["definition"]; !ok {
		t.Fatalf("schema is missing the definition property: %+v", schema)
	}
	if _, ok := props["ontology_terms"]; !ok {
		t.Fatalf("schema is missing ontology_terms: %+v", schema)
	}
	// No enums and no recursion keeps the compiled grammar small and the schema
	// statically cacheable, per the constraints the schema file documents.
	encoded, err := json.Marshal(schema)
	if err != nil {
		t.Fatalf("marshal schema: %v", err)
	}
	if strings.Contains(string(encoded), `"enum"`) {
		t.Fatalf("the abstraction schema must carry no enums: %s", encoded)
	}
}
