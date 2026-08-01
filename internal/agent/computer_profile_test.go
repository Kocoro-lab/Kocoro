package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"sync"
	"testing"

	"github.com/Kocoro-lab/ShanClaw/internal/client"
	"github.com/Kocoro-lab/ShanClaw/internal/executionprofile"
)

type computerProfileTool struct {
	name   string
	native bool
	runs   int
}

func (t *computerProfileTool) Info() ToolInfo {
	return ToolInfo{
		Name:        t.name,
		Description: "Operate a test computer.",
		Parameters: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"action": map[string]any{"type": "string"},
			},
		},
		Required: []string{"action"},
	}
}

func (t *computerProfileTool) Run(context.Context, string) (ToolResult, error) {
	t.runs++
	return ToolResult{Content: "computer-observation"}, nil
}

func (t *computerProfileTool) RequiresApproval() bool { return false }
func (t *computerProfileTool) IsReadOnlyCall(string) bool {
	return true
}
func (t *computerProfileTool) ToolExposure() ToolExposure {
	return ToolExposureDeferred
}
func (t *computerProfileTool) ToolProfileRequirement() ToolProfileRequirement {
	return ToolProfileComputer
}
func (t *computerProfileTool) NativeToolDef() *client.NativeToolDef {
	if !t.native {
		return nil
	}
	return &client.NativeToolDef{
		Type:            "computer_20251124",
		Name:            t.name,
		DisplayWidthPx:  1280,
		DisplayHeightPx: 800,
	}
}

type computerProfileSequenceLLM struct {
	mu              sync.Mutex
	responses       []*client.CompletionResponse
	requests        []client.CompletionRequest
	resolveRequests []client.ComputerExecutionProfileRequest
	events          []string
	resolve         func(client.ComputerExecutionProfileRequest) (executionprofile.Profile, error)
}

func (c *computerProfileSequenceLLM) Complete(
	_ context.Context,
	req client.CompletionRequest,
) (*client.CompletionResponse, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.requests = append(c.requests, req)
	c.events = append(c.events, "complete:"+req.ExecutionProfileID)
	if len(c.responses) == 0 {
		return nil, errors.New("unexpected extra completion request")
	}
	resp := c.responses[0]
	c.responses = c.responses[1:]
	return resp, nil
}

func (c *computerProfileSequenceLLM) CompleteStream(
	ctx context.Context,
	req client.CompletionRequest,
	_ func(client.StreamDelta),
) (*client.CompletionResponse, error) {
	return c.Complete(ctx, req)
}

func (c *computerProfileSequenceLLM) ResolveComputerExecutionProfile(
	_ context.Context,
	req client.ComputerExecutionProfileRequest,
) (executionprofile.Profile, error) {
	c.mu.Lock()
	c.resolveRequests = append(c.resolveRequests, req)
	c.events = append(c.events, "resolve:"+req.RequiredToolContract)
	resolve := c.resolve
	c.mu.Unlock()
	if resolve == nil {
		return executionprofile.Profile{}, errors.New("unexpected computer profile resolution")
	}
	return resolve(req)
}

func (c *computerProfileSequenceLLM) snapshot() (
	[]client.CompletionRequest,
	[]client.ComputerExecutionProfileRequest,
) {
	c.mu.Lock()
	defer c.mu.Unlock()
	requests := append([]client.CompletionRequest(nil), c.requests...)
	resolveRequests := append([]client.ComputerExecutionProfileRequest(nil), c.resolveRequests...)
	return requests, resolveRequests
}

func (c *computerProfileSequenceLLM) recordEvent(event string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.events = append(c.events, event)
}

func (c *computerProfileSequenceLLM) eventSnapshot() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]string(nil), c.events...)
}

func nativeComputerProfileForAgentTest() executionprofile.Profile {
	return executionprofile.Profile{
		SchemaVersion:      executionprofile.ComputerSchemaVersion,
		ContractRevision:   executionprofile.ComputerContractRevision,
		ProfileID:          "ep1_native-agent-test",
		Provider:           "anthropic",
		Model:              "claude-sonnet-4-6",
		APISurface:         executionprofile.AnthropicMessagesAPISurface,
		ExecutionMode:      executionprofile.ComputerExecutionModeNative,
		ToolContract:       executionprofile.AnthropicComputerToolContract,
		BetaContract:       executionprofile.AnthropicComputerBetaContract,
		SupportsImageInput: true,
		SupportsToolImages: true,
		SupportsFunctions:  true,
		ResolutionReason:   "cloud_computer_profile_resolved",
	}
}

func genericComputerProfileForAgentTest() executionprofile.Profile {
	return executionprofile.Profile{
		SchemaVersion:     executionprofile.ComputerSchemaVersion,
		ContractRevision:  executionprofile.ComputerContractRevision,
		ProfileID:         "ep1_generic-agent-test",
		Provider:          "openai",
		Model:             "gpt-5.6-sol",
		APISurface:        "openai_chat_completions",
		ExecutionMode:     executionprofile.ComputerExecutionModeFunction,
		ToolContract:      executionprofile.GenericComputerUseToolContract,
		SupportsFunctions: true,
		ResolutionReason:  "cloud_computer_profile_resolved",
	}
}

