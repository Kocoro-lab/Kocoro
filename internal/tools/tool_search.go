package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"github.com/Kocoro-lab/ShanClaw/internal/agent"
)

// ToolSearchTool is a meta-tool that loads full schemas for deferred tools on demand.
// It only searches tools in the deferred set, not local tools.
type ToolSearchTool struct {
	registry *agent.ToolRegistry
	deferred map[string]bool
}

// NewToolSearchTool creates a tool_search scoped to the given deferred tool names.
func NewToolSearchTool(reg *agent.ToolRegistry, deferred map[string]bool) *ToolSearchTool {
	return &ToolSearchTool{registry: reg, deferred: deferred}
}

func (t *ToolSearchTool) Info() agent.ToolInfo {
	return agent.ToolInfo{
		Name:        "tool_search",
		Description: "Load the full schema for a deferred tool so you can call it. Use \"select:name1,name2\" for exact lookup or a keyword to search by name/description.",
		Parameters: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"query": map[string]any{
					"type":        "string",
					"description": "Either \"select:name1,name2\" for exact match or a keyword to search deferred tools.",
				},
			},
		},
		Required: []string{"query"},
	}
}

func (t *ToolSearchTool) RequiresApproval() bool     { return false }
func (t *ToolSearchTool) IsReadOnlyCall(string) bool { return true }

func (t *ToolSearchTool) Run(_ context.Context, argsJSON string) (agent.ToolResult, error) {
	var args struct {
		Query string `json:"query"`
	}
	if err := json.Unmarshal([]byte(argsJSON), &args); err != nil {
		return agent.ValidationError("invalid arguments: " + err.Error()), nil
	}
	if args.Query == "" {
		return agent.ValidationError("query is required"), nil
	}

	var matched []string

	if strings.HasPrefix(args.Query, "select:") {
		// Exact match mode.
		names := strings.Split(strings.TrimPrefix(args.Query, "select:"), ",")
		for _, name := range names {
			name = strings.TrimSpace(name)
			if name != "" && t.deferred[name] {
				matched = append(matched, name)
			}
		}
	} else {
		// Keyword search mode: match against name and description.
		query := strings.ToLower(args.Query)
		for name := range t.deferred {
			tool, ok := t.registry.Get(name)
			if !ok {
				continue
			}
			info := tool.Info()
			if strings.Contains(strings.ToLower(info.Name), query) ||
				strings.Contains(strings.ToLower(info.Description), query) {
				matched = append(matched, name)
			}
		}
		// Sort for deterministic output.
		sort.Strings(matched)
	}

	// Build result: machine-readable header + human-readable schemas.
	var sb strings.Builder
	sb.WriteString("LOADED:")
	sb.WriteString(strings.Join(matched, ","))

	if len(matched) == 0 {
		sb.WriteString("\nNo matching deferred tools found.")
	} else {
		schemas := t.registry.FullSchemas(matched)
		for i, s := range schemas {
			schemaJSON, _ := json.MarshalIndent(s, "", "  ")
			sb.WriteString(fmt.Sprintf("\n\n## %s\n%s", matched[i], string(schemaJSON)))
		}
	}

	return agent.ToolResult{Content: sb.String()}, nil
}
