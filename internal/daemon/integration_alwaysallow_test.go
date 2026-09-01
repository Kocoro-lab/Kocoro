package daemon

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
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
		ch := deps.EventBus.Subscribe()
		broker := NewApprovalBroker(func(req ApprovalRequest) error { return nil })
		broker.SetAlwaysAllowPersistenceDenied(deps.ToolDisallowsAlwaysAllowPersistence)

		HandleAlwaysAllowDecision(deps, broker, agentName, "gmail_send_email",
			`{"to":"a@b.c","description":"Send an email"}`, true)

		// The refusal is wire-visible: Desktop localizes off the stable
		// notice code and interpolates the tool name.
		select {
		case evt := <-ch:
			if evt.Type != EventApprovalNotice {
				t.Errorf("agent=%q: event type = %s, want %s", agentName, evt.Type, EventApprovalNotice)
			}
			var notice AlwaysAllowNoticePayload
			if err := json.Unmarshal(evt.Payload, &notice); err != nil {
				t.Fatalf("agent=%q: decode notice: %v", agentName, err)
			}
			if notice.Code != NoticeCodeHighRiskNotPersistable || notice.Tool != "gmail_send_email" {
				t.Errorf("agent=%q: notice = {code:%s tool:%s}, want {code:%s tool:gmail_send_email}",
					agentName, notice.Code, notice.Tool, NoticeCodeHighRiskNotPersistable)
			}
		case <-time.After(time.Second):
			t.Errorf("agent=%q: no EventApprovalNotice emitted for the refusal", agentName)
		}
		deps.EventBus.Unsubscribe(ch)

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
	var mu sync.Mutex
	var captured ApprovalRequest
	var broker *ApprovalBroker
	// Resolve from sendFn (off-lock goroutine) so Request returns as soon as
	// the request is captured — no reliance on a context-timeout path.
	broker = NewApprovalBroker(func(req ApprovalRequest) error {
		mu.Lock()
		captured = req
		mu.Unlock()
		go broker.Resolve(req.RequestID, DecisionDeny, nil)
		return nil
	})
	broker.SetAlwaysAllowPersistenceDenied(deps.ToolDisallowsAlwaysAllowPersistence)

	_ = broker.Request(context.Background(), ApprovalRequestMeta{}, "gmail_send_email", "{}")
	mu.Lock()
	flags := captured.Flags
	mu.Unlock()
	found := false
	for _, f := range flags {
		if f == ApprovalFlagAlwaysAllowDisabled {
			found = true
		}
	}
	if !found {
		t.Errorf("gmail_send_email approval request flags = %v, want %q",
			flags, ApprovalFlagAlwaysAllowDisabled)
	}

	_ = broker.Request(context.Background(), ApprovalRequestMeta{}, "notion_search", "{}")
	mu.Lock()
	flags = captured.Flags
	mu.Unlock()
	for _, f := range flags {
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

// Full-replace agent config writes (PUT /agents/{name} with config, PUT
// /agents/{name}/config) DROP always-allow entries the live registry marks
// persistence-denied (integration requires_approval) instead of rejecting:
// config writes are full-replace, so a 400 would make an agent carrying a
// stale entry permanently uneditable — same rationale as the legacy GUI list
// in agents.SanitizeAgentPermissionsConfig. Ordinary entries survive.
func TestAgentConfigWritesDropIntegrationRequiresApproval(t *testing.T) {
	const permsBody = `{"permissions":{"always_allow_tools":["gmail_send_email","file_write"]}}`

	assertOnlyFileWrite := func(t *testing.T, agentsDir string) {
		t.Helper()
		got := readAlwaysAllowFromDisk(t, agentsDir, "operator")
		if len(got) != 1 || got[0] != "file_write" {
			t.Fatalf("persisted always_allow_tools = %v, want [file_write] only", got)
		}
	}

	t.Run("config put", func(t *testing.T) {
		deps := newIntegrationRegistryDeps(t)
		deps.ShannonDir = t.TempDir()
		srv := NewServer(0, nil, deps, "test")
		rr := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodPut, "/agents/operator/config",
			strings.NewReader(permsBody))
		req.SetPathValue("name", "operator")
		srv.handlePutAgentConfig(rr, req)
		if rr.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200; body=%s", rr.Code, rr.Body.String())
		}
		assertOnlyFileWrite(t, deps.AgentsDir)
	})

	t.Run("full update", func(t *testing.T) {
		deps := newIntegrationRegistryDeps(t)
		deps.ShannonDir = t.TempDir()
		deps.SessionCache = NewSessionCache(deps.ShannonDir)
		srv := NewServer(0, nil, deps, "test")
		rr := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodPut, "/agents/operator",
			strings.NewReader(`{"config":`+permsBody+`}`))
		req.SetPathValue("name", "operator")
		srv.handleUpdateAgent(rr, req)
		if rr.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200; body=%s", rr.Code, rr.Body.String())
		}
		assertOnlyFileWrite(t, deps.AgentsDir)
	})

	// Registry miss (integration catalog empty, e.g. mid key-rotation) must NOT
	// drop the entry — the runtime gate in loop.go backstops it, and dropping on
	// a miss would erase a grant for a tool that is merely unlisted right now.
	t.Run("registry miss keeps entry", func(t *testing.T) {
		deps := newDepsWithConfig(t, "operator")
		deps.ShannonDir = t.TempDir()
		srv := NewServer(0, nil, deps, "test")
		rr := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodPut, "/agents/operator/config",
			strings.NewReader(permsBody))
		req.SetPathValue("name", "operator")
		srv.handlePutAgentConfig(rr, req)
		if rr.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200; body=%s", rr.Code, rr.Body.String())
		}
		got := readAlwaysAllowFromDisk(t, deps.AgentsDir, "operator")
		if len(got) != 2 {
			t.Fatalf("persisted always_allow_tools = %v, want both entries kept on registry miss", got)
		}
	})
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
