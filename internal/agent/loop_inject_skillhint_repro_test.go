package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/Kocoro-lab/ShanClaw/internal/client"
	"github.com/Kocoro-lab/ShanClaw/internal/skills"
)

// TestAgentLoop_InjectContinuation_SkillHintDoesNotMaskFollowup reproduces the
// IM-burst "your message had no content" regression.
//
// When an injected follow-up is committed at the end_turn boundary and the loop
// continues, the NEXT iteration runs skill discovery. The discovery-hint
// injector (loop.go ~3085) assumes "later turns have tool results as the last
// message" and appends the hint as a SEPARATE user message. But an inject
// continuation leaves the user's actual follow-up as the last message, so the
// hint gets appended AFTER it — two consecutive user messages with a
// content-less <system-reminder> last. The model treats that reminder as the
// current turn and replies "your message had no content," ignoring the
// follow-up. (Same "separate user messages" trap the turn-0 scaffold avoids.)
//
// Assertion: the inject-continuation LLM call's LAST user message must still
// carry the follow-up text. Fails before the fix (last message is the bare
// hint); passes once the hint is embedded into the follow-up instead.
func TestAgentLoop_InjectContinuation_SkillHintDoesNotMaskFollowup(t *testing.T) {
	injectCh := make(chan InjectedMessage, 10)
	var mu sync.Mutex
	var mainBodies []string
	mainCalls := 0

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		var req client.CompletionRequest
		_ = json.Unmarshal(b, &req)
		if req.MaxTokens == 30 { // skill-discovery probe (small tier, 30 tokens)
			_ = json.NewEncoder(w).Encode(nativeResponse("img-finder", "end_turn", nil, 5, 2))
			return
		}
		// main turn
		mu.Lock()
		mainCalls++
		n := mainCalls
		mainBodies = append(mainBodies, string(b))
		mu.Unlock()
		if n == 1 {
			// IM follow-up arrives mid-run while composing the end_turn reply.
			injectCh <- InjectedMessage{Text: "看看有什么图片", ClientMessageID: "im-1"}
		}
		_ = json.NewEncoder(w).Encode(nativeResponse("done", "end_turn", nil, 10, 5))
	}))
	defer server.Close()

	gw := client.NewGatewayClient(server.URL, "")
	loop := NewAgentLoop(gw, NewToolRegistry(), "medium", "", 10, 2000, 200, nil, nil, nil)
	// >=10 skills to arm discovery; include the one discovery will "match".
	sk := make([]*skills.Skill, 0, 12)
	for i := 0; i < 11; i++ {
		sk = append(sk, &skills.Skill{Name: fmt.Sprintf("skill-%d", i), Description: "filler"})
	}
	sk = append(sk, &skills.Skill{Name: "img-finder", Description: "find images on disk"})
	loop.SetSkills(sk)
	loop.SetInjectCh(injectCh)
	loop.SetInjectFinalDrainFn(func() []InjectedMessage {
		var out []InjectedMessage
		for {
			select {
			case m := <-injectCh:
				out = append(out, m)
			default:
				return out
			}
		}
	})

	if _, _, err := loop.Run(context.Background(), "hello", nil, nil); err != nil {
		t.Fatalf("Run: %v", err)
	}

	mu.Lock()
	defer mu.Unlock()
	if mainCalls < 2 {
		t.Fatalf("expected >=2 main LLM calls (inject continuation), got %d", mainCalls)
	}
	var req2 client.CompletionRequest
	if err := json.Unmarshal([]byte(mainBodies[1]), &req2); err != nil {
		t.Fatalf("unmarshal call2: %v", err)
	}
	last := req2.Messages[len(req2.Messages)-1]
	lastText := last.Content.Text()
	t.Logf("call2 last message: role=%s text=%q", last.Role, lastText)
	// (1) Not masked: the follow-up must remain the last user message, so the
	// model responds to it rather than to a content-less reminder turn.
	if last.Role != "user" || !strings.Contains(lastText, "看看有什么图片") {
		t.Fatalf("MASKED: the inject continuation's last user message is not the follow-up.\n"+
			"call2's last user message = %q", lastText)
	}
	// (2) Not leaked: later-turn messages are persisted/displayed verbatim (no
	// scaffold strip like the turn-0 user message), so embedding the
	// <system-reminder> hint here would surface it to the user in Slack. The
	// inject continuation must drop the hint, not embed it.
	if strings.Contains(lastText, "system-reminder") {
		t.Fatalf("LEAK: discovery hint surfaced in the user-visible follow-up: %q", lastText)
	}
	t.Logf("OK: follow-up is the last user message and carries no leaked hint")
}
