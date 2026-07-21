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
