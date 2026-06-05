package agent

import (
	"strings"
	"testing"
	"time"
)

func TestDrainAndFormatSystemEvents(t *testing.T) {
	a := &AgentLoop{}

	t.Run("nil drain fn yields empty", func(t *testing.T) {
		if got := a.drainAndFormatSystemEvents(); got != "" {
			t.Fatalf("got %q, want empty", got)
		}
	})

	t.Run("drain fn output is formatted", func(t *testing.T) {
		ts := time.Date(2026, 6, 5, 9, 0, 0, 0, time.UTC)
		a.SetSystemEventDrainFn(func() []SystemEvent {
			return []SystemEvent{{Text: "kicked from #ops", Trusted: false, TS: ts}}
		})
		got := a.drainAndFormatSystemEvents()
		if !strings.Contains(got, "System (untrusted): [09:00:00] kicked from #ops") {
			t.Fatalf("missing formatted line: %q", got)
		}
		if !strings.HasPrefix(got, "<system-reminder>") {
			t.Fatalf("missing wrapper: %q", got)
		}
	})
}
