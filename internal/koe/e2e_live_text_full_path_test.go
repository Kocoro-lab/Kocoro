//go:build darwin && cgo

package koe

// Paid live text E2E for the complete Koe delegation and result-delivery path.
// Unlike the narrower selector and result-summary tests, this keeps both live
// boundaries in one run: OpenAI Realtime selects do_task, the production event
// handler delegates to the running daemon and its real provider, ResultMailbox
// returns the completed result to the same Realtime session, and the test
// verifies the final spoken transcript plus the persisted daemon session.
//
// It intentionally has its own gate so KOE_E2E=1 does not add a real daemon
// agent turn to the existing live Realtime suite.
//
//	KOE_LIVE_TEXT_FULL_PATH_E2E=1 \
//	PKG_CONFIG_PATH=/opt/homebrew/lib/pkgconfig \
//	go test ./internal/koe -run '^TestKoeLiveTextFullPathE2E$' \
//	  -count=1 -v -timeout=5m

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/pion/webrtc/v4"
)

const (
	liveTextFullPathGate   = "KOE_LIVE_TEXT_FULL_PATH_E2E"
	liveTextFullPathMarker = "323"
	liveTextFullPathPrompt = "这是一项需要实际执行的真实任务：计算 17 × 19，只用一句简短中文告诉我结果。"
)

type liveTextFullPathDaemonStatus struct {
	Version      string   `json:"version"`
	IsConnected  bool     `json:"is_connected"`
	Capabilities []string `json:"capabilities"`
}

type liveTextFullPathDaemonUsage struct {
	LLMCalls       int     `json:"llm_calls"`
	InputTokens    int     `json:"input_tokens"`
	OutputTokens   int     `json:"output_tokens"`
	TotalTokens    int     `json:"total_tokens"`
	CostUSD        float64 `json:"cost_usd"`
	WebSearchCalls int     `json:"web_search_calls"`
	Model          string  `json:"model"`
}

type liveTextFullPathRealtimeUsage struct {
	InputTokens  int `json:"input_tokens"`
	OutputTokens int `json:"output_tokens"`
	TotalTokens  int `json:"total_tokens"`
}

type liveTextFullPathDoTaskArgs struct {
	Task          string `json:"task"`
	Agent         string `json:"agent"`
	ExecutionMode string `json:"execution_mode"`
	FullReason    string `json:"full_reason"`
}

type liveTextFullPathProbe struct {
	mu sync.Mutex

	doTaskCallIDs          []string
	doTaskArgs             []liveTextFullPathDoTaskArgs
	functionOutputs        map[string]int
	functionOutputStatuses map[string][]string
	resultBatches          []SayResult
	taskResultRequests     int
	taskResultResponseIDs  map[string]struct{}
	taskResultDoneStatuses map[string]string
	transcripts            map[string][]string
	realtimeUsages         []liveTextFullPathRealtimeUsage
	responseDoneCount      int
	apiErrors              []string

	startedAt        time.Time
	doTaskAt         time.Time
	resultBatchAt    time.Time
	taskResultDoneAt time.Time
}

func newLiveTextFullPathProbe() *liveTextFullPathProbe {
	return &liveTextFullPathProbe{
		functionOutputs:        make(map[string]int),
		functionOutputStatuses: make(map[string][]string),
		taskResultResponseIDs:  make(map[string]struct{}),
		taskResultDoneStatuses: make(map[string]string),
		transcripts:            make(map[string][]string),
		startedAt:              time.Now(),
	}
}