func newComputerProfileRegistry(extra ...Tool) (
	*ToolRegistry,
	*computerProfileTool,
	*computerProfileTool,
) {
	reg := NewToolRegistry()
	for _, tool := range extra {
		reg.Register(tool)
	}
	native := &computerProfileTool{name: "computer", native: true}
	generic := &computerProfileTool{name: "computer_use"}
	reg.Register(native)
	reg.Register(generic)
	return reg, native, generic
}

func computerSearchResponse(provider, model string) *client.CompletionResponse {
	return &client.CompletionResponse{
		Provider:     provider,
		Model:        model,
		FinishReason: "tool_use",
		ToolCalls: []client.FunctionCall{{
			ID:        "toolu-search-computer",
			Name:      "tool_search",
			Arguments: json.RawMessage(`{"query":"select:computer"}`),
		}},
	}
}

func computerCallResponse(provider, model, name string) *client.CompletionResponse {
	return &client.CompletionResponse{
		Provider:     provider,
		Model:        model,
		FinishReason: "tool_use",
		ToolCalls: []client.FunctionCall{{
			ID:        "toolu-" + name,
			Name:      name,
			Arguments: json.RawMessage(`{"action":"screenshot"}`),
		}},
	}
}

func finalComputerResponse(provider, model string) *client.CompletionResponse {
	return &client.CompletionResponse{
		Provider:     provider,
		Model:        model,
		OutputText:   "done",
		FinishReason: "end_turn",
	}
}

func findToolSchema(request client.CompletionRequest, name string) (client.Tool, bool) {
	for _, schema := range request.Tools {
		if schemaToolName(schema) == name {
			return schema, true
		}
	}
	return client.Tool{}, false
}

func requestHasLoadedHeader(request client.CompletionRequest, header string) bool {
	for _, message := range request.Messages {
		for _, block := range message.Content.Blocks() {
			if block.Type != "tool_result" {
				continue
			}
			text, ok := block.ToolContent.(string)
			if !ok {
				continue
			}
			if newline := len(text); newline > 0 {
				for i, r := range text {
					if r == '\n' {
						text = text[:i]
						break
					}
				}
			}
			if text == header {
				return true
			}
		}
	}
	return false
}

func TestDeferredInitialSchemasAreSelectorIndependent(t *testing.T) {
	newRegistry := func() *ToolRegistry {
		direct := &toolSearchE2ETool{
			name:     "web_search",
			desc:     "Search the web.",
			source:   SourceGateway,
			exposure: ToolExposureDirect,
			params:   map[string]any{"type": "object"},
		}
		deferred := &toolSearchE2ETool{
			name:      "calendar_create_event",
			desc:      "Create an event.",
			source:    SourceIntegration,
			exposure:  ToolExposureDeferred,
			namespace: "google_calendar",
			params:    map[string]any{"type": "object"},
		}
		reg, _, _ := newComputerProfileRegistry(direct, deferred)
		return reg
	}

	run := func(t *testing.T, explicitModel string) client.CompletionRequest {
		t.Helper()
		llm := &computerProfileSequenceLLM{
			responses: []*client.CompletionResponse{finalComputerResponse("anthropic", "claude-sonnet-4-6")},
		}
		loop := NewAgentLoop(llm, newRegistry(), "medium", "", 3, 1000, 100, nil, nil, nil)
		loop.SetSpecificModel(explicitModel)
		if _, _, err := loop.Run(context.Background(), "Find an answer.", nil, nil); err != nil {
			t.Fatalf("Run: %v", err)
		}
		requests, _ := llm.snapshot()
		if len(requests) != 1 {
			t.Fatalf("completion requests = %d, want 1", len(requests))
		}
		return requests[0]
	}

	tierRequest := run(t, "")
	sonnetRequest := run(t, "claude-sonnet-4-6")
	if !reflect.DeepEqual(tierRequest.Tools, sonnetRequest.Tools) {
		t.Fatalf("first-turn tools differ by selector:\ntier=%+v\nsonnet=%+v", tierRequest.Tools, sonnetRequest.Tools)
	}
	assertToolExposureInRequest(t, tierRequest, "web_search", true, false)
	assertToolExposureInRequest(t, tierRequest, "tool_search", true, false)
	assertToolExposureInRequest(t, tierRequest, "calendar_create_event", false, false)
	assertToolExposureInRequest(t, tierRequest, "computer", false, false)
	assertToolExposureInRequest(t, tierRequest, "computer_use", false, false)
	for _, schema := range tierRequest.Tools {
		if schema.DeferLoading {
			t.Fatalf("first-turn tool %q unexpectedly used defer_loading", schemaToolName(schema))
		}
	}
}

