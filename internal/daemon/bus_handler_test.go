package daemon

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/Kocoro-lab/ShanClaw/internal/agent"
)

// Sanity check that busEventHandler satisfies both interfaces the agent loop
// cares about. A compile-time assertion would work too, but a runtime check
// plays nicely with the rest of the test file we're about to grow.
func TestBusEventHandlerImplementsInterfaces(t *testing.T) {
	var _ agent.EventHandler = (*busEventHandler)(nil)
	var _ agent.RunStatusHandler = (*busEventHandler)(nil)
}

func TestBusEventHandlerSetSessionID(t *testing.T) {
	h := &busEventHandler{}
	h.SetSessionID("sess_123")
	if h.sessionID != "sess_123" {
		t.Fatalf("sessionID = %q, want %q", h.sessionID, "sess_123")
	}
}

// newTestHandler returns a handler attached to a fresh bus; the caller uses
// bus.Subscribe() to drain events. Shared by the remaining tests in this file.
func newTestHandler(t *testing.T) (*busEventHandler, *EventBus) {
	t.Helper()
	bus := NewEventBus()
	deps := &ServerDeps{EventBus: bus}
	h := &busEventHandler{deps: deps, agent: "coding"}
	h.SetSessionID("sess_test")
	return h, bus
}

// drain reads up to `want` events from the bus subscription within 250ms;
// returns whatever it got. Callers assert length + contents.
func drain(t *testing.T, ch <-chan Event, want int) []Event {
	t.Helper()
	out := make([]Event, 0, want)
	deadline := time.After(250 * time.Millisecond)
	for len(out) < want {
		select {
		case evt := <-ch:
			out = append(out, evt)
		case <-deadline:
			return out
		}
	}
	return out
}

func TestBusEventHandlerOnToolCallEmitsRunning(t *testing.T) {
	h, bus := newTestHandler(t)
	ch := bus.Subscribe()
	defer bus.Unsubscribe(ch)

	h.OnToolCall("bash", "ls -la /tmp", "")

	got := drain(t, ch, 1)
	if len(got) != 1 {
		t.Fatalf("got %d events, want 1", len(got))
	}
	evt := got[0]
	if evt.Type != EventToolStatus {
		t.Fatalf("event type = %q, want %q", evt.Type, EventToolStatus)
	}

	var p struct {
		Tool      string `json:"tool"`
		Status    string `json:"status"`
		Args      string `json:"args"`
		SessionID string `json:"session_id"`
		TS        string `json:"ts"`
	}
	if err := json.Unmarshal(evt.Payload, &p); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if p.Tool != "bash" || p.Status != "running" {
		t.Fatalf("bad tool/status: %+v", p)
	}
	if p.Args != "ls -la /tmp" {
		t.Fatalf("args = %q, want unchanged", p.Args)
	}
	if p.SessionID != "sess_test" {
		t.Fatalf("session_id = %q", p.SessionID)
	}
	if p.TS == "" {
		t.Fatalf("ts missing")
	}
}

func TestBusEventHandlerOnToolCallRedactsAndTruncatesArgs(t *testing.T) {
	h, bus := newTestHandler(t)
	ch := bus.Subscribe()
	defer bus.Unsubscribe(ch)

	long := "curl -H 'Authorization: Bearer sk-secretvalue1234567890' https://api.example.com/" + strings.Repeat("x", 500)
	h.OnToolCall("bash", long, "")

	got := drain(t, ch, 1)
	if len(got) != 1 {
		t.Fatalf("want 1 event")
	}
	var p struct {
		Args string `json:"args"`
	}
	_ = json.Unmarshal(got[0].Payload, &p)

	if strings.Contains(p.Args, "sk-secretvalue") {
		t.Fatalf("secret leaked: %q", p.Args)
	}
	if !strings.Contains(p.Args, "[REDACTED]") {
		t.Fatalf("expected [REDACTED] marker, got %q", p.Args)
	}
	if len(p.Args) > 200 {
		t.Fatalf("args len = %d, want ≤ 200", len(p.Args))
	}
}

