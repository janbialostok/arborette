package sleepcycle

import (
	"encoding/json"
	"testing"

	"github.com/arborette/arborette/internal/domain"
	"github.com/arborette/arborette/internal/graph"
)

func boolPtr(b bool) *bool      { return &b }
func strPtr(s string) *string   { return &s }
func numPtr(f float64) *float64 { return &f }

func members(vs ...string) []domain.LiteralValue {
	out := make([]domain.LiteralValue, 0, len(vs))
	for _, v := range vs {
		out = append(out, domain.LiteralValue{String: strPtr(v)})
	}
	return out
}

func mustCanonical(t *testing.T, filters []domain.Constraint) string {
	t.Helper()
	got, err := CanonicalFilters(filters)
	if err != nil {
		t.Fatalf("canonical filters: %v", err)
	}
	return got
}

func TestCanonicalFiltersIsSetValued(t *testing.T) {
	age := domain.Constraint{Field: "Age", Op: domain.LessThanOrEqual, Value: 18}
	planet := domain.Constraint{Field: "HomePlanet", Op: domain.In, Members: members("Europa", "Mars")}

	base := mustCanonical(t, []domain.Constraint{age, planet})

	cases := map[string][]domain.Constraint{
		"permuted":          {planet, age},
		"duplicated":        {age, planet, age},
		"differently cased": {{Field: "AGE", Op: domain.LessThanOrEqual, Value: 18}, {Field: "homeplanet", Op: domain.In, Members: members("Europa", "Mars")}},
		"members reordered": {age, {Field: "HomePlanet", Op: domain.In, Members: members("Mars", "Europa")}},
	}
	for name, filters := range cases {
		t.Run(name, func(t *testing.T) {
			if got := mustCanonical(t, filters); got != base {
				t.Fatalf("canonical key differs for %s:\n got %q\nwant %q", name, got, base)
			}
		})
	}
}

// TestCanonicalFiltersEncodesStructurally guards the two ways rendering the
// predicate (rather than encoding it) would corrupt an id: a bool and a string
// that render alike would fuse two macro-segments onto one node, and a value
// containing the rendered delimiters would collide with a different predicate set.
func TestCanonicalFiltersEncodesStructurally(t *testing.T) {
	boolFilter := domain.Constraint{Field: "flag", Op: domain.Equal, Operand: &domain.LiteralValue{Bool: boolPtr(true)}}
	stringFilter := domain.Constraint{Field: "flag", Op: domain.Equal, Operand: &domain.LiteralValue{String: strPtr("True")}}
	if mustCanonical(t, []domain.Constraint{boolFilter}) == mustCanonical(t, []domain.Constraint{stringFilter}) {
		t.Fatal("a bool true and the string \"True\" must not share a canonical key")
	}

	// A literal carrying the delimiters a rendered predicate set would use.
	sneaky := domain.Constraint{Field: "name", Op: domain.Equal, Operand: &domain.LiteralValue{String: strPtr("a AND name = b")}}
	twoPredicates := []domain.Constraint{
		{Field: "name", Op: domain.Equal, Operand: &domain.LiteralValue{String: strPtr("a")}},
		{Field: "name", Op: domain.Equal, Operand: &domain.LiteralValue{String: strPtr("b")}},
	}
	if mustCanonical(t, []domain.Constraint{sneaky}) == mustCanonical(t, twoPredicates) {
		t.Fatal("a literal containing \" AND \" must not collide with a two-predicate set")
	}

	distinct := domain.Constraint{Field: "Age", Op: domain.GreaterThan, Value: 18}
	if mustCanonical(t, []domain.Constraint{distinct}) == mustCanonical(t, []domain.Constraint{boolFilter}) {
		t.Fatal("distinct predicate sets must not collide")
	}
}

// TestMemberOrderingIsTypeAwareAcrossKinds: member sorting feeds a node *id*, so
// an ordering that is not total across literal kinds would make the same
// predicate set canonicalize two ways — splitting one macro-segment across two
// derived nodes on re-run, which is exactly what the deterministic id prevents.
func TestMemberOrderingIsTypeAwareAcrossKinds(t *testing.T) {
	mixed := func(vs ...domain.LiteralValue) []domain.Constraint {
		return []domain.Constraint{{Field: "col", Op: domain.In, Members: vs}}
	}
	a := domain.LiteralValue{String: strPtr("True")}
	b := domain.LiteralValue{Bool: boolPtr(true)}
	c := domain.LiteralValue{Number: numPtr(1)}

	base := mustCanonical(t, mixed(a, b, c))
	for _, permutation := range [][]domain.LiteralValue{
		{c, b, a}, {b, a, c}, {a, c, b}, {c, a, b},
	} {
		if got := mustCanonical(t, mixed(permutation...)); got != base {
			t.Fatalf("member permutation changed the canonical key:\n got %q\nwant %q", got, base)
		}
	}

	// The kinds stay distinguishable: a bool true and the string "True" render
	// alike but must not sort — or canonicalize — as the same member.
	if mustCanonical(t, mixed(a)) == mustCanonical(t, mixed(b)) {
		t.Fatal("a string member and a bool member that render alike must not share a key")
	}
	if mustCanonical(t, mixed(c)) == mustCanonical(t, mixed(a)) {
		t.Fatal("a numeric member must not share a key with a string member")
	}
}

