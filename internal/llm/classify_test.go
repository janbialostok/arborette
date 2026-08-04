package llm

import (
	"context"
	"strings"
	"testing"

	"github.com/arborette/arborette/internal/domain"
)

func classifyInput() GoalIntentInput {
	return GoalIntentInput{
		GoalText: "do passengers in cryo-sleep get transported more often?",
		Schema:   SandboxSchema{Columns: []SandboxColumn{{Name: "CryoSleep", Type: "BOOLEAN"}}},
	}
}

// TestClassifyGoalIntentExplore: an explore classification carries no claim, and the
// claim fields the schema still requires are ignored rather than parsed.
func TestClassifyGoalIntentExplore(t *testing.T) {
	body := `{"track":"explore","rationale":"the goal asks which segments matter","claim_filters":"","claim_direction":""}`
	c := &client{backend: &fakeCompleter{text: body}}

	got, err := c.ClassifyGoalIntent(context.Background(), classifyInput())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got.Track != TrackExplore {
		t.Fatalf("track = %q, want %q", got.Track, TrackExplore)
	}
	if got.Claim != nil {
		t.Fatalf("explore track must carry no claim, got %+v", got.Claim)
	}
}

// TestClassifyGoalIntentVerify: a verify classification decodes the JSON-encoded
// filter conjunction into typed constraints and maps the claimed direction onto the
// objective direction.
func TestClassifyGoalIntentVerify(t *testing.T) {
	body := `{"track":"verify","rationale":"the goal asserts a specific segment raises the rate",` +
		`"claim_filters":"[{\"field\":\"CryoSleep\",\"op\":\"eq\",\"value\":true}]","claim_direction":"increase"}`
	c := &client{backend: &fakeCompleter{text: body}}

	got, err := c.ClassifyGoalIntent(context.Background(), classifyInput())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got.Track != TrackVerify {
		t.Fatalf("track = %q, want %q", got.Track, TrackVerify)
	}
	if got.Claim == nil {
		t.Fatalf("verify track must carry a claim")
	}
	if got.Claim.Direction != domain.Maximize {
		t.Fatalf("direction = %q, want %q", got.Claim.Direction, domain.Maximize)
	}
	if len(got.Claim.Filters) != 1 {
		t.Fatalf("filters = %+v, want exactly one", got.Claim.Filters)
	}
	f := got.Claim.Filters[0]
	if f.Field != "CryoSleep" || f.Op != domain.Equal || f.Operand == nil || f.Operand.Bool == nil || !*f.Operand.Bool {
		t.Fatalf("filter decoded wrong: %+v", f)
	}
}

// TestClassifyGoalIntentRejectsUnusableOutput pins the deterministic half of the
// free-string fields: the schema bounds neither the claim's shape nor the
// rationale's, so a verify track with an unparseable, empty, or directionless claim
// — and an over-long rationale — must all be errors the caller can fail open on,
// never a claim nothing can test.
func TestClassifyGoalIntentRejectsUnusableOutput(t *testing.T) {
	cases := map[string]string{
		"malformed claim json": `{"track":"verify","rationale":"r","claim_filters":"[{\"field\":","claim_direction":"increase"}`,
		"claim is prose":       `{"track":"verify","rationale":"r","claim_filters":"passengers in cryo-sleep","claim_direction":"increase"}`,
		"empty claim":          `{"track":"verify","rationale":"r","claim_filters":"","claim_direction":"increase"}`,
		"empty claim array":    `{"track":"verify","rationale":"r","claim_filters":"[]","claim_direction":"increase"}`,
		"no direction":         `{"track":"verify","rationale":"r","claim_filters":"[{\"field\":\"CryoSleep\",\"op\":\"eq\",\"value\":true}]","claim_direction":""}`,
		"unknown track":        `{"track":"measure","rationale":"r","claim_filters":"","claim_direction":""}`,
		"rationale too long":   `{"track":"explore","rationale":"` + strings.Repeat("x", maxRationaleLen+1) + `","claim_filters":"","claim_direction":""}`,
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			c := &client{backend: &fakeCompleter{text: body}}
			if _, err := c.ClassifyGoalIntent(context.Background(), classifyInput()); err == nil {
				t.Fatalf("expected an error for %s", name)
			}
		})
	}
}

// TestClassifyGoalIntentGroundsOnColumns: the classifier is shown the introspected
// columns and their value sets, so the claim it extracts can be validated against
// the real schema rather than invented.
func TestClassifyGoalIntentGroundsOnColumns(t *testing.T) {
	fake := &fakeCompleter{text: `{"track":"explore","rationale":"r","claim_filters":"","claim_direction":""}`}
	c := &client{backend: fake}
	in := GoalIntentInput{GoalText: "goal", Schema: SandboxSchema{
		Columns: []SandboxColumn{{Name: "HomePlanet", Type: "VARCHAR", DistinctValues: []string{"Europa", "Mars"}}},
	}}

	if _, err := c.ClassifyGoalIntent(context.Background(), in); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(fake.gotUser, "HomePlanet") || !strings.Contains(fake.gotUser, `"Europa"`) {
		t.Fatalf("prompt is not grounded on the introspected columns: %q", fake.gotUser)
	}
}

// TestClassifyGoalIntentStripsRationaleControlChars pins that a control character in
// the decorative rationale costs the rationale's punctuation and nothing else. The
// caller falls open to explore on any error, so rejecting here would silently discard
// a well-formed claim over how the model chose to lay its justification out.
func TestClassifyGoalIntentStripsRationaleControlChars(t *testing.T) {
	body := `{"track":"verify","rationale":"first\nsecond","claim_filters":"[{\"field\":\"CryoSleep\",\"op\":\"eq\",\"value\":true}]","claim_direction":"increase"}`
	c := &client{backend: &fakeCompleter{text: body}}
	got, err := c.ClassifyGoalIntent(context.Background(), classifyInput())
	if err != nil {
		t.Fatalf("ClassifyGoalIntent: %v", err)
	}
	if got.Rationale != "firstsecond" {
		t.Fatalf("Rationale = %q, want the control character stripped", got.Rationale)
	}
	if got.Claim == nil || len(got.Claim.Filters) != 1 {
		t.Fatalf("Claim = %+v, want the claim preserved alongside the stripped rationale", got.Claim)
	}
}