func TestBusEventHandlerOnToolResultEmitsCompleted(t *testing.T) {
	h, bus := newTestHandler(t)
	ch := bus.Subscribe()
	defer bus.Unsubscribe(ch)

	result := agent.ToolResult{
		Content: "total 12\ndrwxr-x...",
		IsError: false,
	}
	h.OnToolResult("bash", "ls", "", result, 1234*time.Millisecond)

	got := drain(t, ch, 1)
	if len(got) != 1 {
		t.Fatalf("want 1 event")
	}

	var p struct {
		Tool      string  `json:"tool"`
		Status    string  `json:"status"`
		Elapsed   float64 `json:"elapsed"`
		IsError   bool    `json:"is_error"`
		Preview   string  `json:"preview"`
		SessionID string  `json:"session_id"`
	}
	_ = json.Unmarshal(got[0].Payload, &p)
	if p.Status != "completed" {
		t.Fatalf("status = %q, want completed", p.Status)
	}
	if p.Elapsed != 1.234 {
		t.Fatalf("elapsed = %v, want 1.234", p.Elapsed)
	}
	if p.IsError {
		t.Fatalf("is_error = true, want false")
	}
	if !strings.HasPrefix(p.Preview, "total 12") {
		t.Fatalf("preview = %q", p.Preview)
	}
}

func TestBusEventHandlerOnToolResultTruncatesPreview(t *testing.T) {
	h, bus := newTestHandler(t)
	ch := bus.Subscribe()
	defer bus.Unsubscribe(ch)

	longText := strings.Repeat("x", 500)
	result := agent.ToolResult{Content: longText}
	h.OnToolResult("bash", "", "", result, 0)

	got := drain(t, ch, 1)
	var p struct {
		Preview string `json:"preview"`
	}
	_ = json.Unmarshal(got[0].Payload, &p)
	if len(p.Preview) > 200 {
		t.Fatalf("preview len = %d, want ≤ 200", len(p.Preview))
	}
}

func TestBusEventHandlerOnToolResultPropagatesIsError(t *testing.T) {
	h, bus := newTestHandler(t)
	ch := bus.Subscribe()
	defer bus.Unsubscribe(ch)

	h.OnToolResult("bash", "", "", agent.ToolResult{
		Content: "command not found",
		IsError: true,
	}, 5*time.Millisecond)

	got := drain(t, ch, 1)
	if len(got) != 1 {
		t.Fatalf("want 1 event, got %d", len(got))
	}
	var p struct {
		IsError bool `json:"is_error"`
	}
	if err := json.Unmarshal(got[0].Payload, &p); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if !p.IsError {
		t.Fatalf("is_error = false, want true")
	}
}

func TestBusEventHandlerOnToolCallRedactsSecretSpanningTruncation(t *testing.T) {
	h, bus := newTestHandler(t)
	ch := bus.Subscribe()
	defer bus.Unsubscribe(ch)

	// AKIA is fixed-length (`AKIA[0-9A-Z]{16}` = exactly 20 chars), unlike
	// `Bearer ...` which is greedy and would match a truncated fragment too.
	// Position it to straddle the 200-byte cap: 185 filler + 20 secret + tail.
	// truncate-then-redact would see only "AKIA" + 15 chars, miss the {16}
	// requirement, and leak the prefix. redact-then-truncate matches the full
	// pattern before truncation, substitutes [REDACTED], and truncates cleanly.
	input := strings.Repeat("a", 185) + "AKIAABCDEFGHIJKLMNOP" + strings.Repeat("z", 100)
	h.OnToolCall("bash", input, "")

	got := drain(t, ch, 1)
	if len(got) != 1 {
		t.Fatalf("want 1 event")
	}
	var p struct {
		Args string `json:"args"`
	}
	_ = json.Unmarshal(got[0].Payload, &p)
	if strings.Contains(p.Args, "AKIAABCDE") {
		t.Fatalf("secret leaked across truncation boundary: %q", p.Args)
	}
	if !strings.Contains(p.Args, "[REDACTED]") {
		t.Fatalf("expected [REDACTED] marker, got %q", p.Args)
	}
	if len(p.Args) > 200 {
		t.Fatalf("args len = %d, want ≤ 200", len(p.Args))
	}
}

