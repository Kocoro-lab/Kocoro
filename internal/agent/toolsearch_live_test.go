//go:build live

package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Kocoro-lab/ShanClaw/internal/client"
	"github.com/Kocoro-lab/ShanClaw/internal/executionprofile"
)

// These tests make real provider calls and cost real tokens. They stay gated
// in normal test runs:
//
//   TOOLSEARCH_CLOUD_LIVE=1 TOOLSEARCH_CLOUD_ENDPOINT=https://... \
//     TOOLSEARCH_CLOUD_API_KEY=... go test -tags=live ./internal/agent -run TestToolSearchLive_Cloud -v
//   TOOLSEARCH_OLLAMA_LIVE=1 OLLAMA_MODEL=qwen3:4b go test -tags=live ./internal/agent -run TestToolSearchLive_Ollama -v

type recordingLLMClient struct {
	inner client.LLMClient

	mu        sync.Mutex
	requests  []client.CompletionRequest
	responses []client.CompletionResponse

	// Offline E2E covers autonomous selection. The Cloud live gate records the
	// agent-built first request, then sends only its Direct tools and forces
	// tool_search once so the real provider must exercise the tool_reference
	// continuation instead of guessing a Deferred call from its summary.
	forceFirstTool      string
	hideDeferredOnFirst bool
}

func (c *recordingLLMClient) Complete(
	ctx context.Context,
	req client.CompletionRequest,
) (*client.CompletionResponse, error) {
	req = c.recordRequest(req)
	response, err := c.inner.Complete(ctx, req)
	c.recordResponse(response)
	return response, err
}

func (c *recordingLLMClient) CompleteStream(
	ctx context.Context,
	req client.CompletionRequest,
	onDelta func(client.StreamDelta),
) (*client.CompletionResponse, error) {
	req = c.recordRequest(req)
	response, err := c.inner.CompleteStream(ctx, req, onDelta)
	c.recordResponse(response)
	return response, err
}

func (c *recordingLLMClient) recordRequest(req client.CompletionRequest) client.CompletionRequest {
	c.mu.Lock()
	defer c.mu.Unlock()
	first := len(c.requests) == 0
	if first && c.forceFirstTool != "" {
		req.ToolChoice = map[string]any{
			"type": "tool",
			"name": c.forceFirstTool,
		}
	}
	c.requests = append(c.requests, req)
	if first && c.hideDeferredOnFirst {
		outbound := req
		outbound.Tools = make([]client.Tool, 0, len(req.Tools))
		for _, tool := range req.Tools {
			if !tool.DeferLoading {
				outbound.Tools = append(outbound.Tools, tool)
			}
		}
		return outbound
	}
	return req
}

func (c *recordingLLMClient) recordResponse(response *client.CompletionResponse) {
	if response == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.responses = append(c.responses, *response)
}

func (c *recordingLLMClient) snapshot() (
	[]client.CompletionRequest,
	[]client.CompletionResponse,
) {
	c.mu.Lock()
	defer c.mu.Unlock()
	requests := append([]client.CompletionRequest(nil), c.requests...)
	responses := append([]client.CompletionResponse(nil), c.responses...)
	return requests, responses
}

func TestToolSearchLive_CloudToolReference(t *testing.T) {
	if os.Getenv("TOOLSEARCH_CLOUD_LIVE") != "1" {
		t.Skip("set TOOLSEARCH_CLOUD_LIVE=1 to run the real Cloud ToolSearch trace")
	}

	endpoint := strings.TrimSpace(os.Getenv("TOOLSEARCH_CLOUD_ENDPOINT"))
	if endpoint == "" {
		t.Fatal("Cloud live gate is enabled but TOOLSEARCH_CLOUD_ENDPOINT is empty")
	}
	apiKey := strings.TrimSpace(os.Getenv("TOOLSEARCH_CLOUD_API_KEY"))
	if apiKey == "" {
		t.Fatal("Cloud live gate is enabled but TOOLSEARCH_CLOUD_API_KEY is empty")
	}
	model := strings.TrimSpace(os.Getenv("TOOLSEARCH_CLOUD_MODEL"))
	if model == "" {
		model = "claude-sonnet-4-6"
	}

	recorder := &recordingLLMClient{
		inner:               client.NewGatewayClient(endpoint, apiKey),
		forceFirstTool:      "tool_search",
		hideDeferredOnFirst: true,
	}
	runToolSearchLiveTrace(t, recorder, model, true, nil)
}

func TestToolSearchLive_KoeFastProfile(t *testing.T) {
	if os.Getenv("KOE_FAST_PROFILE_LIVE") != "1" {
		t.Skip("set KOE_FAST_PROFILE_LIVE=1 to run the real Koe Fast ToolSearch trace")
	}

	endpoint := strings.TrimSpace(os.Getenv("TOOLSEARCH_CLOUD_ENDPOINT"))
	if endpoint == "" {
		t.Fatal("Koe Fast live gate is enabled but TOOLSEARCH_CLOUD_ENDPOINT is empty")
	}
	gateway := client.NewGatewayClient(
		endpoint,
		strings.TrimSpace(os.Getenv("TOOLSEARCH_CLOUD_API_KEY")),
	)
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	profile, err := gateway.ResolveKoeExecutionProfile(ctx)
	if err != nil {
		t.Fatalf("resolve Koe fast execution profile: %v", err)
	}

	recorder := &recordingLLMClient{
		inner:               gateway,
		hideDeferredOnFirst: true,
	}
	runToolSearchLiveTrace(t, recorder, profile.Model, false, &profile)
}

