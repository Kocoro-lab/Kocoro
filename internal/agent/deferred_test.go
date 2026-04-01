package agent

import (
	"context"
	"testing"

	"github.com/Kocoro-lab/ShanClaw/internal/client"
)

func TestParseLoadedHeader(t *testing.T) {
	tests := []struct {
		name     string
		input    string
		expected []string
	}{
		{"two tools", "LOADED:a,b\nrest of content", []string{"a", "b"}},
		{"one tool", "LOADED:playwright_click\nschema here", []string{"playwright_click"}},
		{"empty header", "LOADED:\nNo matching", nil},
		{"no header", "some random text", nil},
		{"no newline", "LOADED:a,b", []string{"a", "b"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := parseLoadedHeader(tt.input)
			if len(got) != len(tt.expected) {
				t.Fatalf("expected %v, got %v", tt.expected, got)
			}
			for i := range got {
				if got[i] != tt.expected[i] {
					t.Errorf("index %d: expected %q, got %q", i, tt.expected[i], got[i])
				}
			}
		})
	}
}

// mockMCPTool implements ToolSourcer to classify as MCP.
type mockMCPTool struct{ name string }

func (m *mockMCPTool) Info() ToolInfo {
	return ToolInfo{Name: m.name, Description: "mock mcp tool", Parameters: map[string]any{"type": "object", "properties": map[string]any{}}}
}
func (m *mockMCPTool) Run(context.Context, string) (ToolResult, error) { return ToolResult{}, nil }
func (m *mockMCPTool) RequiresApproval() bool                          { return false }
func (m *mockMCPTool) ToolSource() ToolSource                          { return SourceMCP }
func (m *mockMCPTool) IsReadOnlyCall(string) bool                      { return false }

func TestRebuildSchemas_Deterministic(t *testing.T) {
	reg := NewToolRegistry()
	reg.Register(&mockTool{name: "grep"})
	reg.Register(&mockTool{name: "bash"})
	reg.Register(&mockMCPTool{name: "mcp_z"})
	reg.Register(&mockMCPTool{name: "mcp_a"})

	baseSchemas := buildLocalOnlySchemas(reg)

	loaded := map[string]client.Tool{
		"mcp_z": {Type: "function", Function: client.FunctionDef{Name: "mcp_z"}},
	}

	result := rebuildSchemas(reg, baseSchemas, loaded)

	// Canonical order: [bash, grep, mcp_z]
	if len(result) != 3 {
		t.Fatalf("expected 3 schemas, got %d", len(result))
	}
	expected := []string{"bash", "grep", "mcp_z"}
	for i, exp := range expected {
		got := schemaName(result[i])
		if got != exp {
			t.Errorf("index %d: expected %q, got %q", i, exp, got)
		}
	}

	// Determinism: same result on second call.
	result2 := rebuildSchemas(reg, baseSchemas, loaded)
	for i := range result {
		if schemaName(result[i]) != schemaName(result2[i]) {
			t.Errorf("index %d: non-deterministic", i)
		}
	}
}

func TestRebuildSchemas_NoDuplicates(t *testing.T) {
	reg := NewToolRegistry()
	reg.Register(&mockTool{name: "bash"})

	baseSchemas := reg.SortedSchemas()
	loaded := map[string]client.Tool{
		"bash": baseSchemas[0],
	}

	result := rebuildSchemas(reg, baseSchemas, loaded)
	if len(result) != 1 {
		t.Fatalf("expected 1 schema (no duplicate), got %d", len(result))
	}
}

func schemaName(t client.Tool) string {
	if t.Function.Name != "" {
		return t.Function.Name
	}
	return t.Name
}
