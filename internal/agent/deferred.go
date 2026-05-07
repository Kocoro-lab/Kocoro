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
		Description: "Load deferred tool schemas so you can call them in this same request. After calling tool_search, immediately continue the task using the loaded tools — do not stop or ask the user to proceed. Use \"select:name1,name2\" for exact lookup or a keyword to search by name/description.",
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

// SkillExempt opts tool_search out of skill allowed-tools restriction. Without
// this, a skill that omitted tool_search from its allowed list would lock the
// model out of loading deferred tool schemas — including ones the skill itself
// might depend on. tool_search has no I/O of its own; everything it loads is
// still subject to the skill filter and per-tool approval.
func (t *toolSearchTool) SkillExempt() bool { return true }

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
	matched = expandDeferredFamilyCore(t.registry, t.deferred, matched)

	// Build structured tool_reference blocks for the new protocol path.
	// Zero matches → zero blocks (loop.go falls back to the Content string).
	var blocks []client.ContentBlock
	for _, name := range matched {
		blocks = append(blocks, client.ContentBlock{
			Type:     "tool_reference",
			ToolName: name,
		})
	}

	// Legacy Content string: preserved as the fallback path for non-supporting
	// backends (Ollama, pre-3.1 shannon-cloud gateway). Contains the LOADED:
	// header + full schema JSON so the model can still discover tools when
	// the tool_reference protocol is unavailable.
	var sb strings.Builder
	sb.WriteString("LOADED:")
	sb.WriteString(strings.Join(matched, ","))

	if len(matched) == 0 {
		sb.WriteString("\nNo matching deferred tools found.")
	} else {
		sb.WriteString("\nSchemas loaded. Call these tools now to continue the user's task — do not stop or describe what was loaded.")
		schemas := t.registry.FullSchemas(matched)
		for i, s := range schemas {
			schemaJSON, _ := json.MarshalIndent(s, "", "  ")
			sb.WriteString(fmt.Sprintf("\n\n## %s\n%s", matched[i], string(schemaJSON)))
		}
	}

	return ToolResult{
		Content:       sb.String(),
		ContentBlocks: blocks,
	}, nil
}