func TestToolSearchLive_OllamaLegacy(t *testing.T) {
	if os.Getenv("TOOLSEARCH_OLLAMA_LIVE") != "1" {
		t.Skip("set TOOLSEARCH_OLLAMA_LIVE=1 to run the real Ollama ToolSearch trace")
	}

	endpoint := strings.TrimSpace(os.Getenv("OLLAMA_ENDPOINT"))
	if endpoint == "" {
		endpoint = "http://localhost:11434"
	}
	model := strings.TrimSpace(os.Getenv("OLLAMA_MODEL"))
	if model == "" {
		model = "qwen3:4b"
	}

	recorder := &recordingLLMClient{
		inner: client.NewOllamaClient(endpoint, model),
	}
	runToolSearchLiveTrace(t, recorder, model, false, nil)
}

func runToolSearchLiveTrace(
	t *testing.T,
	recorder *recordingLLMClient,
	model string,
	wantToolReference bool,
	profile *executionprofile.Profile,
) {
	t.Helper()

	const verificationReceipt = "TOOLSEARCH_LIVE_RECEIPT_7C91D2"
	probe := &toolSearchE2ETool{
		name:      "quasar_telemetry_probe",
		desc:      "Run a quasar telemetry verification probe.",
		source:    SourceIntegration,
		namespace: "observatory",
		params: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"probe_token": map[string]any{
					"type":        "string",
					"description": "Verification token for the quasar telemetry probe.",
				},
			},
			"required": []string{"probe_token"},
		},
		result: verificationReceipt,
	}
	registry := NewToolRegistry()
	registry.Register(probe)

	loop := NewAgentLoop(recorder, registry, "medium", "", 8, 4000, 500, nil, nil, nil)
	if profile != nil {
		loop.SetKoeExecutionProfile(*profile)
	} else {
		loop.SetSpecificModel(model)
	}
	// Keep the trace compatible with both current Cloud and legacy gateways
	// whose request validator still caps max_tokens at 32K.
	loop.SetMaxTokens(32000)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	traceID := fmt.Sprintf("toolsearch-live-%d", time.Now().UnixNano())
	result, _, err := loop.Run(
		ctx,
		"Use the observatory integration to run the quasar telemetry probe with "+
			"probe_token set to \"verified\", then tell me the receipt it returns. "+
			"The request trace label is "+traceID+".",
		nil,
		nil,
	)
	if err != nil {
		t.Fatalf("live AgentLoop trace: %v", err)
	}
	if !strings.Contains(result, verificationReceipt) {
		t.Fatalf("final answer = %q, want probe receipt", result)
	}
	if probe.runs != 1 {
		t.Fatalf("quasar_telemetry_probe runs = %d, want 1", probe.runs)
	}
	var probeArgs struct {
		ProbeToken string `json:"probe_token"`
	}
	if err := json.Unmarshal([]byte(probe.lastArgs), &probeArgs); err != nil {
		t.Fatalf("decode probe args %q: %v", probe.lastArgs, err)
	}
	if probeArgs.ProbeToken != "verified" {
		t.Fatalf("probe_token = %q, want verified", probeArgs.ProbeToken)
	}

	requests, responses := recorder.snapshot()
	minRequests := 3
	if wantToolReference {
		// Native providers may emit tool_search and the referenced call in one
		// tool-use batch, then continue once with both tool results.
		minRequests = 2
	}
	if len(requests) < minRequests {
		t.Fatalf("completion requests = %d, want at least %d", len(requests), minRequests)
	}
	assertToolExposureInRequest(t, requests[0], "tool_search", true, false)
	assertToolExposureInRequest(
		t,
		requests[0],
		"quasar_telemetry_probe",
		wantToolReference,
		wantToolReference,
	)

	searchCalls := 0
	searchQuery := ""
	for _, response := range responses {
		for _, call := range response.AllToolCalls() {
			if call.Name != "tool_search" {
				continue
			}
			searchCalls++
			var args struct {
				Query string `json:"query"`
			}
			if err := json.Unmarshal([]byte(call.ArgumentsString()), &args); err != nil {
				t.Fatalf("decode live tool_search args %q: %v", call.ArgumentsString(), err)
			}
			searchQuery = args.Query
		}
	}
	if searchCalls != 1 {
		t.Fatalf("tool_search calls = %d, want 1", searchCalls)
	}
	lowerQuery := strings.ToLower(searchQuery)
	if !strings.Contains(lowerQuery, "quasar") &&
		!strings.Contains(lowerQuery, "telemetry") &&
		!strings.Contains(lowerQuery, "observatory") {
		t.Fatalf("tool_search query = %q, want discriminating probe metadata", searchQuery)
	}

	continuedWithLoadedSchema := false
	for _, request := range requests[1:] {
		if wantToolReference {
			continuedWithLoadedSchema = continuedWithLoadedSchema ||
				requestHasToolReference(request, "quasar_telemetry_probe")
			continue
		}
		continuedWithLoadedSchema = continuedWithLoadedSchema ||
			(requestHasToolResultText(request, "LOADED:quasar_telemetry_probe") &&
				requestHasToolResultText(request, `"probe_token"`))
	}
	if !continuedWithLoadedSchema {
		if wantToolReference {
			t.Fatal("Cloud continuation is missing quasar_telemetry_probe tool_reference")
		}
		t.Fatal("Ollama continuation is missing the loaded quasar_telemetry_probe schema")
	}

	t.Logf(
		"provider trace verified: model=%s requests=%d tool_search_query=%q tool_reference=%v",
		model,
		len(requests),
		searchQuery,
		wantToolReference,
	)
}
