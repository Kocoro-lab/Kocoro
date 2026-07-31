package daemon

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/Kocoro-lab/ShanClaw/internal/agent"
	"github.com/Kocoro-lab/ShanClaw/internal/client"
	"github.com/Kocoro-lab/ShanClaw/internal/session"
)

func writeInterruptedSession(t *testing.T, dir, id string, state *session.InterruptedTurn) {
	t.Helper()
	mgr := session.NewManager(dir)
	defer mgr.Close()
	sess := mgr.NewSessionWithID(id)
	now := time.Now().Add(-time.Minute)
	sess.Messages = []client.Message{
		{Role: "user", Content: client.NewTextContent("inspect the project")},
		{Role: "assistant", Content: client.NewBlockContent([]client.ContentBlock{
			client.NewToolUseBlock("tool-1", "file_read", []byte(`{"path":"README.md"}`)),
		})},
		{Role: "user", Content: client.NewBlockContent([]client.ContentBlock{
			client.NewToolResultBlock("tool-1", "saved file contents", false),
		})},
	}
	sess.MessageMeta = []session.MessageMeta{
		{Source: "desktop", Timestamp: &now},
		{Source: "desktop", Timestamp: &now},
		{Source: "desktop", Timestamp: &now},
	}
	sess.Source = "desktop"
	sess.InProgress = true
	if state != nil {
		stateCopy := cloneInterruptedTurn(*state)
		if stateCopy.UpdatedAt.IsZero() {
			stateCopy.UpdatedAt = now
		}
		sess.InterruptedTurn = &stateCopy
	}
	if err := mgr.Save(); err != nil {
		t.Fatalf("save interrupted session: %v", err)
	}
}

