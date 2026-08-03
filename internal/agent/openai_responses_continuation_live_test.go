package agent

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Kocoro-lab/ShanClaw/internal/client"
)

const (
	openAIResponsesLiveGate       = "KOE_SOL_RESPONSES_LIVE"
	openAIResponsesLiveRepeatsEnv = "KOE_SOL_RESPONSES_REPEATS"
	openAIResponsesLiveEndpoint   = "KOE_SOL_RESPONSES_ENDPOINT"
	openAIResponsesLiveAPIKey     = "KOE_SOL_RESPONSES_API_KEY"
	openAIResponsesLiveModelEnv   = "KOE_OPENAI_RESPONSES_MODEL"
	openAIResponsesDefaultModel   = "gpt-5.6-sol"
	anthropicNudgeLiveGate        = "KOE_ANTHROPIC_NUDGE_LIVE"
	anthropicNudgeLiveModelsEnv   = "KOE_ANTHROPIC_NUDGE_MODELS"
)

// TestAgentLoopOpenAIResponsesContinuationLive is a paid, opt-in release gate.
// It exercises the production streaming GatewayClient and AgentLoop across an
// ordinary Responses function call, local tool execution, cursor continuation,
// and final text. Normal go test runs stop at the gate before reading endpoint
// or credential configuration.
func TestAgentLoopOpenAIResponsesContinuationLive(t *testing.T) {
	if os.Getenv(openAIResponsesLiveGate) != "1" {
		t.Skip("set KOE_SOL_RESPONSES_LIVE=1 to run the paid Sol continuation gate")
	}

	repeats := 3
	if raw := strings.TrimSpace(os.Getenv(openAIResponsesLiveRepeatsEnv)); raw != "" {
		parsed, err := strconv.Atoi(raw)
		if err != nil || parsed < 1 || parsed > 20 {
			t.Fatal("KOE_SOL_RESPONSES_REPEATS must be an integer from 1 through 20")
		}
		repeats = parsed
	}
	endpoint := strings.TrimSpace(os.Getenv(openAIResponsesLiveEndpoint))
	if endpoint == "" {
		endpoint = "http://127.0.0.1:8080"
	}
	apiKey := strings.TrimSpace(os.Getenv(openAIResponsesLiveAPIKey))
	if apiKey == "" {
		apiKey = "sk_test_123456"
	}
	model := strings.TrimSpace(os.Getenv(openAIResponsesLiveModelEnv))
	if model == "" {
		model = openAIResponsesDefaultModel
	}

	latencies := make([]time.Duration, 0, repeats)
	var totalCost float64
	for run := 1; run <= repeats; run++ {
		gateway := client.NewGatewayClient(endpoint, apiKey)
		recording := &openAIResponsesLiveRecordingClient{inner: gateway}
		effect := &openAIResponsesLiveLookupTool{}
		registry := NewToolRegistry()
		registry.Register(effect)

		loop := NewAgentLoop(
			recording,
			registry,
			"large",
			t.TempDir(),
			4,
			2000,
			200,
			nil,
			nil,
			nil,
		)
		loop.SetSpecificModel(model)
		loop.SetReasoningEffort("high")
		loop.SetTemperature(0)
		loop.SetMaxTokens(512)
		loop.SetSkillDiscovery(false)
		loop.SetEnableStreaming(true)
		loop.SetBypassPermissions(true)
		loop.SetCacheSource("koe_sol_responses_live")
		loop.SetSessionID(fmt.Sprintf("koe-sol-responses-%d-%d", time.Now().UnixNano(), run))
		loop.SetHandler(&mockHandler{approveResult: true})

		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		started := time.Now()
		result, usage, err := loop.Run(
			ctx,
			"Call lookup exactly once with key cobalt. Treat its result as authoritative. "+
				"After the tool returns, reply with exactly BLUE and nothing else.",
			nil,
			nil,
		)
		elapsed := time.Since(started)
		cancel()
		if err != nil {
			t.Fatalf("run %d AgentLoop failed: %v", run, err)
		}
		if strings.TrimSpace(result) != "BLUE" {
			t.Fatalf("run %d final answer did not match the exact oracle", run)
		}
		if got := effect.executions.Load(); got != 1 {
			t.Fatalf("run %d tool executions = %d, want exactly 1", run, got)
		}

		requests, responses := recording.snapshot()
		if len(requests) != 2 || len(responses) != 2 {
			t.Fatalf(
				"run %d completion shape = %d requests/%d responses, want 2/2",
				run,
				len(requests),
				len(responses),
			)
		}
		if requests[0].PreviousResponseID != "" {
			t.Fatalf("run %d initial request unexpectedly carried a cursor", run)
		}
		if !validOrdinaryOpenAIResponseID(requests[1].PreviousResponseID) {
			t.Fatalf("run %d continuation did not carry a trusted Responses cursor", run)
		}
		if requests[1].SpecificModel != model {
			t.Fatalf(
				"run %d continuation model = %q, want %q",
				run,
				requests[1].SpecificModel,
				model,
			)
		}
		if responses[0].Provider != "openai" ||
			responses[0].Model != model ||
			!responses[0].HasToolCalls() ||
			responses[1].Provider != "openai" ||
			responses[1].Model != model ||
			responses[1].HasToolCalls() {
			t.Fatalf("run %d provider/model/tool trajectory failed", run)
		}
		if usage == nil || usage.TotalTokens <= 0 || usage.CostUSD <= 0 {
			t.Fatalf("run %d did not report non-zero usage and cost", run)
		}
		totalCost += usage.CostUSD
		latencies = append(latencies, elapsed)
		t.Logf(
			"content-free continuation model=%s run=%d pass=true completion_calls=2 tool_effects=1 total_millis=%d cost_usd=%.6f",
			model,
			run,
			elapsed.Milliseconds(),
			usage.CostUSD,
		)
	}

	sort.Slice(latencies, func(i, j int) bool { return latencies[i] < latencies[j] })
	t.Logf(
		"content-free continuation model=%s verdict pass=true runs=%d p50_millis=%d p95_millis=%d total_cost_usd=%.6f",
		model,
		repeats,
		nearestRankDuration(latencies, 50).Milliseconds(),
		nearestRankDuration(latencies, 95).Milliseconds(),
		totalCost,
	)
}

