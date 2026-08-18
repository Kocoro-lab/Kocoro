package agent

import (
	"context"
	"testing"
)

// Integration tool schemas may carry requires_approval:true (Cloud-marked
// consequential tools such as X writes, gated on the
// integration_requires_approval capability). The registered ServerTool exposes
// only the generic Tool contract — RequiresApproval()==true and no SafeChecker
// exemption — so the standard approval flow governs it with zero integration
// special-casing. These tests pin that contract at the permission engine with
// an integration-shaped mock: first use prompts, an in-turn approval is
// cached, always_allow_tools bypasses the prompt, and unattended runs reach
// the handler instead of failing closed. The handler halves are pinned
// elsewhere: daemon.auto_approve in cmd's
// TestDaemonEventHandler_AutoApproveAllowsAllTools, "Always Allow"
// persistence in internal/daemon's alwaysallow tests.

func TestCheckPermissionAndApproval_IntegrationRequiresApprovalPromptsFirstUse(t *testing.T) {
	loop, handler := newApprovalProbeLoop(t, nil)
	handler.approveResult = true

	tool := &mockApprovalTool{name: "x_create_post"}
	cache := NewApprovalCache()
	args := `{"text":"hello","description":"Publish a post on X"}`
	decision, approved := loop.checkPermissionAndApproval(
		context.Background(), "x_create_post", args, tool, cache)
	if decision != "ask" || !approved || !handler.approvalRequested {
		t.Fatalf("first use: got (%s, %v), requested=%v; want prompt reaching the handler",
			decision, approved, handler.approvalRequested)
	}

	// Identical call within the same turn is served from the approval cache —
	// integration tools are not on DisallowsAutoApproval, so no fresh prompt.
	handler.approvalRequested = false
	decision, approved = loop.checkPermissionAndApproval(
		context.Background(), "x_create_post", args, tool, cache)
	if decision != "ask" || !approved || handler.approvalRequested {
		t.Fatalf("cached repeat: got (%s, %v), requested=%v; want cached approval without a prompt",
			decision, approved, handler.approvalRequested)
	}
}

func TestCheckPermissionAndApproval_IntegrationRequiresApprovalHonorsAlwaysAllow(t *testing.T) {
	loop, handler := newApprovalProbeLoop(t, nil)
	// SetAlwaysAllowTools receives the union of global and per-agent
	// always_allow_tools — both persistence scopes land in the same map.
	loop.SetAlwaysAllowTools([]string{"x_create_post"})

	tool := &mockApprovalTool{name: "x_create_post"}
	decision, approved := loop.checkPermissionAndApproval(
		context.Background(), "x_create_post", `{"text":"hello"}`, tool, NewApprovalCache())
	if decision != "allow" || !approved {
		t.Fatalf("always-allow: got (%s, %v); want (allow, true)", decision, approved)
	}
	if handler.approvalRequested {
		t.Error("always-allowed integration tool should not prompt")
	}
}

// Unattended runs (schedules, interrupted recovery) must not fail closed on a
// requires_approval integration tool: it is absent from the unattended
// deny-list, so the request reaches the handler and the handler's answer is
// honored — the schedule and auto_approve handlers approve everything off
// that deny-list.
func TestCheckPermissionAndApproval_IntegrationRequiresApprovalUnattendedReachesHandler(t *testing.T) {
	loop, handler := newApprovalProbeLoop(t, nil)
	handler.approveResult = true
	loop.SetUnattendedRun(true)

	tool := &mockApprovalTool{name: "x_create_post"}
	_, approved := loop.checkPermissionAndApproval(
		context.Background(), "x_create_post", `{"text":"hello"}`, tool, NewApprovalCache())
	if !approved || !handler.approvalRequested {
		t.Fatalf("unattended: approved=%v requested=%v; want the handler consulted and honored",
			approved, handler.approvalRequested)
	}
}