func (p *liveTextFullPathProbe) observeOutbound(value any) {
	body, err := json.Marshal(value)
	if err != nil {
		return
	}
	var event struct {
		Type string `json:"type"`
		Item struct {
			Type    string `json:"type"`
			Role    string `json:"role"`
			CallID  string `json:"call_id"`
			Output  string `json:"output"`
			Content []struct {
				Text string `json:"text"`
			} `json:"content"`
		} `json:"item"`
		Response struct {
			Metadata map[string]string `json:"metadata"`
		} `json:"response"`
	}
	if json.Unmarshal(body, &event) != nil {
		return
	}

	p.mu.Lock()
	defer p.mu.Unlock()
	switch {
	case event.Type == "conversation.item.create" && event.Item.Type == "function_call_output":
		p.functionOutputs[event.Item.CallID]++
		var output struct {
			Status string `json:"status"`
		}
		if json.Unmarshal([]byte(event.Item.Output), &output) == nil {
			p.functionOutputStatuses[event.Item.CallID] = append(p.functionOutputStatuses[event.Item.CallID], output.Status)
		}
	case event.Type == "conversation.item.create" && event.Item.Type == "message" && event.Item.Role == "system":
		for _, content := range event.Item.Content {
			result, ok := parseLiveTextFullPathResult(content.Text)
			if !ok {
				continue
			}
			p.resultBatches = append(p.resultBatches, result)
			if p.resultBatchAt.IsZero() {
				p.resultBatchAt = time.Now()
			}
		}
	case event.Type == "response.create" && event.Response.Metadata["koe_purpose"] == string(responsePurposeTaskResult):
		p.taskResultRequests++
	}
}

