package tools

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Kocoro-lab/ShanClaw/internal/client"
)

func TestServerTool_Info(t *testing.T) {
	schema := client.ServerToolSchema{
		Name:        "web_search",
		Description: "Search the web for information",
		Parameters:  map[string]any{"type": "object", "properties": map[string]any{"query": map[string]any{"type": "string"}}},
	}
	tool := NewServerTool(schema, nil)
	info := tool.Info()

	if info.Name != "web_search" {
		t.Errorf("expected name web_search, got %s", info.Name)
	}
	if info.Description != "Search the web for information" {
		t.Errorf("unexpected description: %s", info.Description)
	}
}

func TestServerTool_RequiresApproval(t *testing.T) {
	tool := NewServerTool(client.ServerToolSchema{Name: "test"}, nil)
	if tool.RequiresApproval() {
		t.Error("server tools should not require approval")
	}
}

// toolExecResp builds a mock tool execute response matching the gateway format.
func toolExecResp(success bool, output any, errMsg *string) client.ToolExecuteResponse {
	var raw json.RawMessage
	if output != nil {
		raw, _ = json.Marshal(output)
	}
	return client.ToolExecuteResponse{
		Success: success,
		Output:  raw,
		Error:   errMsg,
	}
}

func strPtr(s string) *string { return &s }

func TestServerTool_Run(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(toolExecResp(true, map[string]any{"results": []string{"result1"}}, nil))
	}))
	defer server.Close()

	gw := client.NewGatewayClient(server.URL, "")
	schema := client.ServerToolSchema{Name: "web_search", Description: "Search"}
	tool := NewServerTool(schema, gw)

	result, err := tool.Run(context.Background(), `{"query":"test"}`)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.IsError {
		t.Fatalf("unexpected tool error: %s", result.Content)
	}
	if !strings.Contains(result.Content, "result1") {
		t.Errorf("expected output to contain 'result1', got %q", result.Content)
	}
}

func TestServerTool_Run_EmptyArgs(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(toolExecResp(true, map[string]any{"status": "ok"}, nil))
	}))
	defer server.Close()

	gw := client.NewGatewayClient(server.URL, "")
	tool := NewServerTool(client.ServerToolSchema{Name: "ping"}, gw)

	result, err := tool.Run(context.Background(), "{}")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(result.Content, "ok") {
		t.Errorf("expected output to contain 'ok', got %q", result.Content)
	}
}

func TestServerTool_Run_ServerError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(toolExecResp(false, nil, strPtr("Required parameter 'query' is missing")))
	}))
	defer server.Close()

	gw := client.NewGatewayClient(server.URL, "")
	tool := NewServerTool(client.ServerToolSchema{Name: "failing"}, gw)

	result, err := tool.Run(context.Background(), `{}`)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !result.IsError {
		t.Error("expected IsError=true")
	}
	if !strings.Contains(result.Content, "missing") {
		t.Errorf("expected error about missing param, got %q", result.Content)
	}
}

func TestServerTool_Run_NullOutput(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(toolExecResp(true, nil, nil))
	}))
	defer server.Close()

	gw := client.NewGatewayClient(server.URL, "")
	tool := NewServerTool(client.ServerToolSchema{Name: "noop"}, gw)

	result, err := tool.Run(context.Background(), `{}`)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.Content != "no output" {
		t.Errorf("expected 'no output', got %q", result.Content)
	}
}

func TestServerTool_Run_502_TransientPrefix(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
		w.Write([]byte(`{"error":"Tool service unavailable"}`))
	}))
	defer server.Close()

	gw := client.NewGatewayClient(server.URL, "")
	tool := NewServerTool(client.ServerToolSchema{Name: "x_search"}, gw)

	result, err := tool.Run(context.Background(), `{"query":"test"}`)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !result.IsError {
		t.Error("expected IsError=true for 502")
	}
	if !strings.HasPrefix(result.Content, "[transient error]") {
		t.Errorf("expected [transient error] prefix, got %q", result.Content)
	}
}

func TestServerTool_Run_403_PermissionPrefix(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		w.Write([]byte(`{"error":"access denied"}`))
	}))
	defer server.Close()

	gw := client.NewGatewayClient(server.URL, "")
	tool := NewServerTool(client.ServerToolSchema{Name: "x_search"}, gw)

	result, err := tool.Run(context.Background(), `{"query":"test"}`)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !result.IsError {
		t.Error("expected IsError=true for 403")
	}
	if !strings.HasPrefix(result.Content, "[permission error]") {
		t.Errorf("expected [permission error] prefix, got %q", result.Content)
	}
}

