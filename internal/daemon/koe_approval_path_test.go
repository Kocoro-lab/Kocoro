package daemon

// Pins the approval path for source=koe. Voice runs have no Allow/Deny UI, so
// the daemon classifies koe as a non-interactive approval channel and the
// broker resolves approvals locally instead of round-tripping to a human.
// These tests freeze that behavior because the pin-Fast/no-routing product
// direction depends on safety being orthogonal to the execution mode:
//
//   - koe is an unattended run (isUnattendedRun) via
//     IsMessagingPlatform ∧ ¬channelHasInteractiveApproval;
//   - RequiresApproval tools NOT on the unattended deny-list are locally
//     auto-approved (no card reaches the user, only EventApprovalAuto);
//   - always-ask bash (e.g. `rm -rf`) is ALSO locally auto-approved on koe —
//     the human-confirmation step the always-ask gate encodes does not exist
//     on this channel today (recorded here as current behavior, not blessing);
//   - unattended deny-listed tools (computer_use, screenshot) fail closed.
//
// Execution-profile orthogonality (audit conclusion, no runtime hook to pin):
// neither ApprovalBroker.Request nor AgentLoop.checkPermissionAndApproval
// reads the execution profile, so a pinned Fast profile cannot change any
// decision above.

import (
	"context"
	"sync/atomic"
	"testing"
	"time"
)

func TestKoeSourceApprovalClassification(t *testing.T) {
	if !IsNonInteractiveApprovalChannel("koe") {
		t.Fatal("koe must classify as a non-interactive approval channel")
	}
	if !IsNonInteractiveApprovalChannel("koe-reachy") {
		t.Fatal("koe-prefixed sources must classify as non-interactive approval channels")
	}
	if !isUnattendedRun("koe", &busEventHandler{}) {
		t.Fatal("koe must classify as an unattended run")
	}
	if isUnattendedRun("desktop", &busEventHandler{}) {
		t.Fatal("desktop must remain attended")
	}
	if IsNonInteractiveApprovalChannel("slack") {
		t.Fatal("slack has an interactive approval card and must not be non-interactive")
	}
}

func newKoeApprovalBroker(t *testing.T) (*ApprovalBroker, *atomic.Int64, *atomic.Int64) {
	t.Helper()
	var cardsSent, autoApprovals atomic.Int64
	broker := NewApprovalBroker(func(req ApprovalRequest) error {
		cardsSent.Add(1)
		return nil
	})
	broker.SetOnAutoApprove(func(ApprovalRequestMeta, string) {
		autoApprovals.Add(1)
	})
	return broker, &cardsSent, &autoApprovals
}

func TestApprovalBrokerKoeAutoApprovesNonDenylistedTool(t *testing.T) {
	broker, cardsSent, autoApprovals := newKoeApprovalBroker(t)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	decision := broker.Request(ctx, ApprovalRequestMeta{Source: "koe"}, "file_write", `{"path":"/tmp/x","content":"y"}`)
	if decision != DecisionAllow {
		t.Fatalf("file_write on koe = %v, want local auto-approve", decision)
	}
	if cardsSent.Load() != 0 {
		t.Fatal("no approval card must be emitted for a koe auto-approval")
	}
	if autoApprovals.Load() != 1 {
		t.Fatal("auto-approval must fire the observability callback")
	}
}

// Current behavior, deliberately frozen: the always-ask bash gate encodes
// "the user must approve THIS invocation", but on koe there is no approval
// UI, so the broker locally allows it. Any change to this line is a product
// decision (e.g. deny always-ask bash on voice) and must update this test.
func TestApprovalBrokerKoeAutoApprovesAlwaysAskBash(t *testing.T) {
	broker, cardsSent, _ := newKoeApprovalBroker(t)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	decision := broker.Request(ctx, ApprovalRequestMeta{Source: "koe"}, "bash", `{"command":"rm -rf /tmp/scratch"}`)
	if decision != DecisionAllow {
		t.Fatalf("always-ask bash on koe = %v; current behavior is local auto-approve", decision)
	}
	if cardsSent.Load() != 0 {
		t.Fatal("no approval card must be emitted on koe")
	}
}

func TestApprovalBrokerKoeDeniesUnattendedDenylistedTools(t *testing.T) {
	broker, cardsSent, autoApprovals := newKoeApprovalBroker(t)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	for _, tool := range []string{"computer_use", "screenshot"} {
		if decision := broker.Request(ctx, ApprovalRequestMeta{Source: "koe"}, tool, "{}"); decision != DecisionDeny {
			t.Fatalf("%s on koe = %v, want fail-closed deny", tool, decision)
		}
	}
	if cardsSent.Load() != 0 {
		t.Fatal("deny-listed tools must fail closed without emitting a card")
	}
	if autoApprovals.Load() != 0 {
		t.Fatal("deny-listed tools must not fire the auto-approve callback")
	}
}