func TestDirectWebSearchDoesNotResolveComputerProfile(t *testing.T) {
	web := &toolSearchE2ETool{
		name:     "web_search",
		desc:     "Search the web.",
		source:   SourceGateway,
		exposure: ToolExposureDirect,
		params:   map[string]any{"type": "object"},
		result:   "search-result",
	}
	reg, _, _ := newComputerProfileRegistry(web)
	llm := &computerProfileSequenceLLM{
		responses: []*client.CompletionResponse{
			{
				Provider:     "anthropic",
				Model:        "claude-sonnet-4-6",
				FinishReason: "tool_use",
				ToolCalls: []client.FunctionCall{{
					ID:        "toolu-web",
					Name:      "web_search",
					Arguments: json.RawMessage(`{}`),
				}},
			},
			finalComputerResponse("anthropic", "claude-sonnet-4-6"),
		},
	}
	loop := NewAgentLoop(llm, reg, "medium", "", 4, 1000, 100, nil, nil, nil)
	if _, _, err := loop.Run(context.Background(), "Search the web.", nil, nil); err != nil {
		t.Fatalf("Run: %v", err)
	}
	requests, resolveRequests := llm.snapshot()
	if len(resolveRequests) != 0 {
		t.Fatalf("web_search triggered computer profile resolution: %+v", resolveRequests)
	}
	if web.runs != 1 {
		t.Fatalf("web_search runs = %d, want 1", web.runs)
	}
	for i, request := range requests {
		if request.ExecutionProfileID != "" {
			t.Fatalf("request %d unexpectedly has profile %q", i, request.ExecutionProfileID)
		}
		assertToolExposureInRequest(t, request, "computer", false, false)
		assertToolExposureInRequest(t, request, "computer_use", false, false)
	}
}

func TestAnthropicComputerSelectionResolvesNativeProfile(t *testing.T) {
	reg, native, generic := newComputerProfileRegistry()
	llm := &computerProfileSequenceLLM{
		responses: []*client.CompletionResponse{
			computerSearchResponse("anthropic", "claude-sonnet-4-6"),
			computerCallResponse("anthropic", "claude-sonnet-4-6", "computer"),
			finalComputerResponse("anthropic", "claude-sonnet-4-6"),
		},
		resolve: func(req client.ComputerExecutionProfileRequest) (executionprofile.Profile, error) {
			if req.RequiredToolContract != executionprofile.AnthropicComputerToolContract {
				return executionprofile.Profile{}, errors.New("unexpected non-native contract")
			}
			return nativeComputerProfileForAgentTest(), nil
		},
	}
	ws := NewWorkingSet()
	thinking := &client.ThinkingConfig{Type: "enabled", BudgetTokens: 4096}
	loop := NewAgentLoop(llm, reg, "medium", "", 5, 1000, 100, nil, nil, nil)
	loop.SetSpecificModel("claude-sonnet-4-6")
	loop.SetReasoningEffort("high")
	loop.SetEffortTier("xhigh")
	loop.SetThinking(thinking)
	loop.SetWorkingSet(ws)
	if _, _, err := loop.Run(context.Background(), "Inspect the screen.", nil, nil); err != nil {
		t.Fatalf("Run: %v", err)
	}

	requests, resolveRequests := llm.snapshot()
	if len(requests) != 3 {
		t.Fatalf("completion requests = %d, want 3", len(requests))
	}
	if len(resolveRequests) != 1 {
		t.Fatalf("computer profile resolutions = %d, want 1", len(resolveRequests))
	}
	resolveRequest := resolveRequests[0]
	if resolveRequest.SpecificModel != "anthropic:claude-sonnet-4-6" ||
		resolveRequest.ModelTier != "medium" ||
		resolveRequest.RequiredToolContract != executionprofile.AnthropicComputerToolContract ||
		resolveRequest.AllowModelFallback {
		t.Fatalf("native resolver request = %+v", resolveRequest)
	}
	assertToolExposureInRequest(t, requests[0], "computer", false, false)
	assertToolExposureInRequest(t, requests[0], "computer_use", false, false)

	continuation := requests[1]
	if continuation.ModelTier != "medium" ||
		continuation.SpecificModel != "anthropic:claude-sonnet-4-6" ||
		continuation.ExecutionProfileID != "ep1_native-agent-test" {
		t.Fatalf("native continuation route = %+v", continuation)
	}
	if continuation.ReasoningEffort != "high" || continuation.EffortTier != "xhigh" {
		t.Fatalf("native continuation lost effort selectors: %+v", continuation)
	}
	if continuation.Thinking == nil ||
		continuation.Thinking.Type != thinking.Type ||
		continuation.Thinking.BudgetTokens != thinking.BudgetTokens {
		t.Fatalf("native continuation lost thinking: %+v", continuation.Thinking)
	}
	if continuation.ResponseCachePolicy != "" || continuation.ParallelToolCalls {
		t.Fatalf("native continuation leaked fast-only policy: %+v", continuation)
	}
	schema, ok := findToolSchema(continuation, "computer")
	if !ok {
		t.Fatal("native computer schema missing from continuation")
	}
	if schema.Type != "computer_20251124" || schema.DeferLoading {
		t.Fatalf("native computer schema = %+v", schema)
	}
	assertToolExposureInRequest(t, continuation, "computer_use", false, false)
	if !requestHasLoadedHeader(continuation, "LOADED:computer") {
		t.Fatal("native continuation lacks exact LOADED:computer result")
	}
	if native.runs != 1 || generic.runs != 0 {
		t.Fatalf("computer runs native=%d generic=%d, want 1/0", native.runs, generic.runs)
	}
	if ws.Contains("computer") || ws.Contains("computer_use") {
		t.Fatalf("WorkingSet cached profile-bound computer schema: %+v", ws.Schemas())
	}
	activation := loop.ComputerActivation()
	if activation == nil ||
		activation.Profile.ProfileID != "ep1_native-agent-test" ||
		activation.ToolName != "computer" ||
		activation.ToolsetFingerprint == "" {
		t.Fatalf("computer activation = %+v", activation)
	}
}