func (p *liveTextFullPathProbe) observeInbound(raw []byte) {
	var event struct {
		Type       string          `json:"type"`
		Name       string          `json:"name"`
		CallID     string          `json:"call_id"`
		ResponseID string          `json:"response_id"`
		Transcript string          `json:"transcript"`
		Arguments  json.RawMessage `json:"arguments"`
		Response   struct {
			ID            string                        `json:"id"`
			Status        string                        `json:"status"`
			Metadata      map[string]string             `json:"metadata"`
			Usage         liveTextFullPathRealtimeUsage `json:"usage"`
			StatusDetails struct {
				Error struct {
					Code    string `json:"code"`
					Type    string `json:"type"`
					Message string `json:"message"`
				} `json:"error"`
			} `json:"status_details"`
		} `json:"response"`
		Error struct {
			Code    string `json:"code"`
			Type    string `json:"type"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if json.Unmarshal(raw, &event) != nil {
		return
	}

	p.mu.Lock()
	defer p.mu.Unlock()
	switch event.Type {
	case "response.function_call_arguments.done":
		if event.Name == "do_task" {
			p.doTaskCallIDs = append(p.doTaskCallIDs, event.CallID)
			var args liveTextFullPathDoTaskArgs
			_ = json.Unmarshal(unwrapArgs(event.Arguments), &args)
			p.doTaskArgs = append(p.doTaskArgs, args)
			if p.doTaskAt.IsZero() {
				p.doTaskAt = time.Now()
			}
		}
	case "response.created":
		if event.Response.Metadata["koe_purpose"] == string(responsePurposeTaskResult) {
			p.taskResultResponseIDs[event.Response.ID] = struct{}{}
		}
	case "response.output_audio_transcript.done":
		p.transcripts[event.ResponseID] = append(p.transcripts[event.ResponseID], event.Transcript)
	case "response.done":
		p.responseDoneCount++
		p.realtimeUsages = append(p.realtimeUsages, event.Response.Usage)
		if _, ok := p.taskResultResponseIDs[event.Response.ID]; ok {
			p.taskResultDoneStatuses[event.Response.ID] = event.Response.Status
			if p.taskResultDoneAt.IsZero() {
				p.taskResultDoneAt = time.Now()
			}
		}
	case "error", "response.failed":
		failure := event.Error
		if failure.Code == "" && failure.Type == "" && failure.Message == "" {
			failure = event.Response.StatusDetails.Error
		}
		p.apiErrors = append(p.apiErrors, fmt.Sprintf("%s code=%s type=%s message=%s", event.Type, failure.Code, failure.Type, failure.Message))
	}
}

func parseLiveTextFullPathResult(text string) (SayResult, bool) {
	const marker = `{"type":"kocoro.task_results.v1"`
	index := strings.Index(text, marker)
	if index < 0 {
		return SayResult{}, false
	}
	var payload struct {
		Type    string      `json:"type"`
		Results []SayResult `json:"results"`
	}
	if err := json.Unmarshal([]byte(text[index:]), &payload); err != nil || payload.Type != "kocoro.task_results.v1" || len(payload.Results) != 1 {
		return SayResult{}, false
	}
	return payload.Results[0], true
}

func (p *liveTextFullPathProbe) completed() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	for responseID, status := range p.taskResultDoneStatuses {
		if status != "completed" {
			continue
		}
		if strings.Contains(strings.Join(p.transcripts[responseID], " "), liveTextFullPathMarker) {
			return true
		}
	}
	return false
}

func (p *liveTextFullPathProbe) snapshot() liveTextFullPathProbe {
	p.mu.Lock()
	defer p.mu.Unlock()
	return liveTextFullPathProbe{
		doTaskCallIDs:          append([]string(nil), p.doTaskCallIDs...),
		doTaskArgs:             append([]liveTextFullPathDoTaskArgs(nil), p.doTaskArgs...),
		functionOutputs:        cloneStringIntMap(p.functionOutputs),
		functionOutputStatuses: cloneStringSliceMap(p.functionOutputStatuses),
		resultBatches:          append([]SayResult(nil), p.resultBatches...),
		taskResultRequests:     p.taskResultRequests,
		taskResultResponseIDs:  cloneStringSet(p.taskResultResponseIDs),
		taskResultDoneStatuses: cloneStringStringMap(p.taskResultDoneStatuses),
		transcripts:            cloneStringSliceMap(p.transcripts),
		realtimeUsages:         append([]liveTextFullPathRealtimeUsage(nil), p.realtimeUsages...),
		responseDoneCount:      p.responseDoneCount,
		apiErrors:              append([]string(nil), p.apiErrors...),
		startedAt:              p.startedAt,
		doTaskAt:               p.doTaskAt,
		resultBatchAt:          p.resultBatchAt,
		taskResultDoneAt:       p.taskResultDoneAt,
	}
}

func TestKoeLiveTextFullPathE2E(t *testing.T) {
	if os.Getenv(liveTextFullPathGate) != "1" {
		t.Skip("paid live Realtime + daemon/provider E2E: set " + liveTextFullPathGate + "=1")
	}
	t.Setenv("KOE_TASK_LEDGER", "1")
	t.Setenv("KOE_RESULT_DELIVERY", "1")

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

	daemonURL := strings.TrimSpace(os.Getenv("KOE_DAEMON_URL"))
	if daemonURL == "" {
		daemonURL = "http://127.0.0.1:7533"
	}
	status := requireLiveTextFullPathDaemon(t, ctx, daemonURL)
	t.Logf("[daemon] version=%s connected=%t", status.Version, status.IsConnected)

	daemonClient := NewDaemonClient(daemonURL)
	ephemeralKey, err := daemonClient.MintViaDaemon(ctx, e2eModelName())
	if err != nil {
		t.Fatalf("mint Realtime token through daemon: %v", err)
	}
	audio, err := NewAudioIO()
	if err != nil {
		t.Fatalf("NewAudioIO: %v", err)
	}
	audio.SetPlaybackEnabled(false)
	rc, err := newPeerConnection(audio)
	if err != nil {
		audio.Stop()
		t.Fatalf("newPeerConnection: %v", err)
	}
	defer func() {
		rc.Close()
		audio.Stop()
	}()

	probe := newLiveTextFullPathProbe()
	state := NewCallState(fmt.Sprintf("live-text-full-path-%d", time.Now().UnixNano()), "")
	mailbox := NewResultMailbox()
	mailbox.BeginBurst(state.BurstID())
	dispatcher := NewDispatcher(daemonClient, NewAgentResolver(nil, NoopSemanticMatcher{}), state, nil)

	var sendMu sync.Mutex
	send := func(value any) error {
		probe.observeOutbound(value)
		body, err := json.Marshal(value)
		if err != nil {
			return err
		}
		sendMu.Lock()
		defer sendMu.Unlock()
		return rc.dc.SendText(string(body))
	}
	handler := newEventHandlerWithMailbox(dispatcher, state, audio, send, mailbox, nil)
	handler.language = "zh"
	go handler.runResponseSender(ctx)

	configured := make(chan struct{})
	var configuredOnce sync.Once
	rc.dc.OnOpen(func() {
		if err := send(sessionConfig(e2ePersona, "marin", false)); err != nil {
			probe.mu.Lock()
			probe.apiErrors = append(probe.apiErrors, "send session config: "+err.Error())
			probe.mu.Unlock()
		}
	})
	rc.dc.OnMessage(func(message webrtc.DataChannelMessage) {
		handler.handleEvent(ctx, message.Data)
		probe.observeInbound(message.Data)
		var event struct {
			Type string `json:"type"`
		}
		if json.Unmarshal(message.Data, &event) == nil && event.Type == "session.updated" {
			configuredOnce.Do(func() { close(configured) })
		}
	})
	if err := rc.dialOpenAI(ctx, ephemeralKey); err != nil {
		t.Fatalf("dial OpenAI Realtime: %v", err)
	}
	select {
	case <-configured:
	case <-ctx.Done():
		t.Fatalf("wait for session.updated: %v", ctx.Err())
	}

	if err := sendLiveTextFullPathTurn(handler, send, liveTextFullPathPrompt); err != nil {
		t.Fatalf("send text input: %v", err)
	}

	ticker := time.NewTicker(25 * time.Millisecond)
	defer ticker.Stop()
	for !probe.completed() {
		select {
		case <-ctx.Done():
			snapshot := probe.snapshot()
			t.Fatalf("full-path result timed out: %v; calls=%v args=%+v outputs=%v statuses=%v batches=%+v result_requests=%d result_responses=%v done=%v transcripts=%v errors=%v mailbox_pending=%d",
				ctx.Err(), snapshot.doTaskCallIDs, snapshot.doTaskArgs, snapshot.functionOutputs, snapshot.functionOutputStatuses, snapshot.resultBatches,
				snapshot.taskResultRequests, snapshot.taskResultResponseIDs, snapshot.taskResultDoneStatuses,
				snapshot.transcripts, snapshot.apiErrors, mailbox.pending())
		case <-ticker.C:
			snapshot := probe.snapshot()
			if len(snapshot.apiErrors) != 0 {
				t.Fatalf("Realtime failed before completing the full path: %s", strings.Join(snapshot.apiErrors, "; "))
			}
			for callID, statuses := range snapshot.functionOutputStatuses {
				for _, status := range statuses {
					if status != "" && status != "running" {
						t.Fatalf("do_task %s terminated before daemon delegation with status=%q args=%+v", callID, status, snapshot.doTaskArgs)
					}
				}
			}
		}
	}

	snapshot := probe.snapshot()
	if len(snapshot.apiErrors) != 0 {
		t.Fatalf("Realtime errors: %s", strings.Join(snapshot.apiErrors, "; "))
	}
	if len(snapshot.doTaskCallIDs) != 1 || snapshot.doTaskCallIDs[0] == "" {
		t.Fatalf("do_task calls=%v, want exactly one non-empty call_id", snapshot.doTaskCallIDs)
	}
	callID := snapshot.doTaskCallIDs[0]
	if got := snapshot.functionOutputs[callID]; got != 1 {
		t.Fatalf("function_call_output count for %q=%d, want 1", callID, got)
	}
	if len(snapshot.resultBatches) != 1 {
		t.Fatalf("result batches=%d, want 1: %+v", len(snapshot.resultBatches), snapshot.resultBatches)
	}
	result := snapshot.resultBatches[0]
	if result.Status != "ok" || result.SessionID == "" || !strings.Contains(result.Reply, liveTextFullPathMarker) {
		t.Fatalf("unexpected daemon result: %+v", result)
	}
	if snapshot.taskResultRequests != 1 || len(snapshot.taskResultResponseIDs) != 1 || len(snapshot.taskResultDoneStatuses) != 1 {
		t.Fatalf("task-result lifecycle requests=%d responses=%v done=%v, want 1/1/1",
			snapshot.taskResultRequests, snapshot.taskResultResponseIDs, snapshot.taskResultDoneStatuses)
	}
	if snapshot.responseDoneCount != 2 || len(snapshot.realtimeUsages) != 2 {
		t.Fatalf("Realtime completed responses=%d usages=%d, want selector and task-result responses", snapshot.responseDoneCount, len(snapshot.realtimeUsages))
	}
	if pending := mailbox.pending(); pending != 0 {
		t.Fatalf("result mailbox pending=%d after completed response.done, want 0", pending)
	}
	daemonUsage := requireLiveTextFullPathSession(t, ctx, daemonURL, result.SessionID)

	t.Logf("VERDICT: daemon=%s call_id=%s session=%s selector_ms=%d daemon_ms=%d result_voice_ms=%d total_ms=%d",
		status.Version,
		callID,
		result.SessionID,
		durationMillis(snapshot.startedAt, snapshot.doTaskAt),
		durationMillis(snapshot.doTaskAt, snapshot.resultBatchAt),
		durationMillis(snapshot.resultBatchAt, snapshot.taskResultDoneAt),
		durationMillis(snapshot.startedAt, snapshot.taskResultDoneAt),
	)
	t.Logf("USAGE: daemon_model=%s daemon_calls=%d daemon_input=%d daemon_output=%d daemon_total=%d daemon_cost_usd=%.8f web_search_calls=%d realtime=%+v",
		daemonUsage.Model, daemonUsage.LLMCalls, daemonUsage.InputTokens, daemonUsage.OutputTokens,
		daemonUsage.TotalTokens, daemonUsage.CostUSD, daemonUsage.WebSearchCalls, snapshot.realtimeUsages)
}

func sendLiveTextFullPathTurn(handler *eventHandler, send func(any) error, text string) error {
	if handler == nil || handler.toolLoop == nil {
		return fmt.Errorf("Koe event handler is not initialized")
	}
	if err := send(map[string]any{
		"type": "conversation.item.create",
		"item": map[string]any{
			"type": "message",
			"role": "user",
			"content": []map[string]any{{
				"type": "input_text",
				"text": text,
			}},
		},
	}); err != nil {
		return err
	}
	// Text items have no input_audio_buffer.committed event. Establish the same
	// turn authority that the production audio path creates at commit time, then
	// use the serialized response sender so response metadata, tool authority,
	// and response.created acknowledgement remain production-identical.
	turnID := handler.inputCommitSeq.Add(1)
	handler.toolLoop.noteUserCommit(turnID)
	handler.requestResponseWith(responseCreateRequest{
		purpose:  responsePurposeUser,
		turnID:   turnID,
		toolMode: responseToolsEnabled,
	})
	return nil
}

func requireLiveTextFullPathDaemon(t *testing.T, ctx context.Context, daemonURL string) liveTextFullPathDaemonStatus {
	t.Helper()
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(daemonURL, "/")+"/status", nil)
	if err != nil {
		t.Fatal(err)
	}
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatalf("daemon status: %v", err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(response.Body, 4096))
		t.Fatalf("daemon status HTTP %d: %s", response.StatusCode, body)
	}
	var status liveTextFullPathDaemonStatus
	if err := json.NewDecoder(response.Body).Decode(&status); err != nil {
		t.Fatalf("decode daemon status: %v", err)
	}
	if !status.IsConnected {
		t.Fatalf("daemon %s is not connected to Cloud", status.Version)
	}
	if !containsString(status.Capabilities, "koe_fast_profile_v1") {
		t.Fatalf("daemon %s lacks koe_fast_profile_v1", status.Version)
	}
	return status
}

func requireLiveTextFullPathSession(t *testing.T, ctx context.Context, daemonURL, sessionID string) liveTextFullPathDaemonUsage {
	t.Helper()
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(daemonURL, "/")+"/sessions/"+sessionID, nil)
	if err != nil {
		t.Fatal(err)
	}
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatalf("load daemon session %s: %v", sessionID, err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(response.Body, 4096))
		t.Fatalf("load daemon session %s HTTP %d: %s", sessionID, response.StatusCode, body)
	}
	var session struct {
		Usage    liveTextFullPathDaemonUsage `json:"usage"`
		Messages []struct {
			Role    string `json:"role"`
			Content string `json:"content"`
		} `json:"messages"`
	}
	if err := json.NewDecoder(response.Body).Decode(&session); err != nil {
		t.Fatalf("decode daemon session %s: %v", sessionID, err)
	}
	if len(session.Messages) < 2 {
		t.Fatalf("daemon session %s messages=%d, want at least 2", sessionID, len(session.Messages))
	}
	var sawUserTask, sawAssistantResult bool
	for _, message := range session.Messages {
		switch message.Role {
		case "user":
			sawUserTask = sawUserTask || (strings.Contains(message.Content, "17") && strings.Contains(message.Content, "19"))
		case "assistant":
			sawAssistantResult = sawAssistantResult || strings.Contains(message.Content, liveTextFullPathMarker)
		}
	}
	if !sawUserTask || !sawAssistantResult {
		t.Fatalf("daemon session %s did not persist the arithmetic task and its verified result", sessionID)
	}
	if session.Usage.LLMCalls != 1 || session.Usage.TotalTokens == 0 {
		t.Fatalf("daemon session %s usage=%+v, want one metered provider call", sessionID, session.Usage)
	}
	return session.Usage
}

func TestKoeLiveTextFullPathHarnessContract(t *testing.T) {
	probe := newLiveTextFullPathProbe()
	probe.observeInbound([]byte(`{"type":"response.function_call_arguments.done","name":"do_task","call_id":"call-1"}`))
	probe.observeOutbound(map[string]any{
		"type": "conversation.item.create",
		"item": map[string]any{"type": "function_call_output", "call_id": "call-1", "output": `{"status":"running"}`},
	})
	probe.observeOutbound(map[string]any{
		"type": "conversation.item.create",
		"item": map[string]any{
			"type": "message", "role": "system",
			"content": []map[string]any{{"type": "input_text", "text": "result\n" + `{"type":"kocoro.task_results.v1","results":[{"status":"ok","reply":"` + liveTextFullPathMarker + `","session_id":"session-1"}]}`}},
		},
	})
	probe.observeOutbound(map[string]any{
		"type":     "response.create",
		"response": map[string]any{"metadata": map[string]string{"koe_purpose": "task_result"}},
	})
	probe.observeInbound([]byte(`{"type":"response.done","response":{"id":"selector","status":"completed","usage":{"input_tokens":1,"output_tokens":1,"total_tokens":2}}}`))
	probe.observeInbound([]byte(`{"type":"response.created","response":{"id":"result","metadata":{"koe_purpose":"task_result"}}}`))
	probe.observeInbound([]byte(`{"type":"response.output_audio_transcript.done","response_id":"result","transcript":"` + liveTextFullPathMarker + `"}`))
	probe.observeInbound([]byte(`{"type":"response.done","response":{"id":"result","status":"completed","usage":{"input_tokens":2,"output_tokens":1,"total_tokens":3}}}`))

	if !probe.completed() {
		t.Fatal("synthetic completed lifecycle did not satisfy the harness")
	}
	snapshot := probe.snapshot()
	if len(snapshot.doTaskCallIDs) != 1 || snapshot.functionOutputs["call-1"] != 1 || len(snapshot.resultBatches) != 1 || snapshot.taskResultRequests != 1 || snapshot.responseDoneCount != 2 {
		t.Fatalf("unexpected harness snapshot: calls=%v outputs=%v batches=%+v result_requests=%d response_done=%d",
			snapshot.doTaskCallIDs, snapshot.functionOutputs, snapshot.resultBatches, snapshot.taskResultRequests, snapshot.responseDoneCount)
	}
}

func durationMillis(start, end time.Time) int64 {
	if start.IsZero() || end.IsZero() || end.Before(start) {
		return -1
	}
	return end.Sub(start).Milliseconds()
}

func containsString(values []string, target string) bool {
	for _, value := range values {
		if value == target {
			return true
		}
	}
	return false
}

func cloneStringIntMap(source map[string]int) map[string]int {
	clone := make(map[string]int, len(source))
	for key, value := range source {
		clone[key] = value
	}
	return clone
}

func cloneStringStringMap(source map[string]string) map[string]string {
	clone := make(map[string]string, len(source))
	for key, value := range source {
		clone[key] = value
	}
	return clone
}

func cloneStringSet(source map[string]struct{}) map[string]struct{} {
	clone := make(map[string]struct{}, len(source))
	for key := range source {
		clone[key] = struct{}{}
	}
	return clone
}

func cloneStringSliceMap(source map[string][]string) map[string][]string {
	clone := make(map[string][]string, len(source))
	for key, value := range source {
		clone[key] = append([]string(nil), value...)
	}
	return clone
}
