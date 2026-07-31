package daemon

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Kocoro-lab/ShanClaw/internal/agent"
	"github.com/Kocoro-lab/ShanClaw/internal/client"
	"github.com/Kocoro-lab/ShanClaw/internal/executionprofile"
	"github.com/Kocoro-lab/ShanClaw/internal/session"
)

const (
	koeResumeWriteToolName = "resume_write_probe"
	koeResumeOriginalCall  = "call-write-original"
	koeResumeReplayCall    = "call-write-replayed"
	koeResumeCrashDelta    = "interrupt-after-checkpoint"
)

type koeResumeExactlyOnceWriteTool struct {
	path string
}

func (t *koeResumeExactlyOnceWriteTool) Info() agent.ToolInfo {
	return agent.ToolInfo{
		Name:        koeResumeWriteToolName,
		Description: "append one durable test record",
		Parameters: map[string]any{
			"type":       "object",
			"properties": map[string]any{"path": map[string]any{"type": "string"}, "value": map[string]any{"type": "string"}},
			"required":   []string{"path", "value"},
		},
		Required: []string{"path", "value"},
	}
}

func (t *koeResumeExactlyOnceWriteTool) Run(_ context.Context, args string) (agent.ToolResult, error) {
	var input struct {
		Path  string `json:"path"`
		Value string `json:"value"`
	}
	if err := json.Unmarshal([]byte(args), &input); err != nil {
		return agent.ToolResult{}, err
	}
	if input.Path != t.path || input.Value != "once" {
		return agent.ToolResult{}, fmt.Errorf("unexpected write input: %+v", input)
	}
	f, err := os.OpenFile(t.path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err == nil {
		_, err = f.WriteString(input.Value + "\n")
		if closeErr := f.Close(); err == nil {
			err = closeErr
		}
	}
	if err != nil {
		return agent.ToolResult{}, err
	}
	return agent.ToolResult{Content: "durable write completed"}, nil
}

func (*koeResumeExactlyOnceWriteTool) RequiresApproval() bool { return false }

var koeResumeCrashToken = &struct{}{}

type koeResumeCrashHandler struct{ nullEventHandler }

func (h *koeResumeCrashHandler) OnStreamDelta(delta string) {
	if delta == koeResumeCrashDelta {
		panic(koeResumeCrashToken)
	}
}

type koeResumeExactlyOnceGateway struct {
	mu            sync.Mutex
	originalArgs  json.RawMessage
	replayArgs    json.RawMessage
	resolveCalls  int
	completions   []client.CompletionRequest
	resumeStarted chan struct{}
	releaseResume chan struct{}
}

func (g *koeResumeExactlyOnceGateway) handler(w http.ResponseWriter, r *http.Request) {
	switch r.URL.Path {
	case "/api/v1/channels":
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte("[]"))
	case "/v1/completions/resolve":
		g.mu.Lock()
		g.resolveCalls++
		g.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(fastProfileForDaemonTest())
	case "/v1/completions":
		var req client.CompletionRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		g.mu.Lock()
		g.completions = append(g.completions, req)
		call := len(g.completions)
		g.mu.Unlock()

		switch call {
		case 1:
			writeKoeResumeSSEDone(w, client.CompletionResponse{
				Provider: "openai", Model: "gpt-5.6-luna", FinishReason: "tool_use",
				ToolCalls: []client.FunctionCall{{
					ID: koeResumeOriginalCall, Name: koeResumeWriteToolName, Arguments: g.originalArgs,
				}},
			})
		case 2:
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = fmt.Fprintf(w, "data: {\"type\":\"content_delta\",\"text\":%q}\n\n", koeResumeCrashDelta)
			w.(http.Flusher).Flush()
			<-r.Context().Done()
		case 3:
			close(g.resumeStarted)
			<-g.releaseResume
			writeKoeResumeSSEDone(w, client.CompletionResponse{
				Provider: "openai", Model: "gpt-5.6-luna", FinishReason: "tool_use",
				ToolCalls: []client.FunctionCall{{
					ID: koeResumeReplayCall, Name: koeResumeWriteToolName, Arguments: g.replayArgs,
				}},
			})
		default:
			writeKoeResumeSSEDone(w, client.CompletionResponse{
				Provider: "openai", Model: "gpt-5.6-luna",
				FinishReason: "end_turn", OutputText: "resumed exactly once",
			})
		}
	default:
		http.NotFound(w, r)
	}
}

func writeKoeResumeSSEDone(w http.ResponseWriter, response client.CompletionResponse) {
	payload, _ := json.Marshal(struct {
		Type string `json:"type"`
		client.CompletionResponse
	}{Type: "done", CompletionResponse: response})
	w.Header().Set("Content-Type", "text/event-stream")
	_, _ = fmt.Fprintf(w, "data: %s\n\ndata: [DONE]\n\n", payload)
	w.(http.Flusher).Flush()
}

