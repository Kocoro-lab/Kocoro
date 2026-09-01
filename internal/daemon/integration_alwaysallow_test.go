package daemon

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Kocoro-lab/ShanClaw/internal/agent"
	"github.com/Kocoro-lab/ShanClaw/internal/client"
	"github.com/Kocoro-lab/ShanClaw/internal/tools"
)

// newIntegrationRegistryDeps returns deps whose live Registry carries one
// approval-required integration tool (an outbound write such as
// gmail_send_email) and one unmarked integration tool.
func newIntegrationRegistryDeps(t *testing.T) *ServerDeps {
	t.Helper()
	deps := newDepsWithConfig(t, "operator")
	reg := agent.NewToolRegistry()
	reg.Register(tools.NewIntegrationTool(client.ServerToolSchema{
		Name: "gmail_send_email", RequiresApproval: true,
	}, nil))
	reg.Register(tools.NewIntegrationTool(client.ServerToolSchema{
		Name: "notion_search",
	}, nil))
	deps.Registry = reg
	return deps
}

func TestServerDepsToolDisallowsAlwaysAllowPersistence(t *testing.T) {
	deps := newIntegrationRegistryDeps(t)
	if !deps.ToolDisallowsAlwaysAllowPersistence("gmail_send_email") {
		t.Error("requires_approval integration tool must be reported as persistence-denied")
	}
	if deps.ToolDisallowsAlwaysAllowPersistence("notion_search") {
		t.Error("unmarked integration tool must stay persistable")
	}
	if deps.ToolDisallowsAlwaysAllowPersistence("no_such_tool") {
		t.Error("unknown tool names must not be denied (static list is checked separately)")
	}
	var nilDeps *ServerDeps
	if nilDeps.ToolDisallowsAlwaysAllowPersistence("gmail_send_email") {
		t.Error("nil deps must fail open to the static behavior")
	}
}

// One "Always Allow" click on an approval-required integration write tool must
// not persist anywhere (global list, per-agent list, broker cache) — it would
// permanently authorize unattended outbound sends. The click still counts as a
// one-time allow upstream; only persistence is refused.
func TestHandleAlwaysAllowDecision_IntegrationRequiresApprovalNotPersisted(t *testing.T) {
	for _, agentName := range []string{"", "operator"} {
		deps := newIntegrationRegistryDeps(t)
		broker := NewApprovalBroker(func(req ApprovalRequest) error { return nil })
		broker.SetAlwaysAllowPersistenceDenied(deps.ToolDisallowsAlwaysAllowPersistence)

		HandleAlwaysAllowDecision(deps, broker, agentName, "gmail_send_email",
			`{"to":"a@b.c","description":"Send an email"}`, true)

		if len(deps.Config.Permissions.AlwaysAllowTools) != 0 {
			t.Errorf("agent=%q: global always_allow_tools mutated: %v",
				agentName, deps.Config.Permissions.AlwaysAllowTools)
		}
		if cfgData, err := os.ReadFile(filepath.Join(deps.ShannonDir, "config.yaml")); err == nil &&
			strings.Contains(string(cfgData), "gmail_send_email") {
			t.Errorf("agent=%q: global config.yaml persisted the grant:\n%s", agentName, cfgData)
		}
		if got := readAlwaysAllowFromDisk(t, deps.AgentsDir, "operator"); len(got) != 0 {
			t.Errorf("agent=%q: per-agent grant persisted: %v", agentName, got)
		}
		if broker.IsToolAutoApproved("gmail_send_email") {
			t.Errorf("agent=%q: broker cached an auto-approve for a persistence-denied tool", agentName)
		}
	}
}

// The approval card for an approval-required integration tool carries the
// always_allow_disabled flag so Desktop hides the "Always Allow" button.
func TestApprovalBrokerFlagsIntegrationRequiresApproval(t *testing.T) {
	deps := newIntegrationRegistryDeps(t)
	var captured ApprovalRequest
	broker := NewApprovalBroker(func(req ApprovalRequest) error {
		captured = req
		return nil
	})
	broker.SetAlwaysAllowPersistenceDenied(deps.ToolDisallowsAlwaysAllowPersistence)

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	_ = broker.Request(ctx, ApprovalRequestMeta{}, "gmail_send_email", "{}")
	cancel()
	found := false
	for _, f := range captured.Flags {
		if f == ApprovalFlagAlwaysAllowDisabled {
			found = true
		}
	}
	if !found {
		t.Errorf("gmail_send_email approval request flags = %v, want %q",
			captured.Flags, ApprovalFlagAlwaysAllowDisabled)
	}

	captured = ApprovalRequest{}
	ctx, cancel = context.WithTimeout(context.Background(), 50*time.Millisecond)
	_ = broker.Request(ctx, ApprovalRequestMeta{}, "notion_search", "{}")
	cancel()
	for _, f := range captured.Flags {
		if f == ApprovalFlagAlwaysAllowDisabled {
			t.Errorf("unmarked integration tool must not carry %q", ApprovalFlagAlwaysAllowDisabled)
		}
	}
}

// The manual HTTP write endpoints reject approval-required integration tools
// the same way the approval-click path does (400, nothing persisted). The
// runtime gate still backstops entries written while the tool was unknown.
func TestAlwaysAllowEndpointsRejectIntegrationRequiresApproval(t *testing.T) {
	deps := newIntegrationRegistryDeps(t)
	deps.ShannonDir = t.TempDir()
	srv := NewServer(0, nil, deps, "test")

	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/permissions/always-allow",
		strings.NewReader(`{"tool":"gmail_send_email"}`))
	req.Header.Set("Content-Type", "application/json")
	srv.handleAddGlobalAlwaysAllow(rr, req)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("global endpoint = %d, want 400; body=%s", rr.Code, rr.Body.String())
	}
	if len(deps.Config.Permissions.AlwaysAllowTools) != 0 {
		t.Fatalf("global endpoint persisted: %v", deps.Config.Permissions.AlwaysAllowTools)
	}

	rr = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodPost, "/agents/operator/permissions/always-allow",
		strings.NewReader(`{"tool":"gmail_send_email"}`))
	req.Header.Set("Content-Type", "application/json")
	req.SetPathValue("name", "operator")
	srv.handleAddAgentAlwaysAllow(rr, req)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("per-agent endpoint = %d, want 400; body=%s", rr.Code, rr.Body.String())
	}
	if got := readAlwaysAllowFromDisk(t, deps.AgentsDir, "operator"); len(got) != 0 {
		t.Fatalf("per-agent endpoint persisted: %v", got)
	}
}

// Defense-in-depth on the broker cache itself: even a direct SetToolAutoApprove
// (e.g. an old Desktop's Always Allow click relayed over WS) must not arm the
// in-memory bypass for a persistence-denied tool.
func TestApprovalBrokerCacheRefusesPersistenceDeniedTool(t *testing.T) {
	deps := newIntegrationRegistryDeps(t)
	broker := NewApprovalBroker(func(req ApprovalRequest) error { return nil })
	broker.SetAlwaysAllowPersistenceDenied(deps.ToolDisallowsAlwaysAllowPersistence)

	broker.SetToolAutoApprove("gmail_send_email")
	if broker.IsToolAutoApproved("gmail_send_email") {
		t.Error("broker honored an auto-approve cache entry for a persistence-denied tool")
	}
	broker.SetToolAutoApprove("notion_search")
	if !broker.IsToolAutoApproved("notion_search") {
		t.Error("unmarked integration tool lost its normal auto-approve cache behavior")
	}
}
