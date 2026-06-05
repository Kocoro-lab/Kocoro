package daemon

import (
	"strings"
	"testing"
	"time"

	"github.com/Kocoro-lab/ShanClaw/internal/agent"
)

func TestSystemEvent_EnqueueThenNextTurnDrain(t *testing.T) {
	store := NewSystemEventStore(20)
	const rk = "agent:default:slack:C123"
	store.Enqueue(rk, agent.SystemEvent{
		Text:    "reply to #ops FAILED: bot was kicked — the user did not see it",
		Trusted: true,
		TS:      time.Date(2026, 6, 5, 14, 0, 0, 0, time.UTC),
	})

	var loop agent.AgentLoop
	loop.SetSystemEventDrainFn(func() []agent.SystemEvent { return store.Drain(rk) })

	block := agent.FormatSystemEventBlockForTest(&loop)
	if !strings.Contains(block, "System: [14:00:00] reply to #ops FAILED: bot was kicked") {
		t.Fatalf("block missing expected line: %q", block)
	}
	if again := agent.FormatSystemEventBlockForTest(&loop); again != "" {
		t.Fatalf("second drain should be empty, got %q", again)
	}
}
