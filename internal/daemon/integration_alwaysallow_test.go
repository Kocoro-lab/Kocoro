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
	"github.com/Kocoro-lab/ShanClaw/internal/agents"
	"github.com/Kocoro-lab/ShanClaw/internal/client"
	"github.com/Kocoro-lab/ShanClaw/internal/config"
	"github.com/Kocoro-lab/ShanClaw/internal/skills"
	"github.com/Kocoro-lab/ShanClaw/internal/tools"
	"gopkg.in/yaml.v3"
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

	t.Run("create", func(t *testing.T) {
		deps := newIntegrationRegistryDeps(t)
		deps.ShannonDir = t.TempDir()
		deps.SessionCache = NewSessionCache(deps.ShannonDir)
		srv := NewServer(0, nil, deps, "test")
		rr := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodPost, "/agents",
			strings.NewReader(`{"display_name":"Creator","prompt":"p","config":`+permsBody+`}`))
		srv.handleCreateAgent(rr, req)
		if rr.Code != http.StatusCreated {
			t.Fatalf("status = %d, want 201; body=%s", rr.Code, rr.Body.String())
		}
		var resp struct {
			Name string `json:"name"`
		}
		if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil || resp.Name == "" {
			t.Fatalf("decode created agent slug: %v (body=%s)", err, rr.Body.String())
		}
		got := readAlwaysAllowFromDisk(t, deps.AgentsDir, resp.Name)
		if len(got) != 1 || got[0] != "file_write" {
			t.Fatalf("created agent always_allow_tools = %v, want [file_write] only", got)
		}
	})

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

// readGlobalAlwaysAllowFromDisk returns permissions.always_allow_tools from
// the global config.yaml under shannonDir. Nil when absent.
func readGlobalAlwaysAllowFromDisk(t *testing.T, shannonDir string) []string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(shannonDir, "config.yaml"))
	if err != nil {
		return nil
	}
	var raw map[string]interface{}
	if err := yaml.Unmarshal(data, &raw); err != nil {
		t.Fatalf("parse global config: %v", err)
	}
	perms, _ := raw["permissions"].(map[string]interface{})
	if perms == nil {
		return nil
	}
	list, _ := perms["always_allow_tools"].([]interface{})
	out := make([]string, 0, len(list))
	for _, v := range list {
		if s, ok := v.(string); ok {
			out = append(out, s)
		}
	}
	return out
}

// seedDeniedGrantDeps returns deps carrying one denied (gmail_send_email) and
// one ordinary (file_write) always-allow grant in BOTH the global config
// (disk + in-memory mirror) and the "operator" agent's config.yaml, with an
// empty live registry ready for a catalog rebuild.
func seedDeniedGrantDeps(t *testing.T) *ServerDeps {
	t.Helper()
	deps := newDepsWithConfig(t, "operator")
	deps.ShannonDir = t.TempDir()
	deps.Registry = agent.NewToolRegistry()
	deps.Config.Cloud.Enabled = true
	deps.Config.APIKey = "test-key"
	deps.Config.Permissions.AlwaysAllowTools = []string{"gmail_send_email", "file_write"}
	for _, tool := range []string{"gmail_send_email", "file_write"} {
		if _, err := config.AppendGlobalAlwaysAllowToolWithRevision(deps.ShannonDir, tool); err != nil {
			t.Fatalf("seed global grant %s: %v", tool, err)
		}
		if err := agents.AppendAlwaysAllowTool(deps.AgentsDir, "operator", tool); err != nil {
			t.Fatalf("seed agent grant %s: %v", tool, err)
		}
	}
	return deps
}