func (g *koeResumeExactlyOnceGateway) snapshot() (int, []client.CompletionRequest) {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.resolveCalls, append([]client.CompletionRequest(nil), g.completions...)
}

func loadKoeResumeSession(t *testing.T, dir, id string) *session.Session {
	t.Helper()
	mgr := session.NewManager(dir)
	sess, err := mgr.Load(id)
	_ = mgr.Close()
	if err != nil {
		t.Fatal(err)
	}
	return sess
}

func waitKoeResume(t *testing.T, ch <-chan struct{}, timeout time.Duration, failure string) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(timeout):
		t.Fatal(failure)
	}
}

func TestKoeFastResumeExactlyOnceFromDisk(t *testing.T) {
	effectDir := t.TempDir()
	effectPath := filepath.Join(effectDir, "effect.log")
	pathJSON, _ := json.Marshal(effectPath)
	gateway := &koeResumeExactlyOnceGateway{
		originalArgs:  json.RawMessage(fmt.Sprintf(`{"path":%s,"value":"once"}`, pathJSON)),
		replayArgs:    json.RawMessage(fmt.Sprintf("{\n \"value\": \"once\", \"path\": %s\n}", pathJSON)),
		resumeStarted: make(chan struct{}),
		releaseResume: make(chan struct{}),
	}
	released := false
	defer func() {
		if !released {
			close(gateway.releaseResume)
		}
	}()
	server := httptest.NewServer(http.HandlerFunc(gateway.handler))
	defer server.Close()

	deps := runAgentContractTestDeps(t, server.URL)
	shannonDir := deps.ShannonDir
	sessionsDir := filepath.Join(shannonDir, "sessions")
	deps.Config.ModelTier = "large"
	deps.Config.Agent.Model = "claude-sonnet-5"
	deps.Config.Agent.Thinking = true
	deps.Config.Agent.ThinkingMode = "enabled"
	deps.Config.Agent.ThinkingBudget = 4096
	deps.Config.Agent.ReasoningEffort = "high"
	deps.Config.Agent.EffortTier = "xhigh"
	deps.Config.Agent.Temperature = 0.27
	deps.Config.Agent.MaxTokens = 7777
	deps.Config.Agent.ContextWindow = 200_000
	deps.Config.Agent.MaxIterations = 4

	firstTool := &koeResumeExactlyOnceWriteTool{path: effectPath}
	deps.Registry.Register(firstTool)

	const sessionID = "koe-resume-exactly-once-001"
	mgr := session.NewManager(sessionsDir)
	sess := mgr.NewSessionWithID(sessionID)
	sess.CWD = effectDir
	sess.Title = "Existing turn"
	sess.Messages = []client.Message{
		{Role: "user", Content: client.NewTextContent("prior question")},
		{Role: "assistant", Content: client.NewTextContent("prior completed answer")},
	}
	sess.MessageMeta = []session.MessageMeta{{Source: "koe"}, {Source: "koe"}}
	if err := mgr.Save(); err != nil {
		t.Fatal(err)
	}
	if err := mgr.Close(); err != nil {
		t.Fatal(err)
	}

	crashed := false
	func() {
		defer func() {
			recovered := recover()
			if recovered == koeResumeCrashToken {
				crashed = true
				return
			}
			if recovered != nil {
				panic(recovered)
			}
		}()
		_, _ = RunAgent(context.Background(), deps, RunAgentRequest{
			Text: "只把 once 写入文件一次。", SessionID: sessionID, Source: "koe",
			ThreadID: "burst-exactly-once", CWD: effectDir,
			ExecutionMode: executionprofile.ModeFast,
			LogicalTaskID: "burst:exactly-once", ExecutionRunID: "ker1_exactly-once",
		}, &koeResumeCrashHandler{})
	}()
	if !crashed {
		t.Fatal("first runner reached final save instead of the simulated checkpoint crash")
	}
	deps.SessionCache.CloseAll()

	checkpoint := loadKoeResumeSession(t, sessionsDir, sessionID)
	if !checkpoint.InProgress || checkpoint.InterruptedTurn == nil ||
		len(checkpoint.ExecutionRuns) != 1 ||
		len(checkpoint.ExecutionRuns[0].Evidence.ToolOutcomes) != 1 {
		t.Fatalf("durable checkpoint incomplete: %+v", checkpoint.InterruptedTurn)
	}
	checkpointConfig := agent.CloneExecutionConfig(checkpoint.InterruptedTurn.ExecutionConfig)
	checkpointProfile := checkpoint.InterruptedTurn.ExecutionRun.Profile
	originalEvidence := checkpoint.ExecutionRuns[0].Evidence.ToolOutcomes[0]
	if checkpointConfig == nil || checkpointProfile.EffectiveMode != executionprofile.ModeFast ||
		originalEvidence.ToolCallID != koeResumeOriginalCall ||
		!originalEvidence.Validated || originalEvidence.Outcome != "succeeded" ||
		!originalEvidence.SideEffect || originalEvidence.ArgumentsDigest == "" {
		t.Fatalf("invalid original checkpoint config=%+v profile=%+v evidence=%+v",
			checkpointConfig, checkpointProfile, originalEvidence)
	}

	restartConfig := *deps.Config
	restartConfig.ModelTier = "small"
	restartConfig.Agent.Model = "current-config-b"
	restartConfig.Agent.ThinkingMode = "adaptive"
	restartConfig.Agent.ThinkingBudget = 0
	restartConfig.Agent.ReasoningEffort = "low"
	restartConfig.Agent.EffortTier = "low"
	restartConfig.Agent.Language = "English"
	restartConfig.Agent.Temperature = 0.91
	restartConfig.Agent.MaxTokens = 999
	restartConfig.Agent.ContextWindow = 32_000
	restartConfig.Agent.MaxIterations = 1
	restartTool := &koeResumeExactlyOnceWriteTool{path: effectPath}
	restartRegistry := agent.NewToolRegistry()
	restartRegistry.Register(restartTool)
	restartDeps := &ServerDeps{
		Config: &restartConfig, GW: client.NewGatewayClient(server.URL, "test-key"),
		Registry: restartRegistry, BaselineReg: restartRegistry,
		SessionCache: NewSessionCache(shannonDir), ShannonDir: shannonDir,
		AgentsDir: filepath.Join(shannonDir, "agents"),
	}
	defer restartDeps.SessionCache.CloseAll()

	resumeDone := make(chan struct{})
	go func() {
		(&Server{deps: restartDeps}).resumeInterruptedTurns(context.Background())
		close(resumeDone)
	}()
	waitKoeResume(t, gateway.resumeStarted, 5*time.Second, "recreated runner did not reach resumed gateway")

	claimed := loadKoeResumeSession(t, sessionsDir, sessionID)
	if claimed.InterruptedTurn == nil || claimed.InterruptedTurn.ResumeAttempts != 1 ||
		!reflect.DeepEqual(claimed.InterruptedTurn.ExecutionConfig, checkpointConfig) ||
		claimed.InterruptedTurn.ExecutionRun.Profile != checkpointProfile {
		t.Fatalf("recovery claim drifted: %+v", claimed.InterruptedTurn)
	}
	close(gateway.releaseResume)
	released = true
	waitKoeResume(t, resumeDone, 10*time.Second, "recreated runner did not finish recovery")

	final := loadKoeResumeSession(t, sessionsDir, sessionID)
	if final.InProgress || final.InterruptedTurn != nil || len(final.ExecutionRuns) != 1 {
		t.Fatalf("successful recovery left interrupted state: %+v", final.InterruptedTurn)
	}
	run := final.ExecutionRuns[0]
	if run.Profile != checkpointProfile || len(run.Evidence.ToolOutcomes) != 2 {
		t.Fatalf("final execution run drifted: %+v", run)
	}
	replayed := run.Evidence.ToolOutcomes[1]
	if replayed.ToolCallID != koeResumeReplayCall || replayed.ToolName != koeResumeWriteToolName ||
		!replayed.Validated || replayed.Outcome != "failed" || replayed.SideEffect ||
		replayed.PermissionDecision != "replay_blocked" ||
		replayed.ArgumentsDigest != originalEvidence.ArgumentsDigest {
		t.Fatalf("replay was not blocked exactly once: evidence=%+v", replayed)
	}
	if physical, err := os.ReadFile(effectPath); err != nil || string(physical) != "once\n" {
		t.Fatalf("physical side effect = %q, err=%v; want one record", physical, err)
	}

	resolveCalls, requests := gateway.snapshot()
	if resolveCalls != 1 || len(requests) != 4 {
		t.Fatalf("gateway calls: resolve=%d completions=%d", resolveCalls, len(requests))
	}
	for i, req := range requests {
		if req.ExecutionProfileID != checkpointProfile.ProfileID ||
			!req.ParallelToolCalls ||
			req.ResponseCachePolicy != executionprofile.ResponseCacheOff ||
			req.ModelTier != "" || req.SpecificModel != "" ||
			req.Thinking != nil || req.ReasoningEffort != "" || req.EffortTier != "" ||
			req.Temperature != checkpointConfig.Temperature ||
			req.MaxTokens != checkpointConfig.MaxTokens {
			t.Fatalf("completion %d drifted from Fast checkpoint: %+v", i+1, req)
		}
		messages, _ := json.Marshal(req.Messages)
		if !strings.Contains(string(messages), "Always respond in 中文") {
			t.Fatalf("completion %d lost checkpoint response language", i+1)
		}
	}
}
