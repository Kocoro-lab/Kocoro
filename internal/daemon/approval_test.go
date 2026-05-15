package daemon

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/Kocoro-lab/ShanClaw/internal/config"
	"github.com/Kocoro-lab/ShanClaw/internal/permissions"
)

func TestApprovalBroker_RequestResolve(t *testing.T) {
	var sent []ApprovalRequest
	var mu sync.Mutex
	sendFn := func(req ApprovalRequest) error {
		mu.Lock()
		sent = append(sent, req)
		mu.Unlock()
		return nil
	}

	broker := NewApprovalBroker(sendFn)

	// Resolve in a goroutine after a short delay
	go func() {
		time.Sleep(50 * time.Millisecond)
		mu.Lock()
		reqID := sent[0].RequestID
		mu.Unlock()
		broker.Resolve(reqID, DecisionAllow, nil)
	}()

	decision := broker.Request(context.Background(), ApprovalRequestMeta{
		MessageID: "msg-1",
		Channel:   "ch1",
		ThreadID:  "th1",
		Agent:     "bot",
	}, "bash", `{"command":"ls"}`)
	if decision != DecisionAllow {
		t.Errorf("expected allow, got %s", decision)
	}
	if len(sent) != 1 {
		t.Fatalf("expected 1 sent request, got %d", len(sent))
	}
	if sent[0].Tool != "bash" {
		t.Errorf("expected tool=bash, got %s", sent[0].Tool)
	}
	if sent[0].MessageID != "msg-1" {
		t.Errorf("expected MessageID=msg-1 on the broker request, got %q", sent[0].MessageID)
	}
}

func TestApprovalBroker_ContextCancel(t *testing.T) {
	broker := NewApprovalBroker(func(req ApprovalRequest) error { return nil })

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(50 * time.Millisecond)
		cancel()
	}()

	decision := broker.Request(ctx, ApprovalRequestMeta{
		MessageID: "msg-1",
		Channel:   "ch1",
		ThreadID:  "th1",
		Agent:     "bot",
	}, "bash", `{}`)
	if decision != DecisionDeny {
		t.Errorf("expected deny on ctx cancel, got %s", decision)
	}
}

func TestApprovalBroker_CancelAll(t *testing.T) {
	broker := NewApprovalBroker(func(req ApprovalRequest) error { return nil })

	results := make(chan ApprovalDecision, 3)
	for i := 0; i < 3; i++ {
		go func() {
			results <- broker.Request(context.Background(), ApprovalRequestMeta{
				MessageID: "msg-1",
				Channel:   "ch1",
				ThreadID:  "th1",
				Agent:     "bot",
			}, "bash", `{}`)
		}()
	}

	// Let requests register
	time.Sleep(50 * time.Millisecond)

	broker.CancelAll()

	for i := 0; i < 3; i++ {
		select {
		case d := <-results:
			if d != DecisionDeny {
				t.Errorf("expected deny from CancelAll, got %s", d)
			}
		case <-time.After(time.Second):
			t.Fatal("CancelAll did not unblock all pending requests")
		}
	}
}

func TestApprovalBroker_SendFails(t *testing.T) {
	broker := NewApprovalBroker(func(req ApprovalRequest) error {
		return fmt.Errorf("not connected")
	})

	decision := broker.Request(context.Background(), ApprovalRequestMeta{
		MessageID: "msg-1",
		Channel:   "ch1",
		ThreadID:  "th1",
		Agent:     "bot",
	}, "bash", `{}`)
	if decision != DecisionDeny {
		t.Errorf("expected deny on send failure, got %s", decision)
	}
}

func TestApprovalBroker_ResolveUnknown(t *testing.T) {
	broker := NewApprovalBroker(func(req ApprovalRequest) error { return nil })
	// Should not panic
	broker.Resolve("nonexistent", DecisionAllow, nil)
}

func TestApprovalBroker_ConcurrentRequests(t *testing.T) {
	var mu sync.Mutex
	var sent []ApprovalRequest
	broker := NewApprovalBroker(func(req ApprovalRequest) error {
		mu.Lock()
		sent = append(sent, req)
		mu.Unlock()
		return nil
	})

	const n = 5
	results := make(chan ApprovalDecision, n)

	for i := 0; i < n; i++ {
		go func() {
			results <- broker.Request(context.Background(), ApprovalRequestMeta{
				MessageID: "msg-1",
				Channel:   "ch1",
				ThreadID:  "th1",
				Agent:     "bot",
			}, "bash", `{}`)
		}()
	}

	// Let all requests register
	time.Sleep(100 * time.Millisecond)

	mu.Lock()
	for _, req := range sent {
		broker.Resolve(req.RequestID, DecisionAllow, nil)
	}
	mu.Unlock()

	for i := 0; i < n; i++ {
		select {
		case d := <-results:
			if d != DecisionAllow {
				t.Errorf("expected allow, got %s", d)
			}
		case <-time.After(time.Second):
			t.Fatal("not all concurrent requests resolved")
		}
	}
}

