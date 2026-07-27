package agent

import (
	"context"
	"strings"
	"testing"
)

type searchMetadataTool struct {
	name      string
	desc      string
	params    map[string]any
	source    ToolSource
	namespace string
}

func (t *searchMetadataTool) Info() ToolInfo {
	return ToolInfo{
		Name:        t.name,
		Description: t.desc,
		Parameters:  t.params,
	}
}

func (t *searchMetadataTool) Run(context.Context, string) (ToolResult, error) {
	return ToolResult{}, nil
}

func (t *searchMetadataTool) RequiresApproval() bool { return false }
func (t *searchMetadataTool) ToolSource() ToolSource { return t.source }
func (t *searchMetadataTool) ToolSearchNamespace() string {
	return t.namespace
}

func TestBuildToolSearchDocument_DerivesExistingMetadata(t *testing.T) {
	tool := &searchMetadataTool{
		name:      "calendar_create_event",
		desc:      "Create an event.",
		source:    SourceMCP,
		namespace: "google_workspace",
		params: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"calendar_id": map[string]any{
					"type":        "string",
					"description": "Target calendar identifier.",
				},
				"attendees": map[string]any{
					"type": "array",
					"items": map[string]any{
						"anyOf": []any{
							map[string]any{
								"type":        "string",
								"description": "Guest email address.",
							},
						},
					},
				},
			},
		},
	}

	document := buildToolSearchDocument(tool)
	for _, want := range []string{
		"calendar_create_event",
		"calendar create event",
		"Create an event.",
		"calendar_id",
		"Target calendar identifier.",
		"attendees",
		"Guest email address.",
		"google_workspace",
	} {
		if !strings.Contains(document, want) {
			t.Fatalf("search document missing %q:\n%s", want, document)
		}
	}
}

func TestToolSearchBM25_MatchesSchemaPropertyAndNestedDescription(t *testing.T) {
	reg := NewToolRegistry()
	reg.Register(&searchMetadataTool{
		name:   "calendar_list_events",
		desc:   "List events.",
		source: SourceMCP,
		params: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"recurrence_rule": map[string]any{
					"type":        "string",
					"description": "RRULE for repeating appointments.",
				},
			},
		},
	})
	reg.Register(&searchMetadataTool{
		name:   "notion_list_pages",
		desc:   "List pages.",
		source: SourceMCP,
		params: map[string]any{"type": "object"},
	})

	ts := newToolSearchTool(reg, deferredToolNames(reg))
	got := ts.matchKeyword("repeating recurrence")
	if len(got) != 1 || got[0] != "calendar_list_events" {
		t.Fatalf("schema metadata search = %v, want [calendar_list_events]", got)
	}
}

func TestToolSearchBM25_MatchesSourceNamespace(t *testing.T) {
	reg := NewToolRegistry()
	reg.Register(&searchMetadataTool{
		name:      "list_events",
		desc:      "List records.",
		source:    SourceMCP,
		namespace: "google_calendar",
		params:    map[string]any{"type": "object"},
	})
	reg.Register(&searchMetadataTool{
		name:      "list_pages",
		desc:      "List records.",
		source:    SourceMCP,
		namespace: "notion",
		params:    map[string]any{"type": "object"},
	})

	ts := newToolSearchTool(reg, deferredToolNames(reg))
	got := ts.matchKeyword("google calendar")
	if len(got) != 1 || got[0] != "list_events" {
		t.Fatalf("namespace search = %v, want [list_events]", got)
	}
}

func TestToolSearchBM25_RareDiscriminatingTermRanksFirst(t *testing.T) {
	reg := NewToolRegistry()
	reg.Register(&searchMetadataTool{
		name:   "generic_records",
		desc:   "Search shared calendar records and entries.",
		source: SourceMCP,
		params: map[string]any{"type": "object"},
	})
	reg.Register(&searchMetadataTool{
		name:   "calendar_recurrence",
		desc:   "Search shared calendar records.",
		source: SourceMCP,
		params: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"rrule": map[string]any{
					"description": "Recurrence schedule for repeating events.",
				},
			},
		},
	})

	ts := newToolSearchTool(reg, deferredToolNames(reg))
	got := ts.matchKeyword("calendar recurrence")
	if len(got) < 2 || got[0] != "calendar_recurrence" {
		t.Fatalf("BM25 ranking = %v, want calendar_recurrence first", got)
	}
}

