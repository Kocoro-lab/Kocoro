//go:build darwin && cgo

package koe

// Live selector-to-AgentLoop smoke qualification.
//
// Realtime remains the only paid/live boundary: the selected do_task arguments
// are passed through the production Koe Dispatcher and DaemonClient into an
// in-process production daemon HTTP handler. The daemon's GatewayClient points
// at a deterministic fake that drives the shared AgentLoop through one
// side-effecting tool call and one final completion.
//
//	KOE_SELECTOR_AGENTLOOP_E2E=1 \
//	PKG_CONFIG_PATH=/opt/homebrew/lib/pkgconfig \
//	go test ./internal/koe -run '^TestKoeSelectorToAgentLoopTextE2E$' \
//	  -count=1 -v -timeout=5m

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Kocoro-lab/ShanClaw/internal/agent"
	"github.com/Kocoro-lab/ShanClaw/internal/client"
	"github.com/Kocoro-lab/ShanClaw/internal/config"
	"github.com/Kocoro-lab/ShanClaw/internal/daemon"
	"github.com/Kocoro-lab/ShanClaw/internal/executionprofile"
)

const (
	selectorAgentLoopGate          = "KOE_SELECTOR_AGENTLOOP_E2E"
	selectorAgentLoopToolName      = "selector_agentloop_write_once"
	selectorAgentLoopToolCallID    = "call-selector-agentloop-write-once"
	selectorAgentLoopFastProfileID = "kfp1_selector-agentloop"
	selectorAgentLoopFinalReply    = "Selector-to-AgentLoop completed exactly once."
)

type selectorAgentLoopArgs struct {
	Task          string `json:"task"`
	ExecutionMode string `json:"execution_mode"`
	FullReason    string `json:"full_reason"`
}

type selectorAgentLoopCapturedCall struct {
	name string
	id   string
	args json.RawMessage
}

