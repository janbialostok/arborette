package llm

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"unicode"

	"github.com/arborette/arborette/internal/domain"
)

// The two routing tracks a goal can be classified into, aliased from the domain so
// the classifier validates against the same values intake persists. They are a fixed
// two-value enum, so they stay in the strict output schema.
const (
	TrackExplore = domain.TrackExplore
	TrackVerify  = domain.TrackVerify
)

// The claimed effect directions, in the analyst's own terms rather than the
// objective's: a claim says a segment raises or lowers the outcome, which maps onto
// the domain's maximize/minimize after decoding. The empty value is what an explore
// goal returns, where there is no claim to have a direction.
const (
	claimIncrease = "increase"
	claimDecrease = "decrease"
)

// GoalIntentInput is what the classifier sees: the analyst's goal text and the
// introspected columns of the data source it was registered against. Both are
// untrusted (analyst-written text and dataset-derived names/values), so the caller
// renders them inside a fence.
type GoalIntentInput struct {
	GoalText string
	Schema   SandboxSchema
}

// GoalIntentResult is the classified intent: the track, the model's rationale for
// it, and — on the verify track only — the claim it extracted. Claim is nil on the
// explore track.
type GoalIntentResult struct {
	Track     string
	Rationale string
	Claim     *domain.ClaimSpec
}

// maxRationaleLen bounds the returned rationale. The output schema leaves the field a
// free string, and the rationale is persisted-adjacent analyst-facing text, so an
// overlong one is not a rationale but a payload.
const maxRationaleLen = 2000

var (
	errUnknownTrack = errors.New("goal intent classifier returned an unknown track")
	// Named for no single caller: the message reaches an audit detail, where the
	// wrong stage name sends an operator to the wrong place.
	errRationaleTooLong = fmt.Errorf("rationale exceeds %d bytes", maxRationaleLen)
	errNoClaimFilters   = errors.New("verify track returned no claim filters")
	errUnknownDirection = errors.New("goal intent classifier returned an unusable claimed effect direction")
)

func (c *client) ClassifyGoalIntent(ctx context.Context, goal GoalIntentInput) (GoalIntentResult, error) {
	fence := NewFence("GOAL-INTENT")
	body, err := c.backend.complete(ctx, classifyGoalIntentSystem+fence.Directive(),
		classifyPrompt(fence, goal), classifyGoalIntentSchema())
	if err != nil {
		return GoalIntentResult{}, err
	}
	return decodeGoalIntent(body)
}

type goalIntentWire struct {
	Track     string `json:"track"`
	Rationale string `json:"rationale"`
	// ClaimFilters is a JSON-encoded []domain.Constraint rather than a schema'd
	// array: filter columns and values are a growing, dataset-dependent set, and the
	// constrained-decoding grammar has a low ceiling on exactly that axis. The shape
	// is prompt-grounded and parsed deterministically below.
	ClaimFilters   string `json:"claim_filters"`
	ClaimDirection string `json:"claim_direction"`
}

// decodeGoalIntent parses and validates one classification. Every check here is the
// deterministic half of a free-string field the schema does not bound: an unknown
// track, an unshaped rationale, or a claim the model described in prose rather than
// as a filter conjunction. A verify track with no usable claim is an error rather
// than a claimless verify goal — the caller fails open to explore, which is a
// registered goal, whereas a verify goal carrying no claim is a goal nothing can
// ever test.
func decodeGoalIntent(body string) (GoalIntentResult, error) {
	var wire goalIntentWire
	if err := json.Unmarshal([]byte(body), &wire); err != nil {
		return GoalIntentResult{}, fmt.Errorf("parse goal intent: %w", err)
	}
	if wire.Track != TrackExplore && wire.Track != TrackVerify {
		return GoalIntentResult{}, fmt.Errorf("%w: %q", errUnknownTrack, wire.Track)
	}
	if len(wire.Rationale) > maxRationaleLen {
		return GoalIntentResult{}, errRationaleTooLong
	}
	// The rationale is decorative -- it reaches an audit detail and, on the fall-open
	// path, an analyst-facing reason, and nothing routes on it. Control characters are
	// stripped rather than rejected because rejecting is not proportionate here: the
	// caller falls open to the explore track on any error, so a model that justified a
	// perfectly well-formed claim across two lines would have that claim silently
	// discarded over its punctuation.
	result := GoalIntentResult{Track: wire.Track, Rationale: stripControlChars(wire.Rationale)}
	if wire.Track == TrackExplore {
		return result, nil
	}

	filters, err := decodeClaimFilters(wire.ClaimFilters)
	if err != nil {
		return GoalIntentResult{}, err
	}
	direction, err := claimDirection(wire.ClaimDirection)
	if err != nil {
		return GoalIntentResult{}, err
	}
	result.Claim = &domain.ClaimSpec{Filters: filters, Direction: direction}
	return result, nil
}

