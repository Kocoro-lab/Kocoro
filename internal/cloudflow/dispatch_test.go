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

// cloudAgentEvent is the full (agentID, status, message) triple captured from
// OnCloudAgent so tests can assert agent_id passthrough (Layer 2), not just the
// status:message pair the older assertions relied on.
type cloudAgentEvent struct {
	agentID string
	status  string
	msg     string
}

// captureHandler records every callback so tests can assert what cloudflow
// surfaced from a fake Gateway stream. Method set must match agent.EventHandler
// (internal/agent/loop.go:368-378) exactly.
type captureHandler struct {
	cloudAgents   []string
	cloudEvents   []cloudAgentEvent
	streamDeltas  []string
	finalUsage    agent.TurnUsage
	progressCalls int32
}

func (c *captureHandler) OnToolCall(name, args, toolUseID string)                                                   {}
func (c *captureHandler) OnToolResult(name, args, toolUseID string, result agent.ToolResult, elapsed time.Duration) {}
func (c *captureHandler) OnText(text string)                                                             {}
func (c *captureHandler) OnPreamble(text string)                                                         {}
func (c *captureHandler) OnStreamDelta(d string)                                                         { c.streamDeltas = append(c.streamDeltas, d) }
func (c *captureHandler) OnApprovalNeeded(tool, args string) bool                                        { return true }
func (c *captureHandler) OnUsage(u agent.TurnUsage)                                                      { c.finalUsage = u }
func (c *captureHandler) OnCloudAgent(agentID, status, msg string) {
	c.cloudAgents = append(c.cloudAgents, status+":"+msg)
	c.cloudEvents = append(c.cloudEvents, cloudAgentEvent{agentID: agentID, status: status, msg: msg})
}
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
			fmt.Fprintf(w, "event: done\ndata: [DONE]\n\n")
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

// TestRun_StreamDropsBeforeFinal_RecoversViaGetTask exercises the recovery
// path: the SSE stream emits a status event then drops with NO final result
// and NO `done`, but the cloud task actually completed — GetTask returns the
// full result. Run must recover it instead of returning "no response".
func TestRun_StreamDropsBeforeFinal_RecoversViaGetTask(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/api/v1/tasks/stream"):
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusCreated)
			json.NewEncoder(w).Encode(map[string]any{"workflow_id": "wf-9", "task_id": "t-9"})
		case strings.HasPrefix(r.URL.Path, "/api/v1/stream/sse"):
			// Stream ends cleanly (`done`) after a status event but with NO
			// final result — exercises the recoverViaREST path that used to
			// sit behind the finalResult != "" gate.
			w.Header().Set("Content-Type", "text/event-stream")
			fmt.Fprintf(w, "event: AGENT_STARTED\ndata: {\"agent_id\":\"a\"}\n\n")
			fmt.Fprintf(w, "event: done\ndata: [DONE]\n\n")
		case r.Method == http.MethodGet && strings.Contains(r.URL.Path, "/api/v1/tasks/"):
			// The cloud task actually completed — REST has the full result.
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(map[string]any{"task_id": "t-9", "status": "TASK_STATUS_COMPLETED", "result": "Recovered answer."})
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	gw := client.NewGatewayClient(srv.URL, "k")
	res, err := Run(context.Background(), Request{Gateway: gw, APIKey: "k", Query: "q"}, &captureHandler{})
	if err != nil {
		t.Fatalf("expected GetTask recovery, got err: %v", err)
	}
	if res.FinalText != "Recovered answer." {
		t.Fatalf("FinalText = %q, want \"Recovered answer.\"", res.FinalText)
	}
	if !res.FullResultConfirmed {
		t.Fatalf("expected FullResultConfirmed=true after REST recovery")
	}
}

