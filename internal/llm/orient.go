package llm

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
)

// Orientation decision values. The statistics own edge existence; Claude may only
// orient an edge it produced or abstain — it can never add or delete one. The three
// values are a small fixed enum, so they stay in the strict output schema (the
// grammar-ceiling axes — enum cardinality and recursion — are both kept shallow).
const (
	OrientFirstCausesSecond = "first_causes_second"
	OrientSecondCausesFirst = "second_causes_first"
	OrientAbstain           = "abstain"
)

// ColumnSemantics describes one column for the orientation prompt: its name, data
// type, and a few sample values, the domain prior Claude uses to decide direction.
// All three fields are dataset-derived untrusted text, so the caller renders them
// inside a fence.
type ColumnSemantics struct {
	Name    string
	Type    string
	Samples []string
}

// OrientEdge is one still-unoriented edge presented for a direction decision. ID is
// a stable identifier the model must echo back (used to match decisions to edges
// deterministically); First and Second are the two column names.
type OrientEdge struct {
	ID     string
	First  string
	Second string
}

// OrientDecision is the model's choice for one edge: orient toward one endpoint or
// abstain, with a self-reported confidence in [0,1] the caller carries onto an
// llm_prior edge.
type OrientDecision struct {
	EdgeID     string
	Decision   string
	Confidence float64
}

func (c *client) OrientCausalEdges(ctx context.Context, goalText string, columns []ColumnSemantics, edges []OrientEdge) ([]OrientDecision, error) {
	fence := NewFence("COLUMN-CONTEXT")
	body, err := c.backend.complete(ctx, orientCausalEdgesSystem+fence.Directive(),
		orientPrompt(fence, goalText, columns, edges), orientCausalEdgesSchema())
	if err != nil {
		return nil, err
	}
	return decodeOrientDecisions(body)
}

func (c *client) RepairOrientCausalEdges(ctx context.Context, goalText string, columns []ColumnSemantics, edges []OrientEdge, prior []OrientDecision, validationErr string) ([]OrientDecision, error) {
	fence := NewFence("COLUMN-CONTEXT")
	user := orientPrompt(fence, goalText, columns, edges) +
		"\nYour previous decisions were rejected:\n" + fence.Wrap(renderOrientDecisions(prior)) +
		"\n\nThe rejection:\n" + fence.Wrap(validationErr) +
		"\n\nReturn corrected decisions that reference only the edge ids listed above, each still unoriented."
	body, err := c.backend.complete(ctx, orientCausalEdgesRepairSystem+fence.Directive(), user, orientCausalEdgesSchema())
	if err != nil {
		return nil, err
	}
	return decodeOrientDecisions(body)
}

type orientWire struct {
	Decisions []struct {
		EdgeID     string  `json:"edge_id"`
		Decision   string  `json:"decision"`
		Confidence float64 `json:"confidence"`
	} `json:"decisions"`
}

func decodeOrientDecisions(body string) ([]OrientDecision, error) {
	var wire orientWire
	if err := json.Unmarshal([]byte(body), &wire); err != nil {
		return nil, fmt.Errorf("parse orientation decisions: %w", err)
	}
	out := make([]OrientDecision, 0, len(wire.Decisions))
	for _, d := range wire.Decisions {
		out = append(out, OrientDecision{EdgeID: d.EdgeID, Decision: d.Decision, Confidence: d.Confidence})
	}
	return out, nil
}

const orientCausalEdgesSystem = "You orient edges in a causal graph discovered from data. Conditional-independence " +
	"tests already decided which edges exist; your only job is to choose, for each listed edge, whether the first " +
	"column causes the second, the second causes the first, or to abstain when the column semantics give no basis " +
	"to decide. Use the column names, types, and sample values as a domain prior — for example, a demographic or " +
	"time-of-registration column typically causes a downstream outcome, not the reverse. You may only orient or " +
	"abstain on the edges given; you may never add or remove an edge. Return one decision per listed edge id, and " +
	"reference only those ids."

const orientCausalEdgesRepairSystem = "You orient edges in a causal graph discovered from data. A previous set of " +
	"decisions was rejected — most likely it referenced an edge id that was not listed or was already oriented. " +
	"Given the still-unoriented edge list and the rejection reason, return corrected decisions that reference only " +
	"the listed edge ids, orienting each toward one endpoint or abstaining. You may never add or remove an edge."

func orientPrompt(fence Fence, goalText string, columns []ColumnSemantics, edges []OrientEdge) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Analyst goal:\n%s\n\n", fence.Wrap(goalText))
	fmt.Fprintf(&b, "Column semantics:\n%s\n\n", fence.Wrap(renderColumnSemantics(columns)))
	fmt.Fprintf(&b, "Edges to orient (one decision per id):\n%s\n", fence.Wrap(renderOrientEdges(edges)))
	return b.String()
}

func renderColumnSemantics(columns []ColumnSemantics) string {
	var b strings.Builder
	for _, c := range columns {
		fmt.Fprintf(&b, "- %s (%s)", c.Name, c.Type)
		if len(c.Samples) > 0 {
			fmt.Fprintf(&b, " samples: %s", quotedValues(c.Samples))
		}
		b.WriteString("\n")
	}
	return b.String()
}

func renderOrientEdges(edges []OrientEdge) string {
	var b strings.Builder
	for _, e := range edges {
		fmt.Fprintf(&b, "- id %s: %s <-> %s\n", e.ID, e.First, e.Second)
	}
	return b.String()
}

func renderOrientDecisions(decisions []OrientDecision) string {
	var b strings.Builder
	for _, d := range decisions {
		fmt.Fprintf(&b, "- id %s: %s (confidence %.2f)\n", d.EdgeID, d.Decision, d.Confidence)
	}
	return b.String()
}
