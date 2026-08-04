package sleepcycle

import (
	"fmt"
	"testing"

	"github.com/arborette/arborette/internal/domain"
	"github.com/arborette/arborette/internal/sandboxclient"
)

// schemaFixture is a data source of the three shapes the vocabulary types
// differently: a categorical column, a boolean one, and a numeric one carrying
// quantile cuts.
func schemaFixture() sandboxclient.Schema {
	return sandboxclient.Schema{Columns: []sandboxclient.Column{
		{Name: "HomePlanet", Type: "VARCHAR", DistinctValues: []string{"Earth", "Mars"}},
		{Name: "CryoSleep", Type: "BOOLEAN", DistinctValues: []string{"true", "false"}},
		{Name: "Age", Type: "DOUBLE", QuantileCuts: []float64{18, 40}},
		{Name: "Fare", Type: "DOUBLE"},
	}}
}

// TestBuildSchemaAtomsTypesOperandsFromTheColumn is the compile-time contract:
// introspection reports every distinct value as text, and the sandbox rejects a
// filter whose operand type disagrees with its column, so an untyped equality would
// fail to compile on every boolean and numeric column it was derived from.
func TestBuildSchemaAtomsTypesOperandsFromTheColumn(t *testing.T) {
	atoms, err := buildSchemaAtoms(schemaFixture(), 4)
	if err != nil {
		t.Fatalf("build schema atoms: %v", err)
	}

	byField := map[string][]domain.Constraint{}
	for _, a := range atoms {
		byField[a.constraint.Field] = append(byField[a.constraint.Field], a.constraint)
	}
	for _, c := range byField["HomePlanet"] {
		if c.Operand == nil || c.Operand.String == nil {
			t.Fatalf("a categorical equality must carry a string operand: %+v", c)
		}
	}
	for _, c := range byField["CryoSleep"] {
		if c.Operand == nil || c.Operand.Bool == nil {
			t.Fatalf("a boolean equality must carry a boolean operand: %+v", c)
		}
	}
	// Two cuts yield a two-sided predicate each: the population above a boundary and
	// the one at or below it are different segments, not one ranking's complement.
	if len(byField["Age"]) != 4 {
		t.Fatalf("two cuts must yield four threshold predicates, got %+v", byField["Age"])
	}
	if len(byField["Fare"]) != 0 {
		t.Fatalf("a column with no probed values or cuts yields no predicates, got %+v", byField["Fare"])
	}
}

// TestBuildSchemaAtomsWithoutBinsDerivesNoThresholds: the bin count is the gate, so
// a run that did not ask the introspection for cuts gets an equality-only
// vocabulary even if cuts happened to arrive.
func TestBuildSchemaAtomsWithoutBinsDerivesNoThresholds(t *testing.T) {
	atoms, err := buildSchemaAtoms(schemaFixture(), 0)
	if err != nil {
		t.Fatalf("build schema atoms: %v", err)
	}
	for _, a := range atoms {
		if a.constraint.IsNumericThresholdOp() {
			t.Fatalf("no threshold predicate may be derived with the probe off: %+v", a.constraint)
		}
	}
}

// TestBuildSchemaAtomsCarryNoProvenance: an enumerated predicate is not a measured
// finding, so attaching Phase-1 source ids to it would link a winner built from it
// to evidence that never existed.
func TestBuildSchemaAtomsCarryNoProvenance(t *testing.T) {
	atoms, err := buildSchemaAtoms(schemaFixture(), 4)
	if err != nil {
		t.Fatalf("build schema atoms: %v", err)
	}
	for _, a := range atoms {
		if len(a.sourceIDs) != 0 {
			t.Fatalf("schema atom %q must carry no provenance, got %v", a.key, a.sourceIDs)
		}
	}
}

// TestMergeAtomsExcludesOnlyTheSchemaSide pins which half of the vocabulary a
// critic can retract. A findings-derived atom is a predicate the hypothesis loop
// already measured against this objective — evidence, not a guess — and an opinion
// about a column must not delete it.
func TestMergeAtomsExcludesOnlyTheSchemaSide(t *testing.T) {
	findingAtoms, err := buildAtoms(findingsFor("HomePlanet"))
	if err != nil {
		t.Fatalf("build atoms: %v", err)
	}
	schemaAtoms, err := buildSchemaAtoms(schemaFixture(), 4)
	if err != nil {
		t.Fatalf("build schema atoms: %v", err)
	}

	merged, dropped := mergeAtoms(findingAtoms, schemaAtoms, []string{"HomePlanet", "CryoSleep"})

	fields := map[string]int{}
	for _, a := range merged {
		fields[a.constraint.Field]++
	}
	if fields["CryoSleep"] != 0 {
		t.Fatalf("an excluded column must contribute no schema atoms, got %d", fields["CryoSleep"])
	}
	if fields["HomePlanet"] != len(findingAtoms) {
		t.Fatalf("the findings-derived atom on an excluded column must survive: %d of %d", fields["HomePlanet"], len(findingAtoms))
	}
	if fields["Age"] == 0 {
		t.Fatalf("an unexcluded column must still contribute, got none")
	}
	// The dropped count is what the audit reports, so it has to be the filtering
	// that actually happened — every schema atom on either excluded column.
	wantDropped := 0
	for _, a := range schemaAtoms {
		if a.constraint.Field == "HomePlanet" || a.constraint.Field == "CryoSleep" {
			wantDropped++
		}
	}
	if dropped != wantDropped {
		t.Fatalf("dropped = %d, want the %d enumerated predicates on the excluded columns", dropped, wantDropped)
	}
}

