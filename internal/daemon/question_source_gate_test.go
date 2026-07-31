package daemon

import "testing"

// A question needs a client that can BOTH render the selection card and send
// the answer back. Approval capability is not a proxy for that: Cloud routes
// approval cards to Slack/Feishu/Lark/Teams/LINE but has no question transport,
// so an asker there blocks the run for the whole auto-resolution window and
// then reports a decline the user never made.
func TestCanPresentQuestionUI(t *testing.T) {
	attended := []string{"desktop", "kocoro", "shanclaw", "web", ""}
	for _, s := range attended {
		if !CanPresentQuestionUI(s) {
			t.Errorf("CanPresentQuestionUI(%q) = false, want true (attended local surface)", s)
		}
	}

	// Every messaging platform is excluded — including the ones that DO have
	// interactive approval, which is exactly the case the old gate got wrong.
	messaging := []string{
		"slack", "feishu", "lark", "teams", "line",
		"wecom", "wechat", "discord", "telegram",
		"koe", "koe-reachy",
	}
	for _, s := range messaging {
		if CanPresentQuestionUI(s) {
			t.Errorf("CanPresentQuestionUI(%q) = true, want false (no question transport)", s)
		}
	}

	for _, s := range []string{"schedule", "cron", "heartbeat", "watcher", "mcp"} {
		if CanPresentQuestionUI(s) {
			t.Errorf("CanPresentQuestionUI(%q) = true, want false (unattended)", s)
		}
	}
}

// The approval gate must NOT shift: Slack/Feishu/Lark/Teams/LINE keep their
// interactive Allow/Deny flow. Only the question gate is narrower.
func TestQuestionGateDoesNotChangeApprovalGate(t *testing.T) {
	for _, s := range []string{"slack", "feishu", "lark", "teams", "line"} {
		if IsNonInteractiveApprovalChannel(s) {
			t.Errorf("IsNonInteractiveApprovalChannel(%q) = true, want false", s)
		}
		if isUnattendedSource(s) {
			t.Errorf("isUnattendedSource(%q) = true, want false", s)
		}
	}
}