func TestOpenAIComputerSelectionResolvesGenericProfile(t *testing.T) {
	reg, native, generic := newComputerProfileRegistry()
	llm := &computerProfileSequenceLLM{
		responses: []*client.CompletionResponse{
			computerSearchResponse("openai", "gpt-5.6-sol"),
			computerCallResponse("openai", "gpt-5.6-sol", "computer_use"),
			finalComputerResponse("openai", "gpt-5.6-sol"),
		},
		resolve: func(req client.ComputerExecutionProfileRequest) (executionprofile.Profile, error) {
			if req.RequiredToolContract != executionprofile.GenericComputerUseToolContract {
				return executionprofile.Profile{}, errors.New("unexpected native contract")
			}
			return genericComputerProfileForAgentTest(), nil
		},
	}
	loop := NewAgentLoop(llm, reg, "medium", "", 5, 1000, 100, nil, nil, nil)
	if _, _, err := loop.Run(context.Background(), "Inspect the screen.", nil, nil); err != nil {
		t.Fatalf("Run: %v", err)
	}

	requests, resolveRequests := llm.snapshot()
	if len(requests) != 3 {
		t.Fatalf("completion requests = %d, want 3", len(requests))
	}
	if len(resolveRequests) != 1 {
		t.Fatalf("computer profile resolutions = %d, want 1", len(resolveRequests))
	}
	resolveRequest := resolveRequests[0]
	if resolveRequest.SpecificModel != "openai:gpt-5.6-sol" ||
		resolveRequest.RequiredToolContract != executionprofile.GenericComputerUseToolContract {
		t.Fatalf("generic resolver request = %+v", resolveRequest)
	}
	continuation := requests[1]
	if continuation.ModelTier != "medium" ||
		continuation.SpecificModel != "openai:gpt-5.6-sol" ||
		continuation.ExecutionProfileID != "ep1_generic-agent-test" {
		t.Fatalf("generic continuation route = %+v", continuation)
	}
	schema, ok := findToolSchema(continuation, "computer_use")
	if !ok {
		t.Fatal("generic computer_use schema missing from continuation")
	}
	if schema.Type != "function" || schema.DeferLoading {
		t.Fatalf("generic computer_use schema = %+v", schema)
	}
	assertToolExposureInRequest(t, continuation, "computer", false, false)
	if !requestHasLoadedHeader(continuation, "LOADED:computer_use") {
		t.Fatal("generic continuation lacks exact LOADED:computer_use result")
	}
	if native.runs != 0 || generic.runs != 1 {
		t.Fatalf("computer runs native=%d generic=%d, want 0/1", native.runs, generic.runs)
	}
}

func TestComputerResolverFailureFailsClosedBeforeContinuation(t *testing.T) {
	reg, native, generic := newComputerProfileRegistry()
	llm := &computerProfileSequenceLLM{
		responses: []*client.CompletionResponse{
			computerSearchResponse("anthropic", "claude-sonnet-4-6"),
		},
		resolve: func(client.ComputerExecutionProfileRequest) (executionprofile.Profile, error) {
			return executionprofile.Profile{}, errors.New("resolver unavailable")
		},
	}
	ws := NewWorkingSet()
	loop := NewAgentLoop(llm, reg, "medium", "", 5, 1000, 100, nil, nil, nil)
	loop.SetSpecificModel("claude-sonnet-4-6")
	loop.SetWorkingSet(ws)
	if _, _, err := loop.Run(context.Background(), "Inspect the screen.", nil, nil); err == nil ||
		!strings.Contains(err.Error(), "computer execution profile unavailable") {
		t.Fatalf("Run error = %v, want closed profile failure", err)
	}

	requests, resolveRequests := llm.snapshot()
	if len(requests) != 1 {
		t.Fatalf("profile failure allowed %d completion requests, want selector only", len(requests))
	}
	if len(resolveRequests) != 2 {
		t.Fatalf("computer profile resolutions = %d, want native then generic", len(resolveRequests))
	}
	if resolveRequests[0].RequiredToolContract != executionprofile.AnthropicComputerToolContract ||
		resolveRequests[1].RequiredToolContract != executionprofile.GenericComputerUseToolContract {
		t.Fatalf("fallback contract order = %+v", resolveRequests)
	}
	if native.runs != 0 || generic.runs != 0 {
		t.Fatalf("profile failure executed computer native=%d generic=%d", native.runs, generic.runs)
	}
	if ws.Contains("computer") || ws.Contains("computer_use") {
		t.Fatalf("WorkingSet cached fallback computer schema: %+v", ws.Schemas())
	}
	if loop.ComputerActivation() != nil {
		t.Fatalf("profile failure created activation: %+v", loop.ComputerActivation())
	}
}

