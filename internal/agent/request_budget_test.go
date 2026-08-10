package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/Kocoro-lab/ShanClaw/internal/client"
	"github.com/Kocoro-lab/ShanClaw/internal/runstatus"
)

func TestRequestLLMBudget_DefaultLimits(t *testing.T) {
	t.Parallel()

	if got := newRequestLLMBudget(100_000, 256).snapshot().TokenExposureLimit; got != 207_200_000 {
		t.Fatalf("small context token limit = %d, want dispatch-aligned allowance", got)
	}
	if got := newRequestLLMBudget(200_000, 256).snapshot().TokenExposureLimit; got != 414_400_000 {
		t.Fatalf("scaled token limit = %d, want dispatch-aligned allowance", got)
	}
	if got, want := newRequestLLMBudget(0, 256).snapshot().NormalDispatchLimit, 1036; got != want {
		t.Fatalf("default normal dispatch limit = %d, want %d", got, want)
	}
	if got := newRequestLLMBudget(0, 10).snapshot().NormalDispatchLimit; got != requestBudgetMinimumNormalDispatchLimit {
		t.Fatalf("short-loop normal dispatch limit = %d, want floor %d", got, requestBudgetMinimumNormalDispatchLimit)
	}
}

func TestRequestLLMBudget_DispatchLimitSaturatesOnOverflow(t *testing.T) {
	t.Parallel()

	maxInt := int(^uint(0) >> 1)
	if got := newRequestLLMBudget(0, maxInt).snapshot().NormalDispatchLimit; got != maxInt {
		t.Fatalf("normal dispatch limit = %d, want saturation at %d", got, maxInt)
	}
	maxInt64 := int64(^uint64(0) >> 1)
	if got := newRequestLLMBudget(maxInt, maxInt).snapshot().TokenExposureLimit; got != maxInt64 {
		t.Fatalf("token exposure limit = %d, want saturation at %d", got, maxInt64)
	}
}

func TestRequestLLMBudget_DispatchClassesAndTerminalReserve(t *testing.T) {
	t.Parallel()

	b := newRequestLLMBudget(0, 10)
	for i := 0; i < requestBudgetHelperDispatchLimit; i++ {
		if _, err := b.reserve(requestBudgetHelper, 0); err != nil {
			t.Fatalf("helper reserve %d: %v", i+1, err)
		}
	}
	if _, err := b.reserve(requestBudgetHelper, 0); !errors.Is(err, ErrRequestBudgetExhausted) {
		t.Fatalf("helper over limit error = %v, want budget exhausted", err)
	}
	for i := requestBudgetHelperDispatchLimit; i < b.normalDispatchLimit; i++ {
		if _, err := b.reserve(requestBudgetMain, 0); err != nil {
			t.Fatalf("normal reserve %d: %v", i+1, err)
		}
	}
	if _, err := b.reserve(requestBudgetFork, 0); !errors.Is(err, ErrRequestBudgetExhausted) {
		t.Fatalf("normal over limit error = %v, want budget exhausted", err)
	}
	if _, err := b.reserve(requestBudgetTerminal, 2_000_000); err != nil {
		t.Fatalf("independent terminal reserve: %v", err)
	}
	if _, err := b.reserve(requestBudgetTerminal, 0); !errors.Is(err, ErrRequestBudgetExhausted) {
		t.Fatalf("second terminal error = %v, want budget exhausted", err)
	}

	snapshot := b.snapshot()
	if snapshot.NormalDispatches != requestBudgetMinimumNormalDispatchLimit || snapshot.HelperDispatches != 8 || snapshot.TerminalDispatches != 1 {
		t.Fatalf("unexpected dispatch snapshot: %+v", snapshot)
	}
}

