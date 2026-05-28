package daemon

import (
	"strings"
	"testing"
)

func TestStickyContext_AlwaysIncludesAgent(t *testing.T) {
	// Default agent (empty name) must STILL surface its identity in sticky
	// context — otherwise the LLM can't reason about "I am the agent Cloud
	// routed this message to".
	got := buildStickyContext("slack", "C0XXX", "yohei", "", "", "")
	if !strings.Contains(got, "Agent: default") {
		t.Errorf("default agent must surface as 'Agent: default'; got:\n%s", got)
	}

	got = buildStickyContext("slack", "C0XXX", "yohei", "researcher", "", "")
	if !strings.Contains(got, "Agent: researcher") {
		t.Errorf("named agent must surface as 'Agent: researcher'; got:\n%s", got)
	}
}

func TestStickyContext_IMBindings(t *testing.T) {
	// Non-empty imBindings → "IM bindings:" line appears between Agent and extra.
	got := buildStickyContext("webview", "", "hu", "", "default=slack:kocoro-test-slack", "")
	if !strings.Contains(got, "\nIM bindings: default=slack:kocoro-test-slack") {
		t.Errorf("want IM bindings line surfaced; got:\n%s", got)
	}

	// Empty imBindings → line omitted entirely (model reads absence as
	// "no bindings known" — see the IM channel delivery prompt section).
	got = buildStickyContext("webview", "", "hu", "", "", "")
	if strings.Contains(got, "IM bindings") {
		t.Errorf("empty imBindings must NOT emit any IM bindings line; got:\n%s", got)
	}
}

func TestStickyContext_OrderAgentBeforeBindings(t *testing.T) {
	// The LLM-facing routing model in the system prompt assumes a stable
	// line order: Source / Channel / Sender / Agent / IM bindings / extra.
	// Out-of-order would force the model to scan; keep alignment with the
	// "## IM channel delivery" section.
	got := buildStickyContext("slack", "C0XXX", "yohei", "analyst", "analyst=feishu:engineering", "")
	agentIdx := strings.Index(got, "Agent:")
	bindIdx := strings.Index(got, "IM bindings:")
	if agentIdx < 0 || bindIdx < 0 || agentIdx > bindIdx {
		t.Errorf("Agent must come before IM bindings; got:\n%s", got)
	}
}
