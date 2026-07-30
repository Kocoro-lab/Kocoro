package agent

import (
	"context"
	"testing"

	"github.com/Kocoro-lab/ShanClaw/internal/permissions"
)

type approvalAdmissionTool struct {
	*mockApprovalTool
	decision ApprovalAdmissionDecision
	calls    int
}

func (t *approvalAdmissionTool) ApprovalAdmission(context.Context, string) ApprovalAdmissionDecision {
	t.calls++
	return t.decision
}

func TestApprovalAdmissionDenyPrecedesApprovalUI(t *testing.T) {
	loop, handler := newApprovalProbeLoop(t, nil)
	tool := &approvalAdmissionTool{
		mockApprovalTool: &mockApprovalTool{name: "computer_use"},
		decision:         ApprovalAdmissionDeny,
	}

	decision, approved := loop.checkPermissionAndApproval(context.Background(), tool.Info().Name, `{}`, tool, NewApprovalCache())
	if decision != "deny" || approved {
		t.Fatalf("decision = (%q, %v), want (deny, false)", decision, approved)
	}
	if handler.approvalRequested {
		t.Fatal("blocked app reached approval UI")
	}
}

func TestApprovalAdmissionFreshCannotBeBypassed(t *testing.T) {
	perms := &permissions.PermissionsConfig{AllowedCommands: []string{"computer_use"}}
	loop, handler := newApprovalProbeLoop(t, perms)
	loop.SetAlwaysAllowTools([]string{"computer_use"})
	tool := &approvalAdmissionTool{
		mockApprovalTool: &mockApprovalTool{name: "computer_use", safeArgs: func(string) bool { return true }},
		decision:         ApprovalAdmissionRequireFresh,
	}
	cache := NewApprovalCache()
	cache.RecordApproval("computer_use", `{}`)

	decision, approved := loop.checkPermissionAndApproval(context.Background(), tool.Info().Name, `{}`, tool, cache)
	if decision != "ask" || approved {
		t.Fatalf("decision = (%q, %v), want fresh denied approval", decision, approved)
	}
	if !handler.approvalRequested {
		t.Fatal("fresh admission was bypassed by permissions, safe args, always-allow, or cache")
	}
}

func TestApprovalAdmissionInheritPreservesSafeObservation(t *testing.T) {
	loop, handler := newApprovalProbeLoop(t, nil)
	tool := &approvalAdmissionTool{
		mockApprovalTool: &mockApprovalTool{name: "computer_use", safeArgs: func(string) bool { return true }},
		decision:         ApprovalAdmissionInherit,
	}

	decision, approved := loop.checkPermissionAndApproval(context.Background(), tool.Info().Name, `{}`, tool, NewApprovalCache())
	if decision != "allow" || !approved {
		t.Fatalf("decision = (%q, %v), want safe allow", decision, approved)
	}
	if handler.approvalRequested {
		t.Fatal("safe inherited observation unexpectedly prompted")
	}
}

func TestApprovalAdmissionDoesNotReadGUIBeforeUnattendedComputerUseGrant(t *testing.T) {
	loop, handler := newApprovalProbeLoop(t, nil)
	loop.SetUnattendedRun(true)
	tool := &approvalAdmissionTool{
		mockApprovalTool: &mockApprovalTool{
			name:     "computer_use",
			safeArgs: func(string) bool { return true },
		},
		decision: ApprovalAdmissionDeny,
	}

	decision, approved := loop.checkPermissionAndApproval(
		context.Background(),
		tool.Info().Name,
		`{"action":"get_app_state"}`,
		tool,
		NewApprovalCache(),
	)
	if decision != "ask" || approved {
		t.Fatalf("decision = (%q, %v), want unattended handler denial", decision, approved)
	}
	if tool.calls != 0 {
		t.Fatalf("ApprovalAdmission calls = %d, want 0 before global Computer Use grant", tool.calls)
	}
	if !handler.approvalRequested {
		t.Fatal("unattended denial did not reach the approval handler")
	}
}

func TestApprovalAdmissionRunsAfterUnattendedComputerUseGrant(t *testing.T) {
	loop, handler := newApprovalProbeLoop(t, nil)
	loop.SetAlwaysAllowTools([]string{"computer_use"})
	loop.SetUnattendedRun(true)
	tool := &approvalAdmissionTool{
		mockApprovalTool: &mockApprovalTool{name: "computer_use"},
		decision:         ApprovalAdmissionDeny,
	}

	decision, approved := loop.checkPermissionAndApproval(
		context.Background(),
		tool.Info().Name,
		`{"action":"get_app_state","app":"Finder"}`,
		tool,
		NewApprovalCache(),
	)
	if decision != "deny" || approved {
		t.Fatalf("decision = (%q, %v), want app-policy denial", decision, approved)
	}
	if tool.calls != 1 {
		t.Fatalf("ApprovalAdmission calls = %d, want 1 after global Computer Use grant", tool.calls)
	}
	if handler.approvalRequested {
		t.Fatal("app-policy denial unexpectedly reached approval UI")
	}
}
