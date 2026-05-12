package daemon

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/Kocoro-lab/ShanClaw/internal/agent"
	"github.com/Kocoro-lab/ShanClaw/internal/client"
	"github.com/Kocoro-lab/ShanClaw/internal/session"
)

// newSuggestionTestServer returns a *Server wired with the minimum daemon
// state needed by the suggestion handlers: an in-memory SuggestionState, a
// temp AgentsDir with a single existing agent named "myagent".
func newSuggestionTestServer(t *testing.T) *Server {
	t.Helper()
	agentsDir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(agentsDir, "myagent"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(agentsDir, "myagent", "AGENT.md"), []byte("# myagent\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	return &Server{
		deps:        &ServerDeps{AgentsDir: agentsDir},
		suggestions: agent.NewSuggestionState(),
	}
}

// newSuggestionTestServerWithSession extends newSuggestionTestServer with a
// SessionCache rooted in a separate shannon tmpdir, plus a pre-seeded session
// with the given sessionID under agents/<agentName>/sessions/. Used by Task
// 11.5's accept-with-speculation test which exercises the
// AppendAcceptedSpeculation persistence path.
func newSuggestionTestServerWithSession(t *testing.T, agentName, sessionID string) *Server {
	t.Helper()
	s := newSuggestionTestServer(t)

	shannonDir := t.TempDir()
	sessionsDir := filepath.Join(shannonDir, "agents", agentName, "sessions")
	if err := os.MkdirAll(sessionsDir, 0o755); err != nil {
		t.Fatal(err)
	}

	// Pre-save a session with ID=sessionID so ResumeLatest picks it up.
	seedStore := session.NewStore(sessionsDir)
	if err := seedStore.Save(&session.Session{
		ID: sessionID,
		Messages: []client.Message{
			{Role: "user", Content: client.NewTextContent("hello")},
			{Role: "assistant", Content: client.NewTextContent("hi")},
		},
	}); err != nil {
		t.Fatal(err)
	}
	seedStore.Close()

	s.deps.SessionCache = NewSessionCache(shannonDir)
	return s
}

func newSuggestionRequest(method, agentName, sessionID string) (*http.Request, *httptest.ResponseRecorder) {
	url := "/agents/" + agentName + "/sessions/" + sessionID + "/suggestion"
	if method == http.MethodPost {
		url += "/accept"
	}
	req := httptest.NewRequest(method, url, nil)
	req.SetPathValue("name", agentName)
	req.SetPathValue("id", sessionID)
	return req, httptest.NewRecorder()
}

func TestHandleGetSuggestion_Present(t *testing.T) {
	s := newSuggestionTestServer(t)
	s.suggestions.Set("sess1", "fix the bug", time.Now())

	req, w := newSuggestionRequest(http.MethodGet, "myagent", "sess1")
	s.handleGetSuggestion(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body: %s", w.Code, w.Body.String())
	}
	var got map[string]any
	if err := json.NewDecoder(w.Body).Decode(&got); err != nil {
		t.Fatal(err)
	}
	if got["text"] != "fix the bug" {
		t.Errorf("text = %v, want fix the bug", got["text"])
	}
}

func TestHandleGetSuggestion_Absent(t *testing.T) {
	s := newSuggestionTestServer(t)
	req, w := newSuggestionRequest(http.MethodGet, "myagent", "sess1")
	s.handleGetSuggestion(w, req)

	if w.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404", w.Code)
	}
	// Body shape must be JSON {"error": "..."} like all other daemon errors.
	if ct := w.Header().Get("Content-Type"); ct != "application/json" {
		t.Errorf("Content-Type = %q, want application/json", ct)
	}
}

