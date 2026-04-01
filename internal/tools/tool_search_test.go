package tools

import (
	"context"
	"strings"
	"testing"

	"github.com/Kocoro-lab/ShanClaw/internal/agent"
)

// tsTestTool is a mock tool for tool_search tests.
type tsTestTool struct {
	name string
	desc string
}

func (m *tsTestTool) Info() agent.ToolInfo {
	return agent.ToolInfo{
		Name:        m.name,
		Description: m.desc,
		Parameters:  map[string]any{"type": "object", "properties": map[string]any{"x": map[string]any{"type": "string"}}},
	}
}
func (m *tsTestTool) Run(context.Context, string) (agent.ToolResult, error) {
	return agent.ToolResult{Content: "mock"}, nil
}
func (m *tsTestTool) RequiresApproval() bool     { return false }
func (m *tsTestTool) IsReadOnlyCall(string) bool { return false }

func newTestToolSearch() *ToolSearchTool {
	reg := agent.NewToolRegistry()
	reg.Register(&tsTestTool{name: "mock_mcp_a", desc: "Alpha MCP tool for testing"})
	reg.Register(&tsTestTool{name: "mock_mcp_b", desc: "Beta MCP tool for testing"})
	reg.Register(&tsTestTool{name: "mock_gw_c", desc: "Gamma gateway tool"})
	reg.Register(&tsTestTool{name: "bash", desc: "Run shell commands"})

	deferred := map[string]bool{
		"mock_mcp_a": true,
		"mock_mcp_b": true,
		"mock_gw_c":  true,
	}
	return NewToolSearchTool(reg, deferred)
}

func TestToolSearch_SelectExact(t *testing.T) {
	ts := newTestToolSearch()
	result, err := ts.Run(context.Background(), `{"query":"select:mock_mcp_a,mock_mcp_b"}`)
	if err != nil {
		t.Fatal(err)
	}
	if result.IsError {
		t.Fatalf("unexpected error: %s", result.Content)
	}
	if !strings.HasPrefix(result.Content, "LOADED:") {
		t.Error("result should start with LOADED: header")
	}
	header := strings.SplitN(result.Content, "\n", 2)[0]
	if !strings.Contains(header, "mock_mcp_a") || !strings.Contains(header, "mock_mcp_b") {
		t.Errorf("LOADED header should contain both tools, got: %s", header)
	}
}

func TestToolSearch_KeywordSearch(t *testing.T) {
	ts := newTestToolSearch()
	result, err := ts.Run(context.Background(), `{"query":"Alpha"}`)
	if err != nil {
		t.Fatal(err)
	}
	header := strings.SplitN(result.Content, "\n", 2)[0]
	if !strings.Contains(header, "mock_mcp_a") {
		t.Errorf("keyword 'Alpha' should match mock_mcp_a, got header: %s", header)
	}
	if strings.Contains(header, "mock_mcp_b") {
		t.Errorf("keyword 'Alpha' should NOT match mock_mcp_b, got header: %s", header)
	}
}

func TestToolSearch_NoMatches(t *testing.T) {
	ts := newTestToolSearch()
	result, err := ts.Run(context.Background(), `{"query":"nonexistent_xyz"}`)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(result.Content, "LOADED:") {
		t.Error("even no-match result should have LOADED: header")
	}
	header := strings.SplitN(result.Content, "\n", 2)[0]
	if header != "LOADED:" {
		t.Errorf("empty LOADED header expected, got: %s", header)
	}
}

func TestToolSearch_OnlySearchesDeferred(t *testing.T) {
	ts := newTestToolSearch()
	result, err := ts.Run(context.Background(), `{"query":"select:bash"}`)
	if err != nil {
		t.Fatal(err)
	}
	header := strings.SplitN(result.Content, "\n", 2)[0]
	if strings.Contains(header, "bash") {
		t.Error("tool_search should not find local tool 'bash'")
	}
}

func TestToolSearch_IsReadOnly(t *testing.T) {
	ts := newTestToolSearch()
	if !ts.IsReadOnlyCall("{}") {
		t.Error("tool_search should be read-only")
	}
}

func TestToolSearch_RequiresApproval(t *testing.T) {
	ts := newTestToolSearch()
	if ts.RequiresApproval() {
		t.Error("tool_search should not require approval")
	}
}