func TestBusEventHandlerOnUsageEmitsSnapshot(t *testing.T) {
	h, bus := newTestHandler(t)
	ch := bus.Subscribe()
	defer bus.Unsubscribe(ch)

	u := agent.TurnUsage{
		InputTokens:         1200,
		OutputTokens:        450,
		CostUSD:             0.012,
		LLMCalls:            3,
		Model:               "claude-sonnet-4-6",
		CacheReadTokens:     800,
		CacheCreationTokens: 0,
	}
	h.OnUsage(u)

	got := drain(t, ch, 1)
	if len(got) != 1 {
		t.Fatalf("want 1 event, got %d", len(got))
	}
	if got[0].Type != EventUsage {
		t.Fatalf("type = %q, want %q", got[0].Type, EventUsage)
	}

	var p struct {
		InputTokens      int     `json:"input_tokens"`
		OutputTokens     int     `json:"output_tokens"`
		CacheReadTokens  int     `json:"cache_read_tokens"`
		CacheWriteTokens int     `json:"cache_write_tokens"`
		CostUSD          float64 `json:"cost_usd"`
		LLMCalls         int     `json:"llm_calls"`
		Model            string  `json:"model"`
		SessionID        string  `json:"session_id"`
		TS               string  `json:"ts"`
	}
	_ = json.Unmarshal(got[0].Payload, &p)
	if p.InputTokens != 1200 || p.OutputTokens != 450 {
		t.Fatalf("tokens = %+v", p)
	}
	if p.CacheReadTokens != 800 || p.CacheWriteTokens != 0 {
		t.Fatalf("cache tokens = %+v", p)
	}
	if p.LLMCalls != 3 {
		t.Fatalf("llm_calls = %d, want 3", p.LLMCalls)
	}
	if p.Model != "claude-sonnet-4-6" {
		t.Fatalf("model = %q", p.Model)
	}
	if p.SessionID != "sess_test" {
		t.Fatalf("session_id = %q", p.SessionID)
	}
	if p.CostUSD != 0.012 {
		t.Fatalf("cost_usd = %v, want 0.012", p.CostUSD)
	}
	if p.TS == "" {
		t.Fatalf("ts missing")
	}
}

func TestBusEventHandlerOnCloudAgent(t *testing.T) {
	h, bus := newTestHandler(t)
	ch := bus.Subscribe()
	defer bus.Unsubscribe(ch)

	h.OnCloudAgent("research_a", "running", "searching papers")

	got := drain(t, ch, 1)
	if len(got) != 1 || got[0].Type != EventCloudAgent {
		t.Fatalf("events = %+v", got)
	}
	var p struct {
		AgentID   string `json:"agent_id"`
		Status    string `json:"status"`
		Message   string `json:"message"`
		SessionID string `json:"session_id"`
	}
	_ = json.Unmarshal(got[0].Payload, &p)
	if p.AgentID != "research_a" {
		t.Fatalf("agent_id = %q", p.AgentID)
	}
	if p.Status != "running" {
		t.Fatalf("status = %q", p.Status)
	}
	if p.Message != "searching papers" {
		t.Fatalf("message = %q", p.Message)
	}
	if p.SessionID != "sess_test" {
		t.Fatalf("session_id = %q", p.SessionID)
	}
}

func TestBusEventHandlerOnCloudProgress(t *testing.T) {
	h, bus := newTestHandler(t)
	ch := bus.Subscribe()
	defer bus.Unsubscribe(ch)

	h.OnCloudProgress(3, 7)

	got := drain(t, ch, 1)
	if len(got) != 1 || got[0].Type != EventCloudProgress {
		t.Fatalf("events = %+v", got)
	}
	var p struct {
		Completed int    `json:"completed"`
		Total     int    `json:"total"`
		SessionID string `json:"session_id"`
	}
	_ = json.Unmarshal(got[0].Payload, &p)
	if p.Completed != 3 {
		t.Fatalf("completed = %d, want 3", p.Completed)
	}
	if p.Total != 7 {
		t.Fatalf("total = %d, want 7", p.Total)
	}
	if p.SessionID != "sess_test" {
		t.Fatalf("session_id = %q", p.SessionID)
	}
}