func TestComputerExactRouteSupportsColonModelIDs(t *testing.T) {
	resp := &client.CompletionResponse{Provider: "ollama", Model: "qwen3:4b"}
	if got := exactResponseModel(resp, ""); got != "ollama:qwen3:4b" {
		t.Fatalf("exactResponseModel = %q, want ollama-qualified colon model", got)
	}
	resp.Model = "ollama:qwen3:4b"
	if got := exactResponseModel(resp, ""); got != "ollama:qwen3:4b" {
		t.Fatalf("exactResponseModel double-prefixed route: %q", got)
	}
	profile := genericComputerProfileForAgentTest()
	profile.Provider = "ollama"
	profile.Model = "qwen3:4b"
	if err := profile.ValidateComputer(executionprofile.GenericComputerUseToolContract); err != nil {
		t.Fatalf("ValidateComputer colon model: %v", err)
	}
	if err := validateComputerProfileExactRoute(profile, "ollama:qwen3:4b"); err != nil {
		t.Fatalf("validateComputerProfileExactRoute: %v", err)
	}
}

func TestFastComputerSelectionUsesGenericWithoutSecondProfile(t *testing.T) {
	reg, native, generic := newComputerProfileRegistry()
	llm := &computerProfileSequenceLLM{
		responses: []*client.CompletionResponse{
			computerSearchResponse("openai", "gpt-5.6-terra"),
			computerCallResponse("openai", "gpt-5.6-terra", "computer_use"),
			finalComputerResponse("openai", "gpt-5.6-terra"),
		},
	}
	ws := NewWorkingSet()
	loop := NewAgentLoop(llm, reg, "large", "", 5, 1000, 100, nil, nil, nil)
	loop.SetSpecificModel("claude-sonnet-4-6")
	loop.SetReasoningEffort("high")
	loop.SetEffortTier("xhigh")
	loop.SetThinking(&client.ThinkingConfig{Type: "enabled", BudgetTokens: 4096})
	loop.SetKoeExecutionProfile(fastProfileForAgentTest())
	loop.SetWorkingSet(ws)
	if _, _, err := loop.Run(context.Background(), "Inspect the screen.", nil, nil); err != nil {
		t.Fatalf("Run: %v", err)
	}

	requests, resolveRequests := llm.snapshot()
	if len(requests) != 3 {
		t.Fatalf("completion requests = %d, want 3", len(requests))
	}
	if len(resolveRequests) != 0 {
		t.Fatalf("Fast computer selection stacked a second resolver profile: %+v", resolveRequests)
	}
	continuation := requests[1]
	if continuation.ExecutionProfileID != "kfp1_agent-test" ||
		continuation.ModelTier != "" ||
		continuation.SpecificModel != "" ||
		continuation.ReasoningEffort != "" ||
		continuation.EffortTier != "" ||
		continuation.Thinking != nil ||
		!continuation.ParallelToolCalls ||
		continuation.ResponseCachePolicy != executionprofile.ResponseCacheOff {
		t.Fatalf("Fast continuation profile drifted: %+v", continuation)
	}
	schema, ok := findToolSchema(continuation, "computer_use")
	if !ok || schema.Type != "function" || schema.DeferLoading {
		t.Fatalf("Fast generic schema = %+v, present=%v", schema, ok)
	}
	assertToolExposureInRequest(t, continuation, "computer", false, false)
	if native.runs != 0 || generic.runs != 1 {
		t.Fatalf("computer runs native=%d generic=%d, want 0/1", native.runs, generic.runs)
	}
	if ws.Contains("computer") || ws.Contains("computer_use") {
		t.Fatalf("WorkingSet cached Fast computer schema: %+v", ws.Schemas())
	}
	if loop.ComputerActivation() != nil {
		t.Fatalf("Fast path stacked ep1 activation: %+v", loop.ComputerActivation())
	}
}