func TestKoeSelectorToAgentLoopTextE2E(t *testing.T) {
	if os.Getenv(selectorAgentLoopGate) != "1" {
		t.Skip("live Realtime selector smoke: set KOE_SELECTOR_AGENTLOOP_E2E=1")
	}
	t.Setenv("KOE_TASK_LEDGER", "1")

	cases := []struct {
		name       string
		prompt     string
		lang       string
		wantMode   executionprofile.Mode
		wantReason executionprofile.FullReason
	}{
		{
			name:       "fast",
			prompt:     "查一下东京现在的准确时间，并告诉我今天是星期几。",
			lang:       "zh",
			wantMode:   executionprofile.ModeFast,
			wantReason: executionprofile.FullReasonNone,
		},
		{
			name:       "full_explicit",
			prompt:     "Use Full mode for a deep, thorough analysis of the uploaded project plan, including assumptions, dependencies, risks, and decision points.",
			lang:       "en",
			wantMode:   executionprofile.ModeFull,
			wantReason: executionprofile.FullReasonExplicitFullRequest,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
			defer cancel()

			rawArgs, selectorArgs := captureSelectorAgentLoopDoTask(t, ctx, tc.prompt)
			if selectorArgs.Task == "" {
				t.Fatal("Realtime selector emitted an empty task")
			}
			if selectorArgs.ExecutionMode != string(tc.wantMode) {
				t.Fatalf(
					"Realtime selector mode = %s, want %s; reason=%s task=%q",
					selectorArgs.ExecutionMode,
					tc.wantMode,
					selectorArgs.FullReason,
					selectorArgs.Task,
				)
			}
			admission := executionprofile.DecideModeAdmission(
				selectorArgs.ExecutionMode,
				selectorArgs.FullReason,
			)
			if admission.AdmittedMode != tc.wantMode {
				t.Fatalf("selector admission = %+v, want mode=%s", admission, tc.wantMode)
			}
			if admission.RequestedFullReason != tc.wantReason {
				t.Logf(
					"diagnostic reason differs from the example label: got=%s want=%s",
					admission.RequestedFullReason,
					tc.wantReason,
				)
			}

			effect := &selectorAgentLoopEffectTool{}
			gateway := &selectorAgentLoopGateway{}
			gatewayHTTP := httptest.NewServer(gateway.handler())
			t.Cleanup(gatewayHTTP.Close)
			daemonClient := newSelectorAgentLoopDaemonClient(t, gatewayHTTP.URL, effect)

			state := NewCallState("selector-agentloop-"+tc.name, "")
			dispatcher := NewDispatcher(
				daemonClient,
				NewAgentResolver(nil, NoopSemanticMatcher{}),
				state,
				nil,
			)
			req, task, clarify, err := dispatcher.PrepareDoTask(rawArgs, tc.lang, false)
			if err != nil {
				t.Fatalf("PrepareDoTask: %v", err)
			}
			if clarify != nil {
				t.Fatalf("PrepareDoTask unexpectedly requested clarification: %+v", clarify)
			}
			if task == nil {
				t.Fatal("PrepareDoTask did not create a task-ledger entry")
			}
			if req.ExecutionMode != tc.wantMode ||
				req.RequestedExecutionMode == nil ||
				*req.RequestedExecutionMode != selectorArgs.ExecutionMode ||
				req.FullReason != admission.RequestedFullReason {
				t.Fatalf("Dispatcher request lost selector admission evidence: %+v", req)
			}
			if req.LogicalTaskID == "" || req.ExecutionRunID == "" {
				t.Fatalf("Dispatcher request omitted execution lineage: %+v", req)
			}

			// Fault-inject a stale/adversarial advisory field after the production
			// Dispatcher has admitted the selector evidence. The raw requested mode
			// and reason remain unchanged. The daemon must independently recompute
			// admission before it chooses the execution profile.
			wireReq := req
			if tc.wantMode == executionprofile.ModeFast {
				wireReq.ExecutionMode = executionprofile.ModeFull
			} else {
				wireReq.ExecutionMode = executionprofile.ModeFast
			}

			out, err := daemonClient.DoTask(ctx, wireReq)
			if err != nil {
				t.Fatalf("DaemonClient.DoTask: %v", err)
			}
			if out.Kind != OutcomeCompleted ||
				out.Reply != selectorAgentLoopFinalReply ||
				out.Partial ||
				out.FailureCode != "" {
				t.Fatalf("unexpected daemon outcome: %+v", out)
			}
			if out.ExecutionRun == nil {
				t.Fatal("daemon outcome omitted execution_run")
			}
			if out.ExecutionRun.LogicalTaskID != req.LogicalTaskID ||
				out.ExecutionRun.RunID != req.ExecutionRunID ||
				out.ExecutionRun.Profile.RequestedMode != tc.wantMode ||
				out.ExecutionRun.Profile.EffectiveMode != tc.wantMode {
				t.Fatalf("daemon execution run drifted from authoritative admission: %+v", out.ExecutionRun)
			}

			evidence := out.ExecutionRun.Evidence.ToolOutcomes
			if len(evidence) != 1 ||
				evidence[0].ToolCallID != selectorAgentLoopToolCallID ||
				evidence[0].ToolName != selectorAgentLoopToolName ||
				!evidence[0].Validated ||
				evidence[0].Outcome != "succeeded" ||
				!evidence[0].SideEffect {
				t.Fatalf("execution evidence does not contain one successful side effect: %+v", evidence)
			}
			if got := effect.executions.Load(); got != 1 {
				t.Fatalf("side-effect executions = %d, want exactly 1", got)
			}

			assertSelectorAgentLoopGateway(t, gateway, tc.wantMode)
			t.Logf(
				"VERDICT: selector=%s/%s effective=%s profile=%q completions=2 side_effects=1",
				selectorArgs.ExecutionMode,
				selectorArgs.FullReason,
				out.ExecutionRun.Profile.EffectiveMode,
				out.ExecutionRun.Profile.ProfileID,
			)
		})
	}
}