func TestBusEventHandlerOnCloudPlanTruncatesContent(t *testing.T) {
	h, bus := newTestHandler(t)
	ch := bus.Subscribe()
	defer bus.Unsubscribe(ch)

	long := strings.Repeat("x", 5000) // 5KB input — must be capped near 2KB
	h.OnCloudPlan("research", long, true)

	got := drain(t, ch, 1)
	if len(got) != 1 || got[0].Type != EventCloudPlan {
		t.Fatalf("events = %+v", got)
	}
	var p struct {
		Type        string `json:"type"`
		Content     string `json:"content"`
		NeedsReview bool   `json:"needs_review"`
		SessionID   string `json:"session_id"`
	}
	_ = json.Unmarshal(got[0].Payload, &p)
	if p.Type != "research" {
		t.Fatalf("type = %q, want research", p.Type)
	}
	if !p.NeedsReview {
		t.Fatalf("needs_review = false, want true")
	}
	if p.SessionID != "sess_test" {
		t.Fatalf("session_id = %q", p.SessionID)
	}
	// Body must be truncated: original 5000 bytes → capped at 2048 + "… (truncated)" marker
	if len(p.Content) > 2100 { // 2048 + marker slack
		t.Fatalf("content len = %d, want ≤ ~2100", len(p.Content))
	}
	if !strings.HasSuffix(p.Content, "… (truncated)") {
		t.Fatalf("content does not end with truncation marker: %q", p.Content[len(p.Content)-30:])
	}
}

// Guards the opposite path: content under 2KB must NOT have the truncation marker
// appended. Together with TestBusEventHandlerOnCloudPlanTruncatesContent this
// locks the cap's threshold behavior.
func TestBusEventHandlerOnCloudPlanShortContentNotTruncated(t *testing.T) {
	h, bus := newTestHandler(t)
	ch := bus.Subscribe()
	defer bus.Unsubscribe(ch)

	h.OnCloudPlan("analysis", "short plan body", false)

	got := drain(t, ch, 1)
	if len(got) != 1 {
		t.Fatalf("want 1 event, got %d", len(got))
	}
	var p struct {
		Content     string `json:"content"`
		NeedsReview bool   `json:"needs_review"`
	}
	_ = json.Unmarshal(got[0].Payload, &p)
	if p.Content != "short plan body" {
		t.Fatalf("content = %q, want unchanged", p.Content)
	}
	if p.NeedsReview {
		t.Fatalf("needs_review = true, want false")
	}
}

func TestBusEventHandlerOnRunStatus(t *testing.T) {
	h, bus := newTestHandler(t)
	ch := bus.Subscribe()
	defer bus.Unsubscribe(ch)

	h.OnRunStatus("idle_soft", "no LLM activity for 15s (phase=awaiting_llm)")

	got := drain(t, ch, 1)
	if len(got) != 1 || got[0].Type != EventRunStatus {
		t.Fatalf("events = %+v", got)
	}
	var p struct {
		Code      string `json:"code"`
		Detail    string `json:"detail"`
		SessionID string `json:"session_id"`
		Agent     string `json:"agent"`
	}
	_ = json.Unmarshal(got[0].Payload, &p)
	if p.Code != "idle_soft" {
		t.Fatalf("code = %q", p.Code)
	}
	if !strings.Contains(p.Detail, "no LLM activity") {
		t.Fatalf("detail = %q", p.Detail)
	}
	if p.SessionID != "sess_test" {
		t.Fatalf("session_id = %q", p.SessionID)
	}
	if p.Agent != "coding" {
		t.Fatalf("agent = %q, want 'coding'", p.Agent)
	}
}