func TestRequestLLMBudget_ReconcileKnownUnknownAndCached(t *testing.T) {
	t.Parallel()

	b := newRequestLLMBudget(0, 10)
	known, err := b.reserve(requestBudgetMain, 1_000)
	if err != nil {
		t.Fatal(err)
	}
	known.reconcile(&client.CompletionResponse{Usage: client.Usage{
		InputTokens: 40, OutputTokens: 10, CacheReadTokens: 20, CacheCreationTokens: 5,
	}}, nil)
	known.reconcile(nil, errors.New("double reconcile must be ignored"))

	unknown, err := b.reserve(requestBudgetMain, 300)
	if err != nil {
		t.Fatal(err)
	}
	unknown.reconcile(nil, errors.New("transport failed without usage"))

	cached, err := b.reserve(requestBudgetMain, 200)
	if err != nil {
		t.Fatal(err)
	}
	cached.reconcile(&client.CompletionResponse{Cached: true}, nil)

	zero, err := b.reserve(requestBudgetMain, 100)
	if err != nil {
		t.Fatal(err)
	}
	zero.reconcile(&client.CompletionResponse{}, nil)
	totalOnly, err := b.reserve(requestBudgetMain, 500)
	if err != nil {
		t.Fatal(err)
	}
	totalOnly.reconcile(&client.CompletionResponse{Usage: client.Usage{
		TotalTokens: 90, CacheReadTokens: 7, CacheCreationTokens: 3,
	}}, nil)
	costOnly, err := b.reserve(requestBudgetMain, 50)
	if err != nil {
		t.Fatal(err)
	}
	costOnly.reconcile(&client.CompletionResponse{Usage: client.Usage{CostUSD: 0.01, WebSearchCalls: 1}}, nil)

	snapshot := b.snapshot()
	if snapshot.ReservedTokens != 0 {
		t.Fatalf("reserved tokens = %d, want 0", snapshot.ReservedTokens)
	}
	if snapshot.ConsumedTokens != 625 {
		t.Fatalf("consumed tokens = %d, want 625", snapshot.ConsumedTokens)
	}
	if snapshot.UnknownActual != 3 {
		t.Fatalf("unknown usage dispatches = %d, want 3", snapshot.UnknownActual)
	}
}

func TestRequestLLMBudget_TokenReservationReleasesToActual(t *testing.T) {
	t.Parallel()

	b := newRequestLLMBudget(0, 10)
	first, err := b.reserve(requestBudgetMain, 900_000)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := b.reserve(requestBudgetMain, 100_001); !errors.Is(err, ErrRequestBudgetExhausted) {
		t.Fatalf("in-flight token overage error = %v, want budget exhausted", err)
	}
	first.reconcile(&client.CompletionResponse{Usage: client.Usage{InputTokens: 100, OutputTokens: 20}}, nil)
	if _, err := b.reserve(requestBudgetMain, 999_880); err != nil {
		t.Fatalf("actual usage did not release unused reservation: %v", err)
	}
}

func TestRequestLLMBudget_ConcurrentReserveIsAtomic(t *testing.T) {
	t.Parallel()

	b := newRequestLLMBudget(0, 10)
	var successes atomic.Int32
	var wg sync.WaitGroup
	for i := 0; i < 100; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := b.reserve(requestBudgetMain, 0); err == nil {
				successes.Add(1)
			} else if !errors.Is(err, ErrRequestBudgetExhausted) {
				t.Errorf("reserve error = %v", err)
			}
		}()
	}
	wg.Wait()
	if got := successes.Load(); got != requestBudgetMinimumNormalDispatchLimit {
		t.Fatalf("successful reserves = %d, want %d", got, requestBudgetMinimumNormalDispatchLimit)
	}
}

type requestBudgetFakeLLM struct {
	completeCalls atomic.Int32
	streamCalls   atomic.Int32
	response      *client.CompletionResponse
	err           error
}

func (f *requestBudgetFakeLLM) Complete(context.Context, client.CompletionRequest) (*client.CompletionResponse, error) {
	f.completeCalls.Add(1)
	return f.response, f.err
}

func (f *requestBudgetFakeLLM) CompleteStream(context.Context, client.CompletionRequest, func(client.StreamDelta)) (*client.CompletionResponse, error) {
	f.streamCalls.Add(1)
	return f.response, f.err
}

