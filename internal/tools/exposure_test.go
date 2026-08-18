package tools

import (
	"context"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/Kocoro-lab/ShanClaw/internal/agent"
	"github.com/Kocoro-lab/ShanClaw/internal/client"
	"github.com/Kocoro-lab/ShanClaw/internal/config"
	"github.com/Kocoro-lab/ShanClaw/internal/daemon/desktop_rpc"
	"github.com/Kocoro-lab/ShanClaw/internal/executionprofile"
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
	approvalRequired := map[string]bool{
		"file_read":       true,
		"file_write":      true,
		"file_edit":       true,
		"glob":            true,
		"grep":            true,
		"bash":            true,
		"directory_list":  true,
		"archive_extract": true,
		"http":            true,
		"clipboard":       true,
		"notify":          true,
		"process":         true,
		"applescript":     true,
		"accessibility":   true,
		"computer_use":    true,
		"ghostty":         true,
		"browser":         true,
		"screenshot":      true,
		"computer":        true,
		"schedule_create": true,
		"schedule_update": true,
		"schedule_remove": true,
	}

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
		"calculate":           agent.ToolExposureDirect,
		"current_time":        agent.ToolExposureDirect,
		"x_prepare_post":      agent.ToolExposureDeferred,
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
		if got, expected := tool.RequiresApproval(), approvalRequired[name]; got != expected {
			t.Errorf("%s RequiresApproval() = %t, want %t", name, got, expected)
		}
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

func TestDefaultDirectToolSchemaBudget(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	cfg := &config.Config{}
	cfg.Agent.Thinking = true
	reg, _, cleanup := RegisterLocalTools(cfg, nil)
	t.Cleanup(cleanup)

	RegisterMemoryTool(reg, nil, nil)
	RegisterCalendarTools(reg, desktop_rpc.NewDesktopRPCBroker())
	reg.Register(&SessionSearchTool{})
	reg.Register(NewCloudDelegateTool(nil, "", time.Hour, 0, nil, "", ""))

	// Cloud owns the exact web_search/web_fetch schemas. Recent production
	// schemas consume about 2.4K estimated tokens together, so keep a rounded
	// 2.5K reserve while the runtime diagnostic measures their live definitions.
	// Update the reserve from fresh production schemas if Cloud changes them.
	const dynamicDirectSchemaReserveTokens = 2500
	total := agent.EstimateDirectSchemaTokens(reg) + dynamicDirectSchemaReserveTokens
	budget := agent.DirectSchemaTokenBudget()
	type contributor struct {
		name   string
		tokens int
	}
	contributors := make([]contributor, 0, reg.Len())
	for _, name := range reg.SortedNames() {
		tool, ok := reg.Get(name)
		if !ok || agent.EffectiveToolExposure(tool) != agent.ToolExposureDirect {
			continue
		}
		single := agent.NewToolRegistry()
		single.Register(tool)
		contributors = append(contributors, contributor{name: name, tokens: agent.EstimateDirectSchemaTokens(single)})
	}
	sort.Slice(contributors, func(i, j int) bool {
		if contributors[i].tokens != contributors[j].tokens {
			return contributors[i].tokens > contributors[j].tokens
		}
		return contributors[i].name < contributors[j].name
	})
	limit := 32
	if len(contributors) < limit {
		limit = len(contributors)
	}
	t.Logf("default Direct schema estimate=%d budget=%d reserve=%d largest=%+v", total, budget, dynamicDirectSchemaReserveTokens, contributors[:limit])
	if total > budget {
		t.Fatalf(
			"default Direct schemas exceed regression budget: total=%d budget=%d reserve=%d",
			total,
			budget,
			dynamicDirectSchemaReserveTokens,
		)
	}
}

type koeFastExposureCaptureClient struct {
	requests []client.CompletionRequest
}

func (c *koeFastExposureCaptureClient) Complete(_ context.Context, req client.CompletionRequest) (*client.CompletionResponse, error) {
	c.requests = append(c.requests, req)
	return &client.CompletionResponse{OutputText: "ok", FinishReason: "end_turn"}, nil
}

func (c *koeFastExposureCaptureClient) CompleteStream(ctx context.Context, req client.CompletionRequest, _ func(client.StreamDelta)) (*client.CompletionResponse, error) {
	return c.Complete(ctx, req)
}

func TestKoeFastFinalRequestDefersOnlyHeavyLocalLongTail(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	cfg := &config.Config{Agent: config.AgentConfig{Thinking: true}}
	reg, _, cleanup := RegisterLocalTools(cfg, nil)
	t.Cleanup(cleanup)
	RegisterMemoryTool(reg, nil, nil)
	reg.Register(&SessionSearchTool{})
	reg.Register(NewCloudDelegateTool(nil, "", time.Hour, 0, nil, "", ""))

	capture := &koeFastExposureCaptureClient{}
	loop := agent.NewAgentLoop(capture, reg, "medium", t.TempDir(), 2, 2_000, 200, nil, nil, nil)
	loop.SetSkillDiscovery(false)
	loop.SetKoeExecutionProfile(executionprofile.Profile{
		RequestedMode:       executionprofile.ModeFast,
		EffectiveMode:       executionprofile.ModeFast,
		SchemaVersion:       executionprofile.FastSchemaVersion,
		ProfileName:         executionprofile.FastProfileName,
		ProfileVersion:      executionprofile.FastProfileVersion,
		ProfileID:           "kfp1-tool-exposure-test",
		Provider:            "openai",
		Model:               "gpt-5.6-luna",
		APISurface:          "openai_responses",
		ToolContract:        executionprofile.FastToolContract,
		ParallelToolCalls:   true,
		ResponseCachePolicy: executionprofile.ResponseCacheOff,
		ResolutionReason:    "test",
	})
	if _, _, err := loop.Run(context.Background(), "Calculate 17 times 19.", nil, nil); err != nil {
		t.Fatalf("AgentLoop.Run: %v", err)
	}
	if len(capture.requests) != 1 {
		t.Fatalf("completion requests = %d, want 1", len(capture.requests))
	}

	request := capture.requests[0]
	offered := make(map[string]bool, len(request.Tools))
	for _, schema := range request.Tools {
		name := schema.Function.Name
		if name == "" {
			name = schema.Name
		}
		offered[name] = true
	}
	for _, name := range []string{
		"memory_recall", "session_search", "file_read", "file_write", "file_edit", "bash",
		"grep", "glob", "directory_list", "calculate", "current_time", "tool_search",
	} {
		if !offered[name] {
			t.Errorf("Koe Fast final request omitted common opener %q", name)
		}
	}
	for _, name := range []string{
		"cloud_delegate", "archive_extract", "archive_inspect", "pdf_to_text",
		"docx_to_text", "xlsx_to_text", "pptx_to_text", "http", "system_info",
	} {
		if offered[name] {
			t.Errorf("Koe Fast final request exposed cold long-tail schema %q", name)
		}
	}
	messageText := ""
	for _, message := range request.Messages {
		messageText += message.Content.Text()
	}
	for _, name := range []string{"pdf_to_text", "cloud_delegate"} {
		if !strings.Contains(messageText, name) {
			t.Errorf("Koe Fast discovery listing omitted %q", name)
		}
	}
}

func TestDynamicSourceExposureAndNamespace(t *testing.T) {
	gateway := &client.GatewayClient{}
	webSearch := NewServerTool(client.ServerToolSchema{Name: "web_search"}, gateway)
	webFetch := NewServerTool(client.ServerToolSchema{Name: "web_fetch"}, gateway)
	longTail := NewServerTool(client.ServerToolSchema{Name: "alpaca_news"}, gateway)
	xSearch := NewServerTool(client.ServerToolSchema{Name: "x_search"}, gateway)
	spoofedXSearch := NewIntegrationTool(client.ServerToolSchema{Name: "x_search"}, gateway)
	integration := NewIntegrationTool(client.ServerToolSchema{Name: "notion_search"}, gateway)
	mcpTool := NewMCPTool("google_calendar", mcpproto.Tool{Name: "list_events"}, nil)
	spoofedMCPXSearch := NewMCPTool("untrusted", mcpproto.Tool{Name: "x_search"}, nil)

	for _, tool := range []agent.Tool{webSearch, webFetch, xSearch} {
		assertExposure(t, tool, agent.ToolExposureDirect)
	}
	for _, tool := range []agent.Tool{longTail, integration, spoofedXSearch, mcpTool, spoofedMCPXSearch} {
		assertExposure(t, tool, agent.ToolExposureDeferred)
	}
	if got := mcpTool.ToolSearchNamespace(); got != "google_calendar" {
		t.Fatalf("MCP search namespace = %q, want google_calendar", got)
	}
	if got := integration.ToolSearchNamespace(); got != string(agent.SourceIntegration) {
		t.Fatalf("integration search namespace = %q, want %q", got, agent.SourceIntegration)
	}
}
