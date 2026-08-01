package store

import "testing"

func TestCheckVectorVersion(t *testing.T) {
	cases := []struct {
		version string
		ok      bool
	}{
		{"0.7.4", false},
		{"0.8.0", true},
		{"0.9.0", true},
		// 0.10.2 must pass: a lexical string compare gets this wrong ("0.10" < "0.9").
		{"0.10.2", true},
		{"1.0.0", true},
	}
	for _, c := range cases {
		err := checkVectorVersion(c.version)
		if c.ok && err != nil {
			t.Fatalf("version %q must be accepted: %v", c.version, err)
		}
		if !c.ok && err == nil {
			t.Fatalf("version %q must be rejected", c.version)
		}
	}

	if err := checkVectorVersion("garbage"); err == nil {
		t.Fatal("an unparseable version must error")
	}
}

func TestValidateDistanceFloor(t *testing.T) {
	for _, floor := range []float64{0.5, 1, 2} {
		if err := ValidateDistanceFloor(floor); err != nil {
			t.Fatalf("floor %v must be accepted: %v", floor, err)
		}
	}
	for _, floor := range []float64{0, -0.1, 2.1} {
		if err := ValidateDistanceFloor(floor); err == nil {
			t.Fatalf("floor %v outside (0, 2] must be rejected", floor)
		}
	}
}

func TestScopeFromGoalID(t *testing.T) {
	if s := ScopeFromGoalID(""); !s.CrossGoal || s.GoalID != "" {
		t.Fatalf("an empty goal must map to cross-goal, got %+v", s)
	}
	// A whitespace-only id must map to cross-goal identically at both surfaces, not
	// to a whitespace goal that would fail the uuid bind.
	if s := ScopeFromGoalID("   "); !s.CrossGoal || s.GoalID != "" {
		t.Fatalf("a whitespace-only goal must map to cross-goal, got %+v", s)
	}
	if s := ScopeFromGoalID("  g1  "); s.CrossGoal || s.GoalID != "g1" {
		t.Fatalf("a padded goal must map to the trimmed goal-scoped id, got %+v", s)
	}
}

func TestSearchScopeValidate(t *testing.T) {
	if err := (SearchScope{GoalID: "g1"}).validate(); err != nil {
		t.Fatalf("a goal-scoped scope is valid: %v", err)
	}
	if err := (SearchScope{CrossGoal: true}).validate(); err != nil {
		t.Fatalf("a cross-goal scope is valid: %v", err)
	}
	if err := (SearchScope{}).validate(); err == nil {
		t.Fatal("a scope with neither mode set must be rejected")
	}
	if err := (SearchScope{GoalID: "g1", CrossGoal: true}).validate(); err == nil {
		t.Fatal("a scope with both modes set must be rejected")
	}
}
