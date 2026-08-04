package domain

import "testing"

// TestClaimGroundingError pins the composition one shared builder guarantees: intake and the Verifier both write their answer to one analyst-facing
// field, so both must produce the same sentence for the same failure — and neither
// may emit the LLM repair-loop wording ("re-propose…") that a human would read as an
// instruction to themselves.
func TestClaimGroundingError(t *testing.T) {
	columns := []string{"revenue", "tier"}
	values := map[string][]string{"tier": {"gold", "silver"}}

	cases := map[string]struct {
		filters []Constraint
		want    string
	}{
		"grounded": {
			[]Constraint{{Field: "tier", Op: Equal, Operand: &LiteralValue{String: ptr("gold")}}},
			"",
		},
		"unknown column": {
			[]Constraint{{Field: "region", Op: Equal, Operand: &LiteralValue{String: ptr("gold")}}},
			"the claim names filter columns that are not in the data source: region",
		},
		"unknown value": {
			[]Constraint{{Field: "tier", Op: Equal, Operand: &LiteralValue{String: ptr("platinum")}}},
			"the claim names filter values that are not present in their column: tier=platinum",
		},
		"both": {
			[]Constraint{
				{Field: "region", Op: Equal, Operand: &LiteralValue{String: ptr("gold")}},
				{Field: "tier", Op: Equal, Operand: &LiteralValue{String: ptr("platinum")}},
			},
			"the claim names filter columns that are not in the data source: region; " +
				"the claim names filter values that are not present in their column: tier=platinum",
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			if got := ClaimGroundingError(tc.filters, columns, values); got != tc.want {
				t.Fatalf("ClaimGroundingError = %q, want %q", got, tc.want)
			}
		})
	}
}
