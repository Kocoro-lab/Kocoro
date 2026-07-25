package agent

import (
	"context"
	"testing"

	"github.com/Kocoro-lab/ShanClaw/internal/permissions"
)

type approvalAdmissionTool struct {
	*mockApprovalTool
	decision ApprovalAdmissionDecision
}

func (t *approvalAdmissionTool) ApprovalAdmission(context.Context, string) ApprovalAdmissionDecision {
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
