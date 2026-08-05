package domain

import "strings"

// The routing tracks a registered goal can take: explore populates the graph
// observationally, verify additionally tests the claim the intake classifier
// extracted from the goal text.
//
// They live here because three packages must agree on the spelling and two of them
// compare across the boundary -- the classifier validates its own output against
// these values and intake persists them. Two independent declarations would let
// either side's literal change silently route every verify-track goal to explore,
// with no compile error and no test failure at the seam.
const (
	TrackExplore = "explore"
	TrackVerify  = "verify"
)

// The Verifier dispatch kinds, spelled as the serve surface decodes them. They cross
// the same boundary as the tracks and carry the same hazard: the orchestrator builds
// the launcher args and the Verifier routes on them, so two independent spellings
// would answer 422 "unknown kind" on every dispatch with nothing at compile time to
// catch it -- and the cross-service seam test hand-writes its body, so it pins the
// wire shape without pinning these literals.
const (
	DispatchVerify    = "verify"
	DispatchClaim     = "claim"
	DispatchDiscovery = "discovery"
)

// ClaimConstructionFailed is the event type both services report a cannot-construct
// claim under. It is not decoration: the orchestrator's Verifier-event handler
// switches on it to persist the analyst-facing reason on the goal row, so a spelling
// that drifted on the Verifier side would silently stop that durable write with no
// error anywhere.
const ClaimConstructionFailed = "claim_construction_failed"

// ClaimSpec is the causal claim intake extracted from a verify-track goal: the
// segment the analyst named, as a filter conjunction, plus the direction of the
// effect they claimed it has on the objective.
//
// It is a persisted wire contract, not just an in-process value: intake stores its
// JSON on the goal row and the Verifier decodes that same document when it
// constructs and measures the claim, so the claim verified is exactly the claim
// intake validated. The json tags are therefore load-bearing.
type ClaimSpec struct {
	Filters   []Constraint    `json:"filters"`
	Direction TargetDirection `json:"direction"`
}

// ClaimGroundingError reports, in analyst-facing words, why a claim's filters do not
// ground in a data source's schema — or "" when they do.
//
// It lives here, beside the checks it runs, because two services write their answer
// to the same analyst-facing field: intake validates the claim at registration and
// the Verifier re-validates it at dispatch. Wording them separately let the same
// failure read two different ways, and let the intake path persist a message written
// for the LLM repair loop ("re-propose using only columns from…") into a field a
// human reads.
func ClaimGroundingError(filters []Constraint, columns []string, values map[string][]string) string {
	var parts []string
	if unknown := UnknownFilterColumns(filters, columns); len(unknown) > 0 {
		parts = append(parts, "the claim names filter columns that are not in the data source: "+strings.Join(unknown, ", "))
	}
	if unknown := UnknownFilterValues(filters, values); len(unknown) > 0 {
		parts = append(parts, "the claim names filter values that are not present in their column: "+strings.Join(unknown, ", "))
	}
	return strings.Join(parts, "; ")
}