func TestDiscoverInterruptedTurns_DefaultAndNamedAgent(t *testing.T) {
	shannonDir := t.TempDir()
	writeInterruptedSession(t, filepath.Join(shannonDir, "sessions"), "recovery-default-001", nil)
	writeInterruptedSession(t, filepath.Join(shannonDir, "agents", "reviewer", "sessions"), "recovery-agent-001", &session.InterruptedTurn{
		Agent:  "wrong-persisted-agent",
		Source: "web",
	})

	mgr := session.NewManager(filepath.Join(shannonDir, "sessions"))
	complete := mgr.NewSessionWithID("complete-session-001")
	complete.Messages = []client.Message{{Role: "user", Content: client.NewTextContent("done")}}
	if err := mgr.Save(); err != nil {
		t.Fatal(err)
	}
	_ = mgr.Close()

	got, err := discoverInterruptedTurns(shannonDir)
	if err != nil {
		t.Fatalf("discoverInterruptedTurns: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("candidates = %d, want 2: %#v", len(got), got)
	}
	byID := map[string]interruptedTurnCandidate{}
	for _, candidate := range got {
		byID[candidate.SessionID] = candidate
	}
	if byID["recovery-default-001"].Agent != "" {
		t.Fatalf("default candidate agent = %q", byID["recovery-default-001"].Agent)
	}
	if byID["recovery-agent-001"].Agent != "reviewer" ||
		byID["recovery-agent-001"].State.Agent != "reviewer" {
		t.Fatalf("named candidate must use directory authority: %#v", byID["recovery-agent-001"])
	}
	if _, exists := byID["complete-session-001"]; exists {
		t.Fatal("completed session was incorrectly scheduled for recovery")
	}
}

func interruptedExecutionConfigA() *agent.ExecutionConfig {
	return &agent.ExecutionConfig{
		SpecificModel:         "claude-sonnet-5",
		ModelTier:             "large",
		Thinking:              &client.ThinkingConfig{Type: "enabled", BudgetTokens: 4096},
		ReasoningEffort:       "high",
		EffortTier:            "xhigh",
		ResponseLanguage:      "中文",
		Temperature:           0.27,
		MaxTokens:             7777,
		ContextWindow:         200_000,
		ContextWindowExplicit: true,
		MaxIterations:         13,
	}
}

func applyInterruptedExecutionConfigB(loop *agent.AgentLoop) {
	loop.SetSpecificModel("current-config-b")
	loop.SetModelTier("small")
	loop.SetThinking(&client.ThinkingConfig{Type: "adaptive"})
	loop.SetReasoningEffort("low")
	loop.SetEffortTier("low")
	loop.SetResponseLanguage("English")
	loop.SetTemperature(0.9)
	loop.SetMaxTokens(999)
	loop.SetContextWindow(32_000)
	loop.SetMaxIterations(2)
}

func TestInterruptedExecutionConfigCloneIsDeep(t *testing.T) {
	original := session.InterruptedTurn{ExecutionConfig: interruptedExecutionConfigA()}
	cloned := cloneInterruptedTurn(original)
	cloned.ExecutionConfig.Thinking.BudgetTokens = 1
	cloned.ExecutionConfig.SpecificModel = "mutated"
	if original.ExecutionConfig.Thinking.BudgetTokens != 4096 ||
		original.ExecutionConfig.SpecificModel != "claude-sonnet-5" {
		t.Fatalf("clone retained checkpoint config ownership: original=%+v clone=%+v", original, cloned)
	}

	req := buildInterruptedResumeRequest(interruptedTurnCandidate{
		SessionID: "execution-config-clone-001",
		State:     original,
		UpdatedAt: time.Now(),
	}, 3, 4*time.Hour)
	original.ExecutionConfig.Thinking.BudgetTokens = 2
	if req.ExecutionConfig == nil || req.ExecutionConfig.Thinking.BudgetTokens != 4096 {
		t.Fatalf("resume request retained candidate config ownership: %+v", req.ExecutionConfig)
	}
}

func TestInterruptedResumeUsesAuthoritativeCheckpointConfigAOverCurrentB(t *testing.T) {
	shannonDir := t.TempDir()
	dir := filepath.Join(shannonDir, "sessions")
	const id = "recovery-config-authority-001"
	configA := interruptedExecutionConfigA()
	writeInterruptedSession(t, dir, id, &session.InterruptedTurn{
		Source:          "desktop",
		ExecutionConfig: configA,
	})

	candidates, err := discoverInterruptedTurns(shannonDir)
	if err != nil {
		t.Fatal(err)
	}
	if len(candidates) != 1 {
		t.Fatalf("candidates = %d, want 1", len(candidates))
	}
	// Simulate a stale discovery copy. The session loaded under the route lock
	// still contains A and must replace this candidate's B-like mutation.
	candidates[0].State.ExecutionConfig.SpecificModel = "stale-candidate-b"
	candidates[0].State.ExecutionConfig.Thinking.BudgetTokens = 1
	req := buildInterruptedResumeRequest(candidates[0], 3, 4*time.Hour)

	mgr := session.NewManager(dir)
	defer mgr.Close()
	sess, err := mgr.Resume(id)
	if err != nil {
		t.Fatal(err)
	}
	if err := claimInterruptedResume(mgr, sess, &req, ""); err != nil {
		t.Fatalf("claimInterruptedResume: %v", err)
	}
	if !reflect.DeepEqual(req.ExecutionConfig, configA) {
		t.Fatalf("claim kept discovery config instead of authoritative checkpoint: got=%+v want=%+v",
			req.ExecutionConfig, configA)
	}

	// Current global/agent/request discovery has changed to B. The locked A
	// snapshot still wins before loop construction proceeds to a model call.
	loop := agent.NewAgentLoop(nil, agent.NewToolRegistry(), "medium", "", 4, 1000, 100, nil, nil, nil)
	applyInterruptedExecutionConfigB(loop)
	req.ModelOverride = "request-overlay-b"
	lockAgentExecutionConfig(loop, &req)
	if got := loop.ExecutionConfig(); !reflect.DeepEqual(got, *configA) {
		t.Fatalf("resume config = %+v, want checkpoint A %+v", got, *configA)
	}
	if !reflect.DeepEqual(req.ExecutionConfig, configA) {
		t.Fatalf("re-locked request config = %+v, want checkpoint A %+v", req.ExecutionConfig, configA)
	}
}

func TestLockAgentExecutionConfigIncludesRequestOverlayAndLegacyNil(t *testing.T) {
	loop := agent.NewAgentLoop(nil, agent.NewToolRegistry(), "medium", "", 4, 1000, 100, nil, nil, nil)
	req := RunAgentRequest{ModelOverride: "small"}
	lockAgentExecutionConfig(loop, &req)
	if req.ExecutionConfig == nil || req.ExecutionConfig.ModelTier != "small" {
		t.Fatalf("request overlay missing from snapshot: %+v", req.ExecutionConfig)
	}

	legacyLoop := agent.NewAgentLoop(nil, agent.NewToolRegistry(), "large", "", 7, 1000, 100, nil, nil, nil)
	legacyReq := RunAgentRequest{ResumeInterrupted: true}
	lockAgentExecutionConfig(legacyLoop, &legacyReq)
	if legacyReq.ExecutionConfig == nil ||
		legacyReq.ExecutionConfig.ModelTier != "large" ||
		legacyReq.ExecutionConfig.MaxIterations != 7 {
		t.Fatalf("legacy nil snapshot was not upgraded from current config: %+v", legacyReq.ExecutionConfig)
	}
}

func TestRefreshExecutionConfigRuntimeStateUpdatesOnlyContextWindow(t *testing.T) {
	req := RunAgentRequest{ExecutionConfig: interruptedExecutionConfigA()}
	loop := agent.NewAgentLoop(nil, agent.NewToolRegistry(), "small", "", 2, 1000, 100, nil, nil, nil)
	loop.SetSpecificModel("gpt-5.6-terra")
	loop.SetReasoningEffort("none")
	loop.SetContextWindow(1_050_000)

	refreshExecutionConfigRuntimeState(loop, &req)

	want := interruptedExecutionConfigA()
	want.ContextWindow = 1_050_000
	want.ContextWindowExplicit = false
	if !reflect.DeepEqual(req.ExecutionConfig, want) {
		t.Fatalf("runtime refresh changed frozen authority: got=%+v want=%+v", req.ExecutionConfig, want)
	}
}

func TestKoeInterruptedContinuationRestoresCheckpointResponseLanguage(t *testing.T) {
	loop := agent.NewAgentLoop(nil, agent.NewToolRegistry(), "small", "", 2, 1000, 100, nil, nil, nil)
	loop.SetResponseLanguage("English")
	req := RunAgentRequest{
		Source:            "koe",
		Text:              interruptedTurnContinuation,
		ResumeInterrupted: true,
		ExecutionConfig:   interruptedExecutionConfigA(),
	}

	applyKoeResponseLanguage(loop, req.Source, req.Text)
	if got := loop.ResponseLanguage(); got != "English" {
		t.Fatalf("continuation pre-lock language = %q, want English inference", got)
	}
	lockAgentExecutionConfig(loop, &req)
	if got := loop.ResponseLanguage(); got != "中文" {
		t.Fatalf("checkpoint language = %q, want 中文", got)
	}
}

func TestInterruptedResumeGatewayRequestUsesCheckpointConfigAAfterCurrentConfigB(t *testing.T) {
	gw := &fakeGatewayBackend{reply: "continued with checkpoint config"}
	server := httptest.NewServer(gw.handler())
	defer server.Close()

	deps := runAgentContractTestDeps(t, server.URL)
	defer deps.SessionCache.CloseAll()
	deps.Config.ModelTier = "small"
	deps.Config.Agent.Model = "current-config-b"
	deps.Config.Agent.Thinking = true
	deps.Config.Agent.ThinkingMode = "adaptive"
	deps.Config.Agent.ReasoningEffort = "low"
	deps.Config.Agent.EffortTier = "low"
	deps.Config.Agent.Temperature = 0.9
	deps.Config.Agent.MaxTokens = 999
	deps.Config.Agent.ContextWindow = 32_000
	deps.Config.Agent.MaxIterations = 2

	const id = "recovery-config-gateway-001"
	writeInterruptedSession(
		t,
		filepath.Join(deps.ShannonDir, "sessions"),
		id,
		&session.InterruptedTurn{
			Source:          "heartbeat",
			ExecutionConfig: interruptedExecutionConfigA(),
		},
	)
	(&Server{deps: deps}).resumeInterruptedTurns(context.Background())

	requests := gw.requests()
	if len(requests) == 0 {
		t.Fatal("recovery did not call the gateway")
	}
	got := requests[len(requests)-1]
	want := interruptedExecutionConfigA()
	if got.SpecificModel != want.SpecificModel ||
		got.ModelTier != want.ModelTier ||
		got.Thinking == nil ||
		*got.Thinking != *want.Thinking ||
		got.ReasoningEffort != want.ReasoningEffort ||
		got.EffortTier != want.EffortTier ||
		got.Temperature != want.Temperature ||
		got.MaxTokens != want.MaxTokens {
		t.Fatalf("gateway request used current config B instead of checkpoint A: got=%+v want=%+v", got, want)
	}
	var messageText strings.Builder
	for _, message := range got.Messages {
		messageText.WriteString(message.Content.Text())
		messageText.WriteByte('\n')
	}
	if !strings.Contains(messageText.String(), "Always respond in 中文") {
		t.Fatalf("gateway request lost checkpoint response language: %s", messageText.String())
	}
}

func TestInterruptedResumeAttemptPersistenceAndAbandon(t *testing.T) {
	shannonDir := t.TempDir()
	dir := filepath.Join(shannonDir, "sessions")
	id := "recovery-attempt-001"
	writeInterruptedSession(t, dir, id, &session.InterruptedTurn{
		Source:         "desktop",
		ResumeAttempts: 1,
	})

	candidates, err := discoverInterruptedTurns(shannonDir)
	if err != nil {
		t.Fatal(err)
	}
	if len(candidates) != 1 {
		t.Fatalf("candidates = %d, want 1", len(candidates))
	}
	candidate := candidates[0]
	if candidate.StoreDir != dir {
		t.Fatalf("store dir = %q, want %q", candidate.StoreDir, dir)
	}
	attemptMgr := session.NewManager(dir)
	attemptSession, err := attemptMgr.Resume(id)
	if err != nil {
		t.Fatal(err)
	}
	req := buildInterruptedResumeRequest(candidate, 3, 4*time.Hour)
	if err := claimInterruptedResume(attemptMgr, attemptSession, &req, ""); err != nil {
		t.Fatalf("claim attempt: %v", err)
	}
	if req.InterruptedResumeAttempt != 2 {
		t.Fatalf("claimed attempt = %d, want 2", req.InterruptedResumeAttempt)
	}
	_ = attemptMgr.Close()

	mgr := session.NewManager(dir)
	sess, err := mgr.Load(id)
	if err != nil {
		t.Fatal(err)
	}
	if sess.InterruptedTurn == nil || sess.InterruptedTurn.ResumeAttempts != 2 {
		t.Fatalf("resume attempts not persisted: %#v", sess.InterruptedTurn)
	}
	updatedAt := sess.UpdatedAt
	_ = mgr.Close()

	exhaustedCandidate := interruptedTurnCandidate{
		SessionID: id,
		StoreDir:  dir,
		State:     cloneInterruptedTurn(*sess.InterruptedTurn),
		UpdatedAt: sess.UpdatedAt,
	}
	abandonMgr := session.NewManager(dir)
	abandonSession, err := abandonMgr.Resume(id)
	if err != nil {
		t.Fatal(err)
	}
	req = buildInterruptedResumeRequest(exhaustedCandidate, 2, 4*time.Hour)
	if err := claimInterruptedResume(abandonMgr, abandonSession, &req, ""); !errors.Is(err, errInterruptedRecoveryExhausted) {
		t.Fatalf("exhausted claim error = %v, want %v", err, errInterruptedRecoveryExhausted)
	}
	_ = abandonMgr.Close()
	mgr = session.NewManager(dir)
	defer mgr.Close()
	sess, err = mgr.Load(id)
	if err != nil {
		t.Fatal(err)
	}
	if sess.InProgress || sess.InterruptedTurn != nil {
		t.Fatalf("abandoned session retained recovery state: %#v", sess)
	}
	if !sess.UpdatedAt.Equal(updatedAt) {
		t.Fatalf("abandonment changed session recency: got %s want %s", sess.UpdatedAt, updatedAt)
	}
}

func TestResumeInterruptedTurnsAbandonsExhaustedWithoutGatewayCall(t *testing.T) {
	gw := &fakeGatewayBackend{reply: "must not be called"}
	ts := httptest.NewServer(gw.handler())
	defer ts.Close()

	deps := runAgentContractTestDeps(t, ts.URL)
	defer deps.SessionCache.CloseAll()
	deps.Config.Agent.InterruptedResumeMaxAttempts = 2
	id := "recovery-exhausted-001"
	writeInterruptedSession(t, filepath.Join(deps.ShannonDir, "sessions"), id, &session.InterruptedTurn{
		Source:         "heartbeat",
		ResumeAttempts: 2,
	})

	(&Server{deps: deps}).resumeInterruptedTurns(context.Background())
	if requests := gw.requests(); len(requests) != 0 {
		t.Fatalf("exhausted recovery called gateway %d times", len(requests))
	}

	mgr := session.NewManager(filepath.Join(deps.ShannonDir, "sessions"))
	defer mgr.Close()
	sess, err := mgr.Load(id)
	if err != nil {
		t.Fatal(err)
	}
	if sess.InProgress || sess.InterruptedTurn != nil {
		t.Fatalf("exhausted recovery marker not cleared: %#v", sess)
	}
}

func TestResumeInterruptedTurnFailuresPreserveCheckpointAndStopAtLimit(t *testing.T) {
	gateway := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "temporary gateway failure", http.StatusServiceUnavailable)
	}))
	defer gateway.Close()

	deps := runAgentContractTestDeps(t, gateway.URL)
	defer deps.SessionCache.CloseAll()
	id := "recovery-failure-limit-001"
	dir := filepath.Join(deps.ShannonDir, "sessions")
	writeInterruptedSession(t, dir, id, &session.InterruptedTurn{Source: "heartbeat"})

	candidates, err := discoverInterruptedTurns(deps.ShannonDir)
	if err != nil {
		t.Fatal(err)
	}
	if len(candidates) != 1 {
		t.Fatalf("candidates = %d, want 1", len(candidates))
	}
	checkpointAt := candidates[0].State.UpdatedAt
	(&Server{deps: deps}).resumeInterruptedCandidate(context.Background(), candidates[0], 2, 4*time.Hour)

	mgr := session.NewManager(dir)
	firstFailure, err := mgr.Load(id)
	if err != nil {
		t.Fatal(err)
	}
	if !firstFailure.InProgress || firstFailure.InterruptedTurn == nil {
		t.Fatalf("first failure cleared recoverable checkpoint: %#v", firstFailure)
	}
	if got := firstFailure.InterruptedTurn.ResumeAttempts; got != 1 {
		t.Fatalf("resume attempts after first failure = %d, want 1", got)
	}
	if !firstFailure.InterruptedTurn.UpdatedAt.Equal(checkpointAt) {
		t.Fatalf("first failure refreshed checkpoint age: got %s want %s",
			firstFailure.InterruptedTurn.UpdatedAt, checkpointAt)
	}
	_ = mgr.Close()

	candidates, err = discoverInterruptedTurns(deps.ShannonDir)
	if err != nil {
		t.Fatal(err)
	}
	if len(candidates) != 1 {
		t.Fatalf("candidates after first failure = %d, want 1", len(candidates))
	}
	(&Server{deps: deps}).resumeInterruptedCandidate(context.Background(), candidates[0], 2, 4*time.Hour)

	mgr = session.NewManager(dir)
	defer mgr.Close()
	exhausted, err := mgr.Load(id)
	if err != nil {
		t.Fatal(err)
	}
	if exhausted.InProgress || exhausted.InterruptedTurn != nil {
		t.Fatalf("second failed attempt retained exhausted recovery state: %#v", exhausted)
	}
}