func TestAgentLoopOpenAIResponsesLoopNudgeLive(t *testing.T) {
	if os.Getenv(openAIResponsesLiveGate) != "1" {
		t.Skip("set KOE_SOL_RESPONSES_LIVE=1 to run the paid Sol continuation gate")
	}

	endpoint := strings.TrimSpace(os.Getenv(openAIResponsesLiveEndpoint))
	if endpoint == "" {
		endpoint = "http://127.0.0.1:8080"
	}
	apiKey := strings.TrimSpace(os.Getenv(openAIResponsesLiveAPIKey))
	if apiKey == "" {
		apiKey = "sk_test_123456"
	}
	model := strings.TrimSpace(os.Getenv(openAIResponsesLiveModelEnv))
	if model == "" {
		model = openAIResponsesDefaultModel
	}
	runAgentLoopNudgeLive(t, endpoint, apiKey, "openai", model, true)
}

func TestAgentLoopAnthropicLoopNudgeLive(t *testing.T) {
	if os.Getenv(anthropicNudgeLiveGate) != "1" {
		t.Skip("set KOE_ANTHROPIC_NUDGE_LIVE=1 to run the paid Anthropic nudge gate")
	}

	endpoint := strings.TrimSpace(os.Getenv(openAIResponsesLiveEndpoint))
	if endpoint == "" {
		endpoint = "http://127.0.0.1:8080"
	}
	apiKey := strings.TrimSpace(os.Getenv(openAIResponsesLiveAPIKey))
	if apiKey == "" {
		apiKey = "sk_test_123456"
	}
	models := strings.TrimSpace(os.Getenv(anthropicNudgeLiveModelsEnv))
	if models == "" {
		models = "claude-haiku-4-5-20251001,claude-sonnet-5,claude-opus-5"
	}
	for _, rawModel := range strings.Split(models, ",") {
		model := strings.TrimSpace(rawModel)
		if model == "" {
			continue
		}
		t.Run(model, func(t *testing.T) {
			runAgentLoopNudgeLive(t, endpoint, apiKey, "anthropic", model, false)
		})
	}
}

