package daemon

import "testing"

// TestQuestionAskerSourceGate pins the source classification that gates
// QuestionAsker injection in handleMessageSSE. The asker is injected when the
// run's source is NOT unattended — deliberately independent of auto_approve
// (which governs tool-execution approval, not whether a human can answer a
// multiple-choice question). A live incident: an auto_approve Desktop daemon
// gated the asker on !autoApprove, so ask_user_question always hit the
// no-asker fallback ("current channel cannot present an interactive question")
// even from an attended Desktop run. If someone re-gates on auto_approve, or
// reclassifies desktop/web as unattended, this test fails.
func TestQuestionAskerSourceGate(t *testing.T) {
	attended := []string{"desktop", "kocoro", "shanclaw", "web", "tui", ""}
	for _, s := range attended {
		if isUnattendedSource(s) {
			t.Errorf("source %q classified unattended → ask_user_question would get no asker; want attended", s)
		}
	}

	unattended := []string{ChannelSchedule, "cron", "heartbeat", "watcher", "mcp", ChannelWeChat, ChannelTelegram}
	for _, s := range unattended {
		if !isUnattendedSource(s) {
			t.Errorf("source %q classified attended → a background/no-UI run could block on a question; want unattended", s)
		}
	}
}