func TestProfileBoundComputerRequiresFreshActivationEachRun(t *testing.T) {
	reg, native, generic := newComputerProfileRegistry()
	llm := &computerProfileSequenceLLM{
		responses: []*client.CompletionResponse{
			computerSearchResponse("anthropic", "claude-sonnet-4-6"),
			computerCallResponse("anthropic", "claude-sonnet-4-6", "computer"),
			finalComputerResponse("anthropic", "claude-sonnet-4-6"),
			computerSearchResponse("anthropic", "claude-sonnet-4-6"),
			computerCallResponse("anthropic", "claude-sonnet-4-6", "computer"),
			finalComputerResponse("anthropic", "claude-sonnet-4-6"),
		},
		resolve: func(req client.ComputerExecutionProfileRequest) (executionprofile.Profile, error) {
			if req.RequiredToolContract != executionprofile.AnthropicComputerToolContract {
				return executionprofile.Profile{}, errors.New("unexpected non-native contract")
			}
			return nativeComputerProfileForAgentTest(), nil
		},
	}
	ws := NewWorkingSet()
	loop := NewAgentLoop(llm, reg, "medium", "", 5, 1000, 100, nil, nil, nil)
	loop.SetSpecificModel("claude-sonnet-4-6")
	loop.SetWorkingSet(ws)
	for i := 0; i < 2; i++ {
		if _, _, err := loop.Run(context.Background(), "Inspect the screen.", nil, nil); err != nil {
			t.Fatalf("Run %d: %v", i+1, err)
		}
	}

	requests, resolveRequests := llm.snapshot()
	if len(requests) != 6 {
		t.Fatalf("completion requests = %d, want 6", len(requests))
	}
	if len(resolveRequests) != 2 {
		t.Fatalf("computer profile resolutions = %d, want one per Run", len(resolveRequests))
	}
	for _, index := range []int{0, 3} {
		request := requests[index]
		if request.ExecutionProfileID != "" {
			t.Fatalf("Run first request %d reused profile %q", index, request.ExecutionProfileID)
		}
		assertToolExposureInRequest(t, request, "computer", false, false)
		assertToolExposureInRequest(t, request, "computer_use", false, false)
	}
	for _, index := range []int{1, 4} {
		request := requests[index]
		if request.ExecutionProfileID != "ep1_native-agent-test" {
			t.Fatalf("continuation request %d profile = %q", index, request.ExecutionProfileID)
		}
		schema, ok := findToolSchema(request, "computer")
		if !ok || schema.Type != "computer_20251124" || schema.DeferLoading {
			t.Fatalf("continuation request %d native schema = %+v, present=%v", index, schema, ok)
		}
	}
	if native.runs != 2 || generic.runs != 0 {
		t.Fatalf("computer runs native=%d generic=%d, want 2/0", native.runs, generic.runs)
	}
	if ws.Contains("computer") || ws.Contains("computer_use") {
		t.Fatalf("WorkingSet cached profile-bound computer schema: %+v", ws.Schemas())
	}
}

func TestComputerActivationCheckpointPrecedesProfiledContinuation(t *testing.T) {
	reg, _, _ := newComputerProfileRegistry()
	llm := &computerProfileSequenceLLM{
		responses: []*client.CompletionResponse{
			computerSearchResponse("anthropic", "claude-sonnet-4-6"),
			{Provider: "anthropic", Model: "claude-sonnet-4-6", OutputText: "continuing", FinishReason: "end_turn"},
			finalComputerResponse("anthropic", "claude-sonnet-4-6"),
		},
		resolve: func(req client.ComputerExecutionProfileRequest) (executionprofile.Profile, error) {
			if req.RequiredToolContract != executionprofile.AnthropicComputerToolContract {
				return executionprofile.Profile{}, errors.New("unexpected non-native contract")
			}
			return nativeComputerProfileForAgentTest(), nil
		},
	}
	loop := NewAgentLoop(llm, reg, "medium", "", 8, 1000, 100, nil, nil, nil)
	loop.SetSpecificModel("claude-sonnet-4-6")
	loop.SetCheckpointFunc(func(context.Context) error {
		activation := loop.ComputerActivation()
		if activation == nil || activation.Profile.ProfileID != "ep1_native-agent-test" {
			t.Fatalf("checkpoint did not observe activated ep1 profile: %+v", activation)
		}
		llm.recordEvent("checkpoint")
		return nil
	})
	if _, _, err := loop.Run(context.Background(), "Inspect the screen.", nil, nil); err != nil {
		t.Fatalf("Run: %v", err)
	}

	events := llm.eventSnapshot()
	resolveIndex := eventIndex(events, "resolve:"+executionprofile.AnthropicComputerToolContract)
	checkpointIndex := eventIndex(events, "checkpoint")
	continuationIndex := eventIndex(events, "complete:ep1_native-agent-test")
	if resolveIndex < 0 || checkpointIndex < 0 || continuationIndex < 0 {
		t.Fatalf("activation event sequence incomplete: %v", events)
	}
	if !(resolveIndex < checkpointIndex && checkpointIndex < continuationIndex) {
		t.Fatalf("activation was not durably checkpointed before continuation: %v", events)
	}
}

