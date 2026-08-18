package tools

import (
	"context"
	"encoding/json"
	"errors"
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

type integrationIdentityLLM struct{ calls int }

func (c *integrationIdentityLLM) Complete(context.Context, client.CompletionRequest) (*client.CompletionResponse, error) {
	c.calls++
	if c.calls == 1 {
		return &client.CompletionResponse{
			FinishReason: "tool_use",
			ToolCalls: []client.FunctionCall{{
				ID: "toolu_search_x_create", Name: "tool_search",
				Arguments: json.RawMessage(`{"query":"select:x_create_post"}`),
			}},
		}, nil
	}
	if c.calls == 2 {
		return &client.CompletionResponse{
			FinishReason: "tool_use",
			ToolCalls: []client.FunctionCall{{
				ID: "toolu_x_create_1", Name: "x_create_post",
				Arguments: json.RawMessage(`{"text":"hello"}`),
			}},
		}, nil
	}
	return &client.CompletionResponse{OutputText: "done", FinishReason: "stop"}, nil
}

func (c *integrationIdentityLLM) CompleteStream(
	ctx context.Context,
	req client.CompletionRequest,
	_ func(client.StreamDelta),
) (*client.CompletionResponse, error) {
	return c.Complete(ctx, req)
}

type integrationIdentityJournal struct {
	prepared  agent.SideEffectExecution
	committed int
	unknown   int
}

func (j *integrationIdentityJournal) Prepare(_ context.Context, execution agent.SideEffectExecution) (agent.PreparedSideEffectExecution, error) {
	j.prepared = execution
	return agent.PreparedSideEffectExecution{
		ExecutionID: "exec-x-create-1", IdempotencyKey: "idem-x-create-1",
	}, nil
}
func (*integrationIdentityJournal) MarkDispatching(context.Context, string) error { return nil }

func (j *integrationIdentityJournal) MarkCommitted(context.Context, string, string) error {
	j.committed++
	return nil
}
func (*integrationIdentityJournal) MarkFailedNoEffect(context.Context, string, string) error {
	return nil
}
func (*integrationIdentityJournal) MarkAbandoned(context.Context, string, string) error {
	return nil
}
func (j *integrationIdentityJournal) MarkOutcomeUnknown(context.Context, string, string) error {
	j.unknown++
	return nil
}

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

func TestIntegrationTool_CapturedBeforePrincipalMutationFailsClosedBeforeDispatch(t *testing.T) {
	var executeCalls int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		executeCalls++
		_ = json.NewEncoder(w).Encode(toolExecResp(true, map[string]any{"ok": true}, nil))
	}))
	defer server.Close()

	gw := client.NewGatewayClient(server.URL, "old-key")
	oldTool := NewIntegrationTool(
		client.ServerToolSchema{Name: "x_create_post"},
		gw,
	)
	gw.SetAPIKey("new-key")
	gw.BindIntegrationPrincipal("account-b", 2)

	result, err := oldTool.Run(context.Background(), `{"text":"must not dispatch"}`)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !result.IsError || result.ErrorCategory != agent.ErrCategoryBusiness ||
		!result.SideEffectKnownNoEffect || result.SideEffectOutcomeUnknown {
		t.Fatalf("stale captured tool result = %#v", result)
	}
	if executeCalls != 0 {
		t.Fatalf("stale captured tool dispatched %d request(s)", executeCalls)
	}
}

