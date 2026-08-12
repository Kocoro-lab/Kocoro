//go:build live

package tools

import (
	"context"
	"fmt"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Kocoro-lab/ShanClaw/internal/agent"
	"github.com/Kocoro-lab/ShanClaw/internal/client"
	"github.com/Kocoro-lab/ShanClaw/internal/executionprofile"
)

// This test crosses the real Shan AgentLoop, an isolated Shannon Cloud
// deployment, the selected Anthropic adapter, and macOS screen capture. It is
// intentionally gated because it sends a real desktop screenshot to the
// configured provider and incurs provider cost.
//
//	KOE_NATIVE_COMPUTER_LIVE=1 \
//	TOOLSEARCH_CLOUD_ENDPOINT=http://127.0.0.1:18080 \
//	go test -tags=live ./internal/tools -run TestKoeNativeComputerLive -v -count=1
func TestKoeNativeComputerLive(t *testing.T) {
	if os.Getenv("KOE_NATIVE_COMPUTER_LIVE") != "1" {
		t.Skip("set KOE_NATIVE_COMPUTER_LIVE=1 to run the isolated native-computer E2E")
	}
	endpoint := strings.TrimSpace(os.Getenv("TOOLSEARCH_CLOUD_ENDPOINT"))
	if endpoint == "" {
		t.Fatal("TOOLSEARCH_CLOUD_ENDPOINT is required for the isolated native-computer E2E")
	}
	model := strings.TrimSpace(os.Getenv("KOE_NATIVE_COMPUTER_MODEL"))
	if model == "" {
		model = "claude-sonnet-5"
	}

	recorder := &nativeComputerRecordingClient{
		inner: client.NewGatewayClient(
			endpoint,
			strings.TrimSpace(os.Getenv("TOOLSEARCH_CLOUD_API_KEY")),
		),
	}
	registry := agent.NewToolRegistry()
	rawComputer := &ComputerUseTool{}
	computer := &countingNativeComputerTool{
		inner:  NewAnthropicComputerAdapter(rawComputer, 1280, 800),
		events: recorder,
	}
	registry.Register(computer)
	registry.Register(rawComputer)

	loop := agent.NewAgentLoop(recorder, registry, "medium", "", 8, 30000, 500, nil, nil, nil)
	loop.SetSpecificModel(model)
	loop.SetThinking(&client.ThinkingConfig{Type: "adaptive"})
	loop.SetMaxTokens(32000)
	loop.SetSkillDiscovery(false)
	loop.SetBypassPermissions(true)

	var checkpointCount atomic.Int32
	loop.SetCheckpointMinInterval(0)
	loop.SetCheckpointFunc(func(context.Context) error {
		activation := loop.ComputerActivation()
		if activation == nil {
			return fmt.Errorf("computer activation was not visible at the checkpoint")
		}
		if err := activation.Profile.ValidateComputer(executionprofile.AnthropicComputerToolContract); err != nil {
			return fmt.Errorf("invalid checkpointed native computer profile: %w", err)
		}
		checkpointCount.Add(1)
		recorder.recordEvent("checkpoint:" + activation.Profile.ProfileID)
		return nil
	})

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	result, _, err := loop.Run(
		ctx,
		"First call tool_search with select:computer. Then call computer with action screenshot exactly once. Inspect the screenshot and reply with one brief factual observation.",
		nil,
		nil,
	)
	if err != nil {
		t.Fatalf("native computer AgentLoop: %v", err)
	}
	if strings.TrimSpace(result) == "" {
		t.Fatal("native computer AgentLoop returned an empty final answer")
	}
	requests, responses, resolves, events := recorder.snapshot()
	if got := computer.runs.Load(); got != 1 {
		t.Fatalf(
			"computer executions = %d, want exactly 1; calls=%v responses=%v events=%v",
			got,
			nativeComputerCallTrace(responses),
			nativeComputerResponseTrace(responses),
			events,
		)
	}
	if checkpointCount.Load() == 0 {
		t.Fatal("native computer activation was never checkpointed")
	}

	if len(requests) < 3 {
		t.Fatalf("completion requests = %d, want tool_search, computer, and final continuations", len(requests))
	}
	if requests[0].ExecutionProfileID != "" {
		t.Fatalf("initial request unexpectedly carried execution profile %q", requests[0].ExecutionProfileID)
	}
	if requests[0].SpecificModel != model {
		t.Fatalf("initial specific_model = %q, want %q", requests[0].SpecificModel, model)
	}
	assertLiveToolExposure(t, requests[0], "tool_search", true)
	assertLiveToolExposure(t, requests[0], "computer", false)
	assertLiveToolExposure(t, requests[0], "computer_use", false)

	if len(resolves) != 1 {
		t.Fatalf("computer profile resolutions = %d, want 1: %+v", len(resolves), resolves)
	}
	resolve := resolves[0]
	wantResolvedModel := "anthropic:" +
		strings.TrimPrefix(model, "anthropic:")
	if resolve.RequiredToolContract != executionprofile.AnthropicComputerToolContract ||
		resolve.SpecificModel != wantResolvedModel ||
		resolve.AllowModelFallback {
		t.Fatalf("computer resolver request = %+v", resolve)
	}

	activation := loop.ComputerActivation()
	if activation == nil {
		t.Fatal("native computer activation was cleared before Run returned")
	}
	if activation.ToolName != "computer" ||
		activation.Profile.Provider != "anthropic" ||
		activation.Profile.Model != strings.TrimPrefix(model, "anthropic:") ||
		activation.Profile.ToolContract != executionprofile.AnthropicComputerToolContract {
		t.Fatalf("computer activation = %+v", activation)
	}

	profiledRequest := -1
	for i, request := range requests {
		if request.ExecutionProfileID == activation.Profile.ProfileID {
			profiledRequest = i
			assertLiveNativeComputerSchema(t, request)
		}
	}
	if profiledRequest < 0 {
		t.Fatalf("no continuation used resolved profile %q", activation.Profile.ProfileID)
	}

	searchCalls, computerCalls := 0, 0
	for _, response := range responses {
		for _, call := range response.AllToolCalls() {
			switch call.Name {
			case "tool_search":
				searchCalls++
			case "computer":
				computerCalls++
			case "computer_use":
				t.Fatalf("native profile fell back to generic computer_use: %+v", call)
			}
		}
	}
	if searchCalls != 1 || computerCalls != 1 {
		t.Fatalf("provider tool calls tool_search=%d computer=%d, want 1/1", searchCalls, computerCalls)
	}

	resolveIndex := indexLiveEvent(events, "resolve:"+executionprofile.AnthropicComputerToolContract)
	checkpointIndex := indexLiveEventPrefix(events, "checkpoint:")
	profiledIndex := indexLiveEventPrefix(events, "complete:"+activation.Profile.ProfileID)
	toolIndex := indexLiveEvent(events, "tool:computer")
	if resolveIndex < 0 || checkpointIndex < 0 || profiledIndex < 0 || toolIndex < 0 ||
		!(resolveIndex < checkpointIndex && checkpointIndex < profiledIndex && profiledIndex < toolIndex) {
		t.Fatalf("native computer ordering = %v", events)
	}

	t.Logf(
		"VERDICT: PASS — result=%q profile=%s requests=%d events=%v",
		result,
		activation.Profile.ProfileID,
		len(requests),
		events,
	)
}