func captureSelectorAgentLoopDoTask(
	t *testing.T,
	ctx context.Context,
	prompt string,
) (json.RawMessage, selectorAgentLoopArgs) {
	t.Helper()
	session, err := newModeClassifierSession(ctx)
	if err != nil {
		t.Fatalf("connect live Realtime selector: %v", err)
	}
	defer session.Close()

	if err := session.send(map[string]any{
		"type": "conversation.item.create",
		"item": map[string]any{
			"type": "message",
			"role": "user",
			"content": []map[string]any{{
				"type": "input_text",
				"text": prompt,
			}},
		},
	}); err != nil {
		t.Fatalf("send selector input: %v", err)
	}
	if err := session.send(map[string]any{"type": "response.create"}); err != nil {
		t.Fatalf("send selector response.create: %v", err)
	}

	var responseID string
	var calls []selectorAgentLoopCapturedCall
	for {
		select {
		case event := <-session.events:
			switch event.Type {
			case "response.created":
				if responseID == "" {
					responseID = event.Response.ID
				}
			case "response.function_call_arguments.done":
				if responseID != "" && event.ResponseID != "" && event.ResponseID != responseID {
					continue
				}
				args := append(json.RawMessage(nil), unwrapArgs(event.Arguments)...)
				calls = append(calls, selectorAgentLoopCapturedCall{
					name: event.Name,
					id:   event.CallID,
					args: args,
				})
				// Finish the one-shot selector response without doing real work on
				// the Realtime side. The exact captured JSON is dispatched through
				// the production Koe/daemon path after response.done proves that
				// this response emitted one, and only one, do_task call.
				if err := session.send(map[string]any{
					"type": "conversation.item.create",
					"item": map[string]any{
						"type":    "function_call_output",
						"call_id": event.CallID,
						"output":  `{"status":"running","task_id":"selector-agentloop-smoke"}`,
					},
				}); err != nil {
					t.Fatalf("resolve captured Realtime function call: %v", err)
				}
			case "response.done":
				if responseID != "" && event.Response.ID != "" && event.Response.ID != responseID {
					continue
				}
				if event.Response.Status != "" && event.Response.Status != "completed" {
					t.Fatalf("selector response status = %q", event.Response.Status)
				}
				if len(calls) != 1 ||
					calls[0].name != "do_task" ||
					calls[0].id == "" {
					names := make([]string, 0, len(calls))
					for _, call := range calls {
						names = append(names, call.name)
					}
					t.Fatalf("selector emitted %d calls (%s), want exactly one do_task", len(calls), strings.Join(names, ","))
				}
				var args selectorAgentLoopArgs
				if err := json.Unmarshal(calls[0].args, &args); err != nil {
					t.Fatalf("decode captured do_task args: %v; body=%s", err, calls[0].args)
				}
				return calls[0].args, args
			case "error", "response.failed":
				t.Fatalf("live Realtime selector failed: %s", modeClassifierEventError(event))
			}
		case <-ctx.Done():
			t.Fatalf("live Realtime selector timed out: %v", ctx.Err())
		}
	}
}

type selectorAgentLoopEffectTool struct {
	executions atomic.Int32
}

func (t *selectorAgentLoopEffectTool) Info() agent.ToolInfo {
	return agent.ToolInfo{
		Name:        selectorAgentLoopToolName,
		Description: "Apply one controlled in-memory write for selector-to-AgentLoop qualification.",
		Parameters: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"value": map[string]any{"type": "string", "enum": []string{"once"}},
			},
			"required": []string{"value"},
		},
		Required: []string{"value"},
	}
}

func (t *selectorAgentLoopEffectTool) Run(_ context.Context, args string) (agent.ToolResult, error) {
	var input struct {
		Value string `json:"value"`
	}
	if err := json.Unmarshal([]byte(args), &input); err != nil {
		return agent.ToolResult{}, err
	}
	if input.Value != "once" {
		return agent.ToolResult{}, fmt.Errorf("unexpected effect value %q", input.Value)
	}
	if count := t.executions.Add(1); count != 1 {
		return agent.ToolResult{}, fmt.Errorf("duplicate controlled side effect: execution %d", count)
	}
	return agent.ToolResult{Content: "controlled write completed"}, nil
}

func (*selectorAgentLoopEffectTool) RequiresApproval() bool { return false }

type selectorAgentLoopGateway struct {
	mu              sync.Mutex
	resolveRequests []client.ResolveExecutionProfileRequest
	mainRequests    []client.CompletionRequest
	unexpected      []string
}