// A grant persisted while the integration catalog was empty (key rotation /
// principal-transition window) is permanently ignored by the runtime gate once
// the catalog recovers. RefreshIntegrationTools self-heals: after a successful
// catalog rebuild it prunes global and per-agent always-allow entries the
// registry NOW marks persistence-denied. An empty or failed rebuild prunes
// nothing (registry miss judges false — fail-safe against mass deletion).
func TestRefreshIntegrationToolsPrunesDeniedAlwaysAllow(t *testing.T) {
	t.Run("denied grants pruned after rebuild", func(t *testing.T) {
		cloud := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode([]client.ServerToolSchema{
				{Name: "gmail_send_email", RequiresApproval: true},
				{Name: "notion_search"},
			})
		}))
		defer cloud.Close()

		deps := seedDeniedGrantDeps(t)
		deps.GW = client.NewGatewayClient(cloud.URL, "test-key")
		var recorded []config.MutationRevisions
		deps.RecordConfigMutation = func(r config.MutationRevisions) { recorded = append(recorded, r) }
		s := &Server{deps: deps, agentSyncTrigger: make(chan struct{}, 1)}

		// Alias the pre-prune backing array the way a lock-free Snapshot()
		// reader (e.g. config.Clone on an in-flight agent turn) would: the
		// prune must publish a fresh slice, never overwrite these elements
		// in place.
		seeded := deps.Config.Permissions.AlwaysAllowTools

		if err := s.RefreshIntegrationTools(context.Background()); err != nil {
			t.Fatalf("refresh: %v", err)
		}

		if len(seeded) != 2 || seeded[0] != "gmail_send_email" || seeded[1] != "file_write" {
			t.Errorf("prune mutated the previously published backing array in place: %v", seeded)
		}

		if got := readGlobalAlwaysAllowFromDisk(t, deps.ShannonDir); len(got) != 1 || got[0] != "file_write" {
			t.Errorf("global always_allow_tools on disk = %v, want [file_write]", got)
		}
		if got := deps.Config.Permissions.AlwaysAllowTools; len(got) != 1 || got[0] != "file_write" {
			t.Errorf("in-memory global mirror = %v, want [file_write]", got)
		}
		if got := readAlwaysAllowFromDisk(t, deps.AgentsDir, "operator"); len(got) != 1 || got[0] != "file_write" {
			t.Errorf("per-agent always_allow_tools = %v, want [file_write]", got)
		}
		if len(recorded) == 0 {
			t.Error("global prune must report its revision via RecordConfigMutation")
		}
		// A per-agent removal is a real local mutation (config.yaml mtime is
		// now ahead of Cloud's row) — it must request a sync push so the
		// pruned config converges upstream.
		select {
		case <-s.agentSyncTrigger:
		default:
			t.Error("per-agent prune must trigger an agent sync push")
		}
	})

	// The in-memory mirror may hold an entry the global config.yaml does not
	// (external hand-edit since load). The prune must only strip the mirror —
	// and only log/record — on evidence of an actual disk write: never claim
	// bytes it did not write.
	t.Run("mirror-only entry is not stripped without a disk write", func(t *testing.T) {
		cloud := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode([]client.ServerToolSchema{
				{Name: "gmail_send_email", RequiresApproval: true},
			})
		}))
		defer cloud.Close()

		deps := newDepsWithConfig(t, "operator")
		deps.ShannonDir = t.TempDir()
		deps.Registry = agent.NewToolRegistry()
		deps.Config.Cloud.Enabled = true
		deps.Config.APIKey = "test-key"
		deps.Config.Permissions.AlwaysAllowTools = []string{"gmail_send_email", "file_write"}
		// Disk carries only the ordinary grant — the denied entry exists in
		// memory alone.
		if _, err := config.AppendGlobalAlwaysAllowToolWithRevision(deps.ShannonDir, "file_write"); err != nil {
			t.Fatalf("seed global grant: %v", err)
		}
		deps.GW = client.NewGatewayClient(cloud.URL, "test-key")
		var recorded []config.MutationRevisions
		deps.RecordConfigMutation = func(r config.MutationRevisions) { recorded = append(recorded, r) }
		s := &Server{deps: deps}

		if err := s.RefreshIntegrationTools(context.Background()); err != nil {
			t.Fatalf("refresh: %v", err)
		}
		if got := deps.Config.Permissions.AlwaysAllowTools; len(got) != 2 {
			t.Errorf("in-memory mirror = %v, want untouched (no disk write happened)", got)
		}
		for _, r := range recorded {
			if r.After != "" {
				t.Errorf("recorded a mutation revision %+v for a write that never happened", r)
			}
		}
	})

	t.Run("empty catalog prunes nothing", func(t *testing.T) {
		cloud := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode([]client.ServerToolSchema{})
		}))
		defer cloud.Close()

		deps := seedDeniedGrantDeps(t)
		deps.GW = client.NewGatewayClient(cloud.URL, "test-key")
		s := &Server{deps: deps, agentSyncTrigger: make(chan struct{}, 1)}

		if err := s.RefreshIntegrationTools(context.Background()); err != nil {
			t.Fatalf("refresh: %v", err)
		}
		if got := readGlobalAlwaysAllowFromDisk(t, deps.ShannonDir); len(got) != 2 {
			t.Errorf("global always_allow_tools on disk = %v, want both kept", got)
		}
		if got := readAlwaysAllowFromDisk(t, deps.AgentsDir, "operator"); len(got) != 2 {
			t.Errorf("per-agent always_allow_tools = %v, want both kept", got)
		}
		select {
		case <-s.agentSyncTrigger:
			t.Error("no-op prune must not trigger an agent sync push")
		default:
		}
	})

	// Hand-edited config.yaml is the most likely place for a stale grant. A
	// wrong-typed UNRELATED sibling field must not hide the always-allow list
	// from the prune (the agents-package raw reader tolerates it; a typed
	// whole-file unmarshal would not).
	t.Run("hand-edited sibling field does not hide per-agent grants", func(t *testing.T) {
		cloud := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode([]client.ServerToolSchema{
				{Name: "gmail_send_email", RequiresApproval: true},
			})
		}))
		defer cloud.Close()

		deps := seedDeniedGrantDeps(t)
		handEdited := "auto_approve: [not, a, bool]\npermissions:\n  always_allow_tools:\n    - file_write\n    - gmail_send_email\n"
		if err := os.WriteFile(filepath.Join(deps.AgentsDir, "operator", "config.yaml"), []byte(handEdited), 0600); err != nil {
			t.Fatalf("write hand-edited config: %v", err)
		}
		deps.GW = client.NewGatewayClient(cloud.URL, "test-key")
		s := &Server{deps: deps}

		if err := s.RefreshIntegrationTools(context.Background()); err != nil {
			t.Fatalf("refresh: %v", err)
		}
		if got := readAlwaysAllowFromDisk(t, deps.AgentsDir, "operator"); len(got) != 1 || got[0] != "file_write" {
			t.Errorf("per-agent always_allow_tools = %v, want [file_write] despite the hand-edited sibling field", got)
		}
	})

	t.Run("failed rebuild prunes nothing", func(t *testing.T) {
		cloud := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			http.Error(w, "temporary outage", http.StatusBadGateway)
		}))
		defer cloud.Close()

		deps := seedDeniedGrantDeps(t)
		deps.GW = client.NewGatewayClient(cloud.URL, "test-key")
		s := &Server{deps: deps}

		if err := s.RefreshIntegrationTools(context.Background()); err == nil {
			t.Fatal("refresh should surface the list failure")
		}
		if got := readGlobalAlwaysAllowFromDisk(t, deps.ShannonDir); len(got) != 2 {
			t.Errorf("global always_allow_tools on disk = %v, want both kept", got)
		}
		if got := readAlwaysAllowFromDisk(t, deps.AgentsDir, "operator"); len(got) != 2 {
			t.Errorf("per-agent always_allow_tools = %v, want both kept", got)
		}
	})
}