func TestRegisterIntegrationTools_DiscardsSupersededListGeneration(t *testing.T) {
	listEntered := make(chan struct{})
	releaseList := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("X-API-Key"); got != "old-key" {
			t.Fatalf("list key = %q, want old-key", got)
		}
		close(listEntered)
		<-releaseList
		_ = json.NewEncoder(w).Encode([]client.ServerToolSchema{{Name: "x_old_catalog"}})
	}))
	defer server.Close()

	gw := client.NewGatewayClient(server.URL, "old-key")
	reg := agent.NewToolRegistry()
	done := make(chan error, 1)
	go func() {
		done <- RegisterIntegrationTools(context.Background(), gw, reg)
	}()
	<-listEntered
	gw.SetAPIKey("new-key")
	gw.BindIntegrationPrincipal("account-b", 2)
	close(releaseList)

	if err := <-done; err == nil || !strings.Contains(err.Error(), "generation changed") {
		t.Fatalf("refresh error = %v, want superseded generation", err)
	}
	if _, ok := reg.Get("x_old_catalog"); ok {
		t.Fatal("old-account list landed after principal mutation")
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

func TestIntegrationTool_ReadOnlyErrorsHaveKnownOutcome(t *testing.T) {
	readOnly := false
	tests := []struct {
		name        string
		status      int
		code        string
		wantUnknown bool
		wantRetry   bool
		wantText    string
	}{
		{name: "auth expired", status: http.StatusConflict, code: "auth_expired", wantText: "Settings → MCP Servers → X"},
		{name: "not connected", status: http.StatusNotFound, code: "not_connected", wantText: "Settings → MCP Servers → X"},
		{name: "provider unavailable", status: http.StatusServiceUnavailable, code: "provider_unavailable", wantRetry: true},
		{name: "integration limit", status: http.StatusTooManyRequests, code: "integration_limit_exceeded"},
		{name: "idempotency conflict", status: http.StatusConflict, code: "idempotency_conflict", wantText: "request id was reused"},
		{name: "billing error", status: http.StatusServiceUnavailable, code: "billing_error", wantText: "do not issue a new tool call"},
		{name: "explicit outcome unknown", status: http.StatusConflict, code: "outcome_unknown", wantText: "do not repeat automatically"},
		{name: "read call in progress", status: http.StatusConflict, code: "call_in_progress", wantText: "do not issue a new tool call"},
		{name: "unknown conflict", status: http.StatusConflict, code: "unexpected_conflict"},
		{name: "unknown server error", status: http.StatusBadGateway, code: "upstream_failed", wantRetry: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(tt.status)
				_ = json.NewEncoder(w).Encode(map[string]string{"error": tt.code})
			}))
			defer server.Close()

			tool := NewIntegrationTool(client.ServerToolSchema{
				Name: "x_read_home", Provider: "x", MaterialSideEffect: &readOnly,
			}, client.NewGatewayClient(server.URL, ""))
			result, err := tool.Run(context.Background(), `{}`)
			if err != nil {
				t.Fatalf("Run: %v", err)
			}
			if !result.IsError || result.SideEffectOutcomeUnknown != tt.wantUnknown ||
				result.IsRetryable != tt.wantRetry {
				t.Fatalf("result = %#v", result)
			}
			if tt.wantText != "" && !strings.Contains(result.Content, tt.wantText) {
				t.Fatalf("content = %q, want %q", result.Content, tt.wantText)
			}
		})
	}
}

func TestIntegrationTool_BillingRecoveryRetriesOnlyWithSameRequestID(t *testing.T) {
	readOnly := false
	for _, code := range []string{"billing_error", "call_in_progress"} {
		t.Run(code, func(t *testing.T) {
			var requestIDs []string
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				var body client.ToolExecuteRequest
				if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
					t.Fatalf("decode request: %v", err)
				}
				requestIDs = append(requestIDs, body.RequestID)
				if len(requestIDs) < 3 {
					w.WriteHeader(http.StatusConflict)
					_ = json.NewEncoder(w).Encode(map[string]string{"error": code})
					return
				}
				_ = json.NewEncoder(w).Encode(client.ToolExecuteResponse{
					Success: true, Output: json.RawMessage(`{"data":[]}`),
				})
			}))
			defer server.Close()

			tool := NewIntegrationTool(client.ServerToolSchema{
				Name: "x_mentions", Provider: "x", MaterialSideEffect: &readOnly,
			}, client.NewGatewayClient(server.URL, ""))
			ctx := agent.ContextWithToolInvocation(context.Background(), agent.ToolInvocation{
				ToolName: "x_mentions", ToolUseID: "toolu_stable",
			})
			result, err := tool.Run(ctx, `{}`)
			if err != nil || result.IsError {
				t.Fatalf("Run = (%#v, %v)", result, err)
			}
			if len(requestIDs) != 3 {
				t.Fatalf("request ids = %#v", requestIDs)
			}
			for _, got := range requestIDs {
				if got != "toolu_stable" {
					t.Fatalf("request ids = %#v, want one stable identity", requestIDs)
				}
			}
		})
	}
}

func TestIntegrationTool_ReconnectInstructionUsesSchemaProvider(t *testing.T) {
	readOnly := false
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusConflict)
		_ = json.NewEncoder(w).Encode(map[string]string{"error": "auth_expired"})
	}))
	defer server.Close()

	tool := NewIntegrationTool(client.ServerToolSchema{
		Name: "notion_search", Provider: "notion", MaterialSideEffect: &readOnly,
	}, client.NewGatewayClient(server.URL, ""))
	result, err := tool.Run(context.Background(), `{}`)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !strings.Contains(result.Content, "Settings → MCP Servers → Notion") ||
		strings.Contains(result.Content, "Servers → X") {
		t.Fatalf("reconnect content = %q", result.Content)
	}
}

