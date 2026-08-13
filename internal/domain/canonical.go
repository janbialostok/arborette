package domain

import (
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"

	"github.com/google/uuid"
)

// CanonicalSeparator joins the per-predicate encodings inside a canonical key;
// CanonicalComponents is its inverse. It is exported because a caller that builds a
// key by joining already-canonical single-predicate keys must join on this exact
// delimiter, and a second copy of it would drift silently.
const CanonicalSeparator = "\n"

// Roles distinguish the ids derived from one canonical filter, so the nodes a goal
// synthesizes for the same predicate set never collide. The Sleep Cycle mints the
// first four for a macro-segment; the Verifier mints the claim pair for a directly
// constructed claim, which needs its own Outcome role because a claim and a
// macro-segment can name the identical segment while measuring different things.
const (
	RoleIntervention  = "intervention"
	RoleOutcome       = "outcome"
	RoleInsight = "insight"

	// RoleMetaHeuristic is a deprecated alias for RoleInsight.
	//
	// Deprecated: Use RoleInsight instead.
	RoleMetaHeuristic = RoleInsight
	RoleBaselineState = "baseline-state"
	RoleClaim         = "claim"
	RoleClaimOutcome  = "claim-outcome"
)

// normalizeConstraint puts one predicate into the form the canonical key is
// built from: a lower-cased Field (the sandbox compiler resolves columns with
// strings.EqualFold, so case carries no meaning -- see UnknownFilterColumns)
// and Members sorted by a typed key, so IN {Europa, Mars} and IN {Mars, Europa}
// normalize identically.
//
// This is for the canonical key only. The constraints actually sent to the
// sandbox and stored in node properties keep their original field casing.
func normalizeConstraint(c Constraint) Constraint {
	c.Field = strings.ToLower(c.Field)
	if len(c.Members) > 0 {
		members := make([]LiteralValue, len(c.Members))
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
func literalKey(l LiteralValue) string {
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
// It encodes each predicate as JSON rather than via RenderConstraint because
// rendering is lossy in ways that corrupt an id. renderLiteral prints a bool true
// and the string "True" identically, which would fuse two structurally distinct
// segments onto one node id; and it prints literals unquoted, so a categorical
// value containing " AND ", ", ", or "}" could collide with a differently-structured
// predicate set. JSON preserves both the type tag and the delimiters.
func CanonicalFilters(filters []Constraint) (string, error) {
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
	return strings.Join(encoded, CanonicalSeparator), nil
}

// CanonicalComponents splits a canonical key back into its per-predicate
// encodings, so a caller can reason about one key's predicates as a set.
//
// The split is sound because each part is a complete JSON object and
// encoding/json escapes every control character inside a string: no categorical
// value can smuggle in a separator and forge a component boundary, and no
// component can come back empty. An empty key has no predicates at all rather
// than one empty predicate, which is the case a bare strings.Split gets wrong.
func CanonicalComponents(canonical string) []string {
	if canonical == "" {
		return nil
	}
	return strings.Split(canonical, CanonicalSeparator)
}

// GoalNamespace derives the per-goal UUID namespace every derived id is minted
// under, so two goals that surface the same conjoined filter still write
// distinct nodes.
func GoalNamespace(goalID string) uuid.UUID {
	return uuid.NewSHA1(uuid.NameSpaceOID, []byte(goalID))
}

// DerivedID mints the deterministic node id for one role within a goal. Keeping
// it a pure function of (goal id, role, canonical filter) is what makes the
// graph's MERGE-on-id write-back idempotent across re-runs.
func DerivedID(ns uuid.UUID, role, canonical string) string {
	return uuid.NewSHA1(ns, []byte(role+"|"+canonical)).String()
}

// BaselineStateID is the goal's shared global-baseline State id: stable per goal
// and reused by every synthesized triplet of that goal, so they all hang off one
// reference State.
func BaselineStateID(ns uuid.UUID) string {
	return DerivedID(ns, RoleBaselineState, "")
}