// The cloud agent-sync pull is a fourth full-replace config write: a config
// pushed by an older device can carry a requires_approval integration grant.
// The pull must apply the same registry-based drop as the HTTP handlers; a
// registry miss keeps the entry (runtime gate + refresh prune backstop).
func TestPullAndApplyAgentsDropsDeniedAlwaysAllow(t *testing.T) {
	cloudUpdatedAt := time.Now().Add(-time.Hour).Truncate(time.Second)
	newPullItem := func() client.SyncAgentItem {
		return client.SyncAgentItem{
			AgentKey:  "puller",
			Prompt:    "cloud prompt",
			Config:    json.RawMessage(`{"permissions":{"always_allow_tools":["gmail_send_email","file_write"]}}`),
			UpdatedAt: cloudUpdatedAt,
		}
	}
	pullServer := func(t *testing.T, deps *ServerDeps) *Server {
		t.Helper()
		sc := NewSessionCache(filepath.Join(deps.AgentsDir, "_sessions"))
		t.Cleanup(func() { sc.CloseAll() })
		deps.SessionCache = sc
		return &Server{deps: deps, slugLocks: skills.NewSlugLocks()}
	}

	t.Run("denied entry dropped on pull", func(t *testing.T) {
		deps := newIntegrationRegistryDeps(t)
		srv := pullServer(t, deps)
		if err := srv.pullAndApplyAgents(func() ([]client.SyncAgentItem, error) {
			return []client.SyncAgentItem{newPullItem()}, nil
		}); err != nil {
			t.Fatalf("pull: %v", err)
		}
		got := readAlwaysAllowFromDisk(t, deps.AgentsDir, "puller")
		if len(got) != 1 || got[0] != "file_write" {
			t.Fatalf("pulled always_allow_tools = %v, want [file_write] only", got)
		}
		// A drop makes the written config diverge from Cloud's copy, so the
		// LWW clock must stay at "now" (strictly newer): the post-pull push is
		// then accepted by Cloud's strict-newer upsert and the sanitized
		// config converges upstream instead of leaving a stale cloud row.
		if mod := agentLastModified(filepath.Join(deps.AgentsDir, "puller")); !mod.After(cloudUpdatedAt) {
			t.Errorf("local LWW clock = %v, want strictly after cloud UpdatedAt %v after a drop", mod, cloudUpdatedAt)
		}
	})

	// The STATIC sanitize inside WriteAgentConfig (legacy GUI names) diverges
	// the written config from Cloud's copy exactly like the dynamic drop, so
	// it must also leave the LWW clock at "now" — otherwise the stale cloud
	// row carrying e.g. `applescript` keeps reseeding devices forever.
	t.Run("static legacy-GUI drop also skips the stamp", func(t *testing.T) {
		deps := newDepsWithConfig(t, "operator")
		srv := pullServer(t, deps)
		item := newPullItem()
		item.Config = json.RawMessage(`{"permissions":{"always_allow_tools":["applescript","file_write"]}}`)
		if err := srv.pullAndApplyAgents(func() ([]client.SyncAgentItem, error) {
			return []client.SyncAgentItem{item}, nil
		}); err != nil {
			t.Fatalf("pull: %v", err)
		}
		got := readAlwaysAllowFromDisk(t, deps.AgentsDir, "puller")
		if len(got) != 1 || got[0] != "file_write" {
			t.Fatalf("pulled always_allow_tools = %v, want [file_write] only", got)
		}
		if mod := agentLastModified(filepath.Join(deps.AgentsDir, "puller")); !mod.After(cloudUpdatedAt) {
			t.Errorf("local LWW clock = %v, want strictly after cloud UpdatedAt %v after a static drop", mod, cloudUpdatedAt)
		}
	})

	t.Run("registry miss keeps entry", func(t *testing.T) {
		deps := newDepsWithConfig(t, "operator")
		srv := pullServer(t, deps)
		if err := srv.pullAndApplyAgents(func() ([]client.SyncAgentItem, error) {
			return []client.SyncAgentItem{newPullItem()}, nil
		}); err != nil {
			t.Fatalf("pull: %v", err)
		}
		got := readAlwaysAllowFromDisk(t, deps.AgentsDir, "puller")
		if len(got) != 2 {
			t.Fatalf("pulled always_allow_tools = %v, want both entries kept on registry miss", got)
		}
		// No drop → byte mirror of Cloud's copy → mtimes stay stamped to the
		// cloud timestamp (LWW no-op on the next push, as before).
		if mod := agentLastModified(filepath.Join(deps.AgentsDir, "puller")); !mod.Equal(cloudUpdatedAt) {
			t.Errorf("local LWW clock = %v, want stamped to cloud UpdatedAt %v when nothing was dropped", mod, cloudUpdatedAt)
		}
	})
}

