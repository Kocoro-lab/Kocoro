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

func TestAppendDynamicUserBlocks_SystemEventsOrder(t *testing.T) {
	out := appendDynamicUserBlocks(
		"USER_PAYLOAD",
		"<system-reminder>\nSystem: [00:00:00] x\n</system-reminder>",
		"SKILL_LISTING",
		"LANG_DIRECTIVE",
	)
	idxUser := strings.Index(out, "USER_PAYLOAD")
	idxSys := strings.Index(out, "system-reminder")
	idxSkill := strings.Index(out, "SKILL_LISTING")
	idxLang := strings.Index(out, "LANG_DIRECTIVE")
	if !(idxUser < idxSys && idxSys < idxSkill && idxSkill < idxLang) {
		t.Fatalf("bad order: user=%d sys=%d skill=%d lang=%d in %q", idxUser, idxSys, idxSkill, idxLang, out)
	}
}

func TestAppendDynamicUserBlocks_EmptySystemEvents(t *testing.T) {
	out := appendDynamicUserBlocks("U", "", "S", "L")
	if strings.Contains(out, "system-reminder") {
		t.Fatalf("empty system-events should add nothing: %q", out)
	}
}