func TestToolSearchBM25_NameTermsOutrankIncidentalDescriptionMatch(t *testing.T) {
	reg := NewToolRegistry()
	reg.Register(&searchMetadataTool{
		name:   "aaa_records",
		desc:   "Calendar records for users.",
		source: SourceMCP,
		params: map[string]any{"type": "object"},
	})
	reg.Register(&searchMetadataTool{
		name:   "calendar_sync",
		desc:   "Synchronize records.",
		source: SourceMCP,
		params: map[string]any{"type": "object"},
	})

	ts := newToolSearchTool(reg, deferredToolNames(reg))
	got := ts.matchKeyword("calendar")
	if len(got) < 2 || got[0] != "calendar_sync" {
		t.Fatalf("name-weighted ranking = %v, want calendar_sync first", got)
	}
}

func TestToolSearchBM25_BoundsRankedSeedsToEight(t *testing.T) {
	reg := NewToolRegistry()
	for _, name := range []string{
		"tool_a", "tool_b", "tool_c", "tool_d", "tool_e", "tool_f",
		"tool_g", "tool_h", "tool_i", "tool_j", "tool_k", "tool_l",
	} {
		reg.Register(&searchMetadataTool{
			name:   name,
			desc:   "Handle shared calendaring workflows.",
			source: SourceMCP,
			params: map[string]any{"type": "object"},
		})
	}

	ts := newToolSearchTool(reg, deferredToolNames(reg))
	if got := ts.matchKeyword("calendaring"); len(got) != toolSearchDefaultLimit {
		t.Fatalf("ranked seed count = %d (%v), want %d", len(got), got, toolSearchDefaultLimit)
	}
}

func TestToolSearchBM25_StableTieUsesCanonicalOrder(t *testing.T) {
	reg := NewToolRegistry()
	for _, name := range []string{"zeta_tool", "alpha_tool", "middle_tool"} {
		reg.Register(&searchMetadataTool{
			name:   name,
			desc:   "Perform identical quasar operation.",
			source: SourceMCP,
			params: map[string]any{"type": "object"},
		})
	}

	ts := newToolSearchTool(reg, deferredToolNames(reg))
	got := ts.matchKeyword("quasar")
	want := []string{"alpha_tool", "middle_tool", "zeta_tool"}
	if len(got) != len(want) {
		t.Fatalf("tie results = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("tie results = %v, want %v", got, want)
		}
	}
}

func TestToolSearchBM25_CJKMetadataDoesNotScoreZero(t *testing.T) {
	reg := NewToolRegistry()
	reg.Register(&searchMetadataTool{
		name:   "send_mail",
		desc:   "发送邮件到指定地址",
		source: SourceMCP,
		params: map[string]any{"type": "object"},
	})
	reg.Register(&searchMetadataTool{
		name:   "list_events",
		desc:   "列出日历事件",
		source: SourceMCP,
		params: map[string]any{"type": "object"},
	})

	ts := newToolSearchTool(reg, deferredToolNames(reg))
	got := ts.matchKeyword("邮件")
	if len(got) == 0 || got[0] != "send_mail" {
		t.Fatalf("CJK search = %v, want send_mail ranked first", got)
	}
}

func TestWorkingSet_ToolSearchIndexCacheReusesAndRebuildsByFingerprint(t *testing.T) {
	reg := NewToolRegistry()
	reg.Register(&searchMetadataTool{
		name:      "list_events",
		desc:      "List records.",
		source:    SourceMCP,
		namespace: "calendar",
		params:    map[string]any{"type": "object"},
	})
	deferred := deferredToolNames(reg)
	ws := NewWorkingSet()

	first := ws.toolSearchIndex(reg, deferred)
	second := ws.toolSearchIndex(reg, deferred)
	if first != second {
		t.Fatal("identical deferred metadata should reuse the cached search index")
	}

	reg.Register(&searchMetadataTool{
		name:      "list_events",
		desc:      "List records.",
		source:    SourceMCP,
		namespace: "workspace_calendar",
		params:    map[string]any{"type": "object"},
	})
	changed := ws.toolSearchIndex(reg, deferredToolNames(reg))
	if changed == first {
		t.Fatal("namespace metadata change must rebuild the cached search index")
	}
}

func TestExpandDeferredFamilyCore_PreservesRankedSeedsBeforeExpansion(t *testing.T) {
	reg := NewToolRegistry()
	deferred := make(map[string]bool)
	for _, name := range FamilyRegistry["browser"].Core {
		reg.Register(&mockMCPTool{name: name})
		deferred[name] = true
	}

	expanded := expandDeferredFamilyCore(reg, deferred, []string{"browser_navigate"})
	if len(expanded) == 0 || expanded[0] != "browser_navigate" {
		t.Fatalf("family expansion lost ranked seed order: %v", expanded)
	}
}
