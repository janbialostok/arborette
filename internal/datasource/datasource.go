// Package datasource defines the minimal, stable contract concrete data sources
// implement, plus the coarse typing of a DuckDB column. It ships no reader of its
// own; concrete readers must extend the seam additively -- new methods go on a
// companion interface rather than changing this contract, so existing consumers
// keep compiling.
//
// The typing lives here because several services must agree on it. The type names
// are the engine's, but the services that reason about them (the objective
// compiler, the causal sweep's variable model, the Sleep-Cycle vocabulary, the
// orchestrator's window-binding check) build without CGO and so cannot reach the
// compiler that speaks to the engine. This package is dependency-free and CGO-free,
// so it is the one place the set can live without being copied across that
// boundary.
package datasource

import (
	"context"
	"strings"
)

// SourceKind discriminates the shape a source introspects to.
type SourceKind string

const (
	KindTabular  SourceKind = "tabular"
	KindDocument SourceKind = "document"
)

// Column is a tabular column discovered by introspection. DistinctValues carries
// the column's distinct value set when it is a low-cardinality categorical column
// (text/boolean/integer at or under the introspection cap); it is nil for a
// high-cardinality or continuous column, or when the source did not probe values.
// A nil is never a partially-populated set: the probe fills all of a column's
// values or none, so a consumer treats nil as "unknown, do not ground on it".
//
// QuantileCuts carries a numeric column's interior quantile cut points when the
// caller asked for them, and is nil otherwise — for a non-numeric column, for a
// caller that did not ask, and for a constant or empty column whose quantiles came
// back NULL. It is the threshold analogue of DistinctValues: the value set a
// consumer may derive predicates on without inventing boundaries the data does not
// support.
type Column struct {
	Name           string
	Type           string
	DistinctValues []string
	QuantileCuts   []float64
}

// Field is a document candidate-extractable field discovered by introspection
// (e.g. inferred from goal text plus a sample pass over the document).
type Field struct {
	Name        string
	Description string
}

// Schema represents both tabular columns and document fields so the
// introspection contract is shape-stable across future implementations. A
// tabular source populates Columns; a document source populates Fields.
type Schema struct {
	Kind    SourceKind
	Columns []Column
	Fields  []Field
}

// DataSource is the pluggable seam. Kind reports the source shape; Introspect
// returns the schema relevant to that shape.
type DataSource interface {
	Kind() SourceKind
	Introspect(ctx context.Context) (*Schema, error)
}

// integerTypes is the DuckDB integer set, and the only type set spelled out here.
// The numeric set is derived from it rather than repeated, because the two differ
// by exactly the floating-point and fixed-point kinds — writing that relationship
// as code is what stops a future integer width from being added to one and missed
// in the other.
//
// Distinct-value probing is restricted to these plus text and boolean columns, so
// a FLOAT/DOUBLE/DECIMAL column — effectively continuous, never under a
// low-cardinality cap — is not probed.
var integerTypes = map[string]bool{
	"TINYINT":   true,
	"SMALLINT":  true,
	"INTEGER":   true,
	"BIGINT":    true,
	"HUGEINT":   true,
	"UTINYINT":  true,
	"USMALLINT": true,
	"UINTEGER":  true,
	"UBIGINT":   true,
	"UHUGEINT":  true,
}

// IsNumericType reports whether a DuckDB column type is aggregatable as a number:
// every integer kind, the two floating-point kinds, and any DECIMAL(p,s). Fixed
// point is matched by prefix because DESCRIBE reports it with its precision and
// scale (common in Parquet), so an exact-string match would spuriously reject it.
func IsNumericType(t string) bool {
	u := normalizeType(t)
	return integerTypes[u] || u == "FLOAT" || u == "DOUBLE" || strings.HasPrefix(u, "DECIMAL")
}

// IsIntegerType reports whether a DuckDB column type is an integer kind.
func IsIntegerType(t string) bool { return integerTypes[normalizeType(t)] }

// IsBooleanType reports whether a DuckDB column type is boolean.
func IsBooleanType(t string) bool {
	u := normalizeType(t)
	return u == "BOOLEAN" || u == "BOOL"
}

// IsTemporalType reports whether a DuckDB column type is a date or time kind. It is
// matched by prefix because DESCRIBE spells the family several ways (DATE,
// TIME, TIMESTAMP, TIMESTAMP WITH TIME ZONE).
func IsTemporalType(t string) bool {
	u := normalizeType(t)
	return strings.HasPrefix(u, "DATE") || strings.HasPrefix(u, "TIME")
}

func normalizeType(t string) string { return strings.ToUpper(strings.TrimSpace(t)) }
