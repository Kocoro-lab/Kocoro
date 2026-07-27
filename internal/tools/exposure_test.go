package tools

import (
	"testing"

	"github.com/Kocoro-lab/ShanClaw/internal/agent"
	"github.com/Kocoro-lab/ShanClaw/internal/client"
	mcpproto "github.com/mark3labs/mcp-go/mcp"
)

func assertExposure(t *testing.T, tool agent.Tool, want agent.ToolExposure) {
	t.Helper()
	if got := agent.EffectiveToolExposure(tool); got != want {
		t.Fatalf("%s exposure = %v, want %v", tool.Info().Name, got, want)
	}
}

func TestRegisteredLocalToolExposureMatrix(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	reg, _, cleanup := RegisterLocalTools(nil, nil)
	t.Cleanup(cleanup)

	want := map[string]agent.ToolExposure{
		"use_skill":           agent.ToolExposureDirect,
		"file_read":           agent.ToolExposureDirect,
		"file_write":          agent.ToolExposureDirect,
		"file_edit":           agent.ToolExposureDirect,
		"glob":                agent.ToolExposureDirect,
		"grep":                agent.ToolExposureDirect,
		"bash":                agent.ToolExposureDirect,
		"memory_append":       agent.ToolExposureDirect,
		"ask_user_question":   agent.ToolExposureDirect,
		"think":               agent.ToolExposureDirect,
		"directory_list":      agent.ToolExposureDirect,
		"archive_inspect":     agent.ToolExposureDirect,
		"archive_extract":     agent.ToolExposureDirect,
		"pdf_to_text":         agent.ToolExposureDirect,
		"docx_to_text":        agent.ToolExposureDirect,
		"xlsx_to_text":        agent.ToolExposureDirect,
		"pptx_to_text":        agent.ToolExposureDirect,
		"http":                agent.ToolExposureDirect,
		"system_info":         agent.ToolExposureDirect,
		"clipboard":           agent.ToolExposureDirect,
		"notify":              agent.ToolExposureDirect,
		"present_deliverable": agent.ToolExposureDirect,
		"process":             agent.ToolExposureDeferred,
		"applescript":         agent.ToolExposureDeferred,
		"accessibility":       agent.ToolExposureDeferred,
		"computer_use":        agent.ToolExposureDeferred,
		"ghostty":             agent.ToolExposureDeferred,
		"browser":             agent.ToolExposureDeferred,
		"screenshot":          agent.ToolExposureDeferred,
		"computer":            agent.ToolExposureDeferred,
		"wait_for":            agent.ToolExposureDeferred,
		"schedule_create":     agent.ToolExposureDeferred,
		"schedule_list":       agent.ToolExposureDirect,
		"schedule_update":     agent.ToolExposureDeferred,
		"schedule_remove":     agent.ToolExposureDeferred,
		"schedule_show":       agent.ToolExposureDirect,
	}

	if reg.Len() != len(want) {
		t.Fatalf("registered local tool count = %d, matrix count = %d; update the exhaustive exposure matrix", reg.Len(), len(want))
	}
	for _, tool := range reg.All() {
		name := tool.Info().Name
		exposure, ok := want[name]
		if !ok {
			t.Fatalf("registered local tool %q is missing from the exposure matrix", name)
		}
		assertExposure(t, tool, exposure)
	}
}

func TestCalendarToolExposureMatrix(t *testing.T) {
	tests := []struct {
		tool agent.Tool
		want agent.ToolExposure
	}{
		{tool: &CalendarCheckPermissionTool{}, want: agent.ToolExposureDirect},
		{tool: &CalendarRequestPermissionTool{}, want: agent.ToolExposureDeferred},
		{tool: &CalendarListSourcesTool{}, want: agent.ToolExposureDirect},
		{tool: &CalendarListEventsTool{}, want: agent.ToolExposureDirect},
		{tool: &CalendarGetEventTool{}, want: agent.ToolExposureDirect},
		{tool: &CalendarCreateEventTool{}, want: agent.ToolExposureDeferred},
		{tool: &CalendarUpdateEventTool{}, want: agent.ToolExposureDeferred},
		{tool: &CalendarDeleteEventTool{}, want: agent.ToolExposureDeferred},
	}
	for _, tc := range tests {
		assertExposure(t, tc.tool, tc.want)
	}
}

func TestConditionalLocalToolExposureMatrix(t *testing.T) {
	tests := []struct {
		tool agent.Tool
		want agent.ToolExposure
	}{
		{tool: &SessionSearchTool{}, want: agent.ToolExposureDirect},
		{tool: &MemoryTool{}, want: agent.ToolExposureDirect},
		{tool: &CloudDelegateTool{}, want: agent.ToolExposureDirect},
		{tool: &PublishToWebTool{}, want: agent.ToolExposureDeferred},
		{tool: &GenerateImageTool{}, want: agent.ToolExposureDeferred},
		{tool: &EditImageTool{}, want: agent.ToolExposureDeferred},
		{tool: &ListPublishedFilesTool{}, want: agent.ToolExposureDeferred},
		{tool: &RetractPublishedFileTool{}, want: agent.ToolExposureDeferred},
	}
	for _, tc := range tests {
		assertExposure(t, tc.tool, tc.want)
	}
}

func TestDynamicSourceExposureAndNamespace(t *testing.T) {
	gateway := &client.GatewayClient{}
	webSearch := NewServerTool(client.ServerToolSchema{Name: "web_search"}, gateway)
	webFetch := NewServerTool(client.ServerToolSchema{Name: "web_fetch"}, gateway)
	longTail := NewServerTool(client.ServerToolSchema{Name: "alpaca_news"}, gateway)
	integration := NewIntegrationTool(client.ServerToolSchema{Name: "notion_search"}, gateway)
	mcpTool := NewMCPTool("google_calendar", mcpproto.Tool{Name: "list_events"}, nil)

	for _, tool := range []agent.Tool{webSearch, webFetch} {
		assertExposure(t, tool, agent.ToolExposureDirect)
	}
	for _, tool := range []agent.Tool{longTail, integration, mcpTool} {
		assertExposure(t, tool, agent.ToolExposureDeferred)
	}
	if got := mcpTool.ToolSearchNamespace(); got != "google_calendar" {
		t.Fatalf("MCP search namespace = %q, want google_calendar", got)
	}
	if got := integration.ToolSearchNamespace(); got != string(agent.SourceIntegration) {
		t.Fatalf("integration search namespace = %q, want %q", got, agent.SourceIntegration)
	}
}
