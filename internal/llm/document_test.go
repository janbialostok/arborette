package llm

import (
	"context"
	"testing"

	"github.com/arborette/arborette/internal/domain"
)

func TestExtract(t *testing.T) {
	body := `{"value":"January 1, 2024","confidence":0.9}`
	c := &client{backend: &fakeCompleter{text: body}}

	field := domain.TargetField{Name: "effective_date", Description: "the contract's effective date"}
	value, confidence, err := c.Extract(context.Background(), []byte("%PDF-1.4 fake bytes"), field, "read top to bottom")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if value != "January 1, 2024" {
		t.Fatalf("value = %q, want the extracted date", value)
	}
	if confidence != 0.9 {
		t.Fatalf("confidence = %v, want 0.9", confidence)
	}
}

func TestExtractStopReasonSentinels(t *testing.T) {
	field := domain.TargetField{Name: "term", Description: "the contract term"}
	t.Run("refusal", func(t *testing.T) {
		c := &client{backend: &fakeCompleter{err: errRefused}}
		if _, _, err := c.Extract(context.Background(), []byte("pdf"), field, ""); err == nil {
			t.Fatal("expected an error")
		}
	})
	t.Run("max tokens", func(t *testing.T) {
		c := &client{backend: &fakeCompleter{err: errMaxTokens}}
		if _, _, err := c.Extract(context.Background(), []byte("pdf"), field, ""); err == nil {
			t.Fatal("expected an error")
		}
	})
}

func TestExtractMalformedBody(t *testing.T) {
	c := &client{backend: &fakeCompleter{text: "{not json"}}
	if _, _, err := c.Extract(context.Background(), []byte("pdf"), domain.TargetField{Name: "x"}, ""); err == nil {
		t.Fatal("expected a parse error for a malformed extraction body")
	}
}

func TestIntrospectDocumentFieldsMalformedBody(t *testing.T) {
	c := &client{backend: &fakeCompleter{text: "{not json"}}
	if _, err := c.IntrospectDocumentFields(context.Background(), "goal", "sample"); err == nil {
		t.Fatal("expected a parse error for a malformed fields body")
	}
}

func TestIntrospectDocumentFields(t *testing.T) {
	body := `{"fields":[{"name":"effective_date","description":"the effective date"},` +
		`{"name":"termination_date","description":"the termination date"}]}`
	c := &client{backend: &fakeCompleter{text: body}}

	fields, err := c.IntrospectDocumentFields(context.Background(), "extract the key contract dates", "SAMPLE TEXT")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(fields) != 2 || fields[0].Name != "effective_date" || fields[1].Name != "termination_date" {
		t.Fatalf("unexpected fields: %+v", fields)
	}
}

func TestExtractInstructionOmitsApproachWhenNoMethod(t *testing.T) {
	body := `{"value":"v","confidence":1}`
	c := &client{backend: &fakeCompleter{text: body}}
	if _, _, err := c.Extract(context.Background(), []byte("pdf"), domain.TargetField{Name: "term", Description: "the term"}, ""); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}