func (g *selectorAgentLoopGateway) handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v1/channels":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte("[]"))
		case "/v1/completions/resolve":
			var req client.ResolveExecutionProfileRequest
			if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
				g.recordUnexpected("decode execution profile request: %v", err)
				http.Error(w, "invalid execution profile request", http.StatusBadRequest)
				return
			}
			g.mu.Lock()
			g.resolveRequests = append(g.resolveRequests, req)
			g.mu.Unlock()
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(selectorAgentLoopFastProfile())
		case "/v1/completions":
			var req client.CompletionRequest
			if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
				g.recordUnexpected("decode completion request: %v", err)
				http.Error(w, "invalid completion request", http.StatusBadRequest)
				return
			}
			if req.CacheSource == "helper" {
				w.Header().Set("Content-Type", "application/json")
				_ = json.NewEncoder(w).Encode(client.CompletionResponse{
					Provider:     "anthropic",
					Model:        "claude-haiku-4-5",
					OutputText:   "Selector AgentLoop",
					FinishReason: "end_turn",
				})
				return
			}

			g.mu.Lock()
			g.mainRequests = append(g.mainRequests, req)
			call := len(g.mainRequests)
			g.mu.Unlock()
			switch call {
			case 1:
				selectorAgentLoopWriteSSEDone(w, client.CompletionResponse{
					Provider:     selectorAgentLoopProvider(req),
					Model:        selectorAgentLoopModel(req),
					FinishReason: "tool_use",
					ToolCalls: []client.FunctionCall{{
						ID:        selectorAgentLoopToolCallID,
						Name:      selectorAgentLoopToolName,
						Arguments: json.RawMessage(`{"value":"once"}`),
					}},
				})
			default:
				selectorAgentLoopWriteSSEDone(w, client.CompletionResponse{
					Provider:     selectorAgentLoopProvider(req),
					Model:        selectorAgentLoopModel(req),
					OutputText:   selectorAgentLoopFinalReply,
					FinishReason: "end_turn",
				})
			}
		default:
			g.recordUnexpected("%s %s", r.Method, r.URL.Path)
			http.NotFound(w, r)
		}
	})
}

func (g *selectorAgentLoopGateway) recordUnexpected(format string, args ...any) {
	g.mu.Lock()
	g.unexpected = append(g.unexpected, fmt.Sprintf(format, args...))
	g.mu.Unlock()
}

func (g *selectorAgentLoopGateway) snapshot() (
	[]client.ResolveExecutionProfileRequest,
	[]client.CompletionRequest,
	[]string,
) {
	g.mu.Lock()
	defer g.mu.Unlock()
	return append([]client.ResolveExecutionProfileRequest(nil), g.resolveRequests...),
		append([]client.CompletionRequest(nil), g.mainRequests...),
		append([]string(nil), g.unexpected...)
}

func selectorAgentLoopFastProfile() executionprofile.Profile {
	return executionprofile.Profile{
		RequestedMode:       executionprofile.ModeFast,
		EffectiveMode:       executionprofile.ModeFast,
		SchemaVersion:       executionprofile.FastSchemaVersion,
		ProfileName:         executionprofile.FastProfileName,
		ProfileVersion:      executionprofile.FastProfileVersion,
		ProfileID:           selectorAgentLoopFastProfileID,
		Provider:            "openai",
		Model:               "gpt-5.6-luna",
		APISurface:          "openai_responses",
		ToolContract:        executionprofile.FastToolContract,
		ReasoningEffort:     "medium",
		ServiceTier:         "fast",
		ParallelToolCalls:   true,
		ResponseCachePolicy: executionprofile.ResponseCacheOff,
		ResolutionReason:    "cloud_profile_resolved",
	}
}

func selectorAgentLoopWriteSSEDone(w http.ResponseWriter, response client.CompletionResponse) {
	payload, _ := json.Marshal(struct {
		Type string `json:"type"`
		client.CompletionResponse
	}{
		Type:               "done",
		CompletionResponse: response,
	})
	w.Header().Set("Content-Type", "text/event-stream")
	_, _ = fmt.Fprintf(w, "data: %s\n\ndata: [DONE]\n\n", payload)
	if flusher, ok := w.(http.Flusher); ok {
		flusher.Flush()
	}
}

func selectorAgentLoopProvider(req client.CompletionRequest) string {
	if req.ExecutionProfileID != "" {
		return "openai"
	}
	return "anthropic"
}

func selectorAgentLoopModel(req client.CompletionRequest) string {
	if req.ExecutionProfileID != "" {
		return "gpt-5.6-luna"
	}
	return "claude-sonnet-5"
}

func newSelectorAgentLoopDaemonClient(
	t *testing.T,
	gatewayURL string,
	effect *selectorAgentLoopEffectTool,
) *DaemonClient {
	t.Helper()
	root := t.TempDir()
	skillDiscovery := false
	registry := agent.NewToolRegistry()
	registry.Register(effect)
	deps := &daemon.ServerDeps{
		Config: &config.Config{
			Provider:  "gateway",
			ModelTier: "large",
			Agent: config.AgentConfig{
				// Keep the 60%-of-limit progress checkpoint beyond this
				// deterministic two-iteration tool-call/final-reply path.
				MaxIterations:   6,
				MaxTokens:       4096,
				Model:           "claude-sonnet-5",
				Thinking:        true,
				ThinkingMode:    "adaptive",
				ReasoningEffort: "high",
				EffortTier:      "xhigh",
				ContextWindow:   200_000,
				SkillDiscovery:  &skillDiscovery,
			},
		},
		GW:           client.NewGatewayClient(gatewayURL, "selector-agentloop-test-key"),
		Registry:     registry,
		BaselineReg:  agent.NewToolRegistry(),
		SessionCache: daemon.NewSessionCache(root),
		ShannonDir:   root,
		AgentsDir:    filepath.Join(root, "agents"),
	}
	t.Cleanup(deps.SessionCache.CloseAll)

	server := daemon.NewServer(0, nil, deps, "selector-agentloop-test")
	httpServer := httptest.NewServer(server.Handler())
	t.Cleanup(httpServer.Close)
	return NewDaemonClient(httpServer.URL)
}

