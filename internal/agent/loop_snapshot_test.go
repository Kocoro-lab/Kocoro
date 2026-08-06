package agent

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Kocoro-lab/ShanClaw/internal/client"
)

// snapshotCapturingTool captures the conversation snapshot on its first Run call.
// It lets us assert that tools observing the current conversation see the raw
// user message — not the assembled user message with stable/volatile scaffolding.
type snapshotCapturingTool struct {
	captured []client.Message
}

func (s *snapshotCapturingTool) Info() ToolInfo {
	return ToolInfo{
		Name:        "capture_snapshot",
		Description: "captures the live conversation snapshot for testing",
		Parameters:  map[string]any{"type": "object", "properties": map[string]any{}},
	}
}

func (s *snapshotCapturingTool) Run(ctx context.Context, args string) (ToolResult, error) {
	if fn := ConversationSnapshotFromContext(ctx); fn != nil {
		s.captured = fn()
	}
	return ToolResult{Content: "captured"}, nil
}

func (s *snapshotCapturingTool) RequiresApproval() bool { return false }
func (s *snapshotCapturingTool) IsReadOnlyCall(string) bool {
	return true
}

// TestAgentLoop_SnapshotStripsScaffolding verifies the fix for issue #24:
// when a tool reads the conversation snapshot during the current turn, the
// first (current) user message must be the raw user input, not the output
// of assembleUserMessage which prepends StableContext, <!-- cache_break -->,
// VolatileContext (date, CWD, memory), etc.
func TestAgentLoop_SnapshotStripsScaffolding(t *testing.T) {
	callCount := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		callCount++
		if callCount == 1 {
			json.NewEncoder(w).Encode(nativeResponse("", "tool_use",
				toolCall("capture_snapshot", `{}`), 10, 5))
		} else {
			json.NewEncoder(w).Encode(nativeResponse("done", "end_turn", nil, 10, 5))
		}
	}))
	defer server.Close()

	gw := client.NewGatewayClient(server.URL, "")
	reg := NewToolRegistry()
	captor := &snapshotCapturingTool{}
	reg.Register(captor)
	// Give the loop a sticky context so StableContext is non-empty — this
	// makes scaffolding leakage easy to detect.
	loop := NewAgentLoop(gw, reg, "medium", "", 5, 2000, 200, nil, nil, nil)
	loop.SetStickyContext("source=test\nchannel=unit-test")

	rawUser := "remind me to water the plants every morning at 8am"

	if _, _, err := loop.Run(context.Background(), rawUser, nil, nil); err != nil {
		t.Fatalf("Run: %v", err)
	}

	if len(captor.captured) == 0 {
		t.Fatal("tool did not capture any conversation snapshot")
	}

	// The current-turn user message is the LAST user message in the snapshot
	// (system + possibly history + our raw user turn).
	var userMsg *client.Message
	for i := len(captor.captured) - 1; i >= 0; i-- {
		if captor.captured[i].Role == "user" {
			m := captor.captured[i]
			userMsg = &m
			break
		}
	}
	if userMsg == nil {
		t.Fatal("no user message in captured snapshot")
	}
	text := userMsg.Content.Text()

	// Must be exactly the raw user text — no scaffolding.
	if text != rawUser {
		t.Errorf("snapshot user message leaked scaffolding.\n  got:  %q\n  want: %q", text, rawUser)
	}

	// Defensive checks for specific scaffolding markers that must be absent.
	for _, needle := range []string{
		"<!-- cache_break -->",
		"source=test",
		"Session Facts",
		"## Current Date",
	} {
		if strings.Contains(text, needle) {
			t.Errorf("snapshot user message contains scaffolding marker %q:\n%s", needle, text)
		}
	}
}

