package daemon

import (
	"net/http/httptest"
	"strings"
	"testing"
)

// TestSetSessionID_EmitsEarlySSE pins the behavior the Desktop client depends
// on: SetSessionID must emit an SSE `session_started` frame so the consumer
// can bind currentSessionID BEFORE the first delta/tool/done event. The
// regression we're guarding against is the "mid-turn follow-up opens a new
// session" bug — if this event stops firing, Desktop falls back to learning
// the session_id only on `done`, and follow-up messages sent before that
// arrives carry no session_id and trigger fresh-session creation on the
// daemon side.
func TestSetSessionID_EmitsEarlySSE(t *testing.T) {
	rec := httptest.NewRecorder()
	h := &sseEventHandler{
		w:       rec,
		flusher: rec, // httptest.ResponseRecorder implements http.Flusher (no-op flush)
	}
	h.SetSessionID("sess-abc")

	body := rec.Body.String()
	if !strings.Contains(body, "event: session_started") {
		t.Fatalf("expected `event: session_started` in SSE body, got:\n%s", body)
	}
	if !strings.Contains(body, `"session_id":"sess-abc"`) {
		t.Errorf("session_id payload missing, got body:\n%s", body)
	}

	// Calling with an empty id must NOT emit (avoids garbage frames on
	// resolution failure paths).
	rec2 := httptest.NewRecorder()
	h2 := &sseEventHandler{w: rec2, flusher: rec2}
	h2.SetSessionID("")
	if rec2.Body.Len() != 0 {
		t.Errorf("SetSessionID(\"\") should be a no-op, got %q", rec2.Body.String())
	}
}

// TestSetSessionID_NilWriterIsSafe verifies the bus-only handler path
// (non-SSE callers) doesn't panic when SetSessionID fires. The default
// handler used by /message JSON callers leaves w nil — we must early-return.
func TestSetSessionID_NilWriterIsSafe(t *testing.T) {
	h := &sseEventHandler{} // w == nil, flusher == nil
	h.SetSessionID("sess-nil")
	if h.sessionID != "sess-nil" {
		t.Errorf("sessionID field still must be set; got %q", h.sessionID)
	}
}
