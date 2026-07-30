package llm

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/arborette/arborette/internal/domain"
)

// MacroSegment is one measured conjunction the Sleep-Cycle search surfaced: the
// objective it was measured against, the conjoined filter that defines it, and
// the values that make it materially better than the unfiltered baseline.
type MacroSegment struct {
	ObjectiveLabel string
	Direction      domain.TargetDirection
	Filters        []domain.Constraint
	Baseline       float64
	Value          float64
}

// OntologyTerm maps one concrete, dataset-bound variable to the universal
// structural term the abstraction replaced it with.
type OntologyTerm struct {
	Concrete    string
	Ontological string
}

// Abstraction is a generalized Meta-Heuristic: a definition written purely in
// bracketed ontology terms, plus the mapping that produced them.
type Abstraction struct {
	Definition    string
	OntologyTerms []OntologyTerm
}

// LeakedConcreteTerms returns the column names that survive verbatim in a
// definition, deduplicated in first-seen order. It matches case-insensitively,
// mirroring domain.UnknownFilterColumns and the sandbox compiler's own column
// resolution. The output schema leaves the definition a free string, so this is
// the deterministic check that a heuristic really was generalized.
//
// Matching is on word boundaries, not raw substrings. A bare substring test
// fails closed and silently: short column names are common English fragments —
// Age inside "average", Spa inside "space", Name inside "namely" — so a properly
// abstracted definition would be rejected, and since an exhausted repair drops
// the macro-segment, the run would produce no heuristic at all.
func LeakedConcreteTerms(def string, columns []string) []string {
	haystack := strings.ToLower(def)
	seen := map[string]bool{}
	var leaked []string
	for _, c := range columns {
		if c == "" {
			continue
		}
		key := strings.ToLower(c)
		if seen[key] || !containsWord(haystack, key) {
			continue
		}
		seen[key] = true
		leaked = append(leaked, c)
	}
	return leaked
}

// containsWord reports whether needle appears in haystack as a whole word. Both
// arguments must already be lower-cased.
//
// A boundary is only required on an edge where the needle's own edge rune is a
// word rune — the same rule \b encodes. Demanding a boundary unconditionally
// would fail open for the column names a CSV header can legally carry: "% Change"
// ends in "e" but starts with "%", so a preceding digit ("a 5% Change") would
// suppress a genuine leak. Requiring it where the needle's edge IS a word rune
// is what keeps "Age" out of "average".
func containsWord(haystack, needle string) bool {
	if needle == "" {
		return false
	}
	headAnchored := isWordRune(firstRune(needle))
	tailAnchored := isWordRune(lastRune(needle))
	for offset := 0; offset <= len(haystack)-len(needle); {
		i := strings.Index(haystack[offset:], needle)
		if i < 0 {
			return false
		}
		start := offset + i
		end := start + len(needle)
		headOK := !headAnchored || !isWordRune(precedingRune(haystack, start))
		tailOK := !tailAnchored || !isWordRune(followingRune(haystack, end))
		if headOK && tailOK {
			return true
		}
		offset = start + 1
	}
	return false
}

func firstRune(s string) rune {
	r, _ := utf8.DecodeRuneInString(s)
	return r
}

func lastRune(s string) rune {
	r, _ := utf8.DecodeLastRuneInString(s)
	return r
}

func precedingRune(s string, i int) rune {
	if i <= 0 {
		return 0
	}
	r, _ := utf8.DecodeLastRuneInString(s[:i])
	return r
}

func followingRune(s string, i int) rune {
	if i >= len(s) {
		return 0
	}
	r, _ := utf8.DecodeRuneInString(s[i:])
	return r
}

// isWordRune matches \w: underscore counts. Leaving it out would make the
// boundary check fire inside snake_case, so a column named "output" would read
// as leaked out of "[Primary_Output]" -- a false leak, which fails closed and
// drops the macro-segment entirely.
func isWordRune(r rune) bool {
	return r == '_' || unicode.IsLetter(r) || unicode.IsDigit(r)
}

var errEmptyDefinition = errors.New("meta-heuristic definition is empty")

// metaHeuristicWire mirrors the abstraction structured-output body.
type metaHeuristicWire struct {
	Definition    string `json:"definition"`
	OntologyTerms []struct {
		Concrete    string `json:"concrete"`
		Ontological string `json:"ontological"`
	} `json:"ontology_terms"`
}

// decodeAbstraction parses an abstraction body. A malformed body is a generation
// error, mapped by the caller like any other parse failure.
//
// An empty definition is rejected here because nothing downstream would catch
// it: the schema sets no minimum length, and the leak check passes vacuously on
// an empty string. It would be persisted, embedded, and served — permanently, as
// the node id is deterministic and the completed embedding makes every later run
// skip the segment before it reaches Claude again.
func decodeAbstraction(body string) (Abstraction, error) {
	var wire metaHeuristicWire
	if err := json.Unmarshal([]byte(body), &wire); err != nil {
		return Abstraction{}, fmt.Errorf("parse meta-heuristic: %w", err)
	}
	if strings.TrimSpace(wire.Definition) == "" {
		return Abstraction{}, errEmptyDefinition
	}
	terms := make([]OntologyTerm, 0, len(wire.OntologyTerms))
	for _, t := range wire.OntologyTerms {
		terms = append(terms, OntologyTerm{Concrete: t.Concrete, Ontological: t.Ontological})
	}
	return Abstraction{Definition: wire.Definition, OntologyTerms: terms}, nil
}

// macroSegmentPrompt renders the measured macro-segment for abstraction. Filters
// are rendered with domain.RenderConstraint so the prompt and the graph describe
// the same segment in the same spelling.
func macroSegmentPrompt(goalText string, seg MacroSegment) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Analyst goal:\n%s\n\n", goalText)
	fmt.Fprintf(&b, "Objective: %s, to %s.\n", seg.ObjectiveLabel, seg.Direction)
	fmt.Fprintf(&b, "Unfiltered baseline: %v\n", seg.Baseline)
	fmt.Fprintf(&b, "Measured value for this segment: %v\n\n", seg.Value)
	fmt.Fprintf(&b, "The segment is the conjunction of these predicates:\n%s\n", filterSummary(seg.Filters))
	b.WriteString("\nAbstract this finding into one generalized, reusable heuristic.\n")
	return b.String()
}

const metaHeuristicSystem = "You abstract an empirically measured data segment into a generalized, reusable " +
	"heuristic that is portable across datasets. Strip every domain-specific payload — column names, category " +
	"values, dataset jargon — and map each concrete variable to a universal, high-level STRUCTURAL ontology term " +
	"written in square brackets. For example HomePlanet = 'Mars' becomes [Primary Population Center], and a " +
	"Transport Rate objective becomes [System Output]. Return the mapping you used in ontology_terms, and write " +
	"the definition referencing ONLY bracketed ontology terms — a definition containing any raw column name or " +
	"dataset value has not been abstracted and will be rejected. State the relationship the measurement supports, " +
	"not the specific numbers."

const metaHeuristicRepairSystem = "You abstract an empirically measured data segment into a generalized, reusable " +
	"heuristic. A previous definition failed validation because it still contained concrete, dataset-bound terms. " +
	"Given that definition and the exact terms that leaked, rewrite it so every variable is a universal, " +
	"high-level STRUCTURAL ontology term in square brackets (e.g. [Primary Population Center], [System Output]) " +
	"and no raw column name or dataset value survives anywhere in the definition."