func TestResumeInterruptedTurns_ContinuesCheckpointWithoutToolReplay(t *testing.T) {
	gw := &fakeGatewayBackend{reply: "continued from the saved result"}
	ts := httptest.NewServer(gw.handler())
	defer ts.Close()

	deps := runAgentContractTestDeps(t, ts.URL)
	defer deps.SessionCache.CloseAll()
	id := "recovery-session-001"
	writeInterruptedSession(t, filepath.Join(deps.ShannonDir, "sessions"), id, &session.InterruptedTurn{
		Source: "heartbeat",
	})

	srv := &Server{deps: deps}
	srv.resumeInterruptedTurns(context.Background())

	mgr := session.NewManager(filepath.Join(deps.ShannonDir, "sessions"))
	defer mgr.Close()
	sess, err := mgr.Load(id)
	if err != nil {
		t.Fatalf("load resumed session: %v", err)
	}
	if sess.InProgress {
		t.Fatal("successful recovery left InProgress set")
	}
	if sess.InterruptedTurn != nil {
		t.Fatalf("successful recovery left metadata: %#v", sess.InterruptedTurn)
	}

	var injectedContinuation int
	var assistantReply bool
	for i, msg := range sess.Messages {
		if msg.Role == "user" && msg.Content.Text() == interruptedTurnContinuation {
			injectedContinuation++
			if i >= len(sess.MessageMeta) || !sess.MessageMeta[i].SystemInjected {
				t.Fatalf("continuation at %d is not marked SystemInjected", i)
			}
		}
		if msg.Role == "assistant" && msg.Content.Text() == "continued from the saved result" {
			assistantReply = true
		}
	}
	if injectedContinuation != 1 {
		t.Fatalf("injected continuation count = %d, want 1", injectedContinuation)
	}
	if !assistantReply {
		t.Fatal("resumed assistant reply missing")
	}
	for _, msg := range sess.HistoryForLoop() {
		if msg.Role == "user" && msg.Content.Text() == interruptedTurnContinuation {
			t.Fatal("system-injected recovery marker leaked into later turn history")
		}
	}

	requests := gw.requests()
	if len(requests) == 0 {
		t.Fatal("recovery did not call the gateway")
	}
	var sawSavedToolResult bool
	for _, request := range requests {
		for _, msg := range request.Messages {
			for _, block := range msg.Content.Blocks() {
				if block.Type == "tool_result" && block.ToolUseID == "tool-1" {
					sawSavedToolResult = true
				}
			}
		}
	}
	if !sawSavedToolResult {
		t.Fatal("gateway did not receive the checkpointed tool result")
	}
}

