package sleepcycle

import (
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"

	"github.com/google/uuid"

	"github.com/arborette/arborette/internal/domain"
)

// Roles distinguish the ids derived from one canonical filter, so a
// macro-segment's Intervention, Outcome, and Meta-Heuristic never collide.
const (
	roleIntervention  = "intervention"
	roleOutcome       = "outcome"
	roleMetaHeuristic = "metaheuristic"
	roleBaselineState = "baseline-state"
)

// normalizeConstraint puts one predicate into the form the canonical key is
// built from: a lower-cased Field (the sandbox compiler resolves columns with
// strings.EqualFold, so case carries no meaning -- see domain.UnknownFilterColumns)
// and Members sorted by a typed key, so IN {Europa, Mars} and IN {Mars, Europa}
// normalize identically.
//
// This is for the canonical key only. The constraints actually sent to the
// sandbox and stored in node properties keep their original field casing.
func normalizeConstraint(c domain.Constraint) domain.Constraint {
	c.Field = strings.ToLower(c.Field)
	if len(c.Members) > 0 {
		members := make([]domain.LiteralValue, len(c.Members))
		copy(members, c.Members)
		sort.Slice(members, func(i, j int) bool {
			return literalKey(members[i]) < literalKey(members[j])
		})
		c.Members = members
	}
	return c
}

// literalKey orders literals by type tag first, then by value, so a bool and a
// string that render alike still sort deterministically and distinctly.
func literalKey(l domain.LiteralValue) string {
	switch {
	case l.Number != nil:
		return "n:" + strconv.FormatFloat(*l.Number, 'g', -1, 64)
	case l.String != nil:
		return "s:" + *l.String
	case l.Bool != nil:
		return "b:" + strconv.FormatBool(*l.Bool)
	default:
		return "z:"
	}
}

// CanonicalFilters renders a predicate set to a stable string key. The result is
// a pure function of the *set*: order-independent, duplicate-insensitive,
// case-insensitive on Field, and member-order-insensitive.
//
// It encodes each predicate as JSON rather than via domain.RenderConstraint
// because rendering is lossy in ways that corrupt an id. renderLiteral prints a
// bool true and the string "True" identically, which would fuse two structurally
// distinct macro-segments onto one node id; and it prints literals unquoted, so a
// categorical value containing " AND ", ", ", or "}" could collide with a
// differently-structured predicate set. JSON preserves both the type tag and the
// delimiters.
func CanonicalFilters(filters []domain.Constraint) (string, error) {
	encoded := make([]string, 0, len(filters))
	seen := make(map[string]bool, len(filters))
	for _, f := range filters {
		b, err := json.Marshal(normalizeConstraint(f))
		if err != nil {
			return "", fmt.Errorf("encode filter %q: %w", f.Field, err)
		}
		key := string(b)
		if seen[key] {
			continue
		}
		seen[key] = true
		encoded = append(encoded, key)
	}
	sort.Strings(encoded)
	return strings.Join(encoded, "\n"), nil
}

// goalNamespace derives the per-goal UUID namespace every derived id is minted
// under, so two goals that surface the same conjoined filter still write
// distinct nodes.
func goalNamespace(goalID string) uuid.UUID {
	return uuid.NewSHA1(uuid.NameSpaceOID, []byte(goalID))
}

// derivedID mints the deterministic node id for one role within a goal. Keeping
// it a pure function of (goal id, role, canonical filter) is what makes the
// graph's MERGE-on-id write-back idempotent across re-runs.
func derivedID(ns uuid.UUID, role, canonical string) string {
	return uuid.NewSHA1(ns, []byte(role+"|"+canonical)).String()
}

// baselineStateID is the goal's shared global-baseline State id: stable per goal
// and reused by every macro-segment of that goal, so all derived triplets hang
// off one reference State.
func baselineStateID(ns uuid.UUID) string {
	return derivedID(ns, roleBaselineState, "")
}

// decodeConstraints converts a node property back into typed constraints. It is
// mandatory rather than defensive: new_filters is stored as a typed
// []domain.Constraint, but the graph layer marshals the whole property map to a
// JSON string and decodes it back into map[string]any -- so the value arrives as
// []any of map[string]any with nested operand/members objects. A direct type
// assertion fails at runtime and silently yields an empty atom set, which would
// make every run degenerate.
func decodeConstraints(v any) ([]domain.Constraint, error) {
	if v == nil {
		return nil, nil
	}
	b, err := json.Marshal(v)
	if err != nil {
		return nil, fmt.Errorf("re-encode filters: %w", err)
	}
	var filters []domain.Constraint
	if err := json.Unmarshal(b, &filters); err != nil {
		return nil, fmt.Errorf("decode filters: %w", err)
	}
	return filters, nil
}
