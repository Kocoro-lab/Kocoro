package cloudflow

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Kocoro-lab/ShanClaw/internal/agent"
	"github.com/Kocoro-lab/ShanClaw/internal/client"
)

// nilGateway exercises the early-return path when no gateway is configured.
func TestRun_NoGateway_ReturnsError(t *testing.T) {
	_, err := Run(context.Background(), Request{
		Gateway: nil,
		APIKey:  "",
		Query:   "anything",
	}, nil)
	if err == nil {
		t.Fatalf("expected error when Gateway is nil, got nil")
	}
	if !errors.Is(err, ErrGatewayNotConfigured) {
		t.Fatalf("expected ErrGatewayNotConfigured, got: %v", err)
	}
}

// captureHandler records every callback so tests can assert what cloudflow
// surfaced from a fake Gateway stream. Method set must match agent.EventHandler
// (internal/agent/loop.go:368-378) exactly.
type captureHandler struct {
	cloudAgents   []string
	streamDeltas  []string
	finalUsage    agent.TurnUsage
	progressCalls int32
}

func (c *captureHandler) OnToolCall(name, args string)                                                   {}
func (c *captureHandler) OnToolResult(name, args string, result agent.ToolResult, elapsed time.Duration) {}
func (c *captureHandler) OnText(text string)                                                             {}
func (c *captureHandler) OnPreamble(text string)                                                         {}
func (c *captureHandler) OnStreamDelta(d string)                                                         { c.streamDeltas = append(c.streamDeltas, d) }
func (c *captureHandler) OnApprovalNeeded(tool, args string) bool                                        { return true }
func (c *captureHandler) OnUsage(u agent.TurnUsage)                                                      { c.finalUsage = u }
func (c *captureHandler) OnCloudAgent(_, status, msg string)                                             { c.cloudAgents = append(c.cloudAgents, status+":"+msg) }
func (c *captureHandler) OnCloudProgress(completed, total int)                                           { atomic.AddInt32(&c.progressCalls, 1) }
func (c *captureHandler) OnCloudPlan(planType, content string, needsReview bool)                         {}

// Compile-time assertion that captureHandler implements agent.EventHandler.
var _ agent.EventHandler = (*captureHandler)(nil)

func TestAccumulateUsage_ParsesSplitCacheCreation(t *testing.T) {
	var usage agent.TurnUsage

	accumulateUsage(`{
		"metadata": {
			"input_tokens": 120,
			"output_tokens": 30,
			"tokens_used": 180,
			"cost_usd": 0.42,
			"cache_read_tokens": 50,
			"cache_creation_5m_tokens": 100,
			"cache_creation_1h_tokens": 200,
			"model_used": "claude-cloud"
		}
	}`, &usage)

	if usage.InputTokens != 120 || usage.OutputTokens != 30 {
		t.Fatalf("expected input/output 120/30, got %d/%d", usage.InputTokens, usage.OutputTokens)
	}
	if usage.TotalTokens != 180 {
		t.Fatalf("expected total tokens 180, got %d", usage.TotalTokens)
	}
	if usage.CacheCreationTokens != 300 {
		t.Fatalf("expected legacy cache creation total 300, got %d", usage.CacheCreationTokens)
	}
	if usage.CacheCreation5mTokens != 100 || usage.CacheCreation1hTokens != 200 {
		t.Fatalf("expected split cache creation 100/200, got %d/%d", usage.CacheCreation5mTokens, usage.CacheCreation1hTokens)
	}
	if usage.Model != "claude-cloud" {
		t.Fatalf("expected model claude-cloud, got %q", usage.Model)
	}
	if usage.LLMCalls != 1 {
		t.Fatalf("expected 1 LLM call, got %d", usage.LLMCalls)
	}
}

// newFakeGateway returns an httptest.Server stubbing the three Gateway
// endpoints used by Run: POST /api/v1/tasks/stream (returns 201 with a
// workflow_id), GET /api/v1/stream/sse (emits a minimal AGENT_STARTED →
// thread.message.completed → WORKFLOW_COMPLETED sequence), and GET
// /api/v1/tasks/{id} (returns the canonical full result for API fallback).
func newFakeGateway(t *testing.T) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/api/v1/tasks/stream"):
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusCreated) // GatewayClient rejects anything else
			json.NewEncoder(w).Encode(map[string]any{"workflow_id": "wf-123", "task_id": "t-1"})
		case strings.HasPrefix(r.URL.Path, "/api/v1/stream/sse"):
			w.Header().Set("Content-Type", "text/event-stream")
			fmt.Fprintf(w, "event: AGENT_STARTED\ndata: %s\n\n", `{"agent_id":"researcher","message":"Starting"}`)
			fmt.Fprintf(w, "event: thread.message.completed\ndata: %s\n\n", `{"response":"Final answer."}`)
			fmt.Fprintf(w, "event: WORKFLOW_COMPLETED\ndata: %s\n\n", `{"message":"done"}`)
		case r.Method == http.MethodGet && strings.Contains(r.URL.Path, "/api/v1/tasks/"):
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(map[string]any{"task_id": "t-1", "result": "Final answer."})
		default:
			http.NotFound(w, r)
		}
	}))
}

func TestRun_FakeGateway_StreamsToHandler(t *testing.T) {
	srv := newFakeGateway(t)
	defer srv.Close()

	gw := client.NewGatewayClient(srv.URL, "test-key")
	h := &captureHandler{}
	res, err := Run(context.Background(), Request{
		Gateway:      gw,
		APIKey:       "test-key",
		Query:        "test query",
		WorkflowType: "research",
	}, h)
	if err != nil {
		t.Fatalf("Run returned err: %v", err)
	}
	if res.FinalText != "Final answer." {
		t.Fatalf("expected FinalText=%q, got %q", "Final answer.", res.FinalText)
	}
	if len(h.cloudAgents) == 0 {
		t.Fatalf("expected at least one OnCloudAgent call, got 0")
	}
	if !res.FullResultConfirmed {
		t.Fatalf("expected FullResultConfirmed=true after successful API fallback, got false")
	}
}

func TestRun_FakeGateway_InvokesWorkflowStartedCallback(t *testing.T) {
	srv := newFakeGateway(t)
	defer srv.Close()

	var seen atomic.Pointer[string]
	ctx := WithOnWorkflowStarted(context.Background(), func(wfID string) {
		s := wfID
		seen.Store(&s)
	})

	gw := client.NewGatewayClient(srv.URL, "test-key")
	_, err := Run(ctx, Request{
		Gateway: gw,
		APIKey:  "test-key",
		Query:   "q",
	}, &captureHandler{})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	got := seen.Load()
	if got == nil {
		t.Fatalf("OnWorkflowStarted callback was never invoked")
	}
	if *got != "wf-123" {
		t.Fatalf("callback got workflow_id=%q, want %q", *got, "wf-123")
	}
}