func TestCompletedForegroundTurnCancelsPendingRecovery(t *testing.T) {
	gw := &fakeGatewayBackend{reply: "must not be called"}
	ts := httptest.NewServer(gw.handler())
	defer ts.Close()

	deps := runAgentContractTestDeps(t, ts.URL)
	defer deps.SessionCache.CloseAll()
	id := "recovery-superseded-001"
	dir := filepath.Join(deps.ShannonDir, "sessions")
	writeInterruptedSession(t, dir, id, &session.InterruptedTurn{Source: "desktop"})

	candidates, err := discoverInterruptedTurns(deps.ShannonDir)
	if err != nil {
		t.Fatal(err)
	}
	if len(candidates) != 1 {
		t.Fatalf("candidates = %d, want 1", len(candidates))
	}
	candidate := candidates[0]
	req := buildInterruptedResumeRequest(candidate, 3, 4*time.Hour)
	routeKey := ComputeRouteKey(req)
	route := deps.SessionCache.LockRouteWithManager(routeKey, dir)
	if route == nil || route.manager == nil {
		t.Fatal("failed to acquire foreground route manager")
	}

	started := make(chan struct{})
	resumeDone := make(chan struct{})
	go func() {
		defer close(resumeDone)
		close(started)
		(&Server{deps: deps}).resumeInterruptedCandidate(context.Background(), candidate, 3, 4*time.Hour)
	}()
	<-started

	foregroundSession, err := route.manager.Resume(id)
	if err != nil {
		t.Fatal(err)
	}
	foregroundSession.InProgress = false
	foregroundSession.InterruptedTurn = nil
	if err := route.manager.Save(); err != nil {
		t.Fatal(err)
	}
	deps.SessionCache.UnlockRoute(routeKey)

	select {
	case <-resumeDone:
	case <-time.After(2 * time.Second):
		t.Fatal("pending recovery did not finish after foreground route unlocked")
	}
	if requests := gw.requests(); len(requests) != 0 {
		t.Fatalf("superseded recovery called gateway %d times", len(requests))
	}

	mgr := session.NewManager(dir)
	defer mgr.Close()
	persisted, err := mgr.Load(id)
	if err != nil {
		t.Fatal(err)
	}
	if persisted.InProgress || persisted.InterruptedTurn != nil {
		t.Fatalf("superseded recovery rewrote completed session: %#v", persisted)
	}
}
