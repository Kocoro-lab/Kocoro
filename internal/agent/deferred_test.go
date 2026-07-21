package agent

import (
	"context"
	"sort"
	"strings"
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

// --- toolSearchTool tests (runtime implementation in deferred.go) ---

func newTestToolSearchAgent() *toolSearchTool {
	reg := NewToolRegistry()
	reg.Register(&mockMCPTool{name: "mock_mcp_a"})
	reg.Register(&mockMCPTool{name: "mock_mcp_b"})
	reg.Register(&mockTool{name: "bash"}) // local tool, not deferred

	deferred := map[string]bool{
		"mock_mcp_a": true,
		"mock_mcp_b": true,
	}
	return newToolSearchTool(reg, deferred)
}

func TestToolSearchTool_SelectExact(t *testing.T) {
	ts := newTestToolSearchAgent()
	result, err := ts.Run(context.Background(), `{"query":"select:mock_mcp_a,mock_mcp_b"}`)
	if err != nil {
		t.Fatal(err)
	}
	if result.IsError {
		t.Fatalf("unexpected error: %s", result.Content)
	}
	header := strings.SplitN(result.Content, "\n", 2)[0]
	if !strings.Contains(header, "mock_mcp_a") || !strings.Contains(header, "mock_mcp_b") {
		t.Errorf("LOADED header should contain both tools, got: %s", header)
	}
}

func TestToolSearchTool_KeywordSearch(t *testing.T) {
	ts := newTestToolSearchAgent()
	// mockMCPTool has description "mock mcp tool"
	result, err := ts.Run(context.Background(), `{"query":"mcp"}`)
	if err != nil {
		t.Fatal(err)
	}
	header := strings.SplitN(result.Content, "\n", 2)[0]
	if !strings.Contains(header, "mock_mcp_a") {
		t.Errorf("keyword 'mcp' should match mock_mcp_a, got header: %s", header)
	}
}

func TestToolSearchTool_KeywordSearchTokenizesMultiWordQuery(t *testing.T) {
	reg := NewToolRegistry()
	reg.Register(&mockMCPTool{name: "search_gmail_messages"})
	reg.Register(&mockMCPTool{name: "get_gmail_messages_content_batch"})
	reg.Register(&mockMCPTool{name: "calendar_list_events"})
	ts := newToolSearchTool(reg, map[string]bool{
		"search_gmail_messages":            true,
		"get_gmail_messages_content_batch": true,
		"calendar_list_events":             true,
	})

	result, err := ts.Run(context.Background(), `{"query":"email mail inbox"}`)
	if err != nil {
		t.Fatal(err)
	}
	header := strings.SplitN(result.Content, "\n", 2)[0]
	if !strings.Contains(header, "search_gmail_messages") {
		t.Fatalf("multi-word query should match Gmail tools by token; got header: %s", header)
	}
	if strings.Contains(header, "calendar_list_events") {
		t.Fatalf("multi-word Gmail query should not match unrelated calendar tool; got header: %s", header)
	}
}

// mockDescTool is an MCP-classified tool with a caller-supplied description, so
// keyword-match tests can exercise matching against non-default (e.g. CJK) text.
type mockDescTool struct {
	name string
	desc string
}

func (m *mockDescTool) Info() ToolInfo {
	return ToolInfo{Name: m.name, Description: m.desc, Parameters: map[string]any{"type": "object", "properties": map[string]any{}}}
}
func (m *mockDescTool) Run(context.Context, string) (ToolResult, error) { return ToolResult{}, nil }
func (m *mockDescTool) RequiresApproval() bool                          { return false }
func (m *mockDescTool) ToolSource() ToolSource                          { return SourceMCP }
func (m *mockDescTool) IsReadOnlyCall(string) bool                      { return false }

func TestToolSearchTool_KeywordSearchCJKFallback(t *testing.T) {
	reg := NewToolRegistry()
	reg.Register(&mockDescTool{name: "send_gmail", desc: "发送邮件到指定地址"})
	reg.Register(&mockDescTool{name: "calendar_list", desc: "list calendar events"})
	ts := newToolSearchTool(reg, map[string]bool{"send_gmail": true, "calendar_list": true})

	result, err := ts.Run(context.Background(), `{"query":"邮件"}`)
	if err != nil {
		t.Fatal(err)
	}
	header := strings.SplitN(result.Content, "\n", 2)[0]
	if !strings.Contains(header, "send_gmail") {
		t.Fatalf("CJK query 邮件 should match tool with Chinese description; got header: %s", header)
	}
	if strings.Contains(header, "calendar_list") {
		t.Fatalf("CJK query should not match unrelated tool; got header: %s", header)
	}
}

func TestToolSearchTool_KeywordSearchShortToken(t *testing.T) {
	reg := NewToolRegistry()
	reg.Register(&mockDescTool{name: "query_db", desc: "run a db query"})
	reg.Register(&mockDescTool{name: "http_get", desc: "make an http request"})
	ts := newToolSearchTool(reg, map[string]bool{"query_db": true, "http_get": true})

	result, err := ts.Run(context.Background(), `{"query":"db"}`)
	if err != nil {
		t.Fatal(err)
	}
	header := strings.SplitN(result.Content, "\n", 2)[0]
	if !strings.Contains(header, "query_db") {
		t.Fatalf("short token 'db' should match query_db; got header: %s", header)
	}
	if strings.Contains(header, "http_get") {
		t.Fatalf("'db' should not match http_get; got header: %s", header)
	}
}

func TestToolSearchTool_NoMatches(t *testing.T) {
	ts := newTestToolSearchAgent()
	result, err := ts.Run(context.Background(), `{"query":"nonexistent_xyz"}`)
	if err != nil {
		t.Fatal(err)
	}
	header := strings.SplitN(result.Content, "\n", 2)[0]
	if header != "LOADED:" {
		t.Errorf("empty LOADED header expected, got: %s", header)
	}
}

func TestToolSearchTool_OnlySearchesDeferred(t *testing.T) {
	ts := newTestToolSearchAgent()
	result, err := ts.Run(context.Background(), `{"query":"select:bash"}`)
	if err != nil {
		t.Fatal(err)
	}
	header := strings.SplitN(result.Content, "\n", 2)[0]
	if strings.Contains(header, "bash") {
		t.Error("tool_search should not find local tool 'bash'")
	}
}

func TestToolSearchTool_IsReadOnly(t *testing.T) {
	ts := newTestToolSearchAgent()
	if !ts.IsReadOnlyCall("{}") {
		t.Error("tool_search should be read-only")
	}
}

func TestToolSearchTool_RequiresApproval(t *testing.T) {
	ts := newTestToolSearchAgent()
	if ts.RequiresApproval() {
		t.Error("tool_search should not require approval")
	}
}

func TestExpandDeferredFamilyCore_LoadsBrowserCore(t *testing.T) {
	reg := NewToolRegistry()
	deferred := make(map[string]bool)
	for _, name := range FamilyRegistry["browser"].Core {
		reg.Register(&mockMCPTool{name: name})
		deferred[name] = true
	}
	reg.Register(&mockMCPTool{name: "mock_extra"})
	deferred["mock_extra"] = true

	expanded := expandDeferredFamilyCore(reg, deferred, []string{"browser_navigate"})

	if len(expanded) != len(FamilyRegistry["browser"].Core) {
		t.Fatalf("expected browser family core only, got %v", expanded)
	}
	expected := append([]string(nil), FamilyRegistry["browser"].Core...)
	sort.Strings(expected)
	for i, name := range expected {
		if expanded[i] != name {
			t.Fatalf("index %d: expected %q, got %q", i, name, expanded[i])
		}
	}
}

func TestPreseedDeferredSchemas_FiltersToCurrentDeferredSet(t *testing.T) {
	ws := NewWorkingSet()
	ws.Add("mcp_a", client.Tool{Type: "function", Function: client.FunctionDef{Name: "mcp_a"}})
	ws.Add("mcp_b", client.Tool{Type: "function", Function: client.FunctionDef{Name: "mcp_b"}})

	loaded := preseedDeferredSchemas(ws, map[string]bool{
		"mcp_a": true,
	})

	if len(loaded) != 1 {
		t.Fatalf("expected 1 pre-seeded schema, got %d", len(loaded))
	}
	if _, ok := loaded["mcp_a"]; !ok {
		t.Fatal("expected mcp_a to be pre-seeded")
	}
	if _, ok := loaded["mcp_b"]; ok {
		t.Fatal("mcp_b should not be pre-seeded when no longer deferred")
	}
}

func TestRemainingDeferredNames_RemovesWarmedTools(t *testing.T) {
	remaining := remainingDeferredNames(
		map[string]bool{"mcp_a": true, "mcp_b": true},
		map[string]client.Tool{"mcp_a": {Type: "function", Function: client.FunctionDef{Name: "mcp_a"}}},
	)

	if remaining["mcp_a"] {
		t.Fatal("mcp_a should have been removed from deferred list once warmed")
	}
	if !remaining["mcp_b"] {
		t.Fatal("mcp_b should remain deferred")
	}
}

func TestDeferredToolSummariesForNames_Sorted(t *testing.T) {
	reg := NewToolRegistry()
	reg.Register(&mockMCPTool{name: "mcp_b"})
	reg.Register(&mockMCPTool{name: "mcp_a"})

	summaries := deferredToolSummariesForNames(reg, map[string]bool{
		"mcp_b": true,
		"mcp_a": true,
	})

	if len(summaries) != 2 {
		t.Fatalf("expected 2 summaries, got %d", len(summaries))
	}
	if summaries[0].Name != "mcp_a" || summaries[1].Name != "mcp_b" {
		t.Fatalf("expected sorted summaries, got %+v", summaries)
	}
}

func TestDeferredPromptSync_WarmedToolsBecomeLive(t *testing.T) {
	reg := NewToolRegistry()
	reg.Register(&mockTool{name: "bash"})
	reg.Register(&mockMCPTool{name: "mcp_a"})
	reg.Register(&mockMCPTool{name: "mcp_b"})

	loaded := map[string]client.Tool{
		"mcp_a": buildToolSchema(&mockMCPTool{name: "mcp_a"}),
	}
	remaining := remainingDeferredNames(deferredToolNames(reg), loaded)

	effTools := reg.Clone()
	effTools.Register(newToolSearchTool(reg, remaining))

	baseSchemas := buildLocalOnlySchemas(effTools)
	liveSchemas := rebuildSchemas(effTools, baseSchemas, loaded)
	liveNames := liveToolNames(liveSchemas)

	if !containsString(liveNames, "mcp_a") {
		t.Fatal("warmed tool should be promoted into live tool names")
	}
	if !containsString(liveNames, "tool_search") {
		t.Fatal("tool_search should remain available while cold deferred tools remain")
	}

	summaries := deferredToolSummariesForNames(reg, remaining)
	if len(summaries) != 1 || summaries[0].Name != "mcp_b" {
		t.Fatalf("expected only cold deferred tool in summaries, got %+v", summaries)
	}
}

func containsString(items []string, want string) bool {
	for _, item := range items {
		if item == want {
			return true
		}
	}
	return false
}

func TestToolSearch_ReturnsToolReferenceBlocksAlongsideLegacyString(t *testing.T) {
	reg := NewToolRegistry()
	reg.Register(&mockMCPTool{name: "x_search"})
	reg.Register(&mockMCPTool{name: "web_fetch"})
	deferred := map[string]bool{"x_search": true, "web_fetch": true}

	ts := newToolSearchTool(reg, deferred)
	res, err := ts.Run(context.Background(), `{"query":"select:x_search,web_fetch"}`)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// Legacy Content must still have the LOADED: header (non-breaking fallback)
	if !strings.HasPrefix(res.Content, "LOADED:") {
		t.Fatalf("expected LOADED: prefix in legacy Content, got %q", res.Content)
	}
	// New path: ContentBlocks populated with tool_reference blocks
	if len(res.ContentBlocks) != 2 {
		t.Fatalf("expected 2 tool_reference blocks, got %d: %+v", len(res.ContentBlocks), res.ContentBlocks)
	}
	names := map[string]bool{}
	for _, b := range res.ContentBlocks {
		if b.Type != "tool_reference" {
			t.Fatalf("wrong block type: %q", b.Type)
		}
		names[b.ToolName] = true
	}
	if !names["x_search"] || !names["web_fetch"] {
		t.Fatalf("missing expected tool_reference names: %v", names)
	}
}

func TestToolSearch_EmptyMatchNoBlocks(t *testing.T) {
	reg := NewToolRegistry()
	reg.Register(&mockMCPTool{name: "a"})
	deferred := map[string]bool{"a": true}
	ts := newToolSearchTool(reg, deferred)

	res, err := ts.Run(context.Background(), `{"query":"select:does_not_exist"}`)
	if err != nil {
		t.Fatal(err)
	}
	// Zero matches → zero blocks, legacy Content says no match
	if len(res.ContentBlocks) != 0 {
		t.Fatalf("expected no blocks on empty match, got %d", len(res.ContentBlocks))
	}
	if !strings.Contains(res.Content, "No matching") {
		t.Fatalf("expected 'No matching' in Content, got %q", res.Content)
	}
}

func TestModelSupportsToolRef(t *testing.T) {
	cases := []struct {
		model string
		want  bool
	}{
		{"claude-sonnet-4-5-20250929", true},
		{"claude-sonnet-4-20250514", true},
		{"claude-opus-4-6", true},
		{"claude-opus-4-5", true},
		{"claude-haiku-4-5-20251001", false},  // Haiku excluded per Anthropic docs
		{"claude-3-5-sonnet-20241022", false}, // Pre-4 excluded
		{"gpt-4o", false},
		{"llama3", false},
		{"", false},
	}
	for _, c := range cases {
		if got := modelSupportsToolRef(c.model); got != c.want {
			t.Errorf("modelSupportsToolRef(%q)=%v, want %v", c.model, got, c.want)
		}
	}
}

func TestHasAnyNonDeferred(t *testing.T) {
	all := []client.Tool{
		{Name: "a", DeferLoading: true},
		{Name: "b", DeferLoading: true},
	}
	if hasAnyNonDeferred(all) {
		t.Fatal("expected false when every tool is deferred")
	}
	mixed := []client.Tool{
		{Name: "a", DeferLoading: true},
		{Name: "b"},
	}
	if !hasAnyNonDeferred(mixed) {
		t.Fatal("expected true when at least one tool is non-deferred")
	}
}

// Categorical defer (cache-action-plan §1.2) — local tools whose names match
// shouldDeferByCategory must enter the deferred set even though they are
// classified as "local" by partitionBySource. Without this, big-schema rare-
// use tools (browser_*, computer, schedule_*, …) ride the cold-start tools[]
// for every one-shot CLI session and pay full cache_creation cost.

func TestDeferredToolNames_IncludesLocalCategoricals(t *testing.T) {
	reg := NewToolRegistry()
	// Common local tools that must NOT be deferred:
	reg.Register(&mockTool{name: "bash"})
	reg.Register(&mockTool{name: "file_read"})
	reg.Register(&mockTool{name: "file_write"})
	// Categorical local tools that MUST be deferred:
	reg.Register(&mockTool{name: "computer"})
	reg.Register(&mockTool{name: "schedule_create"})
	reg.Register(&mockTool{name: "browser_navigate"})

	deferred := deferredToolNames(reg)

	mustDefer := []string{"computer", "schedule_create", "browser_navigate"}
	for _, n := range mustDefer {
		if !deferred[n] {
			t.Errorf("expected %q to be in deferred set, got %v", n, mapKeys(deferred))
		}
	}
	mustNotDefer := []string{"bash", "file_read", "file_write"}
	for _, n := range mustNotDefer {
		if deferred[n] {
			t.Errorf("expected %q NOT to be in deferred set", n)
		}
	}
}

func TestDeferredToolNames_BrowserPrefixCovered(t *testing.T) {
	reg := NewToolRegistry()
	reg.Register(&mockTool{name: "browser_click"})
	reg.Register(&mockTool{name: "browser_take_screenshot"})
	reg.Register(&mockTool{name: "browser_run_code"})
	reg.Register(&mockTool{name: "file_read"}) // control: must stay non-deferred

	deferred := deferredToolNames(reg)

	for _, name := range []string{"browser_click", "browser_take_screenshot", "browser_run_code"} {
		if !deferred[name] {
			t.Errorf("browser_* prefix not covered: %q missing from %v", name, mapKeys(deferred))
		}
	}
	if deferred["file_read"] {
		t.Error("file_read must remain non-deferred")
	}
}

// web_search / web_fetch are gateway tools but must be excluded from the
// deferred set (neverDeferTools in toolbudget.go) — they are the common
// first-message-of-a-session tools, and deferring them costs an extra
// tool_search round-trip before every new session's first search. Other
// gateway tools stay deferred-eligible as before.
func TestDeferredToolNames_ExcludesNeverDeferGatewayTools(t *testing.T) {
	reg := NewToolRegistry()
	reg.Register(&mockSourcedTool{name: "web_search", source: SourceGateway})
	reg.Register(&mockSourcedTool{name: "web_fetch", source: SourceGateway})
	reg.Register(&mockSourcedTool{name: "alpaca_news", source: SourceGateway})
	reg.Register(&mockMCPTool{name: "mcp_a"})

	deferred := deferredToolNames(reg)

	for _, n := range []string{"web_search", "web_fetch"} {
		if deferred[n] {
			t.Errorf("expected %q NOT to be in deferred set, got %v", n, mapKeys(deferred))
		}
	}
	for _, n := range []string{"alpaca_news", "mcp_a"} {
		if !deferred[n] {
			t.Errorf("expected %q to remain in deferred set, got %v", n, mapKeys(deferred))
		}
	}
}

func TestHasCategoricalDeferred(t *testing.T) {
	cases := []struct {
		name string
		cold map[string]bool
		want bool
	}{
		{"empty cold set", map[string]bool{}, false},
		{"only mcp tools (non-categorical)", map[string]bool{"mcp_a": true}, false},
		{"contains computer", map[string]bool{"mcp_a": true, "computer": true}, true},
		{"contains browser_*", map[string]bool{"browser_click": true}, true},
		{"contains schedule_*", map[string]bool{"schedule_remove": true}, true},
		{"contains process", map[string]bool{"process": true}, true},
		{"memory_recall no longer always-deferred", map[string]bool{"memory_recall": true}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := hasCategoricalDeferred(tc.cold); got != tc.want {
				t.Errorf("hasCategoricalDeferred(%v) = %v, want %v", tc.cold, got, tc.want)
			}
		})
	}
}