func expandDeferredFamilyCore(reg *ToolRegistry, deferred map[string]bool, matched []string) []string {
	if len(matched) == 0 {
		return nil
	}

	selected := make(map[string]bool, len(matched))
	for _, name := range matched {
		if name != "" && deferred[name] {
			selected[name] = true
		}
		family := toolFamily(name)
		spec, ok := FamilyRegistry[family]
		if !ok {
			continue
		}
		for _, coreName := range spec.Core {
			if deferred[coreName] {
				selected[coreName] = true
			}
		}
	}

	expanded := make([]string, 0, len(selected))
	for _, name := range reg.SortedNames() {
		if selected[name] {
			expanded = append(expanded, name)
		}
	}
	return expanded
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

// liveToolNames returns tool names in the same order as the live schema list.
func liveToolNames(schemas []client.Tool) []string {
	names := make([]string, 0, len(schemas))
	for _, schema := range schemas {
		name := schemaToolName(schema)
		if name != "" {
			names = append(names, name)
		}
	}
	return names
}

// partitionLiveToolNamesBySource splits the given live tool name list into
// local / MCP / gateway buckets. Names not found in the registry (or registered
// without a ToolSource) fall into the local bucket — same default as
// ToolRegistry.partitionBySource. Used by the agent loop to feed
// prompt.BuildSystemPrompt's per-source fields without disturbing the live
// schema ordering. See issue #107 (cross-user BP #1 byte stability).
func partitionLiveToolNamesBySource(reg *ToolRegistry, names []string) (local, mcp, gateway []string) {
	for _, name := range names {
		t, ok := reg.Get(name)
		if !ok {
			local = append(local, name)
			continue
		}
		sourcer, hasSource := t.(ToolSourcer)
		if !hasSource {
			local = append(local, name)
			continue
		}
		switch sourcer.ToolSource() {
		case SourceMCP:
			mcp = append(mcp, name)
		case SourceGateway:
			gateway = append(gateway, name)
		default:
			local = append(local, name)
		}
	}
	return
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

// buildLocalActiveSchemas returns local tool schemas with categorical-deferred
// names filtered out. Used by the legacy deferred path (modelSupportsToolRef
// false) where defer_loading flags are not honored on the wire — instead we
// simply omit cold local tools so they're discoverable only via tool_search.
func buildLocalActiveSchemas(reg *ToolRegistry, cold map[string]bool) []client.Tool {
	schemas := buildLocalOnlySchemas(reg)
	if len(cold) == 0 {
		return schemas
	}
	out := make([]client.Tool, 0, len(schemas))
	for _, s := range schemas {
		if cold[schemaToolName(s)] {
			continue
		}
		out = append(out, s)
	}
	return out
}

// deferredToolNames returns the set of tool names that are eligible for
// deferred loading: MCP + gateway tools plus local tools whose category
// matches shouldDeferByCategory (rare-use, big-schema families like
// browser_*, schedule_*, computer, etc. — see toolbudget.go for the list).
//
// The actual decision to defer depends on the deferredMode trigger in
// loop.go, which gates on either total budget overflow OR the presence of
// any categorical-deferred cold tool.
func deferredToolNames(reg *ToolRegistry) map[string]bool {
	local, mcp, gw := reg.partitionBySource()
	names := make(map[string]bool, len(mcp)+len(gw))
	for _, n := range local {
		if shouldDeferByCategory(n) {
			names[n] = true
		}
	}
	for _, n := range mcp {
		names[n] = true
	}
	for _, n := range gw {
		names[n] = true
	}
	return names
}

// hasCategoricalDeferred reports whether any name in the cold deferred set
// belongs to an always-defer category. Used by deferredMode to fire even
// when total schema tokens stay under schemaTokenBudget.
func hasCategoricalDeferred(cold map[string]bool) bool {
	for name := range cold {
		if shouldDeferByCategory(name) {
			return true
		}
	}
	return false
}

// preseedDeferredSchemas filters the session working set down to schemas that
// are still deferred in the current effective registry.
func preseedDeferredSchemas(ws *WorkingSet, deferred map[string]bool) map[string]client.Tool {
	loaded := make(map[string]client.Tool)
	if ws == nil || len(deferred) == 0 {
		return loaded
	}
	for name, schema := range ws.Schemas() {
		if deferred[name] {
			loaded[name] = schema
		}
	}
	return loaded
}

// remainingDeferredNames removes already-warmed schemas from the deferred set.
func remainingDeferredNames(deferred map[string]bool, loaded map[string]client.Tool) map[string]bool {
	remaining := make(map[string]bool, len(deferred))
	for name := range deferred {
		if _, ok := loaded[name]; ok {
			continue
		}
		remaining[name] = true
	}
	return remaining
}

// modelSupportsToolRef reports whether the configured model supports the
// defer_loading + tool_reference protocol. Sonnet 4.0+ / Opus 4.0+ only,
// per Anthropic tool-search docs (Haiku excluded, pre-4 excluded).
// Non-Anthropic providers always fall back to the legacy rebuildSchemas path.
func modelSupportsToolRef(modelID string) bool {
	m := strings.ToLower(modelID)
	if !strings.Contains(m, "claude") {
		return false
	}
	if strings.Contains(m, "haiku") {
		return false
	}
	// claude-sonnet-4*, claude-opus-4*, claude-sonnet-5*, etc.
	return strings.Contains(m, "sonnet-4") ||
		strings.Contains(m, "opus-4") ||
		strings.Contains(m, "sonnet-5") ||
		strings.Contains(m, "opus-5")
}

// hasAnyNonDeferred returns true if at least one tool in the slice is NOT deferred.
// Anthropic rejects requests where every tool has defer_loading: true (400 error).
// tool_search itself is always non-deferred (registered outside the defer set),
// so this invariant holds whenever deferred mode is active.
func hasAnyNonDeferred(tools []client.Tool) bool {
	for _, t := range tools {
		if !t.DeferLoading {
			return true
		}
	}
	return false
}

// buildFullSchemasWithDefer emits the complete tools array (local + MCP + gateway)
// with defer_loading: true on the cold set. Anthropic strips deferred entries from
// the cache-key hash before caching, so tools[] stays byte-stable across sessions
// while retaining full input_schema for tool_search's BM25/regex matching.
//
// Caller is responsible for ensuring at least one tool (typically tool_search
// itself) is non-deferred — verify with hasAnyNonDeferred.
func buildFullSchemasWithDefer(reg *ToolRegistry, cold map[string]bool) []client.Tool {
	out := make([]client.Tool, 0)
	for _, name := range reg.SortedNames() {
		tool, ok := reg.Get(name)
		if !ok {
			continue
		}
		s := buildToolSchema(tool)
		if cold[name] {
			s.DeferLoading = true
		}
		out = append(out, s)
	}
	return out
}

// deferredToolSummariesForNames returns sorted summaries for the named deferred tools.
func deferredToolSummariesForNames(reg *ToolRegistry, names map[string]bool) []ToolSummary {
	if len(names) == 0 {
		return nil
	}

	all := make([]string, 0, len(names))
	for name := range names {
		all = append(all, name)
	}
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