type countingNativeComputerTool struct {
	inner  *AnthropicComputerAdapter
	events *nativeComputerRecordingClient
	runs   atomic.Int32
}

func (t *countingNativeComputerTool) Info() agent.ToolInfo {
	return t.inner.Info()
}

func (t *countingNativeComputerTool) Run(ctx context.Context, args string) (agent.ToolResult, error) {
	t.runs.Add(1)
	t.events.recordEvent("tool:computer")
	return t.inner.Run(ctx, args)
}

func (t *countingNativeComputerTool) RequiresApproval() bool {
	return t.inner.RequiresApproval()
}

func (t *countingNativeComputerTool) IsReadOnlyCall(args string) bool {
	return t.inner.IsReadOnlyCall(args)
}

func (*countingNativeComputerTool) ToolExposure() agent.ToolExposure {
	return agent.ToolExposureDeferred
}

func (*countingNativeComputerTool) ToolProfileRequirement() agent.ToolProfileRequirement {
	return agent.ToolProfileComputer
}

func (t *countingNativeComputerTool) NativeToolDef() *client.NativeToolDef {
	return t.inner.NativeToolDef()
}

type nativeComputerRecordingClient struct {
	inner *client.GatewayClient

	mu        sync.Mutex
	requests  []client.CompletionRequest
	responses []client.CompletionResponse
	resolves  []client.ComputerExecutionProfileRequest
	events    []string
}

