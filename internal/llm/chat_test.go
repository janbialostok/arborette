package llm

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"github.com/anthropics/anthropic-sdk-go"
	"github.com/anthropics/anthropic-sdk-go/option"
)

// The chat client is exercised through the real SDK against a server speaking
// the Anthropic stream wire format, rather than through an injected fake of the
// beta service. Only a real request carries the artifacts that matter here --
// the beta header, max_tokens, and the mcp_servers/tools connector params -- and
// an interface fake produces no HTTP request to assert them against.

// frame renders one SSE event. The SDK's stream decoder dispatches on the event:
// line, so a data-only frame is silently skipped.
func frame(typ, data string) string {
	return "event: " + typ + "\ndata: " + data + "\n\n"
}

const (
	frameMessageStart = `{"type":"message_start","message":{"id":"msg_test","type":"message","role":"assistant",` +
		`"model":"test-model","content":[],"stop_reason":null,"stop_sequence":null,` +
		`"usage":{"input_tokens":1,"output_tokens":1}}}`
	frameTextStart   = `{"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`
	frameTextStop    = `{"type":"content_block_stop","index":0}`
	frameMessageStop = `{"type":"message_stop"}`
)

func frameTextDelta(text string) string {
	return `{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"` + text + `"}}`
}

// frameStopReason carries the turn's stop reason. It is the only event that does,
// so a stream without it never fires the stop-reason-gated terminal event.
func frameStopReason(reason string) string {
	return `{"type":"message_delta","delta":{"stop_reason":"` + reason + `","stop_sequence":null},` +
		`"usage":{"output_tokens":5}}`
}

// textTurn is the minimal complete turn: one text block and a stop reason.
func textTurn(text, stopReason string) string {
	return frame("message_start", frameMessageStart) +
		frame("content_block_start", frameTextStart) +
		frame("content_block_delta", frameTextDelta(text)) +
		frame("content_block_stop", frameTextStop) +
		frame("message_delta", frameStopReason(stopReason)) +
		frame("message_stop", frameMessageStop)
}

// capturedRequest mirrors the outgoing request body's load-bearing fields.
type capturedRequest struct {
	Model     string `json:"model"`
	MaxTokens int64  `json:"max_tokens"`
	System    []struct {
		Text string `json:"text"`
	} `json:"system"`
	Messages []struct {
		Role    string `json:"role"`
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
	} `json:"messages"`
	MCPServers []struct {
		Name               string `json:"name"`
		URL                string `json:"url"`
		AuthorizationToken string `json:"authorization_token"`
		ToolConfiguration  struct {
			AllowedTools []string `json:"allowed_tools"`
		} `json:"tool_configuration"`
	} `json:"mcp_servers"`
	Tools []struct {
		Type          string `json:"type"`
		MCPServerName string `json:"mcp_server_name"`
	} `json:"tools"`
	Thinking struct {
		Type string `json:"type"`
	} `json:"thinking"`

	betaHeader string
}

// chatServer serves one canned stream body per request and records what was sent.
// A request past the last body reuses the last one, so a resume test only has to
// author the turns it cares about.
type chatServer struct {
	t      *testing.T
	bodies []string
	got    []capturedRequest
}

func newChatClient(t *testing.T, bodies ...string) (*ChatClient, *chatServer) {
	t.Helper()
	cs := &chatServer{t: t, bodies: bodies}
	ts := httptest.NewServer(cs)
	t.Cleanup(ts.Close)

	sdk := anthropic.NewClient(option.WithBaseURL(ts.URL), option.WithAPIKey("test-key"))
	return &ChatClient{
		messages:   &sdk.Beta.Messages,
		model:      "test-model",
		mcpURL:     "https://tunnel.example/mcp",
		mcpToken:   "mcp-token",
		configured: true,
	}, cs
}

