package llm

import (
	"context"
	"encoding/base64"
	"fmt"

	"github.com/anthropics/anthropic-sdk-go"
	"github.com/anthropics/anthropic-sdk-go/option"
)

// anthropicCompleter implements completer via the Anthropic SDK.
type anthropicCompleter struct {
	messages messagesClient
	model    anthropic.Model
}

// messagesClient narrows the SDK to the one method we call (test fakes inject here).
type messagesClient interface {
	New(ctx context.Context, params anthropic.MessageNewParams, opts ...option.RequestOption) (*anthropic.Message, error)
}

func newAnthropicCompleter(apiKey, model string) (*anthropicCompleter, error) {
	sdk := anthropic.NewClient(option.WithAPIKey(apiKey))
	return &anthropicCompleter{messages: &sdk.Messages, model: anthropic.Model(model)}, nil
}

func (a *anthropicCompleter) complete(ctx context.Context, system, user string, schema map[string]any) (string, error) {
	return a.completeWithContent(ctx, system, []anthropic.ContentBlockParamUnion{anthropic.NewTextBlock(user)}, schema)
}

func (a *anthropicCompleter) completeWithPDF(ctx context.Context, system string, pdf []byte, instruction string, schema map[string]any) (string, error) {
	doc := anthropic.NewDocumentBlock(anthropic.Base64PDFSourceParam{
		Data: base64.StdEncoding.EncodeToString(pdf),
	})
	return a.completeWithContent(ctx, system,
		[]anthropic.ContentBlockParamUnion{doc, anthropic.NewTextBlock(instruction)}, schema)
}

func (a *anthropicCompleter) completeWithContent(ctx context.Context, system string, content []anthropic.ContentBlockParamUnion, schema map[string]any) (string, error) {
	adaptive := anthropic.ThinkingConfigAdaptiveParam{}
	resp, err := a.messages.New(ctx, anthropic.MessageNewParams{
		Model:     a.model,
		MaxTokens: maxTokens,
		Thinking:  anthropic.ThinkingConfigParamUnion{OfAdaptive: &adaptive},
		OutputConfig: anthropic.OutputConfigParam{
			Effort: anthropic.OutputConfigEffortHigh,
			Format: anthropic.JSONOutputFormatParam{Schema: schema},
		},
		System: []anthropic.TextBlockParam{{Text: system}},
		Messages: []anthropic.MessageParam{
			anthropic.NewUserMessage(content...),
		},
	})
	if err != nil {
		return "", fmt.Errorf("claude request: %w", err)
	}
	switch resp.StopReason {
	case anthropic.StopReasonRefusal:
		return "", errRefused
	case anthropic.StopReasonMaxTokens:
		return "", errMaxTokens
	}
	for _, block := range resp.Content {
		if tb, ok := block.AsAny().(anthropic.TextBlock); ok {
			return tb.Text, nil
		}
	}
	return "", errNoText
}
