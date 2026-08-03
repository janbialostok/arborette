package domain

import (
	"encoding/json"
	"fmt"
)

// DecodeConstraints converts a graph node property back into typed constraints. It
// is mandatory rather than defensive: a filter set (new_filters, effective_filters)
// is stored as a typed []Constraint, but the graph layer marshals the whole property
// map to a JSON string and decodes it back into map[string]any -- so the value
// arrives as []any of map[string]any with nested operand/members objects. A direct
// type assertion fails at runtime and silently yields an empty set. It is a pure
// domain operation shared by the Sleep-Cycle search and the Verifier's adjustment
// stage, which both reconstruct a segment's filter conjunction from stored
// intervention properties.
func DecodeConstraints(v any) ([]Constraint, error) {
	if v == nil {
		return nil, nil
	}
	b, err := json.Marshal(v)
	if err != nil {
		return nil, fmt.Errorf("re-encode filters: %w", err)
	}
	var filters []Constraint
	if err := json.Unmarshal(b, &filters); err != nil {
		return nil, fmt.Errorf("decode filters: %w", err)
	}
	return filters, nil
}