func TestIntegrationTool_MaterialAuthExpiredIsKnownNoEffectButUnknownConflictIsNot(t *testing.T) {
	for _, tt := range []struct {
		code         string
		status       int
		wantUnknown  bool
		wantNoEffect bool
	}{
		{code: "auth_expired", status: http.StatusConflict, wantNoEffect: true},
		{code: "unexpected_conflict", status: http.StatusConflict, wantUnknown: true},
		{code: "outcome_unknown", status: http.StatusConflict, wantUnknown: true},
		{code: "billing_error", status: http.StatusServiceUnavailable, wantUnknown: true},
		{code: "provider_unavailable", status: http.StatusServiceUnavailable, wantNoEffect: true},
		{code: "idempotency_conflict", status: http.StatusConflict, wantNoEffect: true},
		{code: "call_in_progress", status: http.StatusConflict, wantUnknown: true},
	} {
		t.Run(tt.code, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(tt.status)
				_ = json.NewEncoder(w).Encode(map[string]string{"error": tt.code})
			}))
			defer server.Close()

			tool := NewIntegrationTool(
				client.ServerToolSchema{Name: "x_create_post"},
				client.NewGatewayClient(server.URL, ""),
			)
			result, err := tool.Run(context.Background(), `{"text":"hello"}`)
			if err != nil {
				t.Fatalf("Run: %v", err)
			}
			if result.SideEffectOutcomeUnknown != tt.wantUnknown ||
				result.SideEffectKnownNoEffect != tt.wantNoEffect {
				t.Fatalf("result = %#v", result)
			}
			if tt.code == "call_in_progress" &&
				(!result.SideEffectOutcomeUnknown ||
					!strings.Contains(result.Content, "Do not resend")) {
				t.Fatalf("call_in_progress result = %#v", result)
			}
		})
	}
}

func TestIntegrationTool_MaterialCallInProgressExhaustionPersistsOutcomeUnknown(t *testing.T) {
	var requestIDs []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body client.ToolExecuteRequest
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatalf("decode request: %v", err)
		}
		requestIDs = append(requestIDs, body.RequestID)
		w.WriteHeader(http.StatusConflict)
		_ = json.NewEncoder(w).Encode(map[string]string{"error": "call_in_progress"})
	}))
	defer server.Close()

	reg := agent.NewToolRegistry()
	reg.Register(NewIntegrationTool(
		client.ServerToolSchema{Name: "x_create_post", Provider: "x"},
		client.NewGatewayClient(server.URL, ""),
	))
	journal := &integrationIdentityJournal{}
	loop := agent.NewAgentLoop(
		&integrationIdentityLLM{}, reg, "medium", t.TempDir(),
		4, 2000, 200, nil, nil, nil,
	)
	loop.SetCheckpointFunc(func(context.Context) error { return nil })
	loop.SetSideEffectExecutionJournal(journal)
	_, _, err := loop.Run(context.Background(), "post hello", nil, nil)
	if !errors.Is(err, agent.ErrSideEffectOutcomeUnknown) {
		t.Fatalf("Run error = %v, want outcome unknown", err)
	}
	if journal.committed != 0 || journal.unknown != 1 {
		t.Fatalf("journal committed=%d unknown=%d, want 0/1", journal.committed, journal.unknown)
	}
	if len(requestIDs) != 3 {
		t.Fatalf("request ids = %#v, want three bounded polls", requestIDs)
	}
	for _, requestID := range requestIDs {
		if requestID != "exec-x-create-1" {
			t.Fatalf("request ids = %#v, want stable durable identity", requestIDs)
		}
	}
}

