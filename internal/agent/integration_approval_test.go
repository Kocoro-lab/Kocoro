package agent

import (
	"context"
	"testing"
)

// Integration tool schemas may carry requires_approval:true (Cloud-marked
// consequential tools such as X writes, gated on the
// integration_requires_approval capability). The registered ServerTool exposes
// the generic Tool contract — RequiresApproval()==true and no SafeChecker
// exemption — plus AlwaysAllowPersistenceDenier: Cloud defines that set as
// material outbound writes needing per-call human approval, so a persisted
// "Always Allow" grant must never silence future prompts for them. These
// tests pin that contract at the permission engine with an integration-shaped
// mock: every call prompts (the in-turn approval cache is refused too, since
// an identical repeated tool_use re-executes the send under a fresh request
// id), always_allow_tools entries are IGNORED, and unattended runs reach the
// handler instead of failing closed. The handler halves are pinned elsewhere:
// daemon.auto_approve in cmd's TestDaemonEventHandler_AutoApproveAllowsAllTools,
// "Always Allow" persistence refusal in internal/daemon's alwaysallow tests.

// integrationApprovalTool mirrors the integration-variant ServerTool contract:
// RequiresApproval()==true plus the schema-derived always-allow denial.
type integrationApprovalTool struct {
	mockApprovalTool
}

func (m *integrationApprovalTool) DisallowsAlwaysAllowPersistence() bool { return true }

func TestCheckPermissionAndApproval_IntegrationRequiresApprovalPromptsFirstUse(t *testing.T) {
	loop, handler := newApprovalProbeLoop(t, nil)
	handler.approveResult = true

	tool := &integrationApprovalTool{mockApprovalTool{name: "x_create_post"}}
	cache := NewApprovalCache()
	args := `{"text":"hello","description":"Publish a post on X"}`
	decision, approved := loop.checkPermissionAndApproval(
		context.Background(), "x_create_post", args, tool, cache)
	if decision != "ask" || !approved || !handler.approvalRequested {
		t.Fatalf("first use: got (%s, %v), requested=%v; want prompt reaching the handler",
			decision, approved, handler.approvalRequested)
	}

	// A byte-identical repeat within the same turn is NOT served from the
	// approval cache: a repeated tool_use carries a fresh request id, so the
	// idempotency journal does not dedupe it and the send would execute
	// again. Same rationale as the GUI tools' cache refusal — duplicate
	// outbound writes need a human decision each time.
	handler.approvalRequested = false
	decision, approved = loop.checkPermissionAndApproval(
		context.Background(), "x_create_post", args, tool, cache)
	if decision != "ask" || !approved || !handler.approvalRequested {
		t.Fatalf("identical repeat: got (%s, %v), requested=%v; want a fresh prompt reaching the handler",
			decision, approved, handler.approvalRequested)
	}
}

// A persisted always_allow_tools entry (hand-edited config, or one minted
// before this policy) must NOT bypass approval for an approval-required
// integration tool: one historical click cannot permanently authorize
// unattended outbound writes (email sends, X posts). The request still
// reaches the handler for a fresh per-call decision.
func TestCheckPermissionAndApproval_IntegrationRequiresApprovalIgnoresAlwaysAllow(t *testing.T) {
	loop, handler := newApprovalProbeLoop(t, nil)
	handler.approveResult = true
	// SetAlwaysAllowTools receives the union of global and per-agent
	// always_allow_tools — both persistence scopes land in the same map.
	loop.SetAlwaysAllowTools([]string{"x_create_post"})

	tool := &integrationApprovalTool{mockApprovalTool{name: "x_create_post"}}
	decision, approved := loop.checkPermissionAndApproval(
		context.Background(), "x_create_post", `{"text":"hello"}`, tool, NewApprovalCache())
	if decision != "ask" || !approved || !handler.approvalRequested {
		t.Fatalf("always-allow bypass: got (%s, %v), requested=%v; want a fresh prompt reaching the handler",
			decision, approved, handler.approvalRequested)
	}
}

// Unattended runs (schedules, interrupted recovery) must not fail closed on a
// requires_approval integration tool: it is absent from the unattended
// deny-list, so the request reaches the handler and the handler's answer is
// honored — the schedule and auto_approve handlers approve everything off
// that deny-list. This is the deliberately retained carve-out: the
// always-allow persistence denial (which this mock carries) must not leak
// into the unattended path and break scheduled/no-UI integration use.
func TestCheckPermissionAndApproval_IntegrationRequiresApprovalUnattendedReachesHandler(t *testing.T) {
	loop, handler := newApprovalProbeLoop(t, nil)
	handler.approveResult = true
	loop.SetUnattendedRun(true)

	tool := &integrationApprovalTool{mockApprovalTool{name: "x_create_post"}}
	_, approved := loop.checkPermissionAndApproval(
		context.Background(), "x_create_post", `{"text":"hello"}`, tool, NewApprovalCache())
	if !approved || !handler.approvalRequested {
		t.Fatalf("unattended: approved=%v requested=%v; want the handler consulted and honored",
			approved, handler.approvalRequested)
	}
}