func mapKeys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

// End-to-end schema build (tool-ref-supported path, loop.go's
// buildFullSchemasWithDefer call): with deferredMode active because of an
// unrelated categorical tool (computer), web_search/web_fetch schemas must
// still ship with DeferLoading=false — full schema, not deferred — while the
// categorical tool and an ordinary gateway tool stay DeferLoading=true.
func TestBuildFullSchemasWithDefer_WebToolsNeverDeferred(t *testing.T) {
	reg := NewToolRegistry()
	reg.Register(&mockTool{name: "bash"})
	reg.Register(&mockTool{name: "computer"})
	reg.Register(&mockSourcedTool{name: "web_search", source: SourceGateway})
	reg.Register(&mockSourcedTool{name: "web_fetch", source: SourceGateway})
	reg.Register(&mockSourcedTool{name: "alpaca_news", source: SourceGateway})

	cold := deferredToolNames(reg) // no preseeded/warmed schemas
	if !hasCategoricalDeferred(cold) {
		t.Fatal("expected computer to trigger the categorical-defer condition")
	}

	schemas := buildFullSchemasWithDefer(reg, cold)
	deferByName := make(map[string]bool, len(schemas))
	for _, s := range schemas {
		deferByName[schemaToolName(s)] = s.DeferLoading
	}

	for _, n := range []string{"web_search", "web_fetch"} {
		if deferByName[n] {
			t.Errorf("expected %q to ship with DeferLoading=false, got true", n)
		}
	}
	for _, n := range []string{"computer", "alpaca_news"} {
		if !deferByName[n] {
			t.Errorf("expected %q to ship with DeferLoading=true, got false", n)
		}
	}
	if !hasAnyNonDeferred(schemas) {
		t.Error("expected at least one non-deferred schema (web_search/web_fetch)")
	}
}

