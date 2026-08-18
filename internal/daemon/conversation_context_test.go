package daemon

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Kocoro-lab/ShanClaw/internal/agent"
	"github.com/Kocoro-lab/ShanClaw/internal/client"
	ctxwin "github.com/Kocoro-lab/ShanClaw/internal/context"
	"github.com/Kocoro-lab/ShanClaw/internal/session"
)

func TestForkSessionEndpointCreatesOrdinarySession(t *testing.T) {
	shannonDir := t.TempDir()
	deps := &ServerDeps{ShannonDir: shannonDir, SessionCache: NewSessionCache(shannonDir)}
	server := NewServer(0, nil, deps, "test")
	mgr := deps.SessionCache.GetOrCreateManager(deps.SessionCache.SessionsDir(""))
	source := mgr.NewSessionWithID("source-session-http")
	source.Title = "Source title"
	source.CWD = "/tmp/project"
	source.Messages = []client.Message{
		{Role: "user", Content: client.NewTextContent("question")},
		{Role: "assistant", Content: client.NewTextContent("answer")},
	}
	if err := mgr.Save(); err != nil {
		t.Fatal(err)
	}

	body := bytes.NewBufferString(`{"message_index":2}`)
	req := httptest.NewRequest(http.MethodPost, "/sessions/source-session-http/fork", body)
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	server.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
	var response forkSessionResponse
	if err := json.NewDecoder(rec.Body).Decode(&response); err != nil {
		t.Fatal(err)
	}
	if response.SessionID == "" || response.Title != source.Title {
		t.Fatalf("response = %#v", response)
	}
	if _, err := mgr.Load(response.SessionID); err != nil {
		t.Fatalf("fork not loadable: %v", err)
	}
}

// writeTestAgentDefinition creates the minimal on-disk agent (AGENT.md) that
// conversationAgentExists resolves.
func writeTestAgentDefinition(t *testing.T, agentsDir, name string) {
	t.Helper()
	dir := filepath.Join(agentsDir, name)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "AGENT.md"), []byte("test agent"), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestForkSessionEndpointCanTargetAnotherAgent(t *testing.T) {
	shannonDir := t.TempDir()
	agentsDir := filepath.Join(shannonDir, "agents")
	deps := &ServerDeps{ShannonDir: shannonDir, AgentsDir: agentsDir, SessionCache: NewSessionCache(shannonDir)}
	server := NewServer(0, nil, deps, "test")
	writeTestAgentDefinition(t, agentsDir, "research")
	writeTestAgentDefinition(t, agentsDir, "investment")
	sourceManager := deps.SessionCache.GetOrCreateManager(deps.SessionCache.SessionsDir("research"))
	source := sourceManager.NewSessionWithID("source-session-cross-agent")
	source.Title = "Cross-agent branch"
	source.Messages = []client.Message{
		{Role: "user", Content: client.NewTextContent("question")},
		{Role: "assistant", Content: client.NewTextContent("answer")},
	}
	if err := sourceManager.Save(); err != nil {
		t.Fatal(err)
	}

	body := bytes.NewBufferString(`{"message_index":2,"agent":"research","target_agent":"investment"}`)
	req := httptest.NewRequest(http.MethodPost, "/sessions/source-session-cross-agent/fork", body)
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	server.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
	var response forkSessionResponse
	if err := json.NewDecoder(rec.Body).Decode(&response); err != nil {
		t.Fatal(err)
	}
	targetManager := deps.SessionCache.GetOrCreateManager(deps.SessionCache.SessionsDir("investment"))
	if _, err := targetManager.Load(response.SessionID); err != nil {
		t.Fatalf("target agent cannot load fork: %v", err)
	}
	if _, err := sourceManager.Load(response.SessionID); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("fork persisted under source agent: %v", err)
	}
}

