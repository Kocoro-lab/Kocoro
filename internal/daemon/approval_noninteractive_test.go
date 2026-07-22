package daemon

import (
	"context"
	"testing"
	"time"
)

func TestIsNonInteractiveApprovalChannel(t *testing.T) {
	// No approval UI → auto-approve. koe (voice) is messaging-routed with a
	// daemon-local transport and no Allow/Deny surface, so it belongs here too.
	for _, s := range []string{ChannelWeChat, ChannelWeCom, ChannelDiscord, ChannelTelegram, ChannelKoe, "koe-1", "WeChat", " wechat "} {
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

func TestApprovalBroker_NonInteractiveDenyListBeatsBrokerAlwaysAllow(t *testing.T) {
	sent := false
	broker := NewApprovalBroker(func(req ApprovalRequest) error { sent = true; return nil })
	broker.SetToolAutoApprove("computer_use")
	if !broker.IsToolAutoApproved("computer_use") {
		t.Fatal("test setup: attended broker Always Allow was not recorded")
	}

	decision := broker.Request(context.Background(),
		ApprovalRequestMeta{Source: ChannelWeChat}, "computer_use", `{"action":"click","x":1,"y":1}`)
	if decision != DecisionDeny {
		t.Fatalf("non-interactive computer_use bypassed unattended gate via broker Always Allow: %v", decision)
	}
	if sent {
		t.Error("deny-listed non-interactive request must fail closed without an impossible approval round-trip")
	}
}

// A destructive always-ask bash from a non-interactive channel IS auto-approved
// (there is no one to prompt). This is the accepted "全部放行" trade-off — pin it
// explicitly so the security posture is intentional, not emergent — and assert
// the observability notice fires so the unattended run stays visible.
func TestApprovalBroker_NonInteractiveAutoApprovesDestructiveAndNotifies(t *testing.T) {
	var gotMeta ApprovalRequestMeta
	var gotTool string
	notices := 0
	broker := NewApprovalBroker(func(req ApprovalRequest) error { return nil })
	broker.SetOnAutoApprove(func(meta ApprovalRequestMeta, tool string) {
		notices++
		gotMeta = meta
		gotTool = tool
	})
	decision := broker.Request(context.Background(),
		ApprovalRequestMeta{Source: ChannelWeChat, SessionID: "s1", Agent: "a1"}, "bash", "rm -rf /tmp/x")
	if decision != DecisionAllow {
		t.Fatalf("expected destructive bash to be auto-approved for wechat, got %v", decision)
	}
	if notices != 1 || gotTool != "bash" || gotMeta.SessionID != "s1" {
		t.Errorf("expected one auto-approve notice for bash/s1, got n=%d tool=%q meta=%+v", notices, gotTool, gotMeta)
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
