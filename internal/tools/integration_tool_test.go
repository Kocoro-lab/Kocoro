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
	if _, ok := agent.Tool(tool).(agent.ReadOnlyChecker); ok {
		t.Error("server tools must not couple journal materiality to speculative read-only execution")
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

func TestIntegrationTool_PostDispatchResponseLossIsOutcomeUnknown(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, _, err := w.(http.Hijacker).Hijack()
		if err != nil {
			t.Errorf("hijack: %v", err)
			return
		}
		_ = conn.Close()
	}))
	defer server.Close()

	tool := NewIntegrationTool(
		client.ServerToolSchema{Name: "slack_post_message"},
		client.NewGatewayClient(server.URL, ""),
	)
	result, err := tool.Run(context.Background(), `{"channel":"c","text":"hello"}`)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !result.IsError || !result.SideEffectOutcomeUnknown {
		t.Fatalf("result = %#v, want outcome unknown", result)
	}
	if result.IsRetryable || result.ErrorCategory != "" {
		t.Fatalf("outcome unknown must not be retryable: %#v", result)
	}
	if strings.Contains(result.Content, server.URL) {
		t.Fatalf("outcome-unknown diagnostic leaked gateway URL: %q", result.Content)
	}
}

func TestIntegrationTool_MalformedSuccessResponseIsOutcomeUnknown(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"success":`))
	}))
	defer server.Close()

	tool := NewIntegrationTool(
		client.ServerToolSchema{Name: "notion_create_page"},
		client.NewGatewayClient(server.URL, ""),
	)
	result, err := tool.Run(context.Background(), `{"title":"x"}`)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !result.IsError || !result.SideEffectOutcomeUnknown {
		t.Fatalf("result = %#v, want outcome unknown", result)
	}
}

func TestIntegrationTool_ConnectionRefusedIsTransientNoEffect(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	baseURL := server.URL
	server.Close()

	tool := NewIntegrationTool(
		client.ServerToolSchema{Name: "slack_post_message"},
		client.NewGatewayClient(baseURL, ""),
	)
	result, err := tool.Run(context.Background(), `{"channel":"c","text":"hello"}`)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !result.IsError || result.SideEffectOutcomeUnknown {
		t.Fatalf("result = %#v, want known pre-dispatch failure", result)
	}
	if result.ErrorCategory != agent.ErrCategoryTransient || !result.IsRetryable {
		t.Fatalf("result = %#v, want retryable transient", result)
	}
	if !result.SideEffectKnownNoEffect {
		t.Fatalf("result = %#v, want explicit no-effect marker", result)
	}
}

func TestIntegrationTool_HTTPStatusPolicy(t *testing.T) {
	tests := []struct {
		status       int
		wantUnknown  bool
		wantCategory agent.ErrorCategory
		wantRetry    bool
	}{
		{status: http.StatusBadRequest, wantCategory: agent.ErrCategoryValidation},
		{status: http.StatusUnprocessableEntity, wantCategory: agent.ErrCategoryValidation},
		{status: http.StatusUnauthorized, wantCategory: agent.ErrCategoryPermission},
		{status: http.StatusForbidden, wantCategory: agent.ErrCategoryPermission},
		{status: http.StatusNotFound, wantCategory: agent.ErrCategoryBusiness},
		{status: http.StatusConflict, wantUnknown: true},
		{status: http.StatusRequestTimeout, wantUnknown: true},
		{status: http.StatusInternalServerError, wantUnknown: true},
		{status: http.StatusBadGateway, wantUnknown: true},
		{status: http.StatusNotImplemented, wantUnknown: true},
		{status: http.StatusGatewayTimeout, wantUnknown: true},
		{status: http.StatusTooManyRequests, wantCategory: agent.ErrCategoryTransient, wantRetry: true},
		{status: http.StatusServiceUnavailable, wantUnknown: true},
	}
	for _, tt := range tests {
		t.Run(http.StatusText(tt.status), func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(tt.status)
				_, _ = w.Write([]byte(`{"error":"cloud rejected request"}`))
			}))
			defer server.Close()

			tool := NewIntegrationTool(
				client.ServerToolSchema{Name: "slack_post_message"},
				client.NewGatewayClient(server.URL, ""),
			)
			result, err := tool.Run(context.Background(), `{"channel":"c","text":"hello"}`)
			if err != nil {
				t.Fatalf("Run: %v", err)
			}
			if !result.IsError || result.SideEffectOutcomeUnknown != tt.wantUnknown {
				t.Fatalf("result = %#v, want unknown=%t", result, tt.wantUnknown)
			}
			if result.ErrorCategory != tt.wantCategory || result.IsRetryable != tt.wantRetry {
				t.Fatalf("result = %#v, want category=%q retry=%t", result, tt.wantCategory, tt.wantRetry)
			}
			wantNoEffect := !tt.wantUnknown
			if result.SideEffectKnownNoEffect != wantNoEffect {
				t.Fatalf("SideEffectKnownNoEffect = %t, want %t", result.SideEffectKnownNoEffect, wantNoEffect)
			}
		})
	}
}

func TestMaterialServerTool_PreCancelledContextDoesNotDispatch(t *testing.T) {
	var calls int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(toolExecResp(true, map[string]any{"ok": true}, nil))
	}))
	defer server.Close()

	gw := client.NewGatewayClient(server.URL, "")
	tools := []*ServerTool{
		NewIntegrationTool(client.ServerToolSchema{Name: "slack_post_message"}, gw),
		NewServerTool(client.ServerToolSchema{Name: "web_crawl"}, gw),
	}
	for _, tool := range tools {
		t.Run(tool.Info().Name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			cancel()
			result, err := tool.Run(ctx, `{}`)
			if err != nil {
				t.Fatalf("Run: %v", err)
			}
			if !result.IsError || result.SideEffectOutcomeUnknown || result.IsRetryable ||
				!result.SideEffectKnownNoEffect {
				t.Fatalf("result = %#v, want cancelled/no-effect", result)
			}
		})
	}
	if calls != 0 {
		t.Fatalf("gateway calls = %d, want 0", calls)
	}
}

func TestIntegrationTool_CreateRequestFailureIsNonRetryableNoEffect(t *testing.T) {
	tool := NewIntegrationTool(
		client.ServerToolSchema{Name: "slack_post_message"},
		client.NewGatewayClient("://invalid-base-url", ""),
	)
	result, err := tool.Run(context.Background(), `{"channel":"c","text":"hello"}`)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !result.IsError || result.IsRetryable || result.ErrorCategory != agent.ErrCategoryBusiness ||
		!result.SideEffectKnownNoEffect || result.SideEffectOutcomeUnknown {
		t.Fatalf("result = %#v, want non-retryable pre-dispatch no-effect", result)
	}
}

func TestIntegrationTool_ProviderValidationTextIsNotKnownNoEffect(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(toolExecResp(false, nil, strPtr("validation error: provider rejected content")))
	}))
	defer server.Close()

	gw := client.NewGatewayClient(server.URL, "")
	tools := []*ServerTool{
		NewIntegrationTool(client.ServerToolSchema{Name: "notion_create_page"}, gw),
		NewServerTool(client.ServerToolSchema{Name: "web_crawl"}, gw),
	}
	for _, tool := range tools {
		t.Run(tool.Info().Name, func(t *testing.T) {
			result, err := tool.Run(context.Background(), `{}`)
			if err != nil {
				t.Fatalf("Run: %v", err)
			}
			if !result.IsError || result.ErrorCategory != "" || result.SideEffectKnownNoEffect {
				t.Fatalf("result = %#v, want ordinary provider error without no-effect evidence", result)
			}
		})
	}
}

func TestGatewayNonMaterialTool_TransportLossStaysOrdinaryError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, _, err := w.(http.Hijacker).Hijack()
		if err != nil {
			t.Errorf("hijack: %v", err)
			return
		}
		_ = conn.Close()
	}))
	defer server.Close()

	tool := NewServerTool(
		client.ServerToolSchema{Name: "web_search"},
		client.NewGatewayClient(server.URL, ""),
	)
	result, err := tool.Run(context.Background(), `{"query":"x"}`)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !result.IsError || result.SideEffectOutcomeUnknown {
		t.Fatalf("result = %#v, want ordinary read-only transport error", result)
	}
}

func TestGatewayMaterialTool_ResponseLossIsOutcomeUnknown(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, _, err := w.(http.Hijacker).Hijack()
		if err != nil {
			t.Errorf("hijack: %v", err)
			return
		}
		_ = conn.Close()
	}))
	defer server.Close()

	tool := NewServerTool(
		client.ServerToolSchema{Name: "web_crawl"},
		client.NewGatewayClient(server.URL, ""),
	)
	result, err := tool.Run(context.Background(), `{"url":"https://example.test"}`)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !result.IsError || !result.SideEffectOutcomeUnknown || result.IsRetryable {
		t.Fatalf("result = %#v, want non-retryable outcome unknown", result)
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