func (cs *chatServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	raw, err := io.ReadAll(r.Body)
	if err != nil {
		cs.t.Errorf("read request body: %v", err)
		return
	}
	var req capturedRequest
	if err := json.Unmarshal(raw, &req); err != nil {
		cs.t.Errorf("unmarshal request body: %v", err)
		return
	}
	req.betaHeader = r.Header.Get("anthropic-beta")
	cs.got = append(cs.got, req)

	body := cs.bodies[min(len(cs.got)-1, len(cs.bodies)-1)]
	w.Header().Set("Content-Type", "text/event-stream")
	w.WriteHeader(http.StatusOK)
	io.WriteString(w, body)
}

// collect runs a turn and returns the emitted events.
func collect(t *testing.T, c *ChatClient, msgs []ChatMessage) ([]ChatEvent, error) {
	t.Helper()
	var events []ChatEvent
	err := c.Chat(context.Background(), "system prompt", msgs, func(ev ChatEvent) error {
		events = append(events, ev)
		return nil
	})
	return events, err
}

func TestChatSendsConnectorRequest(t *testing.T) {
	c, cs := newChatClient(t, textTurn("hi", "end_turn"))

	if _, err := collect(t, c, []ChatMessage{{Role: "user", Content: "hello"}}); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(cs.got) != 1 {
		t.Fatalf("want 1 request, got %d", len(cs.got))
	}
	req := cs.got[0]

	if !strings.Contains(req.betaHeader, "mcp-client-2025-11-20") {
		t.Fatalf("anthropic-beta header = %q, want the MCP connector beta", req.betaHeader)
	}
	if req.MaxTokens == 0 {
		t.Fatalf("max_tokens must be non-zero on the request")
	}
	if len(req.MCPServers) != 1 {
		t.Fatalf("want 1 mcp server, got %d", len(req.MCPServers))
	}
	server := req.MCPServers[0]
	if server.URL != "https://tunnel.example/mcp" || server.AuthorizationToken != "mcp-token" {
		t.Fatalf("connector target not sent: %+v", server)
	}
	want := []string{"get_optimized_heuristics", "trace_causal_chain"}
	if !reflect.DeepEqual(server.ToolConfiguration.AllowedTools, want) {
		t.Fatalf("allowed_tools = %v, want %v (goal submission must stay excluded)", server.ToolConfiguration.AllowedTools, want)
	}
	// The connector needs both halves: the server definition and a toolset entry
	// referencing it by name.
	if len(req.Tools) != 1 || req.Tools[0].Type != "mcp_toolset" || req.Tools[0].MCPServerName != server.Name {
		t.Fatalf("tools did not activate the connector toolset: %+v", req.Tools)
	}
	if len(req.System) != 1 || req.System[0].Text != "system prompt" {
		t.Fatalf("system prompt not sent: %+v", req.System)
	}
	if req.Thinking.Type != "disabled" {
		t.Fatalf("thinking = %q, want an explicit \"disabled\" on the interactive surface", req.Thinking.Type)
	}
}

// TestChatSendsTranscriptVerbatim pins the payload the whole surface exists to
// deliver: every turn's role and its exact words.
func TestChatSendsTranscriptVerbatim(t *testing.T) {
	c, cs := newChatClient(t, textTurn("hi", "end_turn"))

	transcript := []ChatMessage{
		{Role: RoleUser, Content: "what raises the rate?"},
		{Role: RoleAssistant, Content: "cryosleep does"},
		{Role: RoleUser, Content: "by how much?"},
	}
	if _, err := collect(t, c, transcript); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	sent := cs.got[0].Messages
	if len(sent) != len(transcript) {
		t.Fatalf("sent %d messages, want %d", len(sent), len(transcript))
	}
	for i, want := range transcript {
		if sent[i].Role != want.Role {
			t.Fatalf("message %d role = %q, want %q", i, sent[i].Role, want.Role)
		}
		if len(sent[i].Content) != 1 || sent[i].Content[0].Type != "text" || sent[i].Content[0].Text != want.Content {
			t.Fatalf("message %d content = %+v, want the text %q", i, sent[i].Content, want.Content)
		}
	}
}

