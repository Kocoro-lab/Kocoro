package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/Kocoro-lab/ShanClaw/internal/client"
	"github.com/Kocoro-lab/ShanClaw/internal/skills"
)

// TestAgentLoop_SteerNotBuriedByDiscoveryHint reproduces the steering-injection
// regression: a follow-up committed by the end_turn drain-race guard is buried
// behind the skill-discovery <system-reminder>.
//
// Sequence: the model finishes its iter-0 "done" reply; a non-retracted steer
// lands in injectCh during composition; the end_turn guard commits it and
// continues. The continued iteration re-arms discovery (latestUserText changed
// to the steer text) and, on the i>=1 path, the hint was appended as a SEPARATE
// user message AFTER the steer — leaving a content-free reminder as the last
// user turn, so the model answers "your message came through empty" instead of
// the steer.
//
// The fix merges the hint into the trailing user turn, keeping the steer as the
// last thing the model reads. This test asserts the final LLM request's last
// user message still contains the steer text.
func TestAgentLoop_SteerNotBuriedByDiscoveryHint(t *testing.T) {
	const steerText = "STEER_use_the_bare_domain_not_www"
	const steerID = "local-steer-keep"
	const hintSkill = "skill-00"

	injectCh := make(chan InjectedMessage, 10)
	var mu sync.Mutex
	mainCalls := 0
	var lastMainMessages []client.Message

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req client.CompletionRequest
		_ = json.NewDecoder(r.Body).Decode(&req)

		// Skill-discovery calls (small tier) return a loaded skill name so the
		// hint is non-empty and the ordering bug can manifest.
		if req.ModelTier == "small" {
			_ = json.NewEncoder(w).Encode(nativeResponse(hintSkill, "end_turn", nil, 5, 2))
			return
		}

		// Main turn (medium tier).
		mu.Lock()
		mainCalls++
		n := mainCalls
		lastMainMessages = req.Messages
		mu.Unlock()

		if n == 1 {
			// User enqueues a non-retracted follow-up while the model composes
			// its iter-0 end_turn reply. It lands in injectCh AFTER iter-0's
			// top-of-loop drain, so only the end_turn drain-race guard can
			// pick it up.
			injectCh <- InjectedMessage{ClientMessageID: steerID, Text: steerText}
		}
		_ = json.NewEncoder(w).Encode(nativeResponse("done", "end_turn", nil, 10, 5))
	}))
	defer server.Close()

	// Comfortably above the loop's local discoveryThreshold (10) so first-turn
	// discovery is gated on. Non-policy descriptions keep the pre-filter out, so
	// the hint comes solely from the (mocked) small-model match.
	const numSkills = 12
	loaded := make([]*skills.Skill, 0, numSkills)
	for i := 0; i < numSkills; i++ {
		loaded = append(loaded, &skills.Skill{
			Name:        fmt.Sprintf("skill-%02d", i),
			Description: fmt.Sprintf("generic helper number %d", i),
		})
	}

	gw := client.NewGatewayClient(server.URL, "")
	reg := NewToolRegistry()
	loop := NewAgentLoop(gw, reg, "medium", "", 10, 2000, 200, nil, nil, nil)
	loop.SetSkills(loaded)
	loop.SetSkillDiscovery(true)
	loop.SetInjectCh(injectCh)
	loop.SetHandler(&mockHandler{})

	if _, _, err := loop.Run(context.Background(), "do work", nil, nil); err != nil {
		t.Fatalf("Run: %v", err)
	}

	mu.Lock()
	calls := mainCalls
	msgs := lastMainMessages
	mu.Unlock()

	// Sanity: the guard must have continued into a 2nd main turn, else the
	// steer never went through the drain-race guard and the repro is invalid.
	if calls < 2 {
		t.Fatalf("expected >=2 main LLM calls (steer continued via end_turn guard), got %d", calls)
	}

	lastUser := lastUserMessage(msgs)
	if lastUser == nil {
		t.Fatalf("no user message in final LLM request (%d messages)", len(msgs))
	}
	text := lastUser.Content.Text()
	if !strings.Contains(text, steerText) {
		t.Errorf("steer buried: final request's last user message does not contain the steer.\n  last user message = %q", text)
	}
	// The discovery hint must still be delivered (merged in), not dropped.
	if !strings.Contains(text, "<system-reminder>") {
		t.Errorf("discovery hint should still ride along in the last user message, got %q", text)
	}
}

func lastUserMessage(msgs []client.Message) *client.Message {
	for i := len(msgs) - 1; i >= 0; i-- {
		if msgs[i].Role == "user" {
			return &msgs[i]
		}
	}
	return nil
}

// TestMergeOrAppendUserInjection covers the helper directly: merge into a plain
// trailing user turn (a steer), but append after tool results — which are also
// role "user" (they carry tool_result blocks), so a bare role check would
// wrongly splice the hint into a tool_result message.
func TestMergeOrAppendUserInjection(t *testing.T) {
	const hint = "<system-reminder>relevant skills</system-reminder>"

	t.Run("merge into plain trailing user turn", func(t *testing.T) {
		msgs := []client.Message{
			{Role: "assistant", Content: client.NewTextContent("done")},
			{Role: "user", Content: client.NewTextContent("the steer")},
		}
		out, merged := mergeOrAppendUserInjection(msgs, hint)
		if !merged {
			t.Fatal("expected merged=true for a plain trailing user turn")
		}
		if len(out) != 2 {
			t.Fatalf("merge must not add a message, got len %d", len(out))
		}
		got := out[len(out)-1].Content.Text()
		if !strings.Contains(got, "the steer") || !strings.Contains(got, hint) {
			t.Errorf("merged text must contain both steer and hint, got %q", got)
		}
	})

	t.Run("append after tool-result turn", func(t *testing.T) {
		toolResult := client.NewBlockContent([]client.ContentBlock{
			{Type: "tool_result", ToolUseID: "toolu_x"},
		})
		msgs := []client.Message{
			{Role: "user", Content: client.NewTextContent("orig")},
			{Role: "assistant", Content: client.NewTextContent("calling tool")},
			{Role: "user", Content: toolResult},
		}
		out, merged := mergeOrAppendUserInjection(msgs, hint)
		if merged {
			t.Fatal("expected merged=false when the trailing user turn is tool results")
		}
		if len(out) != 4 {
			t.Fatalf("append must add exactly one message, got len %d", len(out))
		}
		if out[len(out)-1].Content.Text() != hint {
			t.Errorf("appended message should be the bare hint, got %q", out[len(out)-1].Content.Text())
		}
	})

	t.Run("append when no messages", func(t *testing.T) {
		out, merged := mergeOrAppendUserInjection(nil, hint)
		if merged {
			t.Fatal("expected merged=false for empty history")
		}
		if len(out) != 1 || out[0].Content.Text() != hint {
			t.Errorf("expected a single appended hint message, got %v", out)
		}
	})
}
