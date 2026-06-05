package daemon

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func TestStickyContext_AlwaysIncludesAgent(t *testing.T) {
	// Default agent (empty name) must STILL surface its identity in sticky
	// context — otherwise the LLM can't reason about "I am the agent Cloud
	// routed this message to".
	got := buildStickyContext("slack", "C0XXX", "yohei", "", "", "", nil, "")
	if !strings.Contains(got, "Agent: default") {
		t.Errorf("default agent must surface as 'Agent: default'; got:\n%s", got)
	}

	got = buildStickyContext("slack", "C0XXX", "yohei", "researcher", "", "", nil, "")
	if !strings.Contains(got, "Agent: researcher") {
		t.Errorf("named agent must surface as 'Agent: researcher'; got:\n%s", got)
	}
}

// TestStickyContext_ScheduleAddsOutputDiscipline locks that schedule-source
// runs get the "output only the deliverable" discipline (so the reply doesn't
// leak the delivery mechanism / session internals), while interactive/IM
// sources stay byte-stable without it.
func TestStickyContext_ScheduleAddsOutputDiscipline(t *testing.T) {
	got := buildStickyContext(ChannelSchedule, ChannelSchedule+"-abc123", "scheduler", "academic-writer", "", "", nil, "")
	if !strings.Contains(got, "scheduled task") || !strings.Contains(got, "Output ONLY the user-facing message") {
		t.Errorf("schedule-source sticky context missing output-discipline clause; got:\n%s", got)
	}

	// Non-schedule sources must NOT pick up the clause (cache byte-stability).
	for _, src := range []string{"slack", "webview", "line", "feishu"} {
		if other := buildStickyContext(src, "C0XXX", "yohei", "researcher", "", "", nil, ""); strings.Contains(other, "scheduled task") {
			t.Errorf("source %q must not contain the schedule output-discipline clause; got:\n%s", src, other)
		}
	}
}

func TestStickyContext_IMBindings(t *testing.T) {
	// Non-empty imBindings → "IM bindings:" line appears between Agent and extra.
	got := buildStickyContext("webview", "", "hu", "", "default=slack:kocoro-test-slack", "", nil, "")
	if !strings.Contains(got, "\nIM bindings: default=slack:kocoro-test-slack") {
		t.Errorf("want IM bindings line surfaced; got:\n%s", got)
	}

	// Empty imBindings → line omitted entirely (model reads absence as
	// "no bindings known" — see the IM channel delivery prompt section).
	got = buildStickyContext("webview", "", "hu", "", "", "", nil, "")
	if strings.Contains(got, "IM bindings") {
		t.Errorf("empty imBindings must NOT emit any IM bindings line; got:\n%s", got)
	}
}

func TestStickyContext_EmptyWhenNoRoutingSignal(t *testing.T) {
	// Pure-local runs (TUI / one-shot CLI) arrive with no source, channel,
	// sender, agentName, bindings, or extra. buildStickyContext must return
	// "" so the runner.go `if sticky != ""` guard short-circuits and these
	// runs stay byte-identical to pre-PR sessions (cache equivalence across
	// the upgrade boundary).
	if got := buildStickyContext("", "", "", "", "", "", nil, ""); got != "" {
		t.Errorf("want empty sticky context when no routing signal; got %q", got)
	}

	// A lone agentName (named-agent local run) is still a signal — the model
	// should know its identity — so it must NOT collapse to empty.
	if got := buildStickyContext("", "", "", "researcher", "", "", nil, ""); got == "" {
		t.Error("named-agent run must still emit sticky context")
	}

	// A lone extra block is a signal too.
	if got := buildStickyContext("", "", "", "", "", "Note: foo", nil, ""); got == "" {
		t.Error("extra-only sticky context must not collapse to empty")
	}
}

func TestStickyContext_OrderAgentBeforeBindings(t *testing.T) {
	// The LLM-facing routing model in the system prompt assumes a stable
	// line order: Source / Channel / Sender / Agent / IM bindings / extra.
	// Out-of-order would force the model to scan; keep alignment with the
	// "## IM channel delivery" section.
	got := buildStickyContext("slack", "C0XXX", "yohei", "analyst", "analyst=feishu:engineering", "", nil, "")
	agentIdx := strings.Index(got, "Agent:")
	bindIdx := strings.Index(got, "IM bindings:")
	if agentIdx < 0 || bindIdx < 0 || agentIdx > bindIdx {
		t.Errorf("Agent must come before IM bindings; got:\n%s", got)
	}
}

