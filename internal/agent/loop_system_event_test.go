package agent

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Kocoro-lab/ShanClaw/internal/client"
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

// TestSystemEvents_NotPersisted is the definition-of-done gate: an S0 system
// event drained onto the scaffolded first user turn must NOT survive into the
// persisted run messages. The existing first-turn scaffold strip
// (captureRunMessages restoring rawUserMessage, see loop.go ~L2265) already
// removes ALL scaffold framing on the first user message, so the injected
// `System:` line and its `<system-reminder>` wrapper disappear with no
// S0-specific strip code.
func TestSystemEvents_NotPersisted(t *testing.T) {
	ts := time.Date(2026, 6, 5, 9, 0, 0, 0, time.UTC)

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(nativeResponse("Sunny.", "end_turn", nil, 10, 5))
	}))
	defer server.Close()

	gw := client.NewGatewayClient(server.URL, "")
	reg := NewToolRegistry()
	loop := NewAgentLoop(gw, reg, "medium", "", 25, 2000, 200, nil, nil, nil)
	loop.SetSystemEventDrainFn(func() []SystemEvent {
		return []SystemEvent{{Text: "reply FAILED: bot was kicked", Trusted: false, TS: ts}}
	})

	if _, _, err := loop.Run(context.Background(), "what is the weather?", nil, nil); err != nil {
		t.Fatalf("Run: %v", err)
	}

	msgs := loop.SanitizedRunMessages()
	for _, m := range msgs {
		txt := m.Content.Text()
		if strings.Contains(txt, "reply FAILED: bot was kicked") {
			t.Fatalf("S0 line leaked into persisted run messages: %q", txt)
		}
		if strings.Contains(txt, "system-reminder") {
			t.Fatalf("system-reminder wrapper leaked into persisted run messages: %q", txt)
		}
	}
	if len(msgs) == 0 {
		t.Fatalf("expected at least the user message to be persisted, got none")
	}
	if msgs[0].Content.Text() != "what is the weather?" {
		t.Fatalf("first user message should be raw user text, got %q", msgs[0].Content.Text())
	}
}
