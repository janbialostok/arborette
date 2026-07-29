package llm

import (
	"context"
	"errors"
	"fmt"

	"github.com/anthropics/anthropic-sdk-go"
	"github.com/anthropics/anthropic-sdk-go/option"
)

// chatMaxTokens bounds each streamed chat turn. It must be non-zero: MaxTokens is
// always marshaled, and the API reads a zero as "warm the prompt cache, generate
// nothing" -- a silently empty chat rather than an error. Sized with headroom
// because the budget covers the whole turn, tool results included.
const chatMaxTokens = 16000

// maxChatResumes bounds how many times one browser message is resubmitted after
// the API pauses a long-running connector turn. Each resume is a fresh streamed
// request, so this caps the work a single message can trigger; exhausting it
// ends the stream with an error rather than a truncated answer.
const maxChatResumes = 3

// chatServerName names the MCP server in the connector definition. The
// mcp_toolset tool entry references the server by this name, so the two must
// agree or the request is rejected.
const chatServerName = "arborette"

// chatAllowedTools scopes the agent to the read-only tools, enforced server-side
// by the connector. Goal submission is deliberately excluded: it is a write with
// no confirmation affordance on a chat surface. The names are the MCP server's,
// duplicated here rather than imported so this package stays independent of it;
// a test pins the two lists against each other.
var chatAllowedTools = []string{"get_optimized_heuristics", "trace_causal_chain"}

// ErrChatDisabled reports that the client has no usable connector target, so no
// request was made. The caller maps it to an explicit "not configured" response
// rather than a confusing connector failure.
var ErrChatDisabled = errors.New("agent chat is not configured")

// The event types a chat stream emits. Every stream ends with exactly one
// terminal event (ChatEventDone or ChatEventError), whatever the stop reason.
const (
	ChatEventText       = "text"
	ChatEventToolUse    = "tool_use"
	ChatEventToolResult = "tool_result"
	ChatEventDone       = "done"
	ChatEventError      = "error"
)

// The roles a transcript turn may carry. Role is cast straight through to the
// API, so an invalid value surfaces only as an opaque rejection.
const (
	RoleUser      = "user"
	RoleAssistant = "assistant"
)

// ChatMessage is one turn of the transcript. Role is RoleUser or RoleAssistant;
// the caller validates it.
type ChatMessage struct {
	Role    string
	Content string
}

// ChatEvent is one item of a streamed chat turn. Which fields carry meaning
// depends on Type: Text on ChatEventText, Tool on ChatEventToolUse, IsError on
// ChatEventToolResult, and Message on ChatEventError.
type ChatEvent struct {
	Type    string
	Text    string
	Tool    string
	IsError bool
	Message string
}

// ChatClient runs the analyst-facing agent: a streamed Claude conversation
// scoped to arborette's read-only MCP tools, reached through Claude's native MCP
// connector. It is distinct from Client because the connector lives only on the
// beta Messages API and the surface is streaming rather than structured output.
type ChatClient struct {
	messages   *anthropic.BetaMessageService
	model      anthropic.Model
	mcpURL     string
	mcpToken   string
	configured bool
}

// NewChatClient builds a chat client from primitive settings (infra-constructor
// convention).
//
// An unconfigured client is a real value whose Chat returns ErrChatDisabled,
// never a nil collaborator. The token is part of that predicate: the MCP server
// serves unauthenticated when no token is set, and pointing the connector at
// that endpoint would let a working preview mask missing auth on a public
// listener.
func NewChatClient(apiKey, model, mcpURL, mcpToken string) *ChatClient {
	sdk := anthropic.NewClient(option.WithAPIKey(apiKey))
	return &ChatClient{
		messages:   &sdk.Beta.Messages,
		model:      anthropic.Model(model),
		mcpURL:     mcpURL,
		mcpToken:   mcpToken,
		configured: apiKey != "" && mcpURL != "" && mcpToken != "",
	}
}

// Chat streams one assistant turn for the transcript, invoking emit for each
// event as it arrives. It returns ErrChatDisabled before contacting the API when
// unconfigured, an error if the stream or emit fails, and otherwise nil after
// emitting exactly one terminal event. A paused connector turn is resubmitted
// per the API's contract rather than reported as a finished answer.
func (c *ChatClient) Chat(ctx context.Context, system string, msgs []ChatMessage, emit func(ChatEvent) error) error {
	if !c.configured {
		return ErrChatDisabled
	}
	// Set explicitly: several models think adaptively when the field is omitted,
	// and their thinking blocks stream as empty text -- a visible hang on an
	// interactive surface, drawing on the same budget as the answer.
	disabled := anthropic.BetaThinkingConfigDisabledParam{}
	params := anthropic.BetaMessageNewParams{
		Model:     c.model,
		MaxTokens: chatMaxTokens,
		Betas:     []anthropic.AnthropicBeta{anthropic.AnthropicBetaMCPClient2025_11_20},
		Thinking:  anthropic.BetaThinkingConfigParamUnion{OfDisabled: &disabled},
		System:    []anthropic.BetaTextBlockParam{{Text: system}},
		Messages:  toBetaMessages(msgs),
		MCPServers: []anthropic.BetaRequestMCPServerURLDefinitionParam{{
			Name:               chatServerName,
			URL:                c.mcpURL,
			AuthorizationToken: anthropic.String(c.mcpToken),
			ToolConfiguration: anthropic.BetaRequestMCPServerToolConfigurationParam{
				AllowedTools: chatAllowedTools,
			},
		}},
		Tools: []anthropic.BetaToolUnionParam{anthropic.BetaToolUnionParamOfMCPToolset(chatServerName)},
	}

	turns := len(params.Messages)
	for resumes := 0; ; resumes++ {
		stop, assistant, err := c.streamTurn(ctx, params, emit)
		if err != nil {
			return err
		}
		if stop == anthropic.BetaStopReasonPauseTurn && resumes < maxChatResumes {
			// Replace the paused partial rather than appending another turn: the
			// API's contract is to resubmit the transcript plus the partial
			// assistant turn, and it warns against accumulating one message per
			// pause. Truncating back to the original transcript first keeps the
			// second and third resumes the same shape as the first.
			params.Messages = append(params.Messages[:turns], assistant.ToParam())
			continue
		}
		return emit(terminalEvent(stop))
	}
}

