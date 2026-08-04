package sleepcycle

import (
	"context"
	"errors"
	"testing"

	"github.com/arborette/arborette/internal/llm"
)

// TestVocabularyIsFindingsOnlyWithoutTheFlag: the schema-derived vocabulary is
// opt-in, so a default run pays no critic call and searches exactly the predicates
// the hypothesis loop measured.
func TestVocabularyIsFindingsOnlyWithoutTheFlag(t *testing.T) {
	h := newHarness(t, uctConfig())
	findingAtoms, err := buildAtoms(findingsFor("a", "b"))
	if err != nil {
		t.Fatalf("build atoms: %v", err)
	}

	vocab := h.worker.vocabulary(context.Background(), "g1", "grow revenue", schemaFixture(), findingAtoms)

	if len(vocab.atoms) != len(findingAtoms) {
		t.Fatalf("atoms = %d, want the findings-derived set of %d", len(vocab.atoms), len(findingAtoms))
	}
	if h.claude.critiqueCall != 0 {
		t.Fatalf("no critic call may be paid with the vocabulary flag off, got %d", h.claude.critiqueCall)
	}
}

// TestVocabularyWidensUnderTheCritic: with the flag on, the enumerated predicates
// join the measured ones minus whatever the critic excluded, and the counts are
// audited so an operator can see whether a critique narrowed the search or gutted it.
func TestVocabularyWidensUnderTheCritic(t *testing.T) {
	cfg := uctConfig()
	cfg.SchemaAtoms = true
	h := newHarness(t, cfg)
	h.claude.critique = llm.AtomCritique{
		ExcludedColumns: []string{"CryoSleep"},
		RankedColumns:   []string{"HomePlanet", "Age"},
	}
	findingAtoms, err := buildAtoms(findingsFor("a"))
	if err != nil {
		t.Fatalf("build atoms: %v", err)
	}

	vocab := h.worker.vocabulary(context.Background(), "g1", "grow revenue", schemaFixture(), findingAtoms)

	if len(vocab.atoms) <= len(findingAtoms) {
		t.Fatalf("atoms = %d, want the enumerated predicates joined on", len(vocab.atoms))
	}
	for _, a := range vocab.atoms {
		if a.constraint.Field == "CryoSleep" {
			t.Fatalf("an excluded column contributed a predicate: %+v", a.constraint)
		}
	}
	record, ok := h.audits.find("sleepcycle_atom_vocabulary")
	if !ok {
		t.Fatal("the merged vocabulary must be audited")
	}
	if record.detail["excluded_atoms"].(int) == 0 {
		t.Fatalf("the audit must report how many predicates the exclusions dropped: %+v", record.detail)
	}
}

// TestVocabularyDegradesWhenTheCriticFails is the disposition that matters most
// here: an unreviewed enumeration is worse than no enumeration, because the
// predicates the critic exists to remove — identifiers, post-outcome columns,
// restatements of the objective — are exactly the ones that would consume the budget.
func TestVocabularyDegradesWhenTheCriticFails(t *testing.T) {
	cfg := uctConfig()
	cfg.SchemaAtoms = true
	h := newHarness(t, cfg)
	h.claude.critiqueErr = errors.New("claude down")
	findingAtoms, err := buildAtoms(findingsFor("a", "b"))
	if err != nil {
		t.Fatalf("build atoms: %v", err)
	}

	vocab := h.worker.vocabulary(context.Background(), "g1", "grow revenue", schemaFixture(), findingAtoms)

	if len(vocab.atoms) != len(findingAtoms) {
		t.Fatalf("atoms = %d, want only the findings-derived set of %d", len(vocab.atoms), len(findingAtoms))
	}
	if _, recorded := h.audits.find("sleepcycle_atom_vocabulary_failure"); !recorded {
		t.Fatal("a failed critique must be recorded")
	}
}

// TestAtomPriorsRankTheCriticsColumns: the ranking is advisory and per column, so it
// has to reach every atom on a ranked column, strongest first, and leave the rest to
// the policy's uniform floor.
func TestAtomPriorsRankTheCriticsColumns(t *testing.T) {
	schemaAtoms, err := buildSchemaAtoms(schemaFixture(), 3)
	if err != nil {
		t.Fatalf("build schema atoms: %v", err)
	}
	vocab := atomVocabulary{atoms: rankAtoms(schemaAtoms), ranking: []string{"HomePlanet", "Age"}}

	priors := atomPriors(vocab)

	var homePlanet, age float64
	for _, a := range vocab.atoms {
		switch a.constraint.Field {
		case "HomePlanet":
			homePlanet = priors[a.key]
		case "Age":
			age = priors[a.key]
		case "CryoSleep":
			if _, ranked := priors[a.key]; ranked {
				t.Fatalf("an unranked column must carry no prior: %+v", a.constraint)
			}
		}
	}
	if !(homePlanet > age && age > 0) {
		t.Fatalf("priors must descend with the ranking, got HomePlanet %v and Age %v", homePlanet, age)
	}
}

// TestAtomPriorsAreAbsentWithoutARanking: a critic that ranked nothing leaves every
// move equally weighted, which is the unbiased search the policy degrades to.
func TestAtomPriorsAreAbsentWithoutARanking(t *testing.T) {
	if priors := atomPriors(atomVocabulary{atoms: []atom{{key: "k"}}}); len(priors) != 0 {
		t.Fatalf("priors = %v, want none", priors)
	}
}