func TestComputerActivationCheckpointFailureBlocksContinuation(t *testing.T) {
	reg, _, _ := newComputerProfileRegistry()
	llm := &computerProfileSequenceLLM{
		responses: []*client.CompletionResponse{
			computerSearchResponse("anthropic", "claude-sonnet-4-6"),
		},
		resolve: func(req client.ComputerExecutionProfileRequest) (executionprofile.Profile, error) {
			if req.RequiredToolContract != executionprofile.AnthropicComputerToolContract {
				return executionprofile.Profile{}, errors.New("unexpected non-native contract")
			}
			return nativeComputerProfileForAgentTest(), nil
		},
	}
	loop := NewAgentLoop(llm, reg, "medium", "", 4, 1000, 100, nil, nil, nil)
	loop.SetSpecificModel("claude-sonnet-4-6")
	checkpointErr := errors.New("checkpoint unavailable")
	loop.SetCheckpointFunc(func(context.Context) error {
		llm.recordEvent("checkpoint-failed")
		return checkpointErr
	})

	_, _, err := loop.Run(context.Background(), "Inspect the screen.", nil, nil)
	if err == nil || !errors.Is(err, checkpointErr) {
		t.Fatalf("Run error = %v, want checkpoint activation failure", err)
	}
	requests, resolveRequests := llm.snapshot()
	if len(requests) != 1 {
		t.Fatalf("checkpoint failure allowed %d completion requests, want 1", len(requests))
	}
	if len(resolveRequests) != 1 {
		t.Fatalf("computer profile resolutions = %d, want 1", len(resolveRequests))
	}
	events := llm.eventSnapshot()
	if eventIndex(events, "complete:ep1_native-agent-test") >= 0 {
		t.Fatalf("checkpoint failure leaked profiled continuation: %v", events)
	}
	wantEvents := []string{
		"complete:",
		"resolve:" + executionprofile.AnthropicComputerToolContract,
		"checkpoint-failed",
	}
	if !reflect.DeepEqual(events, wantEvents) {
		t.Fatalf("checkpoint failure events = %v, want %v", events, wantEvents)
	}
}

func TestComputerActivationCheckpointFailureBlocksForceStop(t *testing.T) {
	reg, _, _ := newComputerProfileRegistry()
	repeatedSearch := computerSearchResponse("anthropic", "claude-sonnet-4-6")
	repeatedSearch.ToolCalls = make([]client.FunctionCall, 4)
	for i := range repeatedSearch.ToolCalls {
		repeatedSearch.ToolCalls[i] = client.FunctionCall{
			ID:        fmt.Sprintf("toolu-search-computer-%d", i),
			Name:      "tool_search",
			Arguments: json.RawMessage(`{"query":"select:computer"}`),
		}
	}
	llm := &computerProfileSequenceLLM{
		responses: []*client.CompletionResponse{repeatedSearch},
		resolve: func(req client.ComputerExecutionProfileRequest) (executionprofile.Profile, error) {
			if req.RequiredToolContract != executionprofile.AnthropicComputerToolContract {
				return executionprofile.Profile{}, errors.New("unexpected non-native contract")
			}
			return nativeComputerProfileForAgentTest(), nil
		},
	}
	loop := NewAgentLoop(llm, reg, "medium", "", 8, 1000, 100, nil, nil, nil)
	loop.SetSpecificModel("claude-sonnet-4-6")
	checkpointErr := errors.New("force-stop checkpoint unavailable")
	loop.SetCheckpointFunc(func(context.Context) error {
		llm.recordEvent("checkpoint-failed")
		return checkpointErr
	})

	_, _, err := loop.Run(context.Background(), "Inspect the screen.", nil, nil)
	if err == nil || !errors.Is(err, checkpointErr) {
		t.Fatalf("Run error = %v, want force-stop checkpoint failure", err)
	}
	if !strings.Contains(err.Error(), "before force stop") {
		t.Fatalf("Run error = %v, want force-stop checkpoint path", err)
	}
	requests, resolveRequests := llm.snapshot()
	if len(requests) != 1 {
		t.Fatalf("force-stop checkpoint failure allowed %d completion requests, want 1", len(requests))
	}
	if len(resolveRequests) != 1 {
		t.Fatalf("computer profile resolutions = %d, want 1", len(resolveRequests))
	}
	if eventIndex(llm.eventSnapshot(), "complete:ep1_native-agent-test") >= 0 {
		t.Fatalf("checkpoint failure leaked profiled force-stop continuation: %v", llm.eventSnapshot())
	}
}