func TestClassifyServerError(t *testing.T) {
	tests := []struct {
		msg  string
		want string
	}{
		{"tool x_search returned 502: {\"error\":\"Tool service unavailable\"}", "[transient error] "},
		{"tool x_search returned 429: rate limited", "[transient error] "},
		{"tool x_search returned 503: service unavailable", "[transient error] "},
		{"request failed: context deadline exceeded (Client.Timeout)", "[transient error] "},
		{"request failed: dial tcp: connection refused", "[transient error] "},
		{"request failed: EOF", "[transient error] "},
		{"tool x_search returned 403: forbidden", "[permission error] "},
		{"tool x_search returned 401: unauthorized", "[permission error] "},
		{"tool x_search returned 400: bad request", "[validation error] "},
		{"tool x_search returned 422: unprocessable entity", "[validation error] "},
		{"tool x_search returned 404: not found", ""},
		{"some unknown error", ""},
	}
	for _, tt := range tests {
		got := classifyServerError(tt.msg)
		if got != tt.want {
			t.Errorf("classifyServerError(%q) = %q, want %q", tt.msg, got, tt.want)
		}
	}
}

func TestServerTool_Run_InvalidJSON(t *testing.T) {
	tool := NewServerTool(client.ServerToolSchema{Name: "test"}, nil)
	result, err := tool.Run(context.Background(), "not json")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !result.IsError {
		t.Error("expected IsError=true for invalid JSON")
	}
}

// --- buildFallbackLadder / metadata.attempts → tool_result content ---

func TestBuildFallbackLadder_Empty(t *testing.T) {
	cases := []struct {
		name string
		meta map[string]any
	}{
		{"nil metadata", nil},
		{"empty metadata", map[string]any{}},
		{"no attempts key", map[string]any{"other": "x"}},
		{"attempts wrong type", map[string]any{"attempts": "not-an-array"}},
		{"empty attempts array", map[string]any{"attempts": []any{}}},
	}
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			if got := buildFallbackLadder(tt.meta); got != "" {
				t.Errorf("expected empty ladder, got %q", got)
			}
		})
	}
}

func TestBuildFallbackLadder_FiltersNonFailures(t *testing.T) {
	meta := map[string]any{
		"attempts": []any{
			map[string]any{"provider": "firecrawl", "status": "success"},
			map[string]any{"provider": "exa", "status": "attempted"},
			map[string]any{"provider": "python", "status": "sparse_fallback"},
			map[string]any{"provider": "exa", "status": "skipped", "reason": "not configured"},
		},
	}
	if got := buildFallbackLadder(meta); got != "" {
		t.Errorf("expected empty (only non-failures present), got %q", got)
	}
}

func TestBuildFallbackLadder_TwoLayerLadder(t *testing.T) {
	meta := map[string]any{
		"attempts": []any{
			map[string]any{"provider": "firecrawl", "status": "failed", "error": "Firecrawl error: 403: do not support this site"},
			map[string]any{"provider": "exa", "status": "failed", "error": "Exa API returned no content"},
		},
	}
	got := buildFallbackLadder(meta)
	if !strings.HasPrefix(got, "Provider attempts:\n") {
		t.Errorf("expected ladder header, got %q", got)
	}
	if !strings.Contains(got, "- firecrawl failed: Firecrawl error: 403: do not support this site") {
		t.Errorf("missing firecrawl line, got %q", got)
	}
	if !strings.Contains(got, "- exa failed: Exa API returned no content") {
		t.Errorf("missing exa line, got %q", got)
	}
}

func TestBuildFallbackLadder_FailedWithoutErrorFallsToReason(t *testing.T) {
	meta := map[string]any{
		"attempts": []any{
			map[string]any{"provider": "firecrawl", "status": "failed", "reason": "rate-limited"},
		},
	}
	got := buildFallbackLadder(meta)
	if !strings.Contains(got, "- firecrawl failed: rate-limited") {
		t.Errorf("expected reason fallback, got %q", got)
	}
}

func TestBuildFallbackLadder_SkippedWithInformativeReason(t *testing.T) {
	meta := map[string]any{
		"attempts": []any{
			map[string]any{"provider": "exa", "status": "skipped", "reason": "domain blocklisted"},
		},
	}
	got := buildFallbackLadder(meta)
	if !strings.Contains(got, "- exa skipped: domain blocklisted") {
		t.Errorf("expected informative skipped line, got %q", got)
	}
}

func TestBuildFallbackLadder_RedactsSecrets(t *testing.T) {
	meta := map[string]any{
		"attempts": []any{
			map[string]any{"provider": "p", "status": "failed", "error": "auth failed: Bearer abc123xyz-token-DEF"},
			map[string]any{"provider": "q", "status": "failed", "error": "key sk-abcdefghijklmnopqrstuvwxyz0123"},
		},
	}
	got := buildFallbackLadder(meta)
	if strings.Contains(got, "abc123xyz-token-DEF") {
		t.Errorf("Bearer token leaked: %q", got)
	}
	if strings.Contains(got, "sk-abcdefghijklmnopqrstuvwxyz0123") {
		t.Errorf("sk- key leaked: %q", got)
	}
	if !strings.Contains(got, "[REDACTED]") {
		t.Errorf("expected [REDACTED] marker, got %q", got)
	}
}