func runAgentLoopNudgeLive(
	t *testing.T,
	endpoint string,
	apiKey string,
	expectedProvider string,
	model string,
	expectsCursor bool,
) {
	t.Helper()

	gateway := client.NewGatewayClient(endpoint, apiKey)
	recording := &openAIResponsesLiveRecordingClient{inner: gateway}
	effect := &openAIResponsesLiveCountdownTool{
		final: fmt.Sprintf("VERIFIED_%d", time.Now().UnixNano()),
	}
	registry := NewToolRegistry()
	registry.Register(effect)

	loop := NewAgentLoop(
		recording,
		registry,
		"large",
		t.TempDir(),
		8,
		2000,
		200,
		nil,
		nil,
		nil,
	)
	loop.SetSpecificModel(model)
	if expectedProvider == "openai" {
		loop.SetReasoningEffort("none")
	}
	loop.SetTemperature(0)
	loop.SetMaxTokens(512)
	loop.SetSkillDiscovery(false)
	loop.SetEnableStreaming(true)
	loop.SetBypassPermissions(true)
	loop.SetCacheSource("koe_tool_nudge_live")
	loop.SetSessionID(fmt.Sprintf("koe-tool-nudge-%d", time.Now().UnixNano()))
	loop.SetHandler(&mockHandler{approveResult: true})

	// Three identical successful calls make request 4 carry the loop nudge;
	// the tool's third result is the runtime oracle that ends the live turn.
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	started := time.Now()
	result, usage, err := loop.Run(
		ctx,
		"Call countdown exactly once per response with key cobalt. If its result "+
			"starts with CONTINUE, call countdown again with the same key. "+
			"Otherwise reply with that exact result and nothing else.",
		nil,
		nil,
	)
	if err != nil {
		t.Fatalf("AgentLoop failed: %v", err)
	}

	requests, responses := recording.snapshot()
	if len(requests) != 4 || len(responses) != 4 {
		t.Fatalf(
			"completion shape = %d requests/%d responses, want 4/4",
			len(requests),
			len(responses),
		)
	}
	for index, request := range requests {
		if request.SpecificModel != model {
			t.Fatalf("request %d model = %q, want %q", index+1, request.SpecificModel, model)
		}
		if index == 0 || !expectsCursor {
			if request.PreviousResponseID != "" {
				t.Fatalf("request %d unexpectedly carried cursor %q", index+1, request.PreviousResponseID)
			}
		} else if !validOrdinaryOpenAIResponseID(request.PreviousResponseID) {
			t.Fatalf("continuation request %d lost its cursor", index+1)
		}
		response := responses[index]
		if response.Provider != expectedProvider || response.Model != model {
			t.Fatalf(
				"response %d route = %s/%s, want %s/%s",
				index+1,
				response.Provider,
				response.Model,
				expectedProvider,
				model,
			)
		}
	}
	if responses[3].HasToolCalls() {
		t.Fatal("final response unexpectedly contained a tool call")
	}

	nudgeRequest := findLoopNudgeRequest(requests)
	if nudgeRequest == nil {
		t.Fatal("no continuation request carried the loop nudge")
	}
	assertToolResultPrecedesNudge(t, nudgeRequest)
	if got := effect.executions.Load(); got != 3 {
		t.Fatalf("tool executions = %d, want exactly 3", got)
	}
	if !strings.Contains(result, effect.final) {
		digest := sha256.Sum256([]byte(result))
		t.Fatalf(
			"final answer missed runtime oracle: result_empty=%t result_len=%d result_sha256=%x",
			result == "",
			len(result),
			digest[:6],
		)
	}
	if usage == nil || usage.TotalTokens <= 0 || usage.CostUSD <= 0 {
		t.Fatal("run did not report non-zero usage and cost")
	}
	t.Logf(
		"content-free loop nudge provider=%s model=%s cursor=%t verdict pass=true completion_calls=4 tool_effects=3 total_millis=%d cost_usd=%.6f",
		expectedProvider,
		model,
		expectsCursor,
		time.Since(started).Milliseconds(),
		usage.CostUSD,
	)
}

