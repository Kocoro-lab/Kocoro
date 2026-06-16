package daemon

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"context"

	"github.com/Kocoro-lab/ShanClaw/internal/agent"
	"github.com/Kocoro-lab/ShanClaw/internal/client"
)

// TestInjectAtEndTurn_RealDrain_ContentReachesNextCall is the same end_turn
// inject scenario as the agent-package loop test, but wired to the REAL
// SessionCache.InjectMessage (locked send) and the REAL
// SessionCache.DrainSurvivorsOrCloseInject (with its entry.injectCh-nil early
// return and window-close branches). If the agent test passes but this one
// fails, the regression lives in the router-backed drain path, not the loop.
func TestInjectAtEndTurn_RealDrain_ContentReachesNextCall(t *testing.T) {
	dir := t.TempDir()
	sc := NewSessionCache(dir)
	const key = "default:slack:thread-x"
	injectCh := make(chan agent.InjectedMessage, 10)
	sc.mu.Lock()
	sc.routes[key] = &routeEntry{injectCh: injectCh, done: make(chan struct{})}
	sc.mu.Unlock()

	var mu sync.Mutex
	var bodies []string
	callCount := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		mu.Lock()
		callCount++
		n := callCount
		bodies = append(bodies, string(b))
		mu.Unlock()
		if n == 1 {
			// IM follow-up arrives mid-run via the REAL locked InjectMessage.
			if res := sc.InjectMessage(key, agent.InjectedMessage{Text: "看看有什么图片", ClientMessageID: "im-1"}); res != InjectOK {
				t.Errorf("InjectMessage = %v, want InjectOK", res)
			}
		}
		resp := client.CompletionResponse{
			Model:        "test-model",
			OutputText:   "done",
			FinishReason: "end_turn",
			Usage:        client.Usage{InputTokens: 10, OutputTokens: 5, TotalTokens: 15},
			RequestID:    "r",
		}
		_ = json.NewEncoder(w).Encode(resp)
	}))
	defer server.Close()

	gw := client.NewGatewayClient(server.URL, "")
	loop := agent.NewAgentLoop(gw, agent.NewToolRegistry(), "medium", "", 10, 2000, 200, nil, nil, nil)
	loop.SetInjectCh(injectCh)
	loop.SetInjectFinalDrainFn(func() []agent.InjectedMessage {
		return sc.DrainSurvivorsOrCloseInject(key, true)
	})

	if _, _, err := loop.Run(context.Background(), "hello", nil, nil); err != nil {
		t.Fatalf("Run: %v", err)
	}

	mu.Lock()
	defer mu.Unlock()
	t.Logf("LLM call count = %d", callCount)
	if callCount < 2 {
		t.Fatalf("expected >=2 LLM calls (inject must continue the turn), got %d", callCount)
	}
	if !strings.Contains(bodies[1], "看看有什么图片") {
		t.Fatalf("REPRO with REAL drain: 2nd LLM call is MISSING injected content.\n--- body2 ---\n%s", bodies[1])
	}
	t.Logf("OK: injected content reached the 2nd LLM call (real drain)")
}