func (c *nativeComputerRecordingClient) Complete(
	ctx context.Context,
	request client.CompletionRequest,
) (*client.CompletionResponse, error) {
	c.recordRequest(request)
	response, err := c.inner.Complete(ctx, request)
	c.recordResponse(response)
	return response, err
}

func (c *nativeComputerRecordingClient) CompleteStream(
	ctx context.Context,
	request client.CompletionRequest,
	onDelta func(client.StreamDelta),
) (*client.CompletionResponse, error) {
	c.recordRequest(request)
	response, err := c.inner.CompleteStream(ctx, request, onDelta)
	c.recordResponse(response)
	return response, err
}

func (c *nativeComputerRecordingClient) ResolveComputerExecutionProfile(
	ctx context.Context,
	request client.ComputerExecutionProfileRequest,
) (executionprofile.Profile, error) {
	c.mu.Lock()
	c.resolves = append(c.resolves, request)
	c.events = append(c.events, "resolve:"+request.RequiredToolContract)
	c.mu.Unlock()
	return c.inner.ResolveComputerExecutionProfile(ctx, request)
}

func (c *nativeComputerRecordingClient) recordRequest(request client.CompletionRequest) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.requests = append(c.requests, request)
	c.events = append(c.events, "complete:"+request.ExecutionProfileID)
}

func (c *nativeComputerRecordingClient) recordResponse(response *client.CompletionResponse) {
	if response == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.responses = append(c.responses, *response)
}

func (c *nativeComputerRecordingClient) recordEvent(event string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.events = append(c.events, event)
}

func (c *nativeComputerRecordingClient) snapshot() (
	[]client.CompletionRequest,
	[]client.CompletionResponse,
	[]client.ComputerExecutionProfileRequest,
	[]string,
) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]client.CompletionRequest(nil), c.requests...),
		append([]client.CompletionResponse(nil), c.responses...),
		append([]client.ComputerExecutionProfileRequest(nil), c.resolves...),
		append([]string(nil), c.events...)
}

func liveToolName(schema client.Tool) string {
	if schema.Name != "" {
		return schema.Name
	}
	return schema.Function.Name
}

func assertLiveToolExposure(t *testing.T, request client.CompletionRequest, name string, want bool) {
	t.Helper()
	for _, schema := range request.Tools {
		if liveToolName(schema) == name {
			if !want {
				t.Fatalf("initial request unexpectedly advertised %q: %+v", name, schema)
			}
			return
		}
	}
	if want {
		t.Fatalf("request did not advertise %q: %+v", name, request.Tools)
	}
}

func assertLiveNativeComputerSchema(t *testing.T, request client.CompletionRequest) {
	t.Helper()
	found := false
	for _, schema := range request.Tools {
		switch liveToolName(schema) {
		case "computer":
			found = true
			if schema.Type != "computer_20251124" || schema.DeferLoading ||
				schema.DisplayWidthPx <= 0 || schema.DisplayHeightPx <= 0 {
				t.Fatalf("native computer schema = %+v", schema)
			}
		case "computer_use":
			t.Fatalf("profiled native request also advertised generic computer_use: %+v", schema)
		}
	}
	if !found {
		t.Fatalf("profiled request omitted native computer schema: %+v", request.Tools)
	}
}

func indexLiveEvent(events []string, want string) int {
	for i, event := range events {
		if event == want {
			return i
		}
	}
	return -1
}

func indexLiveEventPrefix(events []string, prefix string) int {
	for i, event := range events {
		if strings.HasPrefix(event, prefix) {
			return i
		}
	}
	return -1
}

func nativeComputerCallTrace(
	responses []client.CompletionResponse,
) []string {
	var trace []string
	for _, response := range responses {
		for _, call := range response.AllToolCalls() {
			trace = append(trace, fmt.Sprintf(
				"%s:%s:%s",
				call.ID,
				call.Name,
				call.ArgumentsString(),
			))
		}
	}
	return trace
}

func nativeComputerResponseTrace(
	responses []client.CompletionResponse,
) []string {
	trace := make([]string, 0, len(responses))
	for _, response := range responses {
		var blockTypes []string
		for _, block := range response.ContentBlocks {
			blockTypes = append(blockTypes, block.Type)
		}
		trace = append(trace, fmt.Sprintf(
			"finish=%s text=%d calls=%d blocks=%v",
			response.FinishReason,
			len(strings.TrimSpace(response.OutputText)),
			len(response.AllToolCalls()),
			blockTypes,
		))
	}
	return trace
}