func TestBuildStickyContext_WithOriginSlackChannel(t *testing.T) {
	o := &MessageOrigin{Platform: "slack", ChannelID: "C0ABC", ChannelLabel: "#shannon", Scope: "channel", ThreadID: "1234.5678"}
	got := buildStickyContext("slack", "slack", "yohei", "", "", "", o, "")
	if !strings.Contains(got, "Channel: slack · #shannon · channel") {
		t.Fatalf("missing enriched Channel line:\n%s", got)
	}
	if !strings.Contains(got, "Thread: 1234.5678") {
		t.Fatalf("missing Thread line:\n%s", got)
	}
}

func TestBuildStickyContext_NilOriginFallsBackToCoarse(t *testing.T) {
	got := buildStickyContext("feishu", "feishu", "lin", "", "", "", nil, "")
	if !strings.Contains(got, "Channel: feishu") {
		t.Fatalf("expected coarse Channel: feishu fallback:\n%s", got)
	}
	if strings.Contains(got, "·") || strings.Contains(got, "Thread:") {
		t.Fatalf("nil origin must not render enriched/Thread lines:\n%s", got)
	}
}

func TestBuildStickyContext_OriginWithoutThreadOmitsThreadLine(t *testing.T) {
	o := &MessageOrigin{Platform: "wecom", ChannelID: "chat1", Scope: "group"}
	got := buildStickyContext("wecom", "wecom", "li", "", "", "", o, "")
	if strings.Contains(got, "Thread:") {
		t.Fatalf("no ThreadID → no Thread line:\n%s", got)
	}
	if !strings.Contains(got, "Channel: wecom · chat1 · group") {
		t.Fatalf("missing Channel line:\n%s", got)
	}
}

func TestStickyFromRequest_UsesOrigin(t *testing.T) {
	blob := json.RawMessage(`{"platform":"slack","workspace_id":"T1","channel_id":"C0ABC","message_ts":"99.1"}`)
	got := stickyFromRequest("slack", "slack", "yohei", "", "", "", blob, nil)
	if !strings.Contains(got, "Channel: slack · C0ABC · channel") || !strings.Contains(got, "Thread: 99.1") {
		t.Fatalf("runner glue did not apply origin:\n%s", got)
	}
}

func TestBuildStickyContext_WithConnState(t *testing.T) {
	o := &MessageOrigin{Platform: "slack", ChannelID: "C1", Scope: "channel"}
	got := buildStickyContext("slack", "slack", "yo", "", "", "", o, "the bot was removed from this channel and can no longer read or post here until re-added")
	if !strings.Contains(got, "Connection: the bot was removed from this channel") {
		t.Fatalf("missing Connection line:\n%s", got)
	}
}

func TestBuildStickyContext_EmptyConnStateNoLine(t *testing.T) {
	o := &MessageOrigin{Platform: "slack", ChannelID: "C1", Scope: "channel"}
	got := buildStickyContext("slack", "slack", "yo", "", "", "", o, "")
	if strings.Contains(got, "Connection:") {
		t.Fatalf("empty connState must not render a Connection line:\n%s", got)
	}
}

func TestStickyFromRequest_LooksUpConnState(t *testing.T) {
	cache := NewConnectionStateCache()
	cache.Apply(ChannelStateEventPayload{Axis: AxisMembership, Platform: "slack", ChannelID: "C1", Change: ChangeKicked, TS: "x"}, time.Now())
	blob := json.RawMessage(`{"platform":"slack","workspace_id":"T1","channel_id":"C1","message_ts":"9.1"}`)
	got := stickyFromRequest("slack", "slack", "yo", "", "", "", blob, cache)
	if !strings.Contains(got, "Connection: the bot was removed") {
		t.Fatalf("stickyFromRequest should render cached connection state:\n%s", got)
	}
}

func TestStickyFromRequest_NilCacheNoConnLine(t *testing.T) {
	blob := json.RawMessage(`{"platform":"slack","workspace_id":"T1","channel_id":"C1","message_ts":"9.1"}`)
	got := stickyFromRequest("slack", "slack", "yo", "", "", "", blob, nil)
	if strings.Contains(got, "Connection:") {
		t.Fatalf("nil cache must not render a Connection line:\n%s", got)
	}
}
