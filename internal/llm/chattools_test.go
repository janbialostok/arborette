package llm

import (
	"reflect"
	"testing"

	"github.com/arborette/arborette/internal/mcpserver"
)

// TestChatAllowedToolsMatchesMCPServer pins the connector allowlist against the
// names the MCP server actually registers. The coupling is enforced by Anthropic
// server-side, so a rename on either side compiles and passes every other test
// while silently stripping the agent's access -- which reads as an empty
// knowledge graph rather than a wiring bug. The import is test-only, so the
// production dependency still runs one way.
func TestChatAllowedToolsMatchesMCPServer(t *testing.T) {
	want := []string{mcpserver.ToolGetOptimizedHeuristics, mcpserver.ToolTraceCausalChain}
	if !reflect.DeepEqual(chatAllowedTools, want) {
		t.Fatalf("chatAllowedTools = %v, want the MCP server's read-only tools %v", chatAllowedTools, want)
	}
	for _, name := range chatAllowedTools {
		if name == mcpserver.ToolSubmitAnalystGoal {
			t.Fatalf("%q must not be reachable from the chat agent", name)
		}
	}
}
