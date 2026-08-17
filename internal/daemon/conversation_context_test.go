package daemon

import (
	"bytes"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

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

func TestSideChatEndpointUsesReadOnlyEphemeralContext(t *testing.T) {
	gateway := &fakeGatewayBackend{reply: "focused answer"}
	gatewayServer := httptest.NewServer(gateway.handler())
	defer gatewayServer.Close()

	deps := runAgentContractTestDeps(t, gatewayServer.URL)
	defer deps.SessionCache.CloseAll()
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
		if len(request.Tools) != 0 {
			t.Fatalf("side chat exposed %d tools", len(request.Tools))
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
