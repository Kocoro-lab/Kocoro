package tools

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Kocoro-lab/ShanClaw/internal/agent"
	"github.com/Kocoro-lab/ShanClaw/internal/client"
)

// fakeLocalTool is a minimal agent.Tool for exercising name-collision priority.
type fakeLocalTool struct{ name string }

func (f fakeLocalTool) Info() agent.ToolInfo { return agent.ToolInfo{Name: f.name} }
func (f fakeLocalTool) Run(context.Context, string) (agent.ToolResult, error) {
	return agent.ToolResult{Content: "local"}, nil
}
func (f fakeLocalTool) RequiresApproval() bool { return false }

func TestIntegrationTool_Metadata(t *testing.T) {
	tool := NewIntegrationTool(client.ServerToolSchema{Name: "notion_search"}, nil)
	if tool.RequiresApproval() {
		t.Error("integration tools should not require local approval")
	}
	if tool.ToolSource() != agent.SourceIntegration {
		t.Errorf("ToolSource = %q, want %q", tool.ToolSource(), agent.SourceIntegration)
	}
}

// TestIntegrationTool_Run_HitsIntegrationEndpoint verifies the tool proxies to
// the integrations execute endpoint (not the generic gateway tools endpoint).
func TestIntegrationTool_Run_HitsIntegrationEndpoint(t *testing.T) {
	var gotPath string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(toolExecResp(true, map[string]any{"pages": []string{"p1"}}, nil))
	}))
	defer server.Close()

	gw := client.NewGatewayClient(server.URL, "")
	tool := NewIntegrationTool(client.ServerToolSchema{Name: "notion_search"}, gw)

	result, err := tool.Run(context.Background(), `{"query":"x"}`)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.IsError {
		t.Fatalf("unexpected tool error: %s", result.Content)
	}
	if want := "/api/v1/integrations/tools/notion_search/execute"; gotPath != want {
		t.Errorf("hit path %q, want %q", gotPath, want)
	}
	if !strings.Contains(result.Content, "p1") {
		t.Errorf("expected output to contain 'p1', got %q", result.Content)
	}
}

func TestRegisterIntegrationTools_NilGateway_NoOp(t *testing.T) {
	reg := agent.NewToolRegistry()
	if err := RegisterIntegrationTools(context.Background(), nil, reg); err != nil {
		t.Fatalf("nil gateway should be a no-op, got %v", err)
	}
	if len(reg.All()) != 0 {
		t.Errorf("expected empty registry, got %d tools", len(reg.All()))
	}
}

func TestRegisterIntegrationTools_RegistersAndRespectsLocalPriority(t *testing.T) {
	schemas := []client.ServerToolSchema{
		{Name: "notion_search", Description: "Search Notion"},
		{Name: "slack_post", Description: "Post to Slack"},
		{Name: "file_read", Description: "cloud dupe that must NOT override local"},
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/integrations/tools" {
			t.Errorf("unexpected list path %q", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(schemas)
	}))
	defer server.Close()

	gw := client.NewGatewayClient(server.URL, "")
	reg := agent.NewToolRegistry()
	reg.Register(fakeLocalTool{name: "file_read"}) // pre-existing local tool

	if err := RegisterIntegrationTools(context.Background(), gw, reg); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	for _, name := range []string{"notion_search", "slack_post"} {
		got, ok := reg.Get(name)
		if !ok {
			t.Errorf("integration tool %q not registered", name)
			continue
		}
		if sourcer, ok := got.(agent.ToolSourcer); !ok || sourcer.ToolSource() != agent.SourceIntegration {
			t.Errorf("tool %q not marked as integration source", name)
		}
	}

	// Local file_read must win — the cloud dupe must not have replaced it.
	got, _ := reg.Get("file_read")
	if sourcer, ok := got.(agent.ToolSourcer); ok && sourcer.ToolSource() == agent.SourceIntegration {
		t.Error("integration tool overrode a local tool of the same name")
	}
}

// TestRegisterIntegrationTools_ListFailurePreservesExisting verifies that a
// failed Cloud round-trip leaves the previously registered integration tools in
// place (fetch-then-replace), rather than wiping them.
func TestRegisterIntegrationTools_ListFailurePreservesExisting(t *testing.T) {
	var fail bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if fail {
			w.WriteHeader(http.StatusBadGateway)
			w.Write([]byte(`{"error":"upstream down"}`))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode([]client.ServerToolSchema{{Name: "notion_search"}})
	}))
	defer server.Close()

	gw := client.NewGatewayClient(server.URL, "")
	reg := agent.NewToolRegistry()

	// First sync succeeds → notion_search registered.
	if err := RegisterIntegrationTools(context.Background(), gw, reg); err != nil {
		t.Fatalf("first sync: %v", err)
	}
	if _, ok := reg.Get("notion_search"); !ok {
		t.Fatal("notion_search should be registered after successful sync")
	}

	// Second sync fails → must return error AND keep the existing tool.
	fail = true
	if err := RegisterIntegrationTools(context.Background(), gw, reg); err == nil {
		t.Error("expected error when list fails")
	}
	if _, ok := reg.Get("notion_search"); !ok {
		t.Error("notion_search must survive a failed refresh (fetch-then-replace)")
	}
}

func TestRegisterIntegrationTools_NotFoundIsOptionalAndPreservesExisting(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.NotFound(w, r)
	}))
	defer server.Close()

	gw := client.NewGatewayClient(server.URL, "")
	reg := agent.NewToolRegistry()
	reg.Register(NewIntegrationTool(client.ServerToolSchema{Name: "notion_search"}, gw))

	if err := RegisterIntegrationTools(context.Background(), gw, reg); err != nil {
		t.Fatalf("404 feature absence should be a no-op, got %v", err)
	}
	if _, ok := reg.Get("notion_search"); !ok {
		t.Error("existing integration tool must survive a feature-absent refresh")
	}
}

// TestRegisterIntegrationTools_RefreshDropsStale verifies a second call reflects
// the current active set: tools no longer returned are removed, so a
// disconnected provider's tools disappear.
func TestRegisterIntegrationTools_RefreshDropsStale(t *testing.T) {
	var current []client.ServerToolSchema
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(current)
	}))
	defer server.Close()

	gw := client.NewGatewayClient(server.URL, "")
	reg := agent.NewToolRegistry()

	// First sync: notion + slack connected.
	current = []client.ServerToolSchema{{Name: "notion_search"}, {Name: "slack_post"}}
	if err := RegisterIntegrationTools(context.Background(), gw, reg); err != nil {
		t.Fatalf("first sync: %v", err)
	}
	if _, ok := reg.Get("slack_post"); !ok {
		t.Fatal("slack_post should be registered after first sync")
	}

	// Second sync: slack disconnected — only notion remains.
	current = []client.ServerToolSchema{{Name: "notion_search"}}
	if err := RegisterIntegrationTools(context.Background(), gw, reg); err != nil {
		t.Fatalf("second sync: %v", err)
	}
	if _, ok := reg.Get("slack_post"); ok {
		t.Error("slack_post should have been dropped after disconnect")
	}
	if _, ok := reg.Get("notion_search"); !ok {
		t.Error("notion_search should still be registered")
	}
}
