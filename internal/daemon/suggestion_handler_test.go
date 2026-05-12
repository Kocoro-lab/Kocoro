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
	s := newSuggestionTestServer(t)
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
	cur, _ := s.suggestions.Get("sess1")
	if cur.AcceptedAt == nil {
		t.Error("AcceptedAt should be set after /accept")
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