// TestChatReportsFailedToolResult covers the flag that distinguishes "the lookup
// failed" from "the lookup found nothing" -- the distinction the agent's own
// grounding instructions depend on.
func TestChatReportsFailedToolResult(t *testing.T) {
	body := frame("message_start", frameMessageStart) +
		frame("content_block_start", `{"type":"content_block_start","index":0,"content_block":`+
			`{"type":"mcp_tool_result","tool_use_id":"mcptu_1","is_error":true,`+
			`"content":[{"type":"text","text":"internal error"}]}}`) +
		frame("content_block_stop", frameTextStop) +
		frame("message_delta", frameStopReason("end_turn")) +
		frame("message_stop", frameMessageStop)
	c, _ := newChatClient(t, body)

	events, err := collect(t, c, []ChatMessage{{Role: RoleUser, Content: "hi"}})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	want := []ChatEvent{{Type: ChatEventToolResult, IsError: true}, {Type: ChatEventDone}}
	if !reflect.DeepEqual(events, want) {
		t.Fatalf("events = %+v, want %+v", events, want)
	}
}

// TestChatSkipsNonTextDeltas proves the tool-call argument JSON the API streams
// for every MCP invocation is not rendered to the analyst as prose.
func TestChatSkipsNonTextDeltas(t *testing.T) {
	body := frame("message_start", frameMessageStart) +
		frame("content_block_start", `{"type":"content_block_start","index":0,"content_block":`+
			`{"type":"mcp_tool_use","id":"mcptu_1","name":"trace_causal_chain",`+
			`"server_name":"arborette","input":{}}}`) +
		frame("content_block_delta", `{"type":"content_block_delta","index":0,`+
			`"delta":{"type":"input_json_delta","partial_json":"{\"meta_heuristic_id\":\"mh-1\"}"}}`) +
		frame("content_block_stop", frameTextStop) +
		frame("message_delta", frameStopReason("end_turn")) +
		frame("message_stop", frameMessageStop)
	c, _ := newChatClient(t, body)

	events, err := collect(t, c, []ChatMessage{{Role: RoleUser, Content: "hi"}})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	want := []ChatEvent{{Type: ChatEventToolUse, Tool: "trace_causal_chain"}, {Type: ChatEventDone}}
	if !reflect.DeepEqual(events, want) {
		t.Fatalf("events = %+v, want %+v (tool-call JSON must not reach the transcript)", events, want)
	}
}

// TestChatReturnsEmitErrorUnwrapped pins the contract the caller's disconnect
// handling rests on: an emit failure comes back as itself, so the caller can
// recognize its own sentinel with errors.Is across the package boundary.
func TestChatReturnsEmitErrorUnwrapped(t *testing.T) {
	sentinel := errors.New("caller went away")
	c, _ := newChatClient(t, textTurn("hi", "end_turn"))

	err := c.Chat(context.Background(), "system", []ChatMessage{{Role: RoleUser, Content: "hi"}},
		func(ChatEvent) error { return sentinel })

	if !errors.Is(err, sentinel) {
		t.Fatalf("error = %v, want the caller's own error unwrapped", err)
	}
}

func TestChatEmitsTextThenSingleTerminal(t *testing.T) {
	body := frame("message_start", frameMessageStart) +
		frame("content_block_start", frameTextStart) +
		frame("content_block_delta", frameTextDelta("Segments ")) +
		frame("content_block_delta", frameTextDelta("with high lift")) +
		frame("content_block_stop", frameTextStop) +
		frame("message_delta", frameStopReason("end_turn")) +
		frame("message_stop", frameMessageStop)
	c, _ := newChatClient(t, body)

	events, err := collect(t, c, []ChatMessage{{Role: "user", Content: "what works?"}})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	want := []ChatEvent{
		{Type: ChatEventText, Text: "Segments "},
		{Type: ChatEventText, Text: "with high lift"},
		{Type: ChatEventDone},
	}
	if !reflect.DeepEqual(events, want) {
		t.Fatalf("events = %+v, want %+v", events, want)
	}
}

