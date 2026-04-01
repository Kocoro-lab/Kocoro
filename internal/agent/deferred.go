package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"github.com/Kocoro-lab/ShanClaw/internal/client"
)

// toolSearchTool is a meta-tool that loads full schemas for deferred tools on demand.
// Defined in the agent package to avoid an import cycle with internal/tools.
type toolSearchTool struct {
	registry *ToolRegistry
	deferred map[string]bool
}

// newToolSearchTool creates a tool_search scoped to the given deferred tool names.
func newToolSearchTool(reg *ToolRegistry, deferred map[string]bool) *toolSearchTool {
	return &toolSearchTool{registry: reg, deferred: deferred}
}

func (t *toolSearchTool) Info() ToolInfo {
	return ToolInfo{
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

func (t *toolSearchTool) RequiresApproval() bool     { return false }
func (t *toolSearchTool) IsReadOnlyCall(string) bool { return true }

func (t *toolSearchTool) Run(_ context.Context, argsJSON string) (ToolResult, error) {
	var args struct {
		Query string `json:"query"`
	}
	if err := json.Unmarshal([]byte(argsJSON), &args); err != nil {
		return ValidationError("invalid arguments: " + err.Error()), nil
	}
	if args.Query == "" {
		return ValidationError("query is required"), nil
	}

	var matched []string

	if strings.HasPrefix(args.Query, "select:") {
		names := strings.Split(strings.TrimPrefix(args.Query, "select:"), ",")
		for _, name := range names {
			name = strings.TrimSpace(name)
			if name != "" && t.deferred[name] {
				matched = append(matched, name)
			}
		}
	} else {
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
		sort.Strings(matched)
	}

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

	return ToolResult{Content: sb.String()}, nil
}

// parseLoadedHeader extracts tool names from the LOADED: header line
// in a tool_search result. Returns nil if no valid header found.
func parseLoadedHeader(content string) []string {
	if !strings.HasPrefix(content, "LOADED:") {
		return nil
	}
	line := content
	if idx := strings.Index(content, "\n"); idx >= 0 {
		line = content[:idx]
	}
	nameStr := strings.TrimPrefix(line, "LOADED:")
	nameStr = strings.TrimSpace(nameStr)
	if nameStr == "" {
		return nil
	}
	return strings.Split(nameStr, ",")
}

// rebuildSchemas produces a deterministic tool schema list by iterating
// the registry's canonical source-aware order (SortedNames: local alpha →
// MCP alpha → gateway alpha) and including tools that are either in base
// or loaded. This preserves cache stability.
func rebuildSchemas(reg *ToolRegistry, baseSchemas []client.Tool, loaded map[string]client.Tool) []client.Tool {
	baseNames := make(map[string]bool, len(baseSchemas))
	for _, s := range baseSchemas {
		baseNames[schemaToolName(s)] = true
	}

	result := make([]client.Tool, 0, len(baseSchemas)+len(loaded))
	for _, name := range reg.SortedNames() {
		if baseNames[name] {
			if t, ok := reg.Get(name); ok {
				result = append(result, buildToolSchema(t))
			}
		} else if s, ok := loaded[name]; ok {
			result = append(result, s)
		}
	}
	return result
}

// schemaToolName extracts the tool name from a client.Tool.
func schemaToolName(t client.Tool) string {
	if t.Function.Name != "" {
		return t.Function.Name
	}
	return t.Name
}

// buildLocalOnlySchemas returns sorted schemas for local tools only.
func buildLocalOnlySchemas(reg *ToolRegistry) []client.Tool {
	local, _, _ := reg.partitionBySource()
	sort.Strings(local)
	schemas := make([]client.Tool, 0, len(local))
	for _, name := range local {
		if t, ok := reg.Get(name); ok {
			schemas = append(schemas, buildToolSchema(t))
		}
	}
	return schemas
}

// deferredToolNames returns the set of non-local tool names (MCP + gateway).
func deferredToolNames(reg *ToolRegistry) map[string]bool {
	_, mcp, gw := reg.partitionBySource()
	names := make(map[string]bool, len(mcp)+len(gw))
	for _, n := range mcp {
		names[n] = true
	}
	for _, n := range gw {
		names[n] = true
	}
	return names
}

// deferredToolSummaries returns sorted summaries for non-local tools.
func deferredToolSummaries(reg *ToolRegistry) []ToolSummary {
	_, mcp, gw := reg.partitionBySource()
	all := append(mcp, gw...)
	sort.Strings(all)
	summaries := make([]ToolSummary, 0, len(all))
	for _, name := range all {
		if t, ok := reg.Get(name); ok {
			info := t.Info()
			summaries = append(summaries, ToolSummary{Name: info.Name, Description: info.Description})
		}
	}
	return summaries
}