// TestMergeAtomsKeepsFindingProvenanceOnACollision: the two vocabularies can derive
// the same predicate, and only one side carries the source ids that link a winner
// back to the triplets it generalizes.
func TestMergeAtomsKeepsFindingProvenanceOnACollision(t *testing.T) {
	shared := domain.Constraint{Field: "HomePlanet", Op: domain.Equal, Operand: stringOperand("Mars")}
	key := mustCanonical(t, []domain.Constraint{shared})
	findingAtoms := []atom{{key: key, constraint: shared, sourceIDs: []string{"i-1"}}}
	schemaAtoms := []atom{{key: key, constraint: shared}}

	merged, _ := mergeAtoms(findingAtoms, schemaAtoms, nil)

	if len(merged) != 1 {
		t.Fatalf("a repeated predicate must merge to one atom, got %d", len(merged))
	}
	if len(merged[0].sourceIDs) != 1 || merged[0].sourceIDs[0] != "i-1" {
		t.Fatalf("the findings-derived provenance must win, got %v", merged[0].sourceIDs)
	}
}

// TestMergeAtomsRanksTheUnion: rank is a pure function of the atom set, which is
// what makes a re-run walk the lattice in the same order.
func TestMergeAtomsRanksTheUnion(t *testing.T) {
	schemaAtoms, err := buildSchemaAtoms(schemaFixture(), 4)
	if err != nil {
		t.Fatalf("build schema atoms: %v", err)
	}
	merged, _ := mergeAtoms(nil, schemaAtoms, nil)
	for i, a := range merged {
		if a.rank != i {
			t.Fatalf("atom %d carries rank %d; ranks must number the sorted union", i, a.rank)
		}
		if i > 0 && merged[i-1].key >= a.key {
			t.Fatalf("the union must be sorted by canonical key: %q then %q", merged[i-1].key, a.key)
		}
	}
}

func stringOperand(v string) *domain.LiteralValue {
	return &domain.LiteralValue{String: &v}
}

// TestBuildSchemaAtomsSkipsUntypeableValues: introspection reports every value cast
// to text from arbitrary source data, so a value that does not parse as its column's
// type has no well-typed equality to offer. Emitting one anyway would fail at
// compile time for every atom derived from that column, spending budget on
// measurements that can never succeed.
func TestBuildSchemaAtomsSkipsUntypeableValues(t *testing.T) {
	schema := sandboxclient.Schema{Columns: []sandboxclient.Column{
		{Name: "flag", Type: "BOOLEAN", DistinctValues: []string{"true", "maybe"}},
		{Name: "count", Type: "INTEGER", DistinctValues: []string{"7", "many"}},
	}}

	atoms, err := buildSchemaAtoms(schema, 0)
	if err != nil {
		t.Fatalf("build schema atoms: %v", err)
	}

	if len(atoms) != 2 {
		t.Fatalf("atoms = %d, want only the values that type against their column", len(atoms))
	}
	for _, a := range atoms {
		switch a.constraint.Field {
		case "flag":
			if a.constraint.Operand == nil || a.constraint.Operand.Bool == nil {
				t.Fatalf("the surviving boolean predicate must carry a boolean operand: %+v", a.constraint)
			}
		case "count":
			if a.constraint.Operand == nil || a.constraint.Operand.Number == nil {
				t.Fatalf("the surviving integer predicate must carry a numeric operand: %+v", a.constraint)
			}
		}
	}
}

// TestBuildSchemaAtomsTypesEveryDuckDBNumericKind guards the column-type classifier
// against drifting from the compiler's: an unsigned or wide integer column that
// classified as text would produce a string operand the sandbox rejects, and the
// symptom would be a vocabulary of atoms that all fail to measure.
func TestBuildSchemaAtomsTypesEveryDuckDBNumericKind(t *testing.T) {
	kinds := []string{"TINYINT", "SMALLINT", "INTEGER", "BIGINT", "HUGEINT",
		"UTINYINT", "USMALLINT", "UINTEGER", "UBIGINT", "UHUGEINT", "DECIMAL(7,2)"}
	columns := make([]sandboxclient.Column, 0, len(kinds))
	for i, kind := range kinds {
		columns = append(columns, sandboxclient.Column{
			Name: fmt.Sprintf("c%d", i), Type: kind, DistinctValues: []string{"1"},
		})
	}

	atoms, err := buildSchemaAtoms(sandboxclient.Schema{Columns: columns}, 0)
	if err != nil {
		t.Fatalf("build schema atoms: %v", err)
	}
	if len(atoms) != len(kinds) {
		t.Fatalf("atoms = %d, want one per numeric kind", len(atoms))
	}
	for _, a := range atoms {
		if a.constraint.Operand == nil || a.constraint.Operand.Number == nil {
			t.Fatalf("column type %q produced a non-numeric operand: %+v", a.constraint.Field, a.constraint)
		}
	}
}