func TestChatEmitsToolRoundTrip(t *testing.T) {
	body := frame("message_start", frameMessageStart) +
		frame("content_block_start", frameTextStart) +
		frame("content_block_delta", frameTextDelta("Checking the graph")) +
		frame("content_block_stop", frameTextStop) +
		frame("content_block_start", `{"type":"content_block_start","index":1,"content_block":`+
			`{"type":"mcp_tool_use","id":"mcptu_1","name":"get_optimized_heuristics",`+
			`"server_name":"arborette","input":{}}}`) +
		frame("content_block_stop", `{"type":"content_block_stop","index":1}`) +
		frame("content_block_start", `{"type":"content_block_start","index":2,"content_block":`+
			`{"type":"mcp_tool_result","tool_use_id":"mcptu_1","is_error":false,`+
			`"content":[{"type":"text","text":"{}"}]}}`) +
		frame("content_block_stop", `{"type":"content_block_stop","index":2}`) +
		frame("message_delta", frameStopReason("end_turn")) +
		frame("message_stop", frameMessageStop)
	c, _ := newChatClient(t, body)

	events, err := collect(t, c, []ChatMessage{{Role: "user", Content: "what works?"}})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	want := []ChatEvent{
		{Type: ChatEventText, Text: "Checking the graph"},
		{Type: ChatEventToolUse, Tool: "get_optimized_heuristics"},
		{Type: ChatEventToolResult},
		{Type: ChatEventDone},
	}
	if !reflect.DeepEqual(events, want) {
		t.Fatalf("events = %+v, want %+v", events, want)
	}
}

// TestChatTerminalEventPerStopReason pins the invariant the chat surface depends
// on: whatever the stop reason, the stream ends with exactly one terminal event.
// The unknown reason stands in for one the API adds later -- it must terminate,
// not hang.
func TestChatTerminalEventPerStopReason(t *testing.T) {
	cases := []struct {
		name       string
		stopReason string
		wantType   string
	}{
		{"end turn", "end_turn", ChatEventDone},
		{"stop sequence", "stop_sequence", ChatEventDone},
		{"tool use", "tool_use", ChatEventDone},
		{"refusal", "refusal", ChatEventError},
		{"max tokens", "max_tokens", ChatEventError},
		{"context window exceeded", "model_context_window_exceeded", ChatEventError},
		{"unknown future reason", "some_future_reason", ChatEventDone},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c, _ := newChatClient(t, textTurn("partial", tc.stopReason))

			events, err := collect(t, c, []ChatMessage{{Role: "user", Content: "hi"}})
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			terminal := events[len(events)-1]
			if terminal.Type != tc.wantType {
				t.Fatalf("terminal event = %q, want %q", terminal.Type, tc.wantType)
			}
			for _, ev := range events[:len(events)-1] {
				if ev.Type == ChatEventDone || ev.Type == ChatEventError {
					t.Fatalf("more than one terminal event: %+v", events)
				}
			}
			if tc.wantType == ChatEventError && terminal.Message == "" {
				t.Fatalf("an error terminal must carry a reason to show the analyst")
			}
		})
	}
}

// TestChatResumesPausedTurn covers the connector's long-turn pause: the partial
// assistant turn is resubmitted, and the stream ends on the resumed turn's own
// stop reason rather than reporting the pause as a finished answer.
func TestChatResumesPausedTurn(t *testing.T) {
	c, cs := newChatClient(t, textTurn("thinking", "pause_turn"), textTurn("done thinking", "end_turn"))

	events, err := collect(t, c, []ChatMessage{{Role: "user", Content: "hi"}})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(cs.got) != 2 {
		t.Fatalf("want the paused turn resubmitted (2 requests), got %d", len(cs.got))
	}
	// The resubmission carries the partial assistant turn back.
	resumed := cs.got[1].Messages
	if len(resumed) != 2 || resumed[1].Role != "assistant" {
		t.Fatalf("resume did not append the assistant turn: %+v", resumed)
	}
	want := []ChatEvent{
		{Type: ChatEventText, Text: "thinking"},
		{Type: ChatEventText, Text: "done thinking"},
		{Type: ChatEventDone},
	}
	if !reflect.DeepEqual(events, want) {
		t.Fatalf("events = %+v, want %+v", events, want)
	}
}

