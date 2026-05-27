package daemon

import (
	"strings"
	"testing"
)

func TestStickyContext_AlwaysIncludesAgent(t *testing.T) {
	// Default agent (empty name) must STILL surface its identity in sticky
	// context — otherwise the LLM can't reason about "I am the agent Cloud
	// routed this message to".
	got := buildStickyContext("slack", "C0XXX", "yohei", "", "")
	if !strings.Contains(got, "Agent: default") {
		t.Errorf("default agent must surface as 'Agent: default'; got:\n%s", got)
	}

	got = buildStickyContext("slack", "C0XXX", "yohei", "researcher", "")
	if !strings.Contains(got, "Agent: researcher") {
		t.Errorf("named agent must surface as 'Agent: researcher'; got:\n%s", got)
	}
}