// TestAgentLoop_SnapshotExcludesInjectedMessages verifies that loop-internal
// guardrail / nudge messages (appended with markInjected, e.g. the fabricated
// tool call guardrail) are filtered out of the conversation snapshot. Without
// this filter, schedule_create would persist those internal control messages
// as if they were real user input.
func TestAgentLoop_SnapshotExcludesInjectedMessages(t *testing.T) {
	callCount := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		callCount++
		switch callCount {
		case 1:
			// Model hallucinates a tool call as text — triggers the fabricated
			// tool call guardrail, which appends an injected user-role nudge.
			fabricated := "I called screenshot({\"target\":\"fullscreen\"}).\n\nResult:\nscreenshot saved"
			json.NewEncoder(w).Encode(nativeResponse(fabricated, "end_turn", nil, 10, 5))
		case 2:
			// After the nudge, model calls the snapshot-capturing tool.
			json.NewEncoder(w).Encode(nativeResponse("", "tool_use",
				toolCall("capture_snapshot", `{}`), 10, 5))
		default:
			json.NewEncoder(w).Encode(nativeResponse("done", "end_turn", nil, 10, 5))
		}
	}))
	defer server.Close()

	gw := client.NewGatewayClient(server.URL, "")
	reg := NewToolRegistry()
	captor := &snapshotCapturingTool{}
	reg.Register(captor)
	loop := NewAgentLoop(gw, reg, "medium", "", 10, 2000, 200, nil, nil, nil)

	if _, _, err := loop.Run(context.Background(), "take a screenshot please", nil, nil); err != nil {
		t.Fatalf("Run: %v", err)
	}

	if len(captor.captured) == 0 {
		t.Fatal("tool did not capture any conversation snapshot")
	}

	// The injected nudge text must NOT appear anywhere in the snapshot — tools
	// should only ever see real user/assistant turns, not loop internals.
	const nudgeMarker = "STOP. You wrote out tool calls as text"
	for i, m := range captor.captured {
		text := m.Content.Text()
		if strings.Contains(text, nudgeMarker) {
			t.Errorf("injected guardrail nudge leaked into snapshot at index %d (role=%s):\n%s", i, m.Role, text)
		}
	}
}

// TestAgentLoop_SnapshotDoesNotLeakHistoryContent is a regression test for a
// related but different leak path: history passed INTO loop.Run() (e.g. from
// a resumed daemon session) is copied verbatim into the `messages` slice and
// participates in the snapshot. Callers (daemon runner, TUI) are responsible
// for pre-filtering injected messages out of history via
// session.Session.HistoryForLoop. This test verifies the loop does not
// secretly add them back: whatever the caller puts in history is exactly what
// the snapshot exposes for those positions.
func TestAgentLoop_SnapshotDoesNotLeakHistoryContent(t *testing.T) {
	callCount := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		callCount++
		if callCount == 1 {
			json.NewEncoder(w).Encode(nativeResponse("", "tool_use",
				toolCall("capture_snapshot", `{}`), 10, 5))
			return
		}
		json.NewEncoder(w).Encode(nativeResponse("done", "end_turn", nil, 10, 5))
	}))
	defer server.Close()

	// Prepare a history slice that simulates what HistoryForLoop would return:
	// real conversation only, no prior nudges. The snapshot must contain these
	// messages plus the current-turn raw user input, in order.
	preFilteredHistory := []client.Message{
		{Role: "user", Content: client.NewTextContent("first real question")},
		{Role: "assistant", Content: client.NewTextContent("first real answer")},
	}

	gw := client.NewGatewayClient(server.URL, "")
	reg := NewToolRegistry()
	captor := &snapshotCapturingTool{}
	reg.Register(captor)
	loop := NewAgentLoop(gw, reg, "medium", "", 5, 2000, 200, nil, nil, nil)

	if _, _, err := loop.Run(context.Background(), "second real question", nil, preFilteredHistory); err != nil {
		t.Fatalf("Run: %v", err)
	}

	var userTexts, assistantTexts []string
	for _, m := range captor.captured {
		switch m.Role {
		case "user":
			userTexts = append(userTexts, m.Content.Text())
		case "assistant":
			assistantTexts = append(assistantTexts, m.Content.Text())
		}
	}
	wantUsers := []string{"first real question", "second real question"}
	if len(userTexts) != len(wantUsers) {
		t.Fatalf("snapshot user msgs = %v, want %v", userTexts, wantUsers)
	}
	for i, want := range wantUsers {
		if userTexts[i] != want {
			t.Errorf("user[%d] = %q, want %q", i, userTexts[i], want)
		}
	}
	if len(assistantTexts) != 1 || assistantTexts[0] != "first real answer" {
		t.Errorf("snapshot assistant msgs = %v, want [\"first real answer\"]", assistantTexts)
	}
}
