package daemon

import (
	"context"
	"testing"
	"time"
)

// A question needs a client that can BOTH render the selection card and send
// the answer back. Approval capability is not a proxy for that: Cloud routes
// approval cards to Slack/Feishu/Lark/Teams/LINE but has no question transport,
// so an asker there blocks the run for the whole resolution window and then
// reports a decline the user never made.
func TestCanPresentQuestionUI(t *testing.T) {
	for _, s := range []string{"desktop", "kocoro", "shanclaw", "web", ""} {
		if !CanPresentQuestionUI(s) {
			t.Errorf("CanPresentQuestionUI(%q) = false, want true (has a question consumer)", s)
		}
	}

	// Messaging platforms are excluded wholesale — including the five that DO
	// have interactive approval, which is exactly what the old gate got wrong.
	for _, s := range []string{
		"slack", "feishu", "lark", "teams", "line",
		"wecom", "wechat", "discord", "telegram",
		"koe", "koe-reachy",
	} {
		if CanPresentQuestionUI(s) {
			t.Errorf("CanPresentQuestionUI(%q) = true, want false (no question transport)", s)
		}
	}

	for _, s := range []string{ChannelSchedule, "cron", "heartbeat", "watcher", "mcp"} {
		if CanPresentQuestionUI(s) {
			t.Errorf("CanPresentQuestionUI(%q) = true, want false (unattended)", s)
		}
	}

	// The allow-list must fail CLOSED. These reach handleMessageSSE but have no
	// question consumer, and a deny-list built on IsMessagingPlatform would let
	// them through — a wedged run, not merely a wasted call.
	for _, s := range []string{"webhook", ChannelSystem, "some-channel-cloud-adds-later"} {
		if CanPresentQuestionUI(s) {
			t.Errorf("CanPresentQuestionUI(%q) = true, want false (unknown source must default to prose)", s)
		}
	}
}

// The approval gate must NOT shift with the question gate: Slack/Feishu/Lark/
// Teams/LINE keep their interactive Allow/Deny flow, and auto_approve still has
// no say over either (a live incident once gated the asker on !autoApprove, so
// an auto_approve Desktop run could never ask).
func TestQuestionGateDoesNotChangeApprovalGate(t *testing.T) {
	for _, s := range []string{"slack", "feishu", "lark", "teams", "line"} {
		if IsNonInteractiveApprovalChannel(s) {
			t.Errorf("IsNonInteractiveApprovalChannel(%q) = true, want false", s)
		}
		if isUnattendedSource(s) {
			t.Errorf("isUnattendedSource(%q) = true, want false", s)
		}
	}
	for _, s := range []string{ChannelSchedule, "cron", "heartbeat", "watcher", "mcp", ChannelWeChat, ChannelTelegram} {
		if !isUnattendedSource(s) {
			t.Errorf("isUnattendedSource(%q) = false, want true", s)
		}
	}
}

// The broker is the layer that backstops a call site getting the gate wrong —
// which is exactly what happened here. Even if the asker were injected for an
// IM source again, Request must decline immediately rather than mint an ID,
// emit a frame and block for the resolution window.
func TestQuestionBrokerDeclinesInteractiveApprovalIMSources(t *testing.T) {
	srv := NewServer(0, nil, nil, "test")
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	for _, source := range []string{"slack", "feishu", "lark", "teams", "line", "koe"} {
		res := srv.questionBroker.Request(ctx, ApprovalRequestMeta{Source: source}, &QuestionRequest{
			Questions: []Question{{ID: "q0", Question: "?", Options: []QuestionOption{{Label: "a"}, {Label: "b"}}}},
		})
		if res.Action != QuestionActionDecline {
			t.Errorf("source %q action = %q, want %q — a run with no question UI must not block",
				source, res.Action, QuestionActionDecline)
		}
	}
}