// The verified-principal transition (sign-in / account switch / key rotation)
// is the exact catalog-empty window that lets a denied grant persist, so its
// catalog rebuild must self-heal the same way RefreshIntegrationTools does.
// Sign-out clears the catalog without a rebuild and must prune nothing.
func TestResetIntegrationToolsForPrincipalPrunesDeniedAlwaysAllow(t *testing.T) {
	t.Run("prunes after principal rebuild", func(t *testing.T) {
		cloud := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode([]client.ServerToolSchema{
				{Name: "gmail_send_email", RequiresApproval: true},
			})
		}))
		defer cloud.Close()

		deps := seedDeniedGrantDeps(t)
		deps.GW = client.NewGatewayClient(cloud.URL, "test-key")
		s := &Server{deps: deps, agentSyncTrigger: make(chan struct{}, 1)}

		if err := s.resetIntegrationToolsForPrincipal(context.Background(), true); err != nil {
			t.Fatalf("reset: %v", err)
		}
		if got := readGlobalAlwaysAllowFromDisk(t, deps.ShannonDir); len(got) != 1 || got[0] != "file_write" {
			t.Errorf("global always_allow_tools on disk = %v, want [file_write]", got)
		}
		if got := readAlwaysAllowFromDisk(t, deps.AgentsDir, "operator"); len(got) != 1 || got[0] != "file_write" {
			t.Errorf("per-agent always_allow_tools = %v, want [file_write]", got)
		}
		// The principal-transition prune must NOT request a sync push:
		// agentPullClean survives an account switch (set-once, startup-only
		// pull), so a full_sync push here would upload the PREVIOUS account's
		// local agent set and soft-delete the new account's cloud-only agents.
		// These grants converge on the next ordinary refresh instead.
		select {
		case <-s.agentSyncTrigger:
			t.Error("principal-transition prune must not trigger an agent sync push")
		default:
		}
	})

	t.Run("sign-out prunes nothing", func(t *testing.T) {
		deps := seedDeniedGrantDeps(t)
		deps.GW = client.NewGatewayClient("http://127.0.0.1:1", "test-key")
		s := &Server{deps: deps}

		if err := s.resetIntegrationToolsForPrincipal(context.Background(), false); err != nil {
			t.Fatalf("reset: %v", err)
		}
		if got := readGlobalAlwaysAllowFromDisk(t, deps.ShannonDir); len(got) != 2 {
			t.Errorf("global always_allow_tools on disk = %v, want both kept", got)
		}
		if got := readAlwaysAllowFromDisk(t, deps.AgentsDir, "operator"); len(got) != 2 {
			t.Errorf("per-agent always_allow_tools = %v, want both kept", got)
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
