package datasource_test

import (
	"context"
	"testing"

	"github.com/arborette/arborette/internal/datasource"
)

// stubSource is a compile-checked implementation of the DataSource contract,
// standing in for the concrete file-based and document readers.
type stubSource struct {
	kind   datasource.SourceKind
	schema *datasource.Schema
}

func (s stubSource) Kind() datasource.SourceKind { return s.kind }

func (s stubSource) Introspect(ctx context.Context) (*datasource.Schema, error) {
	return s.schema, nil
}

var _ datasource.DataSource = stubSource{}

func TestTabularIntrospection(t *testing.T) {
	src := stubSource{
		kind:   datasource.KindTabular,
		schema: &datasource.Schema{Kind: datasource.KindTabular, Columns: []datasource.Column{{Name: "amount", Type: "float"}}},
	}
	schema, err := src.Introspect(context.Background())
	if err != nil {
		t.Fatalf("introspect: %v", err)
	}
	if schema.Kind != datasource.KindTabular || len(schema.Columns) != 1 {
		t.Fatalf("unexpected tabular schema: %+v", schema)
	}
}

func TestDocumentIntrospection(t *testing.T) {
	src := stubSource{
		kind:   datasource.KindDocument,
		schema: &datasource.Schema{Kind: datasource.KindDocument, Fields: []datasource.Field{{Name: "effective_date"}}},
	}
	schema, err := src.Introspect(context.Background())
	if err != nil {
		t.Fatalf("introspect: %v", err)
	}
	if schema.Kind != datasource.KindDocument || len(schema.Fields) != 1 {
		t.Fatalf("unexpected document schema: %+v", schema)
	}
}

// TestColumnTypeClassification pins the DuckDB type sets this package owns.
//
// It is a table of every spelling rather than a spot check because these sets are
// the reason the package exists: they had been copied across the services and had
// drifted apart, and the symptom was silent — a column classified as text yields a
// string operand the compiler rejects, so an entire derived vocabulary fails to
// measure while every test stays green. A deliberately exhaustive table is what
// makes the next divergence a test failure.
func TestColumnTypeClassification(t *testing.T) {
	integers := []string{"TINYINT", "SMALLINT", "INTEGER", "BIGINT", "HUGEINT",
		"UTINYINT", "USMALLINT", "UINTEGER", "UBIGINT", "UHUGEINT"}
	for _, kind := range integers {
		if !datasource.IsIntegerType(kind) {
			t.Errorf("IsIntegerType(%q) = false, want true", kind)
		}
		// Every integer kind is numeric too; deriving one set from the other is what
		// keeps that true without repeating the names.
		if !datasource.IsNumericType(kind) {
			t.Errorf("IsNumericType(%q) = false, want true", kind)
		}
	}

	for _, kind := range []string{"FLOAT", "DOUBLE", "DECIMAL(7,2)", "DECIMAL(38,10)"} {
		if !datasource.IsNumericType(kind) {
			t.Errorf("IsNumericType(%q) = false, want true", kind)
		}
		// Floating and fixed point are aggregatable but effectively continuous, so
		// they are never probed for a low-cardinality value set.
		if datasource.IsIntegerType(kind) {
			t.Errorf("IsIntegerType(%q) = true, want false", kind)
		}
	}

	for _, kind := range []string{"BOOLEAN", "BOOL"} {
		if !datasource.IsBooleanType(kind) {
			t.Errorf("IsBooleanType(%q) = false, want true", kind)
		}
		if datasource.IsNumericType(kind) {
			t.Errorf("IsNumericType(%q) = true, want false", kind)
		}
	}

	for _, kind := range []string{"DATE", "TIME", "TIMESTAMP", "TIMESTAMP WITH TIME ZONE"} {
		if !datasource.IsTemporalType(kind) {
			t.Errorf("IsTemporalType(%q) = false, want true", kind)
		}
		if datasource.IsNumericType(kind) {
			t.Errorf("IsNumericType(%q) = true, want false", kind)
		}
	}

	for _, kind := range []string{"VARCHAR", "TEXT", "BLOB", "", "REAL"} {
		if datasource.IsNumericType(kind) || datasource.IsIntegerType(kind) ||
			datasource.IsBooleanType(kind) || datasource.IsTemporalType(kind) {
			t.Errorf("%q must classify as none of the known families", kind)
		}
	}

	// DESCRIBE reports upper-case, but a caller may hold a type from another source;
	// normalizing here is what lets every call site pass its value through unchanged.
	for _, kind := range []string{"integer", "  BigInt  ", "boolean", "decimal(5,2)"} {
		if !datasource.IsNumericType(kind) && !datasource.IsBooleanType(kind) {
			t.Errorf("%q must classify regardless of case and surrounding space", kind)
		}
	}
}
