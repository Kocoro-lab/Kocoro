package daemon

import (
	"bytes"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/Kocoro-lab/ShanClaw/internal/client"
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

func TestForkSessionEndpointCanTargetAnotherAgent(t *testing.T) {
	shannonDir := t.TempDir()
	deps := &ServerDeps{ShannonDir: shannonDir, SessionCache: NewSessionCache(shannonDir)}
	server := NewServer(0, nil, deps, "test")
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