// streamTurn runs one streamed request, translating the SDK's stream events into
// ChatEvents and returning the turn's stop reason plus the accumulated assistant
// message (which a paused turn is resubmitted with). The stop reason arrives only
// on message_delta, so it is captured there rather than inferred from the end of
// the stream.
func (c *ChatClient) streamTurn(ctx context.Context, params anthropic.BetaMessageNewParams, emit func(ChatEvent) error) (anthropic.BetaStopReason, anthropic.BetaMessage, error) {
	stream := c.messages.NewStreaming(ctx, params)
	defer stream.Close()

	var (
		assistant anthropic.BetaMessage
		stop      anthropic.BetaStopReason
	)
	for stream.Next() {
		event := stream.Current()
		if err := assistant.Accumulate(event); err != nil {
			return "", assistant, fmt.Errorf("accumulate chat stream: %w", err)
		}
		switch event := event.AsAny().(type) {
		case anthropic.BetaRawContentBlockDeltaEvent:
			delta, ok := event.Delta.AsAny().(anthropic.BetaTextDelta)
			if !ok {
				continue
			}
			if err := emit(ChatEvent{Type: ChatEventText, Text: delta.Text}); err != nil {
				return "", assistant, err
			}
		case anthropic.BetaRawContentBlockStartEvent:
			ev, ok := toolEvent(event.ContentBlock)
			if !ok {
				continue
			}
			if err := emit(ev); err != nil {
				return "", assistant, err
			}
		case anthropic.BetaRawMessageDeltaEvent:
			stop = event.Delta.StopReason
		}
	}
	if err := stream.Err(); err != nil {
		return "", assistant, fmt.Errorf("claude chat stream: %w", err)
	}
	return stop, assistant, nil
}

// toolEvent maps the connector's tool blocks to the affordances the chat surface
// renders. Every other block type (text, thinking, and anything the API adds
// later) is not an event of its own.
func toolEvent(block anthropic.BetaRawContentBlockStartEventContentBlockUnion) (ChatEvent, bool) {
	switch block := block.AsAny().(type) {
	case anthropic.BetaMCPToolUseBlock:
		return ChatEvent{Type: ChatEventToolUse, Tool: block.Name}, true
	case anthropic.BetaMCPToolResultBlock:
		return ChatEvent{Type: ChatEventToolResult, IsError: block.IsError}, true
	default:
		return ChatEvent{}, false
	}
}

// terminalEvent maps a turn's stop reason to the one event that ends the stream.
// Every reason that leaves the answer missing or cut short is an error --
// reporting one as done would present an incomplete answer as a complete one,
// and the streamed prefix is exactly why that matters: the terminal event is the
// only signal the client has about whether what it rendered is the whole answer.
// Every other reason, including ones the API adds later, is done by default, so a
// new stop reason can never leave a client waiting on a frame that never arrives.
func terminalEvent(stop anthropic.BetaStopReason) ChatEvent {
	switch stop {
	case anthropic.BetaStopReasonRefusal:
		return ChatEvent{Type: ChatEventError, Message: "the agent declined to answer"}
	case anthropic.BetaStopReasonPauseTurn:
		return ChatEvent{Type: ChatEventError, Message: "the agent stopped before finishing; ask again to continue"}
	case anthropic.BetaStopReasonMaxTokens, anthropic.BetaStopReasonModelContextWindowExceeded:
		return ChatEvent{Type: ChatEventError, Message: "the answer was cut short; ask a narrower question to see the rest"}
	default:
		return ChatEvent{Type: ChatEventDone}
	}
}

// toBetaMessages converts the browser-held transcript into request params. The
// SDK offers a user-message constructor only, so both roles are built directly
// from the validated role string.
func toBetaMessages(msgs []ChatMessage) []anthropic.BetaMessageParam {
	out := make([]anthropic.BetaMessageParam, 0, len(msgs))
	for _, m := range msgs {
		out = append(out, anthropic.BetaMessageParam{
			Role:    anthropic.BetaMessageParamRole(m.Role),
			Content: []anthropic.BetaContentBlockParamUnion{anthropic.NewBetaTextBlock(m.Content)},
		})
	}
	return out
}