func TestBudgetedLLMClient_ReservesEveryProviderVariant(t *testing.T) {
	t.Parallel()

	delegate := &requestBudgetFakeLLM{response: &client.CompletionResponse{
		Usage: client.Usage{InputTokens: 10, OutputTokens: 2},
	}}
	b := newRequestLLMBudget(0, 10)
	llm := newBudgetedLLMClient(delegate, b, requestBudgetMain, nil)
	req := client.CompletionRequest{MaxTokens: 20}
	if _, err := llm.Complete(context.Background(), req); err != nil {
		t.Fatal(err)
	}
	if _, err := llm.CompleteStream(context.Background(), req, nil); err != nil {
		t.Fatal(err)
	}
	if delegate.completeCalls.Load() != 1 || delegate.streamCalls.Load() != 1 {
		t.Fatalf("delegate calls = complete:%d stream:%d", delegate.completeCalls.Load(), delegate.streamCalls.Load())
	}
	snapshot := b.snapshot()
	if snapshot.NormalDispatches != 2 || snapshot.ConsumedTokens != 24 {
		t.Fatalf("budget snapshot = %+v", snapshot)
	}
}

func TestEstimateCompletionTokenExposure_UsesLargerOfCalibrationAndSchema(t *testing.T) {
	t.Parallel()

	base := client.CompletionRequest{
		Messages:  []client.Message{{Role: "user", Content: client.NewTextContent("hello")}},
		MaxTokens: 100,
	}
	withTool := base
	withTool.Tools = []client.Tool{{
		Type: "function",
		Function: client.FunctionDef{
			Name: "lookup",
			Parameters: map[string]any{
				"type":       "object",
				"properties": map[string]any{"query": map[string]any{"type": "string"}},
			},
		},
	}}

	baseEstimate := estimateCompletionTokenExposure(base, 0)
	schemaEstimate := estimateCompletionTokenExposure(withTool, 0) - baseEstimate
	if schemaEstimate <= 0 {
		t.Fatalf("schema estimate = %d, want > 0", schemaEstimate)
	}
	calibrated := int(schemaEstimate) + 50
	if got, want := estimateCompletionTokenExposure(withTool, calibrated), baseEstimate+int64(calibrated); got != want {
		t.Fatalf("calibrated estimate = %d, want %d (schema must not be added twice)", got, want)
	}
}

func TestAgentLoop_RequestBudgetExhaustionUsesOneTerminalAndContentFreeTrace(t *testing.T) {
	main := nativeResponseWithID("", "tool_use", toolCallWithID("step", `{}`, "call-step"), 1_000_001, 5)
	terminal := nativeResponse("budget summary", "end_turn", nil, 10, 5)
	llm := &budgetCaptureLLMClient{responses: []*client.CompletionResponse{&main, &terminal}}
	registry := NewToolRegistry()
	registry.Register(&mockSimpleTool{name: "step", result: ToolResult{Content: "private tool result"}})
	handler := &runTraceRecorder{}
	loop := NewAgentLoop(llm, registry, "medium", "", 10, 2_000, 200, nil, nil, nil)
	loop.SetHandler(handler)

	result, _, err := loop.Run(context.Background(), "private user request", nil, nil)
	if !errors.Is(err, ErrRequestBudgetExhausted) {
		t.Fatalf("Run error = %v, want request budget exhausted", err)
	}
	if errors.Is(err, ErrMaxIterReached) {
		t.Fatalf("budget exhaustion was mislabeled as max iterations: %v", err)
	}
	if result != "budget summary" {
		t.Fatalf("result = %q, want terminal synthesis", result)
	}
	if len(llm.requests) != 2 {
		t.Fatalf("provider dispatches = %d, want one main plus one terminal", len(llm.requests))
	}
	status := loop.LastRunStatus()
	if !status.Partial || status.FailureCode != "budget_exhausted" {
		t.Fatalf("LastRunStatus = %+v", status)
	}

	var terminalEvents []RunTraceEvent
	for _, event := range handler.events {
		if event.Type == RunTraceEventTerminal {
			terminalEvents = append(terminalEvents, event)
		}
	}
	if len(terminalEvents) != 1 || terminalEvents[0].Terminal == nil {
		t.Fatalf("terminal events = %+v", terminalEvents)
	}
	trace := terminalEvents[0].Terminal
	if !trace.Partial || trace.FailureCode != "budget_exhausted" || trace.ProviderDispatchesAtTerminal != 2 ||
		trace.ProviderDispatchLimit != requestBudgetMinimumNormalDispatchLimit ||
		trace.HelperDispatchesAtTerminal != 0 || trace.UnknownUsageDispatchesAtTerminal != 0 ||
		trace.TokenExposureAtTerminal != 1_000_006 || trace.TokenLimit != 1_000_000 || trace.TerminalTokenExposure != 15 {
		t.Fatalf("terminal trace = %+v", trace)
	}
	wire, marshalErr := json.Marshal(trace)
	if marshalErr != nil {
		t.Fatal(marshalErr)
	}
	for _, secret := range []string{"private user request", "private tool result", "budget summary", "call-step"} {
		if strings.Contains(string(wire), secret) {
			t.Fatalf("terminal budget trace leaked %q: %s", secret, wire)
		}
	}
}