func TestDerivedIDsAreStableAndRoleScoped(t *testing.T) {
	ns := goalNamespace("goal-1")
	canonical := mustCanonical(t, []domain.Constraint{atomOn("a"), atomOn("b")})

	intervention := derivedID(ns, roleIntervention, canonical)
	if intervention != derivedID(ns, roleIntervention, canonical) {
		t.Fatal("a derived id must be stable for the same goal, role, and filter")
	}
	if intervention == derivedID(ns, roleOutcome, canonical) {
		t.Fatal("roles must not share an id for the same filter")
	}
	if intervention == derivedID(goalNamespace("goal-2"), roleIntervention, canonical) {
		t.Fatal("two goals must not share an id for the same filter")
	}

	// The baseline State is per goal and independent of any filter, so every
	// macro-segment of a goal hangs off exactly one shared node.
	if baselineStateID(ns) != baselineStateID(goalNamespace("goal-1")) {
		t.Fatal("the baseline state id must be stable per goal")
	}
	if baselineStateID(ns) == baselineStateID(goalNamespace("goal-2")) {
		t.Fatal("two goals must not share a baseline state id")
	}
}

// TestDecodeConstraintsRoundTripsGraphShape feeds decodeConstraints the shape the
// graph layer actually returns -- []any of map[string]any from the JSON-string
// property blob -- rather than a typed slice, because a direct type assertion
// against that shape fails at runtime and would silently empty the atom set.
func TestDecodeConstraintsRoundTripsGraphShape(t *testing.T) {
	original := []domain.Constraint{
		{Field: "Age", Op: domain.LessThanOrEqual, Value: 18},
		{Field: "CryoSleep", Op: domain.Equal, Operand: &domain.LiteralValue{Bool: boolPtr(true)}},
		{Field: "HomePlanet", Op: domain.In, Members: members("Europa", "Mars")},
	}

	// Exactly the marshal/unmarshal the graph property round-trip performs.
	encoded, err := json.Marshal(map[string]any{"new_filters": original})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var props map[string]any
	if err := json.Unmarshal(encoded, &props); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if _, ok := props["new_filters"].([]domain.Constraint); ok {
		t.Fatal("the graph shape must not already be a typed slice, or this guard proves nothing")
	}

	decoded, err := domain.DecodeConstraints(props["new_filters"])
	if err != nil {
		t.Fatalf("decode constraints: %v", err)
	}
	if len(decoded) != 3 {
		t.Fatalf("expected 3 constraints, got %d", len(decoded))
	}
	if decoded[0].Field != "Age" || decoded[0].Op != domain.LessThanOrEqual || decoded[0].Value != 18 {
		t.Fatalf("numeric threshold did not round-trip: %+v", decoded[0])
	}
	if decoded[1].Operand == nil || decoded[1].Operand.Bool == nil || !*decoded[1].Operand.Bool {
		t.Fatalf("equality operand did not round-trip: %+v", decoded[1])
	}
	if len(decoded[2].Members) != 2 || decoded[2].Members[0].String == nil || *decoded[2].Members[0].String != "Europa" {
		t.Fatalf("membership members did not round-trip: %+v", decoded[2])
	}
	if mustCanonical(t, decoded) != mustCanonical(t, original) {
		t.Fatal("a decoded constraint set must canonicalize identically to the original")
	}
}

// TestNodeCanonicalMatchesFilters pins the invariant the write-back depends on:
// the key the beam builds by joining per-atom keys is byte-identical to what
// CanonicalFilters produces from the node's own filters, so the id a
// macro-segment is persisted under cannot drift from the node that was searched.
func TestNodeCanonicalMatchesFilters(t *testing.T) {
	atoms, err := buildAtoms([]graph.CausalTriplet{
		finding("1", 1, testObjectiveLabel, "a"),
		finding("2", 1, testObjectiveLabel, "b"),
		finding("3", 1, testObjectiveLabel, "c"),
	})
	if err != nil {
		t.Fatalf("build atoms: %v", err)
	}

	node := &Node{
		keys:      []string{atoms[0].key},
		ranks:     []int{atoms[0].rank},
		filters:   []domain.Constraint{atoms[0].constraint},
		canonical: atoms[0].key,
	}
	for _, a := range atoms[1:] {
		node = conjoin(node, a)
	}
	if node.canonical != mustCanonical(t, node.filters) {
		t.Fatalf("node key %q does not match CanonicalFilters over its own filters %q",
			node.canonical, mustCanonical(t, node.filters))
	}
}
