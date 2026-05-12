package daemon

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/Kocoro-lab/ShanClaw/internal/agent"
)

func TestHandleGetSuggestion_Present(t *testing.T) {
	s := &Server{suggestions: agent.NewSuggestionState()}
	s.suggestions.Set("sess1", "fix the bug", time.Now())

	req := httptest.NewRequest("GET", "/agents/myagent/sessions/sess1/suggestion", nil)
	req.SetPathValue("name", "myagent")
	req.SetPathValue("id", "sess1")
	w := httptest.NewRecorder()
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
	s := &Server{suggestions: agent.NewSuggestionState()}
	req := httptest.NewRequest("GET", "/agents/myagent/sessions/sess1/suggestion", nil)
	req.SetPathValue("name", "myagent")
	req.SetPathValue("id", "sess1")
	w := httptest.NewRecorder()
	s.handleGetSuggestion(w, req)

	if w.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404", w.Code)
	}
}

func TestHandleAcceptSuggestion(t *testing.T) {
	s := &Server{suggestions: agent.NewSuggestionState()}
	s.suggestions.Set("sess1", "fix the bug", time.Now())
	s.suggestions.SetSpeculation("sess1", "fix the bug", "Here is the fix")

	req := httptest.NewRequest("POST", "/agents/myagent/sessions/sess1/suggestion/accept", nil)
	req.SetPathValue("name", "myagent")
	req.SetPathValue("id", "sess1")
	w := httptest.NewRecorder()
	s.handleAcceptSuggestion(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	var got map[string]any
	_ = json.NewDecoder(w.Body).Decode(&got)
	if got["suggestion"] != "fix the bug" {
		t.Errorf("suggestion = %v, want fix the bug", got["suggestion"])
	}
	if got["speculated_response"] != "Here is the fix" {
		t.Errorf("speculated_response = %v", got["speculated_response"])
	}

	// MarkAccepted should have been called
	cur, _ := s.suggestions.Get("sess1")
	if cur.AcceptedAt == nil {
		t.Error("AcceptedAt should be set after /accept")
	}
}