func TestAgentLoop_RequestBudgetDispatchBoundaryTracksIterationFuse(t *testing.T) {
	const maxIterations = 65
	responses := make([]*client.CompletionResponse, 0, maxIterations+1)
	for step := 1; step <= maxIterations; step++ {
		resp := nativeResponse(
			"", "tool_use",
			toolCall("boundary_progress", fmt.Sprintf(`{"step":%d,"token":"token-%02d"}`, step, step-1)),
			1, 1,
		)
		responses = append(responses, &resp)
	}
	terminal := nativeResponse("bounded partial summary", "end_turn", nil, 1, 1)
	responses = append(responses, &terminal)
	llm := &budgetCaptureLLMClient{responses: responses}
	tool := &progressingBoundaryTool{}
	registry := NewToolRegistry()
	registry.Register(tool)
	loop := NewAgentLoop(llm, registry, "medium", "", maxIterations, 2_000, 200, nil, nil, nil)

	result, _, err := loop.Run(context.Background(), "advance until the provider budget stops", nil, nil)
	if !errors.Is(err, ErrMaxIterReached) || errors.Is(err, ErrRequestBudgetExhausted) {
		t.Fatalf("Run error = %v, want iteration fuse before provider budget", err)
	}
	if result != "bounded partial summary" {
		t.Fatalf("result = %q", result)
	}
	if tool.runs != maxIterations {
		t.Fatalf("tool runs = %d, want iteration fuse %d", tool.runs, maxIterations)
	}
	if len(llm.requests) != maxIterations+1 {
		t.Fatalf("provider calls = %d, want %d iterations plus one terminal", len(llm.requests), maxIterations+1)
	}
	if got := len(llm.requests[len(llm.requests)-1].Tools); got != 0 {
		t.Fatalf("terminal request exposed %d tools", got)
	}
	status := loop.LastRunStatus()
	if !status.Partial || status.FailureCode != runstatus.CodeIterationLimit {
		t.Fatalf("LastRunStatus = %+v", status)
	}
}

func TestAgentLoop_LastSentRequestWithClientKeepsForkOnOriginatingBudget(t *testing.T) {
	response := nativeResponse("done", "end_turn", nil, 1_000_001, 1)
	delegate := &budgetCaptureLLMClient{responses: []*client.CompletionResponse{&response}}
	loop := NewAgentLoop(delegate, NewToolRegistry(), "medium", "", 4, 2_000, 200, nil, nil, nil)
	if result, _, err := loop.Run(context.Background(), "finish once", nil, nil); err != nil || result != "done" {
		t.Fatalf("Run result=%q err=%v", result, err)
	}

	req, forkLLM, ok := loop.LastSentRequestWithClient()
	if !ok || forkLLM == nil {
		t.Fatal("missing budget-bound fork snapshot")
	}
	if _, err := forkLLM.Complete(context.Background(), req); !errors.Is(err, ErrRequestBudgetExhausted) {
		t.Fatalf("fork error = %v, want originating budget exhausted", err)
	}
	if len(delegate.requests) != 1 {
		t.Fatalf("budget-rejected fork reached provider: dispatches=%d", len(delegate.requests))
	}
}