// Legacy path (modelTier-based; toolRefSupported=false) must filter cold
// deferred tools out of the active tools[] array. Otherwise categorical local
// tools ride the wire even though deferredMode triggered.
func TestBuildLocalActiveSchemas_FiltersCold(t *testing.T) {
	reg := NewToolRegistry()
	reg.Register(&mockTool{name: "bash"})
	reg.Register(&mockTool{name: "file_read"})
	reg.Register(&mockTool{name: "computer"})
	reg.Register(&mockTool{name: "schedule_create"})
	reg.Register(&mockTool{name: "browser_navigate"})

	cold := map[string]bool{"computer": true, "schedule_create": true, "browser_navigate": true}

	schemas := buildLocalActiveSchemas(reg, cold)
	names := liveToolNames(schemas)

	want := []string{"bash", "file_read"}
	if len(names) != len(want) {
		t.Fatalf("expected %d active tools, got %d: %v", len(want), len(names), names)
	}
	for _, w := range want {
		if !containsString(names, w) {
			t.Errorf("expected %q in active set, got %v", w, names)
		}
	}
	for _, c := range []string{"computer", "schedule_create", "browser_navigate"} {
		if containsString(names, c) {
			t.Errorf("cold tool %q must NOT appear in active schemas", c)
		}
	}
}