// decodeClaimFilters parses the JSON-encoded filter conjunction the claim names.
// It decodes through decodeFilterConjunction, the same polymorphic-value decoder
// the intervention proposals use, so a claimed segment and a proposed one are built
// from one shape — the prompt describes that shape identically for both, and a
// second decoder here would let the two drift apart.
func decodeClaimFilters(raw string) ([]domain.Constraint, error) {
	if strings.TrimSpace(raw) == "" {
		return nil, errNoClaimFilters
	}
	filters, err := decodeFilterConjunction(raw)
	if err != nil {
		return nil, fmt.Errorf("parse claim filters: %w", err)
	}
	if len(filters) == 0 {
		return nil, errNoClaimFilters
	}
	return filters, nil
}

// claimDirection maps the analyst-facing claim direction onto the objective
// direction the rest of the system reasons in.
func claimDirection(direction string) (domain.TargetDirection, error) {
	switch direction {
	case claimIncrease:
		return domain.Maximize, nil
	case claimDecrease:
		return domain.Minimize, nil
	default:
		return "", fmt.Errorf("%w: %q", errUnknownDirection, direction)
	}
}

// stripControlChars removes control runes, keeping the rationale a single line of
// plain text.
func stripControlChars(s string) string {
	if !hasControlChars(s) {
		return s
	}
	return strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return -1
		}
		return r
	}, s)
}

const classifyGoalIntentSystem = "You classify what an analyst wants from a registered optimization goal, given the " +
	"goal text and the columns of the data source it runs against. Return the track \"verify\" when the goal states a " +
	"specific causal claim to test — a named segment of the data the analyst asserts raises or lowers the outcome — " +
	"and \"explore\" when it asks which segments matter without asserting one. On the verify track, additionally " +
	"extract the claim: the filter conjunction that defines the claimed segment, and whether the analyst claims it " +
	"increases or decreases the outcome. Reference ONLY columns present in the provided \"Available columns\" list, " +
	"using each column's exact name as written there, and when a column lists a value set use only values drawn " +
	"verbatim from it — a claim naming a column or value that is not there cannot be constructed and will be " +
	"rejected. On the explore track return an empty claim_filters string and an empty claim_direction." +
	claimShapeGuide

const claimShapeGuide = ` The "claim_filters" field MUST be a JSON string containing a JSON array of filter objects — not a bare array. Each filter is {"field":<column>,"op":<operator>,"value":<value>}, and the array is read as a conjunction (every filter must hold). Choose the operator and value by the column's type: a numeric column uses "lt"/"lte"/"gt"/"gte" with a number; a boolean column uses "eq"/"neq" with true or false; a categorical/string column uses "eq"/"neq" with a string, or "in"/"not_in" with an array of strings. For example, the claim that passengers travelling in cryo-sleep are transported more often is claim_filters "[{\"field\":\"CryoSleep\",\"op\":\"eq\",\"value\":true}]" with claim_direction "increase".`

func classifyPrompt(fence Fence, goal GoalIntentInput) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Analyst goal:\n%s\n\n", fence.Wrap(goal.GoalText))
	fmt.Fprintf(&b, "Available columns:\n%s\n", fence.Wrap(columnSummary(goal.Schema)))
	return b.String()
}