func TestComputerForceStopForbidsFurtherToolCalls(t *testing.T) {
	reg, _, _ := newComputerProfileRegistry()
	repeatedSearch := computerSearchResponse("anthropic", "claude-sonnet-4-6")
	repeatedSearch.ToolCalls = make([]client.FunctionCall, 4)
	for i := range repeatedSearch.ToolCalls {
		repeatedSearch.ToolCalls[i] = client.FunctionCall{
			ID:        fmt.Sprintf("toolu-search-computer-%d", i),
			Name:      "tool_search",
			Arguments: json.RawMessage(`{"query":"select:computer"}`),
		}
	}
	llm := &computerProfileSequenceLLM{
		responses: []*client.CompletionResponse{
			repeatedSearch,
			finalComputerResponse("anthropic", "claude-sonnet-4-6"),
		},
		resolve: func(req client.ComputerExecutionProfileRequest) (executionprofile.Profile, error) {
			return nativeComputerProfileForAgentTest(), nil
		},
	}
	loop := NewAgentLoop(llm, reg, "medium", "", 8, 1000, 100, nil, nil, nil)
	loop.SetSpecificModel("claude-sonnet-4-6")
	loop.SetCheckpointFunc(func(context.Context) error { return nil })

	if _, _, err := loop.Run(context.Background(), "Inspect the screen.", nil, nil); err != nil {
		t.Fatalf("Run: %v", err)
	}
	requests, _ := llm.snapshot()
	if len(requests) != 2 {
		t.Fatalf("completion requests = %d, want selector plus force stop", len(requests))
	}
	forceStop := requests[1]
	if forceStop.ExecutionProfileID != "ep1_native-agent-test" ||
		forceStop.ToolChoice != "none" {
		t.Fatalf("force-stop profile/tool choice = %+v", forceStop)
	}
	if schema, ok := findToolSchema(forceStop, "computer"); !ok || schema.Type != "computer_20251124" {
		t.Fatalf("force-stop native schema = %+v, present=%v", schema, ok)
	}
}

func TestResumeInterruptedRejectsComputerToolsetDriftBeforeLLM(t *testing.T) {
	reg, _, _ := newComputerProfileRegistry()
	llm := &computerProfileSequenceLLM{}
	loop := NewAgentLoop(llm, reg, "medium", "", 4, 1000, 100, nil, nil, nil)
	activation := &executionprofile.ComputerActivation{
		Profile:            nativeComputerProfileForAgentTest(),
		ToolName:           "computer",
		ToolsetFingerprint: "stale-toolset",
	}
	if err := loop.RestoreComputerActivation(activation); err != nil {
		t.Fatalf("RestoreComputerActivation: %v", err)
	}

	_, _, err := loop.ResumeInterrupted(
		context.Background(),
		"Continue the interrupted task.",
		[]client.Message{{Role: "user", Content: client.NewTextContent("Inspect the screen.")}},
	)
	if !errors.Is(err, ErrComputerActivationToolsetChanged) {
		t.Fatalf("ResumeInterrupted error = %v, want ErrComputerActivationToolsetChanged", err)
	}
	requests, resolveRequests := llm.snapshot()
	if len(requests) != 0 || len(resolveRequests) != 0 {
		t.Fatalf("toolset drift reached LLM/resolver: requests=%d resolves=%d", len(requests), len(resolveRequests))
	}
	if got := loop.ComputerActivation(); got == nil ||
		got.Profile.ProfileID != activation.Profile.ProfileID ||
		got.ToolsetFingerprint != activation.ToolsetFingerprint {
		t.Fatalf("toolset drift discarded activation: %+v", got)
	}
}

func eventIndex(events []string, want string) int {
	for i, event := range events {
		if event == want {
			return i
		}
	}
	return -1
}

// Recovery runs at daemon start while MCP servers are still connecting, so
// registry churn OUTSIDE the activation's own tool must not fail the resume —
// the old whole-registry fingerprint burned a recovery attempt on every
// restart. Only the computer tool's own contract is pinned.
func TestResumeInterruptedToleratesUnrelatedRegistryChurn(t *testing.T) {
	reg, _, _ := newComputerProfileRegistry()
	llm := &computerProfileSequenceLLM{
		responses: []*client.CompletionResponse{{
			OutputText:   "resumed fine",
			FinishReason: "end_turn",
		}},
	}
	loop := NewAgentLoop(llm, reg, "medium", "", 4, 1000, 100, nil, nil, nil)
	activation := &executionprofile.ComputerActivation{
		Profile:            nativeComputerProfileForAgentTest(),
		ToolName:           "computer",
		ToolsetFingerprint: computerActivationFingerprint(reg, "computer"),
	}
	if err := loop.RestoreComputerActivation(activation); err != nil {
		t.Fatalf("RestoreComputerActivation: %v", err)
	}

	// A late-connecting MCP server registers a tool the checkpoint never saw.
	reg.Register(&mockSimpleTool{name: "late_mcp_tool", result: ToolResult{Content: "x"}})

	_, _, err := loop.ResumeInterrupted(
		context.Background(),
		"Continue the interrupted task.",
		[]client.Message{{Role: "user", Content: client.NewTextContent("Inspect the screen.")}},
	)
	if errors.Is(err, ErrComputerActivationToolsetChanged) {
		t.Fatalf("unrelated registry churn abandoned a valid checkpoint: %v", err)
	}
	requests, _ := llm.snapshot()
	if len(requests) == 0 {
		t.Fatal("resume never reached the LLM")
	}
}
