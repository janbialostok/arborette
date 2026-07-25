package llm

import (
	"context"
	"strings"
	"testing"

	"github.com/anthropics/anthropic-sdk-go"

	"github.com/arborette/arborette/internal/domain"
)

func TestExtract(t *testing.T) {
	body := `{"value":"January 1, 2024","confidence":0.9}`
	fake := &fakeMessages{resp: message(t, anthropic.StopReasonEndTurn, textBlockJSON(body))}
	c := &Client{messages: fake, model: "test-model"}

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

	// The request must constrain the output to the extraction schema (which is what
	// makes Citations impossible) and must NOT enable citations.
	if fake.got.OutputConfig.Format.Schema == nil {
		t.Fatal("extraction request did not set a JSON output schema")
	}
	doc := fake.got.Messages[0].Content[0].OfDocument
	if doc == nil {
		t.Fatal("first content block is not a document block")
	}
	if doc.Citations.Enabled.Valid() {
		t.Fatal("extraction must not enable Citations (incompatible with structured outputs)")
	}
	instruction := fake.got.Messages[0].Content[1].OfText.Text
	if !strings.Contains(instruction, field.Name) || !strings.Contains(instruction, "read top to bottom") {
		t.Fatalf("instruction missing field or method: %q", instruction)
	}
}

func TestExtractStopReasonSentinels(t *testing.T) {
	field := domain.TargetField{Name: "term", Description: "the contract term"}
	t.Run("refusal", func(t *testing.T) {
		fake := &fakeMessages{resp: message(t, anthropic.StopReasonRefusal, textBlockJSON(`{"value":"x","confidence":1}`))}
		c := &Client{messages: fake, model: "test-model"}
		if _, _, err := c.Extract(context.Background(), []byte("pdf"), field, ""); err == nil {
			t.Fatal("expected a refusal error")
		}
	})
	t.Run("max tokens", func(t *testing.T) {
		fake := &fakeMessages{resp: message(t, anthropic.StopReasonMaxTokens, textBlockJSON(`{"value":"x","confidence":1}`))}
		c := &Client{messages: fake, model: "test-model"}
		if _, _, err := c.Extract(context.Background(), []byte("pdf"), field, ""); err == nil {
			t.Fatal("expected a max_tokens error")
		}
	})
}

func TestExtractInstructionOmitsApproachWhenNoMethod(t *testing.T) {
	fake := &fakeMessages{resp: message(t, anthropic.StopReasonEndTurn, textBlockJSON(`{"value":"v","confidence":1}`))}
	c := &Client{messages: fake, model: "test-model"}
	if _, _, err := c.Extract(context.Background(), []byte("pdf"), domain.TargetField{Name: "term", Description: "the term"}, ""); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if instr := fake.got.Messages[0].Content[1].OfText.Text; strings.Contains(instr, "Approach:") {
		t.Fatalf("an empty method must omit the Approach line: %q", instr)
	}
}

func TestExtractMalformedBody(t *testing.T) {
	fake := &fakeMessages{resp: message(t, anthropic.StopReasonEndTurn, textBlockJSON("{not json"))}
	c := &Client{messages: fake, model: "test-model"}
	if _, _, err := c.Extract(context.Background(), []byte("pdf"), domain.TargetField{Name: "x"}, ""); err == nil {
		t.Fatal("expected a parse error for a malformed extraction body")
	}
}

func TestIntrospectDocumentFieldsMalformedBody(t *testing.T) {
	fake := &fakeMessages{resp: message(t, anthropic.StopReasonEndTurn, textBlockJSON("{not json"))}
	c := &Client{messages: fake, model: "test-model"}
	if _, err := c.IntrospectDocumentFields(context.Background(), "goal", "sample"); err == nil {
		t.Fatal("expected a parse error for a malformed fields body")
	}
}

func TestIntrospectDocumentFields(t *testing.T) {
	body := `{"fields":[{"name":"effective_date","description":"the effective date"},` +
		`{"name":"termination_date","description":"the termination date"}]}`
	fake := &fakeMessages{resp: message(t, anthropic.StopReasonEndTurn, textBlockJSON(body))}
	c := &Client{messages: fake, model: "test-model"}

	fields, err := c.IntrospectDocumentFields(context.Background(), "extract the key contract dates", "SAMPLE TEXT")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(fields) != 2 || fields[0].Name != "effective_date" || fields[1].Name != "termination_date" {
		t.Fatalf("unexpected fields: %+v", fields)
	}
	if fake.got.OutputConfig.Format.Schema == nil {
		t.Fatal("field-introspection request did not set a JSON output schema")
	}
	if user := fake.got.Messages[0].Content[0].OfText.Text; !strings.Contains(user, "SAMPLE TEXT") {
		t.Fatalf("prompt missing the document sample: %q", user)
	}
}