type openAIResponsesLiveLookupTool struct {
	executions atomic.Int32
}

func (t *openAIResponsesLiveLookupTool) Info() ToolInfo {
	return ToolInfo{
		Name:        "lookup",
		Description: "Return the authoritative value for one key.",
		Parameters: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"key": map[string]any{"type": "string"},
			},
			"required": []string{"key"},
		},
	}
}

func (t *openAIResponsesLiveLookupTool) Run(_ context.Context, args string) (ToolResult, error) {
	var input struct {
		Key string `json:"key"`
	}
	if err := json.Unmarshal([]byte(args), &input); err != nil {
		return ValidationError("lookup: invalid arguments: " + err.Error()), nil
	}
	if input.Key != "cobalt" {
		return ValidationError("lookup: key must be cobalt"), nil
	}
	t.executions.Add(1)
	return ToolResult{Content: "BLUE"}, nil
}

func (*openAIResponsesLiveLookupTool) RequiresApproval() bool { return false }

type openAIResponsesLiveCountdownTool struct {
	executions atomic.Int32
	final      string
}

func (t *openAIResponsesLiveCountdownTool) Info() ToolInfo {
	return ToolInfo{
		Name:        "countdown",
		Description: "Advance a fixed verification countdown by one step.",
		Parameters: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"key": map[string]any{"type": "string"},
			},
			"required": []string{"key"},
		},
	}
}

func (t *openAIResponsesLiveCountdownTool) Run(_ context.Context, args string) (ToolResult, error) {
	var input struct {
		Key string `json:"key"`
	}
	if err := json.Unmarshal([]byte(args), &input); err != nil {
		return ValidationError("countdown: invalid arguments: " + err.Error()), nil
	}
	if input.Key != "cobalt" {
		return ValidationError("countdown: key must be cobalt"), nil
	}
	step := t.executions.Add(1)
	if step < 3 {
		return ToolResult{Content: fmt.Sprintf("CONTINUE_%d", step)}, nil
	}
	if step == 3 {
		return ToolResult{Content: t.final}, nil
	}
	return ToolResult{Content: "countdown called too many times", IsError: true}, nil
}

func (*openAIResponsesLiveCountdownTool) RequiresApproval() bool { return false }

type openAIResponsesLiveRecordingClient struct {
	inner client.LLMClient

	mu        sync.Mutex
	requests  []client.CompletionRequest
	responses []client.CompletionResponse
}

func (c *openAIResponsesLiveRecordingClient) Complete(
	ctx context.Context,
	req client.CompletionRequest,
) (*client.CompletionResponse, error) {
	c.recordRequest(req)
	response, err := c.inner.Complete(ctx, req)
	c.recordResponse(response)
	return response, err
}

func (c *openAIResponsesLiveRecordingClient) CompleteStream(
	ctx context.Context,
	req client.CompletionRequest,
	onDelta func(client.StreamDelta),
) (*client.CompletionResponse, error) {
	c.recordRequest(req)
	response, err := c.inner.CompleteStream(ctx, req, onDelta)
	c.recordResponse(response)
	return response, err
}

func (c *openAIResponsesLiveRecordingClient) recordRequest(req client.CompletionRequest) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.requests = append(c.requests, req)
}

func (c *openAIResponsesLiveRecordingClient) recordResponse(response *client.CompletionResponse) {
	if response == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.responses = append(c.responses, *response)
}

func (c *openAIResponsesLiveRecordingClient) snapshot() (
	[]client.CompletionRequest,
	[]client.CompletionResponse,
) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]client.CompletionRequest(nil), c.requests...),
		append([]client.CompletionResponse(nil), c.responses...)
}

func nearestRankDuration(sorted []time.Duration, percentile int) time.Duration {
	if len(sorted) == 0 {
		return 0
	}
	index := (percentile*len(sorted)+99)/100 - 1
	if index < 0 {
		index = 0
	}
	if index >= len(sorted) {
		index = len(sorted) - 1
	}
	return sorted[index]
}