func TestForkSessionEndpointRejectsUnknownTargetAgent(t *testing.T) {
	shannonDir := t.TempDir()
	deps := &ServerDeps{
		ShannonDir:   shannonDir,
		AgentsDir:    filepath.Join(shannonDir, "agents"),
		SessionCache: NewSessionCache(shannonDir),
	}
	server := NewServer(0, nil, deps, "test")
	mgr := deps.SessionCache.GetOrCreateManager(deps.SessionCache.SessionsDir(""))
	source := mgr.NewSessionWithID("source-session-unknown-target")
	source.Messages = []client.Message{
		{Role: "user", Content: client.NewTextContent("question")},
		{Role: "assistant", Content: client.NewTextContent("answer")},
	}
	if err := mgr.Save(); err != nil {
		t.Fatal(err)
	}

	body := bytes.NewBufferString(`{"message_index":2,"target_agent":"nope"}`)
	req := httptest.NewRequest(http.MethodPost, "/sessions/source-session-unknown-target/fork", body)
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	server.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
	var payload struct {
		Code string `json:"code"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil || payload.Code != "agent_not_found" {
		t.Fatalf("code = %q err=%v body=%s", payload.Code, err, rec.Body.String())
	}
	// A rejected fork must not litter an orphan session directory.
	if _, err := os.Stat(filepath.Join(shannonDir, "agents", "nope")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("orphan target agent dir created: %v", err)
	}
}

func TestForkSessionEndpointRejectsIncompleteTurn(t *testing.T) {
	shannonDir := t.TempDir()
	deps := &ServerDeps{ShannonDir: shannonDir, SessionCache: NewSessionCache(shannonDir)}
	server := NewServer(0, nil, deps, "test")
	mgr := deps.SessionCache.GetOrCreateManager(deps.SessionCache.SessionsDir(""))
	source := mgr.NewSessionWithID("source-session-tool")
	source.Messages = []client.Message{
		{Role: "user", Content: client.NewTextContent("question")},
		{Role: "assistant", Content: client.NewBlockContent([]client.ContentBlock{{Type: "tool_use", ID: "call-1", Name: "read_file"}})},
	}
	if err := mgr.Save(); err != nil {
		t.Fatal(err)
	}

	req := httptest.NewRequest(http.MethodPost, "/sessions/source-session-tool/fork", bytes.NewBufferString(`{"message_index":2}`))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	server.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
}

func TestConversationContextCapabilityAdvertised(t *testing.T) {
	for _, capability := range Capabilities {
		if capability == CapConversationContextActionsV1 {
			return
		}
	}
	t.Fatalf("Capabilities missing %q", CapConversationContextActionsV1)
}

// sideChatProbeTool is a minimal approval-requiring tool for pinning that
// side chats run with the normal registry + approval flow.
type sideChatProbeTool struct{ calls int }

func (t *sideChatProbeTool) Info() agent.ToolInfo {
	return agent.ToolInfo{Name: "sidechat_probe", Description: "side chat probe tool"}
}
func (t *sideChatProbeTool) RequiresApproval() bool { return true }
func (t *sideChatProbeTool) Run(context.Context, string) (agent.ToolResult, error) {
	t.calls++
	return agent.ToolResult{Content: "probe ok"}, nil
}

func TestSideChatEndpointRunsToolEnabledEphemeralContext(t *testing.T) {
	gateway := &fakeGatewayBackend{reply: "focused answer"}
	gatewayServer := httptest.NewServer(gateway.handler())
	defer gatewayServer.Close()

	deps := runAgentContractTestDeps(t, gatewayServer.URL)
	defer deps.SessionCache.CloseAll()
	deps.Registry.Register(&sideChatProbeTool{})
	deps.BaselineReg.Register(&sideChatProbeTool{})
	mgr := deps.SessionCache.GetOrCreateManager(deps.SessionCache.SessionsDir(""))
	source := mgr.NewSessionWithID("side-chat-source")
	source.CWD = t.TempDir()
	source.Messages = []client.Message{
		{Role: "user", Content: client.NewTextContent("original question")},
		{Role: "assistant", Content: client.NewTextContent("original answer")},
	}
	if err := mgr.Save(); err != nil {
		t.Fatal(err)
	}

	server := NewServer(0, nil, deps, "test")
	events := server.EventBus().Subscribe()
	body := `{"message_index":2,"text":"explain this","history":[{"role":"user","content":"side follow-up"},{"role":"assistant","content":"side response"}]}`
	req := httptest.NewRequest(http.MethodPost, "/sessions/side-chat-source/side-chat", bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	server.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
	select {
	case event := <-events:
		t.Fatalf("side chat published global event %q", event.Type)
	default:
	}

	requests := gateway.requests()
	if len(requests) == 0 {
		t.Fatal("gateway captured no side-chat request")
	}
	var transcript strings.Builder
	for _, request := range requests {
		// Side chats expose the normal registry — same capability as the
		// primary conversation, only the lifecycle is ephemeral.
		if len(request.Messages) == 0 {
			continue // tool-less internal probe call, not a model turn
		}
		found := false
		for _, tool := range request.Tools {
			if tool.Function.Name == "sidechat_probe" {
				found = true
			}
		}
		if !found {
			t.Fatalf("side chat request lost the registered tool: %d tools", len(request.Tools))
		}
		for _, message := range request.Messages {
			transcript.WriteString(message.Content.Text())
			transcript.WriteByte('\n')
		}
	}
	for _, want := range []string{"original question", "original answer", "side follow-up", "side response", "explain this"} {
		if !strings.Contains(transcript.String(), want) {
			t.Fatalf("gateway context omitted %q: %s", want, transcript.String())
		}
	}
	summaries, err := mgr.List()
	if err != nil {
		t.Fatal(err)
	}
	if len(summaries) != 1 || summaries[0].ID != source.ID {
		t.Fatalf("side chat persisted an extra session: %#v", summaries)
	}
}

// TestSideChatEndpointStreamsSSE covers the transport Desktop actually uses:
// Accept: text/event-stream routes through handleMessageSSE, and the ephemeral
// guarantees (no extra session, no global events) must hold there too.
func TestSideChatEndpointStreamsSSE(t *testing.T) {
	gateway := &fakeGatewayBackend{reply: "streamed answer"}
	gatewayServer := httptest.NewServer(gateway.handler())
	defer gatewayServer.Close()

	deps := runAgentContractTestDeps(t, gatewayServer.URL)
	defer deps.SessionCache.CloseAll()
	mgr := deps.SessionCache.GetOrCreateManager(deps.SessionCache.SessionsDir(""))
	source := mgr.NewSessionWithID("side-chat-sse-source")
	source.Messages = []client.Message{
		{Role: "user", Content: client.NewTextContent("original question")},
		{Role: "assistant", Content: client.NewTextContent("original answer")},
	}
	if err := mgr.Save(); err != nil {
		t.Fatal(err)
	}

	server := NewServer(0, nil, deps, "test")
	events := server.EventBus().Subscribe()
	body := `{"message_index":2,"text":"explain this"}`
	req := httptest.NewRequest(http.MethodPost, "/sessions/side-chat-sse-source/side-chat", bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "text/event-stream")
	rec := httptest.NewRecorder()
	server.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
	if ct := rec.Header().Get("Content-Type"); !strings.Contains(ct, "text/event-stream") {
		t.Fatalf("content-type = %q", ct)
	}
	stream := rec.Body.String()
	if !strings.Contains(stream, "event: done") || !strings.Contains(stream, "streamed answer") {
		t.Fatalf("SSE stream missing done frame: %s", stream)
	}
	select {
	case event := <-events:
		t.Fatalf("side chat SSE published global event %q", event.Type)
	default:
	}
	summaries, err := mgr.List()
	if err != nil {
		t.Fatal(err)
	}
	if len(summaries) != 1 || summaries[0].ID != source.ID {
		t.Fatalf("side chat SSE persisted an extra session: %#v", summaries)
	}
}

// TestSideChatUsesCompactionCheckpointHistory pins that a compacted source
// session feeds the side chat its checkpoint+tail model view, not the full
// raw archive the checkpoint already replaced.
func TestSideChatUsesCompactionCheckpointHistory(t *testing.T) {
	gateway := &fakeGatewayBackend{reply: "focused answer"}
	gatewayServer := httptest.NewServer(gateway.handler())
	defer gatewayServer.Close()

	deps := runAgentContractTestDeps(t, gatewayServer.URL)
	defer deps.SessionCache.CloseAll()
	mgr := deps.SessionCache.GetOrCreateManager(deps.SessionCache.SessionsDir(""))
	source := mgr.NewSessionWithID("side-chat-compacted-source")
	source.Messages = []client.Message{
		{Role: "user", Content: client.NewTextContent("archived question")},
		{Role: "assistant", Content: client.NewTextContent("archived bulky answer")},
		{Role: "user", Content: client.NewTextContent("tail question")},
		{Role: "assistant", Content: client.NewTextContent("tail answer")},
	}
	source.CompactionCheckpoint = &session.CompactionCheckpoint{
		SchemaVersion:       session.CompactionCheckpointSchemaVersion,
		ArchiveThroughIndex: 2,
		Messages: []client.Message{
			{Role: "user", Content: client.NewTextContent("archived question")},
			{Role: "user", Content: client.NewTextContent(ctxwin.CompactionSummaryPrefix + "summary of the archived turn")},
		},
	}
	if err := mgr.Save(); err != nil {
		t.Fatal(err)
	}

	server := NewServer(0, nil, deps, "test")
	body := `{"message_index":4,"text":"explain this"}`
	req := httptest.NewRequest(http.MethodPost, "/sessions/side-chat-compacted-source/side-chat", bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	server.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}

	var transcript strings.Builder
	for _, request := range gateway.requests() {
		for _, message := range request.Messages {
			transcript.WriteString(message.Content.Text())
			transcript.WriteByte('\n')
		}
	}
	if !strings.Contains(transcript.String(), "summary of the archived turn") ||
		!strings.Contains(transcript.String(), "tail question") {
		t.Fatalf("side chat lost checkpoint+tail view: %s", transcript.String())
	}
	if strings.Contains(transcript.String(), "archived bulky answer") {
		t.Fatalf("side chat re-fed the raw archive past the checkpoint: %s", transcript.String())
	}
}

func TestShouldInjectQuestionAskerSkipsEphemeralRuns(t *testing.T) {
	if !shouldInjectQuestionAsker(RunAgentRequest{Source: "desktop"}) {
		t.Fatal("attended desktop run must get an asker")
	}
	// Side chats are ephemeral panels with no question UI: an asker would
	// block for the auto-resolution window then report a phantom decline.
	if shouldInjectQuestionAsker(RunAgentRequest{Source: "desktop", Ephemeral: true}) {
		t.Fatal("ephemeral run must not get an asker")
	}
	if shouldInjectQuestionAsker(RunAgentRequest{Source: "slack"}) {
		t.Fatal("messaging source must not get an asker")
	}
}

// TestSideChatSSEEmitsApprovalRequestAndRunsTool drives the full loop over a
// live HTTP server: the model calls an approval-requiring tool in a side chat,
// the per-request SSE stream must carry the approval frame, POST /approval
// resolves it, and the tool actually runs.
func TestSideChatSSEEmitsApprovalRequestAndRunsTool(t *testing.T) {
	// Streamed SSE done-frames so the loop's streaming path succeeds; the
	// response is chosen by transcript state (tool_result present or not) so
	// retries and probe calls cannot desynchronize a call counter.
	gatewayServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req client.CompletionRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		resp := client.CompletionResponse{Provider: "anthropic", Model: "test-model"}
		hasToolResult := false
		for _, message := range req.Messages {
			for _, block := range message.Content.Blocks() {
				if block.Type == "tool_result" {
					hasToolResult = true
				}
			}
		}
		if hasToolResult {
			resp.FinishReason = "end_turn"
			resp.OutputText = "probe finished"
		} else {
			resp.FinishReason = "tool_use"
			resp.ToolCalls = []client.FunctionCall{{
				ID: "call-probe-1", Name: "sidechat_probe",
				Arguments: json.RawMessage(`{"description":"run the probe"}`),
			}}
		}
		payload, _ := json.Marshal(struct {
			Type string `json:"type"`
			client.CompletionResponse
		}{Type: "done", CompletionResponse: resp})
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = fmt.Fprintf(w, "data: %s\n\ndata: [DONE]\n\n", payload)
		if flusher, ok := w.(http.Flusher); ok {
			flusher.Flush()
		}
	}))
	defer gatewayServer.Close()

	deps := runAgentContractTestDeps(t, gatewayServer.URL)
	defer deps.SessionCache.CloseAll()
	probe := &sideChatProbeTool{}
	deps.Registry.Register(probe)
	deps.BaselineReg.Register(probe)
	mgr := deps.SessionCache.GetOrCreateManager(deps.SessionCache.SessionsDir(""))
	source := mgr.NewSessionWithID("side-chat-approval-source")
	source.Messages = []client.Message{
		{Role: "user", Content: client.NewTextContent("original question")},
		{Role: "assistant", Content: client.NewTextContent("original answer")},
	}
	if err := mgr.Save(); err != nil {
		t.Fatal(err)
	}

	server := NewServer(0, nil, deps, "test")
	httpServer := httptest.NewServer(server.Handler())
	defer httpServer.Close()

	body := `{"message_index":2,"text":"run the probe"}`
	req, err := http.NewRequest(http.MethodPost, httpServer.URL+"/sessions/side-chat-approval-source/side-chat", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "text/event-stream")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}

	sawApproval := false
	sawDone := false
	scanner := bufio.NewScanner(resp.Body)
	scanner.Buffer(make([]byte, 1<<20), 1<<20)
	event := ""
	for scanner.Scan() {
		line := scanner.Text()
		switch {
		case strings.HasPrefix(line, "event: "):
			event = strings.TrimPrefix(line, "event: ")
		case strings.HasPrefix(line, "data: "):
			data := strings.TrimPrefix(line, "data: ")
			switch event {
			case "approval":
				sawApproval = true
				var frame struct {
					RequestID string `json:"request_id"`
					Tool      string `json:"tool"`
				}
				if err := json.Unmarshal([]byte(data), &frame); err != nil || frame.Tool != "sidechat_probe" {
					t.Fatalf("approval frame = %s err=%v", data, err)
				}
				resolve := fmt.Sprintf(`{"request_id":%q,"decision":"allow"}`, frame.RequestID)
				resolveResp, err := http.Post(httpServer.URL+"/approval", "application/json", strings.NewReader(resolve))
				if err != nil {
					t.Fatal(err)
				}
				resolveResp.Body.Close()
				if resolveResp.StatusCode != http.StatusOK {
					t.Fatalf("resolve status = %d", resolveResp.StatusCode)
				}
			case "done":
				sawDone = true
				if !strings.Contains(data, "probe finished") {
					t.Fatalf("done frame = %s", data)
				}
			}
		}
	}
	if !sawApproval || !sawDone {
		t.Fatalf("stream missing frames: approval=%t done=%t", sawApproval, sawDone)
	}
	if probe.calls == 0 {
		t.Fatal("approved probe tool never ran")
	}
}