func TestBuildLocalActiveSchemas_NoColdReturnsAllLocals(t *testing.T) {
	reg := NewToolRegistry()
	reg.Register(&mockTool{name: "bash"})
	reg.Register(&mockTool{name: "file_read"})

	schemas := buildLocalActiveSchemas(reg, nil)
	if len(schemas) != 2 {
		t.Errorf("nil cold set: expected 2 schemas, got %d", len(schemas))
	}
	schemas = buildLocalActiveSchemas(reg, map[string]bool{})
	if len(schemas) != 2 {
		t.Errorf("empty cold set: expected 2 schemas, got %d", len(schemas))
	}
}

// Legacy path (modelSupportsToolRef returns false — reached for local/
// non-primary providers, small-tier model failover, or the invariant-
// violation downgrade in loop.go) assembles tools[] via
// buildLocalActiveSchemas → rebuildSchemas, and scopes tool_search to
// coldDeferred (deferredToolNames()). Before the neverDeferTools graft in
// buildLocalActiveSchemas, web_search/web_fetch were completely unreachable
// there: buildLocalActiveSchemas is local-only (gateway tools filtered out)
// and deferredToolNames() also excludes them from the cold/tool_search set
// (see toolbudget.go neverDeferTools) — regression introduced by commit
// ca91d14e. This pins the fix: web_search/web_fetch ship full schemas on the
// legacy path, while other gateway tools (alpaca_news) and MCP tools stay
// cold/tool_search-only, in deterministic canonical order (local alpha ->
// MCP alpha -> gateway alpha).
func TestBuildLocalActiveSchemas_LegacyPathWebToolsReachable(t *testing.T) {
	reg := NewToolRegistry()
	reg.Register(&mockTool{name: "bash"})
	reg.Register(&mockTool{name: "file_read"})
	reg.Register(&mockTool{name: "computer"}) // categorical always-defer local tool
	reg.Register(&mockSourcedTool{name: "web_search", source: SourceGateway})
	reg.Register(&mockSourcedTool{name: "web_fetch", source: SourceGateway})
	reg.Register(&mockSourcedTool{name: "alpaca_news", source: SourceGateway}) // must stay tool_search-only
	reg.Register(&mockMCPTool{name: "mcp_a"})

	cold := deferredToolNames(reg)
	if !cold["computer"] {
		t.Fatal("expected computer to be in the cold/deferred set")
	}
	if !cold["alpaca_news"] || !cold["mcp_a"] {
		t.Fatal("expected alpaca_news and mcp_a to remain deferred-eligible (tool_search-only)")
	}
	for _, n := range []string{"web_search", "web_fetch"} {
		if cold[n] {
			t.Fatalf("expected %q to be excluded from the cold set (neverDeferTools)", n)
		}
	}

	baseSchemas := buildLocalActiveSchemas(reg, cold)
	assertLegacyWebToolsAssembly(t, baseSchemas)

	// rebuildSchemas with an empty loaded set (no warmed deferred schemas yet,
	// mirroring loop.go's len(loadedDeferred) > 0 branch) must reproduce the
	// same reachable set via the registry's canonical order.
	rebuilt := rebuildSchemas(reg, baseSchemas, map[string]client.Tool{})
	assertLegacyWebToolsAssembly(t, rebuilt)

	// Determinism: repeated assembly produces byte-identical name ordering.
	again := buildLocalActiveSchemas(reg, cold)
	names1 := liveToolNames(baseSchemas)
	names2 := liveToolNames(again)
	if len(names1) != len(names2) {
		t.Fatalf("non-deterministic length: %v vs %v", names1, names2)
	}
	for i := range names1 {
		if names1[i] != names2[i] {
			t.Errorf("non-deterministic order at index %d: %q vs %q", i, names1[i], names2[i])
		}
	}

	// Canonical order: local alpha (bash, file_read) then gateway alpha
	// (web_fetch, web_search); computer/alpaca_news/mcp_a stay cold.
	wantOrder := []string{"bash", "file_read", "web_fetch", "web_search"}
	if len(names1) != len(wantOrder) {
		t.Fatalf("expected order %v, got %v", wantOrder, names1)
	}
	for i, w := range wantOrder {
		if names1[i] != w {
			t.Errorf("index %d: expected %q, got %q (full: %v)", i, w, names1[i], names1)
		}
	}
}

