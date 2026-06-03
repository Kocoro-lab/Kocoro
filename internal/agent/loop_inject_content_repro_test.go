package agent

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/Kocoro-lab/ShanClaw/internal/client"
)

// TestAgentLoop_InjectAtEndTurn_ContentReachesNextCall reproduces the IM-burst
// regression the user hit: a follow-up that arrives at the end_turn boundary is
// committed (appears in the transcript) but its CONTENT does not reach the next
// LLM call — so the model replies "your message had no content."
//
// It records every LLM request body. Call 1 returns end_turn and, while doing
// so, a follow-up lands in injectCh (mirrors an IM message arriving mid-run).
// The end_turn drain-race guard should drain+commit it and continue, so call 2's
// request MUST contain the injected text. The finalDrainFn here mimics the
// daemon's router-backed DrainSurvivorsOrCloseInject (drain + return survivors).
func TestAgentLoop_InjectAtEndTurn_ContentReachesNextCall(t *testing.T) {
	injectCh := make(chan InjectedMessage, 10)
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
			// IM follow-up arrives mid-run, AFTER iter-0's top-of-loop drain,
			// while the model composes its end_turn reply.
			injectCh <- InjectedMessage{Text: "看看有什么图片", ClientMessageID: "im-1"}
		}
		_ = json.NewEncoder(w).Encode(nativeResponse("done", "end_turn", nil, 10, 5))
	}))
	defer server.Close()

	gw := client.NewGatewayClient(server.URL, "")
	loop := NewAgentLoop(gw, NewToolRegistry(), "medium", "", 10, 2000, 200, nil, nil, nil)
	loop.SetInjectCh(injectCh)
	// Mimic the daemon's DrainSurvivorsOrCloseInject: drain everything buffered
	// and return it (retraction-filter omitted — no tombstones in this test).
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
	t.Logf("LLM call count = %d", callCount)
	if callCount < 2 {
		t.Fatalf("expected >=2 LLM calls (inject must continue the turn), got %d", callCount)
	}
	if !strings.Contains(bodies[1], "看看有什么图片") {
		t.Fatalf("REGRESSION REPRO: 2nd LLM call request is MISSING the injected content.\n--- body2 ---\n%s", bodies[1])
	}
	t.Logf("OK: injected content reached the 2nd LLM call")
}