func TestBuildFallbackLadder_TruncatesLongDetail(t *testing.T) {
	longErr := strings.Repeat("x", 500)
	meta := map[string]any{
		"attempts": []any{
			map[string]any{"provider": "p", "status": "failed", "error": longErr},
		},
	}
	got := buildFallbackLadder(meta)
	if !strings.HasSuffix(got, "...") {
		t.Errorf("expected truncation suffix, got %q", got)
	}
	xCount := strings.Count(got, "x")
	if xCount > 210 {
		t.Errorf("not truncated to ~200 chars, x-count=%d, got %q", xCount, got)
	}
}

func TestServerTool_Run_AppendsLadderOnError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		resp := client.ToolExecuteResponse{
			Success: false,
			Error:   strPtr("Exa API returned no content"),
			Metadata: map[string]any{
				"attempts": []any{
					map[string]any{"provider": "firecrawl", "status": "failed", "error": "Firecrawl error: 403: do not support this site"},
					map[string]any{"provider": "exa", "status": "failed", "error": "Exa API returned no content"},
				},
			},
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(resp)
	}))
	defer server.Close()

	gw := client.NewGatewayClient(server.URL, "")
	tool := NewServerTool(client.ServerToolSchema{Name: "web_fetch"}, gw)
	result, err := tool.Run(context.Background(), `{"url":"https://www.reddit.com/r/x"}`)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !result.IsError {
		t.Error("expected IsError=true")
	}
	if !strings.Contains(result.Content, "Exa API returned no content") {
		t.Errorf("expected base error preserved, got %q", result.Content)
	}
	if !strings.Contains(result.Content, "Provider attempts:") {
		t.Errorf("expected ladder header, got %q", result.Content)
	}
	if !strings.Contains(result.Content, "firecrawl failed: Firecrawl error: 403: do not support this site") {
		t.Errorf("expected firecrawl root-cause detail, got %q", result.Content)
	}
}

func TestServerTool_Run_NoLadderOnSuccess(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		resp := client.ToolExecuteResponse{
			Success: true,
			Output:  json.RawMessage(`"ok"`),
			Metadata: map[string]any{
				"attempts": []any{
					map[string]any{"provider": "firecrawl", "status": "failed", "error": "transient"},
					map[string]any{"provider": "exa", "status": "success"},
				},
			},
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(resp)
	}))
	defer server.Close()

	gw := client.NewGatewayClient(server.URL, "")
	tool := NewServerTool(client.ServerToolSchema{Name: "web_fetch"}, gw)
	result, err := tool.Run(context.Background(), `{}`)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.IsError {
		t.Errorf("unexpected error: %s", result.Content)
	}
	if strings.Contains(result.Content, "Provider attempts:") {
		t.Errorf("ladder must not appear on success, got %q", result.Content)
	}
}

func TestServerTool_Run_LadderNotDoubledWhenAlreadyPresent(t *testing.T) {
	preExisting := "base failure\n\nProvider attempts:\n- some line"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		resp := client.ToolExecuteResponse{
			Success: false,
			Error:   &preExisting,
			Metadata: map[string]any{
				"attempts": []any{
					map[string]any{"provider": "firecrawl", "status": "failed", "error": "should-not-be-appended"},
				},
			},
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(resp)
	}))
	defer server.Close()

	gw := client.NewGatewayClient(server.URL, "")
	tool := NewServerTool(client.ServerToolSchema{Name: "web_fetch"}, gw)
	result, err := tool.Run(context.Background(), `{}`)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if strings.Contains(result.Content, "should-not-be-appended") {
		t.Errorf("ladder was doubled, got %q", result.Content)
	}
	if got := strings.Count(result.Content, "Provider attempts:"); got != 1 {
		t.Errorf("expected exactly one 'Provider attempts:' marker, got %d in %q", got, result.Content)
	}
}

func TestServerTool_Run_LadderWhenErrorEmptyAndSuccessFalse(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		resp := client.ToolExecuteResponse{
			Success: false,
			Metadata: map[string]any{
				"attempts": []any{
					map[string]any{"provider": "firecrawl", "status": "failed", "error": "Firecrawl error: 403"},
				},
			},
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(resp)
	}))
	defer server.Close()

	gw := client.NewGatewayClient(server.URL, "")
	tool := NewServerTool(client.ServerToolSchema{Name: "web_fetch"}, gw)
	result, err := tool.Run(context.Background(), `{}`)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !result.IsError {
		t.Error("expected IsError=true")
	}
	if !strings.Contains(result.Content, "tool execution failed") {
		t.Errorf("expected generic base, got %q", result.Content)
	}
	if !strings.Contains(result.Content, "Provider attempts:") {
		t.Errorf("expected ladder, got %q", result.Content)
	}
	if !strings.Contains(result.Content, "firecrawl failed: Firecrawl error: 403") {
		t.Errorf("expected firecrawl detail, got %q", result.Content)
	}
}
