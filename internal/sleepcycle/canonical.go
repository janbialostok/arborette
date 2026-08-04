package sleepcycle

import (
	"github.com/google/uuid"

	"github.com/arborette/arborette/internal/domain"
)

// The canonical key and the derived-id scheme live in internal/domain: the Verifier
// synthesizes triplets for a directly constructed claim and must mint their ids the
// same way, and it cannot import this package. What stays here is the delegation, so
// the search's own call sites keep reading in this package's vocabulary.

// canonicalSeparator joins the per-predicate encodings inside a canonical key.
const canonicalSeparator = domain.CanonicalSeparator

// Roles distinguish the ids derived from one canonical filter, so a
// macro-segment's Intervention, Outcome, and Meta-Heuristic never collide.
const (
	roleIntervention  = domain.RoleIntervention
	roleOutcome       = domain.RoleOutcome
	roleMetaHeuristic = domain.RoleMetaHeuristic
	roleBaselineState = domain.RoleBaselineState
)

// CanonicalFilters renders a predicate set to a stable string key: a pure function
// of the set, order- and duplicate-insensitive.
func CanonicalFilters(filters []domain.Constraint) (string, error) {
	return domain.CanonicalFilters(filters)
}

func canonicalComponents(canonical string) []string {
	return domain.CanonicalComponents(canonical)
}

func goalNamespace(goalID string) uuid.UUID {
	return domain.GoalNamespace(goalID)
}

func derivedID(ns uuid.UUID, role, canonical string) string {
	return domain.DerivedID(ns, role, canonical)
}

func baselineStateID(ns uuid.UUID) string {
	return domain.BaselineStateID(ns)
}