func TestIntegrationTool_ReadOnlyUsesToolUseIDAndEmitsFullUsage(t *testing.T) {
	readOnly := false
	var requestID, idempotencyKey string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body client.ToolExecuteRequest
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatalf("decode request: %v", err)
		}
		requestID = body.RequestID
		idempotencyKey = r.Header.Get("Idempotency-Key")
		_ = json.NewEncoder(w).Encode(client.ToolExecuteResponse{
			Success: true,
			Output:  json.RawMessage(`{"posts":[]}`),
			Usage: &client.ToolUsage{
				Provider: "x", Model: "x-api-v2", UnitType: "posts",
				Units: 2, CostUSD: 0.01, CostModel: "x_pay_per_use",
			},
		})
	}))
	defer server.Close()

	tool := NewIntegrationTool(client.ServerToolSchema{
		Name: "x_read_home", Provider: "x", MaterialSideEffect: &readOnly,
	}, client.NewGatewayClient(server.URL, ""))
	var emitted agent.TurnUsage
	ctx := agent.ContextWithToolInvocation(context.Background(), agent.ToolInvocation{
		ToolName: "x_read_home", ToolUseID: "toolu_x_read_1",
	})
	ctx = agent.WithUsageEmit(ctx, func(usage agent.TurnUsage) { emitted = usage })
	result, err := tool.Run(ctx, `{}`)
	if err != nil || result.IsError {
		t.Fatalf("Run = (%#v, %v)", result, err)
	}
	if requestID != "toolu_x_read_1" || idempotencyKey != "" {
		t.Fatalf("request_id=%q idempotency-key=%q", requestID, idempotencyKey)
	}
	if result.Usage == nil || result.Usage.Provider != "x" ||
		result.Usage.Model != "x-api-v2" || result.Usage.UnitType != "posts" ||
		result.Usage.Units != 2 || result.Usage.CostUSD != 0.01 ||
		result.Usage.CostModel != "x_pay_per_use" {
		t.Fatalf("result usage = %#v", result.Usage)
	}
	if emitted.Provider != "x" || emitted.Model != "x-api-v2" ||
		emitted.UnitType != "posts" || emitted.Units != 2 || emitted.CostUSD != 0.01 ||
		emitted.CostModel != "x_pay_per_use" {
		t.Fatalf("emitted usage = %#v", emitted)
	}
}

func TestIntegrationTool_ReadOnlyDoesNotReuseIdentityAcrossCalls(t *testing.T) {
	readOnly := false
	var requestIDs []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body client.ToolExecuteRequest
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatalf("decode request: %v", err)
		}
		requestIDs = append(requestIDs, body.RequestID)
		_ = json.NewEncoder(w).Encode(client.ToolExecuteResponse{
			Success: true, Output: json.RawMessage(`{"posts":[]}`),
		})
	}))
	defer server.Close()

	tool := NewIntegrationTool(client.ServerToolSchema{
		Name: "x_read_home", Provider: "x", MaterialSideEffect: &readOnly,
	}, client.NewGatewayClient(server.URL, ""))
	for _, toolUseID := range []string{"toolu_x_read_1", "toolu_x_read_2"} {
		ctx := agent.ContextWithToolInvocation(context.Background(), agent.ToolInvocation{
			ToolName: "x_read_home", ToolUseID: toolUseID,
		})
		if result, err := tool.Run(ctx, `{}`); err != nil || result.IsError {
			t.Fatalf("Run(%s) = (%#v, %v)", toolUseID, result, err)
		}
	}
	if len(requestIDs) != 2 || requestIDs[0] != "toolu_x_read_1" ||
		requestIDs[1] != "toolu_x_read_2" || requestIDs[0] == requestIDs[1] {
		t.Fatalf("request IDs = %#v", requestIDs)
	}
}

func TestIntegrationTool_MaterialUsesDurableExecutionIdentity(t *testing.T) {
	var requestID, idempotencyKey string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body client.ToolExecuteRequest
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatalf("decode request: %v", err)
		}
		requestID = body.RequestID
		idempotencyKey = r.Header.Get("Idempotency-Key")
		_ = json.NewEncoder(w).Encode(client.ToolExecuteResponse{
			Success: true, Output: json.RawMessage(`{"created":true}`),
		})
	}))
	defer server.Close()

	reg := agent.NewToolRegistry()
	reg.Register(NewIntegrationTool(
		client.ServerToolSchema{Name: "x_create_post", Provider: "x"},
		client.NewGatewayClient(server.URL, ""),
	))
	journal := &integrationIdentityJournal{}
	loop := agent.NewAgentLoop(
		&integrationIdentityLLM{}, reg, "medium", t.TempDir(),
		4, 2000, 200, nil, nil, nil,
	)
	loop.SetCheckpointFunc(func(context.Context) error { return nil })
	loop.SetSideEffectExecutionJournal(journal)
	text, _, err := loop.Run(context.Background(), "post hello", nil, nil)
	if err != nil || text != "done" {
		t.Fatalf("Run = (%q, %v)", text, err)
	}
	if journal.prepared.ToolUseID != "toolu_x_create_1" ||
		requestID != "exec-x-create-1" || idempotencyKey != "idem-x-create-1" {
		t.Fatalf("prepared=%#v request_id=%q idempotency-key=%q",
			journal.prepared, requestID, idempotencyKey)
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