// assertLegacyWebToolsAssembly checks that web_search/web_fetch carry full
// schemas while other gateway/MCP tools are absent (i.e. still only
// discoverable via tool_search).
func assertLegacyWebToolsAssembly(t *testing.T, schemas []client.Tool) {
	t.Helper()
	names := liveToolNames(schemas)
	for _, n := range []string{"web_search", "web_fetch"} {
		if !containsString(names, n) {
			t.Errorf("expected %q reachable in legacy tool assembly, got %v", n, names)
		}
	}
	for _, n := range []string{"computer", "alpaca_news", "mcp_a"} {
		if containsString(names, n) {
			t.Errorf("expected %q to stay cold/tool_search-only, got %v", n, names)
		}
	}
	for _, s := range schemas {
		name := schemaToolName(s)
		if name != "web_search" && name != "web_fetch" {
			continue
		}
		if s.Function.Name == "" {
			t.Errorf("%q missing full schema (Function.Name empty) — looks like a stub, not a real schema", name)
		}
	}
}

// TestToolSearchSkillExempt asserts that the tool_search meta-tool opts into
// SkillExempt. Without this, a skill that omits tool_search from its
// allowed-tools would lock the model out of loading deferred tool schemas.
func TestToolSearchSkillExempt(t *testing.T) {
	reg := NewToolRegistry()
	tool := newToolSearchTool(reg, map[string]bool{})
	if !IsSkillExempt(tool) {
		t.Errorf("toolSearchTool must implement SkillExempt() returning true; otherwise " +
			"any skill that omits tool_search from allowed-tools traps the model")
	}
}
