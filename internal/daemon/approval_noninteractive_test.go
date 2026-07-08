package daemon

import (
	"context"
	"testing"
	"time"
)

func TestIsNonInteractiveApprovalChannel(t *testing.T) {
	// No approval UI → auto-approve.
	for _, s := range []string{ChannelWeChat, ChannelWeCom, ChannelDiscord, ChannelTelegram, "WeChat", " wechat "} {
		if !IsNonInteractiveApprovalChannel(s) {
			t.Errorf("expected %q to be a non-interactive approval channel", s)
		}
	}
	// Interactive IM channels keep human approval.
	for _, s := range []string{ChannelSlack, ChannelFeishu, ChannelLark, ChannelTeams, ChannelLINE} {
		if IsNonInteractiveApprovalChannel(s) {
			t.Errorf("expected %q to keep interactive approval, not auto-approve", s)
		}
	}
	// Local / non-IM sources are unaffected (normal in-process approval).
	for _, s := range []string{"", "web", "tui", "kocoro", ChannelSchedule, ChannelSystem} {
		if IsNonInteractiveApprovalChannel(s) {
			t.Errorf("expected local/non-IM source %q to be unaffected", s)
		}
	}
}

func TestApprovalBroker_NonInteractiveChannelAutoApproves(t *testing.T) {
	sent := false
	broker := NewApprovalBroker(func(req ApprovalRequest) error { sent = true; return nil })
	decision := broker.Request(context.Background(),
		ApprovalRequestMeta{Source: ChannelWeChat}, "bash", "echo hi")
	if decision != DecisionAllow {
		t.Fatalf("expected DecisionAllow for wechat, got %v", decision)
	}
	if sent {
		t.Error("sendFn must NOT be called for a non-interactive channel (no cloud round-trip / no stall)")
	}
}

func TestApprovalBroker_InteractiveChannelStillPrompts(t *testing.T) {
	sent := make(chan ApprovalRequest, 1)
	broker := NewApprovalBroker(func(req ApprovalRequest) error { sent <- req; return nil })
	done := make(chan ApprovalDecision, 1)
	go func() {
		done <- broker.Request(context.Background(),
			ApprovalRequestMeta{Source: ChannelSlack}, "bash", "echo hi")
	}()
	select {
	case req := <-sent:
		broker.Resolve(req.RequestID, DecisionAllow, nil)
	case <-time.After(2 * time.Second):
		t.Fatal("sendFn must be called for an interactive channel (human approval)")
	}
	if d := <-done; d != DecisionAllow {
		t.Errorf("expected DecisionAllow after resolve, got %v", d)
	}
}