func TestHandleAcceptSuggestion_WithSpeculation(t *testing.T) {
	// T11.5: when speculation is present, accept must persist the
	// (user=suggestion, assistant=speculated_response) pair to session before
	// returning speculated_response. Use the SessionCache-wired helper so
	// AppendAcceptedSpeculation has a real session to append to.
	s := newSuggestionTestServerWithSession(t, "myagent", "sess1")
	s.suggestions.Set("sess1", "fix the bug", time.Now())
	s.suggestions.SetSpeculation("sess1", "fix the bug", "Here is the fix")

	req, w := newSuggestionRequest(http.MethodPost, "myagent", "sess1")
	s.handleAcceptSuggestion(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body: %s", w.Code, w.Body.String())
	}
	var got map[string]any
	_ = json.NewDecoder(w.Body).Decode(&got)
	if got["suggestion"] != "fix the bug" {
		t.Errorf("suggestion = %v, want fix the bug", got["suggestion"])
	}
	if got["speculated_response"] != "Here is the fix" {
		t.Errorf("speculated_response = %v", got["speculated_response"])
	}
	// MarkAccepted runs even though the entry is then cleared (Get returns the
	// pre-clear snapshot for the user's view; MarkAccepted+Clear mutate state).
	// After Clear, Get returns ok=false — verify the suggestion was consumed.
	if _, ok := s.suggestions.Get("sess1"); ok {
		t.Error("expected suggestion to be cleared after /accept")
	}
	// And the persist call must have appended both messages to the session.
	sessMgr := s.deps.SessionCache.GetOrCreate("myagent")
	got2, err := sessMgr.Load("sess1")
	if err != nil {
		t.Fatalf("reload sess1: %v", err)
	}
	if len(got2.Messages) != 4 {
		t.Fatalf("messages len after persist = %d, want 4 (2 seed + 2 appended)", len(got2.Messages))
	}
	if got2.Messages[2].Role != "user" || got2.Messages[2].Content.Text() != "fix the bug" {
		t.Errorf("appended user msg = %+v", got2.Messages[2])
	}
	if got2.Messages[3].Role != "assistant" || got2.Messages[3].Content.Text() != "Here is the fix" {
		t.Errorf("appended assistant msg = %+v", got2.Messages[3])
	}
}

func TestHandleAcceptSuggestion_WithoutSpeculation(t *testing.T) {
	// When no speculation has completed, the accept response still 200s but
	// omits speculated_response (omitempty). Desktop interprets missing field
	// as "no speculation — fall back to normal send flow".
	s := newSuggestionTestServer(t)
	s.suggestions.Set("sess1", "fix the bug", time.Now())

	req, w := newSuggestionRequest(http.MethodPost, "myagent", "sess1")
	s.handleAcceptSuggestion(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	var got map[string]any
	_ = json.NewDecoder(w.Body).Decode(&got)
	if _, hasField := got["speculated_response"]; hasField {
		t.Errorf("expected speculated_response to be omitted, got body: %s", w.Body.String())
	}
	if got["text"] != "fix the bug" {
		t.Errorf("text = %v, want fix the bug", got["text"])
	}
}

func TestHandleAcceptSuggestion_Absent(t *testing.T) {
	s := newSuggestionTestServer(t)
	req, w := newSuggestionRequest(http.MethodPost, "myagent", "sess1")
	s.handleAcceptSuggestion(w, req)
	if w.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404", w.Code)
	}
}

func TestHandleSuggestion_MissingSessionID(t *testing.T) {
	s := newSuggestionTestServer(t)
	req, w := newSuggestionRequest(http.MethodGet, "myagent", "")
	s.handleGetSuggestion(w, req)
	if w.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400 for empty session id", w.Code)
	}
}

func TestHandleSuggestion_PathTraversalSessionID(t *testing.T) {
	s := newSuggestionTestServer(t)
	req, w := newSuggestionRequest(http.MethodGet, "myagent", "../escape")
	s.handleGetSuggestion(w, req)
	if w.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400 for path-traversal session id", w.Code)
	}
}

func TestHandleSuggestion_UnknownAgent(t *testing.T) {
	s := newSuggestionTestServer(t) // only "myagent" exists in AgentsDir
	req, w := newSuggestionRequest(http.MethodGet, "nonexistent", "sess1")
	s.handleGetSuggestion(w, req)
	if w.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404 for unknown agent", w.Code)
	}
}

func TestHandleSuggestion_InvalidAgentName(t *testing.T) {
	// ValidateAgentName fires before agentExists — no deps needed.
	// Regex is ^[a-z0-9][a-z0-9_-]{0,63}$ so an uppercase leading char rejects.
	s := &Server{suggestions: agent.NewSuggestionState()}
	req, w := newSuggestionRequest(http.MethodGet, "BadName", "sess1")
	s.handleGetSuggestion(w, req)
	if w.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400 for invalid agent name", w.Code)
	}
}

func TestSuggestionState_ClearedOnNewTurn(t *testing.T) {
	// Unit-level proof of the contract: Clear() removes the entry, Get()
	// returns false afterwards. Integration-style proof of "RunAgent clears
	// before next turn" happens in the E2E test (Task 16).
	state := agent.NewSuggestionState()
	state.Set("sess1", "old suggestion", time.Now())

	state.Clear("sess1")
	if _, ok := state.Get("sess1"); ok {
		t.Error("expected suggestion to be cleared on new turn")
	}
}