func TestAlwaysAllowBashPersistence(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "config.yaml"), []byte("endpoint: test\n"), 0644)

	// Persist a bash command
	err := config.AppendAllowedCommand(dir, "git status")
	if err != nil {
		t.Fatalf("persist failed: %v", err)
	}

	// Verify it matches via permission engine
	cfg := &permissions.PermissionsConfig{
		AllowedCommands: []string{"git status"},
	}
	decision, _ := permissions.CheckCommand("git status", cfg)
	if decision != "allow" {
		t.Errorf("expected allow, got %s", decision)
	}

	// Different command should not match
	decision, _ = permissions.CheckCommand("git push", cfg)
	if decision == "allow" {
		t.Error("git push should not be auto-allowed by git status pattern")
	}
}

func TestApprovalBroker_ToolAutoApprove(t *testing.T) {
	broker := NewApprovalBroker(func(req ApprovalRequest) error { return nil })
	broker.SetToolAutoApprove("file_write")

	if !broker.IsToolAutoApproved("file_write") {
		t.Error("file_write should be auto-approved")
	}
	if broker.IsToolAutoApproved("bash") {
		t.Error("bash should not be auto-approved")
	}
}

// TestApprovalBroker_HighRiskNeverAutoApproved guards the fail-safe added in
// PR 3: callers may unconditionally invoke SetToolAutoApprove after a
// DecisionAlwaysAllow click, but the broker itself must refuse to honor
// high-risk tools so a single click cannot self-elevate the rest of the session.
// This is the regression check pointed out in PR 3 review.
func TestApprovalBroker_HighRiskNeverAutoApproved(t *testing.T) {
	broker := NewApprovalBroker(func(req ApprovalRequest) error { return nil })

	for _, tool := range []string{"publish_to_web", "generate_image", "edit_image"} {
		// Caller path: unconditional set after Always-Allow click.
		broker.SetToolAutoApprove(tool)
		if broker.IsToolAutoApproved(tool) {
			t.Errorf("%s must NOT be auto-approved even after SetToolAutoApprove", tool)
		}
	}

	// Sanity: safe tools still work after the high-risk attempts above —
	// the set-side guard must not corrupt the underlying map.
	broker.SetToolAutoApprove("file_write")
	if !broker.IsToolAutoApproved("file_write") {
		t.Error("file_write should still be auto-approved after high-risk set attempts")
	}
}

// TestApprovalBroker_HighRiskCheckSideOnly verifies the get-side fail-safe
// in isolation: even if a future regression bypassed SetToolAutoApprove and
// wrote directly to the map, IsToolAutoApproved must still refuse high-risk.
func TestApprovalBroker_HighRiskCheckSideOnly(t *testing.T) {
	broker := NewApprovalBroker(func(req ApprovalRequest) error { return nil })

	// Bypass the set-side guard by writing the map directly.
	broker.mu.Lock()
	broker.toolAutoApprove["publish_to_web"] = true
	broker.mu.Unlock()

	if broker.IsToolAutoApproved("publish_to_web") {
		t.Error("IsToolAutoApproved must refuse high-risk tools even if the map contains them")
	}
}

// TestApprovalBroker_FlagsHighRiskTools verifies the policy hint sent to UI
// clients on the approval payload. Non-technical UIs use this flag to disable
// the "Always Allow" affordance for tools whose nature (paid quota /
// permanent public output) requires fresh consent every call.
func TestApprovalBroker_FlagsHighRiskTools(t *testing.T) {
	var captured ApprovalRequest
	broker := NewApprovalBroker(func(req ApprovalRequest) error {
		captured = req
		return nil
	})
	// High-risk tool: flag MUST be present so UI hides "Always Allow".
	for _, tool := range []string{"publish_to_web", "generate_image", "edit_image"} {
		captured = ApprovalRequest{}
		go broker.Resolve(captured.RequestID, DecisionDeny, nil) // no-op (req not in-flight yet)
		ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
		_ = broker.Request(ctx, ApprovalRequestMeta{}, tool, "{}")
		cancel()
		found := false
		for _, f := range captured.Flags {
			if f == ApprovalFlagAlwaysAllowDisabled {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("%s: expected flags to contain %q, got %v", tool, ApprovalFlagAlwaysAllowDisabled, captured.Flags)
		}
	}
	// Safe tool: flag MUST be absent so UI shows the "Always Allow" button.
	for _, tool := range []string{"bash", "file_write", "http", "browser"} {
		captured = ApprovalRequest{}
		ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
		_ = broker.Request(ctx, ApprovalRequestMeta{}, tool, "{}")
		cancel()
		for _, f := range captured.Flags {
			if f == ApprovalFlagAlwaysAllowDisabled {
				t.Errorf("%s: flag %q should NOT be set for safe tools, got %v", tool, ApprovalFlagAlwaysAllowDisabled, captured.Flags)
			}
		}
	}
}
