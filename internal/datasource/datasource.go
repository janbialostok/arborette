// Package datasource defines the minimal, stable contract concrete data sources
// implement. It ships only the interface and Schema types; concrete readers must
// extend this seam additively -- new methods go on a companion interface rather
// than changing this contract, so existing consumers keep compiling.
package datasource

import "context"

// SourceKind discriminates the shape a source introspects to.
type SourceKind string

const (
	KindTabular  SourceKind = "tabular"
	KindDocument SourceKind = "document"
)

// Column is a tabular column discovered by introspection.
type Column struct {
	Name string
	Type string
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