// TestChatExhaustedResumesEndInError proves a turn that never stops pausing ends
// as an error rather than a bare done.
func TestChatExhaustedResumesEndInError(t *testing.T) {
	c, cs := newChatClient(t, textTurn("still working", "pause_turn"))

	events, err := collect(t, c, []ChatMessage{{Role: "user", Content: "hi"}})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(cs.got) != maxChatResumes+1 {
		t.Fatalf("want %d requests, got %d", maxChatResumes+1, len(cs.got))
	}
	terminal := events[len(events)-1]
	if terminal.Type != ChatEventError {
		t.Fatalf("terminal event = %q, want %q", terminal.Type, ChatEventError)
	}
	// The request shape must not grow across resumes.
	for i, req := range cs.got[1:] {
		if len(req.Messages) != 2 {
			t.Fatalf("resume %d sent %d messages, want 2 (transcript + one partial)", i+1, len(req.Messages))
		}
	}
}

func TestChatUnconfiguredReturnsSentinel(t *testing.T) {
	c, cs := newChatClient(t, textTurn("hi", "end_turn"))
	c.configured = false

	events, err := collect(t, c, []ChatMessage{{Role: "user", Content: "hi"}})
	if !errors.Is(err, ErrChatDisabled) {
		t.Fatalf("error = %v, want ErrChatDisabled", err)
	}
	if len(cs.got) != 0 {
		t.Fatalf("an unconfigured client must not reach the API, got %d requests", len(cs.got))
	}
	if len(events) != 0 {
		t.Fatalf("an unconfigured client must emit nothing, got %+v", events)
	}
}

// TestNewChatClientRequiresEveryConnectorInput pins the disabled predicate; a
// missing token counts (see NewChatClient).
func TestNewChatClientRequiresEveryConnectorInput(t *testing.T) {
	cases := []struct {
		name                     string
		apiKey, mcpURL, mcpToken string
		wantConfigured           bool
	}{
		{"fully configured", "key", "https://tunnel.example/mcp", "token", true},
		{"no api key", "", "https://tunnel.example/mcp", "token", false},
		{"no public url", "key", "", "token", false},
		{"no mcp token", "key", "https://tunnel.example/mcp", "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := NewChatClient(tc.apiKey, "test-model", tc.mcpURL, tc.mcpToken)
			if c.configured != tc.wantConfigured {
				t.Fatalf("configured = %v, want %v", c.configured, tc.wantConfigured)
			}
			// Four positional strings: a transposed pair compiles cleanly, and
			// only this test observes the mapping.
			if c.model != "test-model" || c.mcpURL != tc.mcpURL || c.mcpToken != tc.mcpToken {
				t.Fatalf("constructor mapped its arguments wrong: model=%q url=%q token=%q",
					c.model, c.mcpURL, c.mcpToken)
			}
		})
	}
}

// TestChatMidStreamFaultReturnsError proves a stream that fails partway surfaces
// as an error return (which the caller masks into a terminal frame), not a panic
// and not a silent truncation.
func TestChatMidStreamFaultReturnsError(t *testing.T) {
	body := frame("message_start", frameMessageStart) +
		frame("content_block_start", frameTextStart) +
		frame("content_block_delta", frameTextDelta("partial")) +
		frame("error", `{"type":"error","error":{"type":"overloaded_error","message":"overloaded"}}`)
	c, _ := newChatClient(t, body)

	events, err := collect(t, c, []ChatMessage{{Role: "user", Content: "hi"}})
	if err == nil {
		t.Fatalf("expected an error when the stream faults mid-turn")
	}
	if len(events) != 1 || events[0].Type != ChatEventText {
		t.Fatalf("partial text before the fault should still be emitted, got %+v", events)
	}
}