func assertSelectorAgentLoopGateway(
	t *testing.T,
	gateway *selectorAgentLoopGateway,
	mode executionprofile.Mode,
) {
	t.Helper()
	resolves, requests, unexpected := gateway.snapshot()
	if len(unexpected) != 0 {
		t.Fatalf("fake gateway received unexpected requests: %v", unexpected)
	}
	if len(requests) != 2 {
		summaries := make([]string, 0, len(requests))
		for index, req := range requests {
			summaries = append(summaries, fmt.Sprintf(
				"%d:{stream=%t cache=%q messages=%d tools=%d tool_choice=%v profile=%q model_tier=%q}",
				index+1,
				req.Stream,
				req.CacheSource,
				len(req.Messages),
				len(req.Tools),
				req.ToolChoice,
				req.ExecutionProfileID,
				req.ModelTier,
			))
		}
		t.Fatalf(
			"main AgentLoop completion requests = %d, want 2: %s",
			len(requests),
			strings.Join(summaries, " "),
		)
	}
	if mode == executionprofile.ModeFast {
		if len(resolves) != 1 {
			t.Fatalf("Fast resolver requests = %d, want 1", len(resolves))
		}
		resolve := resolves[0]
		if resolve.Capability != executionprofile.FastCapability ||
			resolve.SchemaVersion != executionprofile.FastSchemaVersion ||
			resolve.AllowModelFallback {
			t.Fatalf("Fast resolver request escaped the closed contract: %+v", resolve)
		}
	} else if len(resolves) != 0 {
		t.Fatalf("Full resolver requests = %d, want 0", len(resolves))
	}

	for index, req := range requests {
		if !req.Stream ||
			req.CacheSource != "koe" ||
			req.SessionID == "" ||
			!selectorAgentLoopRequestHasTool(req, selectorAgentLoopToolName) {
			t.Fatalf("main completion %d lost AgentLoop request identity/tool schema: %+v", index+1, req)
		}
		if mode == executionprofile.ModeFast {
			if req.ExecutionProfileID != selectorAgentLoopFastProfileID ||
				req.ModelTier != "" ||
				req.SpecificModel != "" ||
				req.Thinking != nil ||
				req.ReasoningEffort != "" ||
				req.EffortTier != "" ||
				!req.ParallelToolCalls ||
				req.ResponseCachePolicy != executionprofile.ResponseCacheOff {
				t.Fatalf("Fast completion %d profile drifted: %+v", index+1, req)
			}
		} else {
			if req.ExecutionProfileID != "" ||
				req.ModelTier != "large" ||
				req.SpecificModel != "claude-sonnet-5" ||
				req.Thinking == nil ||
				req.Thinking.Type != "adaptive" ||
				req.ReasoningEffort != "high" ||
				req.EffortTier != "xhigh" ||
				req.ParallelToolCalls ||
				req.ResponseCachePolicy != "" {
				t.Fatalf("Full completion %d did not preserve the Agent baseline: %+v", index+1, req)
			}
		}
	}
	if !selectorAgentLoopRequestHasToolResult(requests[1], selectorAgentLoopToolCallID) {
		t.Fatalf("second completion omitted tool result for %q", selectorAgentLoopToolCallID)
	}
}

func selectorAgentLoopRequestHasTool(req client.CompletionRequest, name string) bool {
	for _, tool := range req.Tools {
		if tool.Function.Name == name || tool.Name == name {
			return true
		}
	}
	return false
}

func selectorAgentLoopRequestHasToolResult(req client.CompletionRequest, callID string) bool {
	for _, message := range req.Messages {
		for _, block := range message.Content.Blocks() {
			if block.Type == "tool_result" && block.ToolUseID == callID {
				return true
			}
		}
	}
	return false
}