// TestRun_ForwardsAgentIDForActivityEvents pins the Layer-2 contract: a cloud
// worker's mid-run activity events (AGENT_THINKING, TOOL_INVOKED,
// TOOL_OBSERVATION) must reach the handler tagged with the originating
// agent_id (the station nickname) so the desktop can route each event to the
// right sub-agent row instead of leaving it stuck on "Working…". Before Layer
// 2 the dispatcher blanked agent_id on thinking/tool events and dropped
// TOOL_OBSERVATION entirely.
func TestRun_ForwardsAgentIDForActivityEvents(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/api/v1/tasks/stream"):
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusCreated)
			json.NewEncoder(w).Encode(map[string]any{"workflow_id": "wf-2", "task_id": "t-2"})
		case strings.HasPrefix(r.URL.Path, "/api/v1/stream/sse"):
			// One worker ("Osaki") runs a full reason→tool→observe→done cycle.
			// AGENT_STARTED / AGENT_COMPLETED carry empty messages (the DAG
			// path's actual behavior); the activity lives in the middle three.
			w.Header().Set("Content-Type", "text/event-stream")
			fmt.Fprintf(w, "event: AGENT_STARTED\ndata: %s\n\n", `{"agent_id":"Osaki","message":""}`)
			fmt.Fprintf(w, "event: AGENT_THINKING\ndata: %s\n\n", `{"agent_id":"Osaki","message":"Thinking: where to look"}`)
			fmt.Fprintf(w, "event: TOOL_INVOKED\ndata: %s\n\n", `{"agent_id":"Osaki","message":"Looking this up: 'Google acquisitions'"}`)
			fmt.Fprintf(w, "event: TOOL_OBSERVATION\ndata: %s\n\n", `{"agent_id":"Osaki","message":"Search: Google has acquired 250+ companies"}`)
			fmt.Fprintf(w, "event: AGENT_COMPLETED\ndata: %s\n\n", `{"agent_id":"Osaki","message":""}`)
			fmt.Fprintf(w, "event: WORKFLOW_COMPLETED\ndata: %s\n\n", `{"message":"done"}`)
			fmt.Fprintf(w, "event: done\ndata: [DONE]\n\n")
		case r.Method == http.MethodGet && strings.Contains(r.URL.Path, "/api/v1/tasks/"):
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(map[string]any{"task_id": "t-2", "status": "TASK_STATUS_COMPLETED", "result": "Final."})
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	gw := client.NewGatewayClient(srv.URL, "k")
	h := &captureHandler{}
	if _, err := Run(context.Background(), Request{Gateway: gw, APIKey: "k", Query: "q", WorkflowType: "research"}, h); err != nil {
		t.Fatalf("Run: %v", err)
	}

	// Every mid-run activity event must carry "Osaki" — not the blank agent_id
	// the pre-Layer-2 dispatcher emitted for thinking/tool events.
	var sawThinking, sawInvoked, sawObservation bool
	for _, ev := range h.cloudEvents {
		switch {
		case ev.status == "thinking":
			sawThinking = true
			if ev.agentID != "Osaki" {
				t.Errorf("AGENT_THINKING: agentID = %q, want \"Osaki\"", ev.agentID)
			}
		case ev.status == "tool" && strings.HasPrefix(ev.msg, "Looking this up"):
			sawInvoked = true
			if ev.agentID != "Osaki" {
				t.Errorf("TOOL_INVOKED: agentID = %q, want \"Osaki\"", ev.agentID)
			}
		case ev.status == "tool" && strings.HasPrefix(ev.msg, "Search:"):
			sawObservation = true
			if ev.agentID != "Osaki" {
				t.Errorf("TOOL_OBSERVATION: agentID = %q, want \"Osaki\"", ev.agentID)
			}
		}
	}
	if !sawThinking {
		t.Errorf("AGENT_THINKING was not forwarded: %+v", h.cloudEvents)
	}
	if !sawInvoked {
		t.Errorf("TOOL_INVOKED was not forwarded: %+v", h.cloudEvents)
	}
	if !sawObservation {
		t.Errorf("TOOL_OBSERVATION was not forwarded (still dropped?): %+v", h.cloudEvents)
	}
}
