package daemon

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Kocoro-lab/ShanClaw/internal/agents"
	"github.com/Kocoro-lab/ShanClaw/internal/config"
	"gopkg.in/yaml.v3"
)

// newDepsWithConfig returns ServerDeps that has both an isolated agentsDir
// (with one named agent) and a real *config.Config so the legacy
// allowed_commands write path can mutate it.
func newDepsWithConfig(t *testing.T, agentName string) *ServerDeps {
	t.Helper()
	deps := setupDepsWithAgent(t, agentName)
	deps.ShannonDir = filepath.Dir(deps.AgentsDir)
	deps.Config = &config.Config{}
	return deps
}

// setupDepsWithAgent creates an isolated agentsDir with one agent and returns
// ServerDeps wired with an EventBus.
func setupDepsWithAgent(t *testing.T, agentName string) *ServerDeps {
	t.Helper()
	root := t.TempDir()
	agentDir := filepath.Join(root, agentName)
	if err := os.MkdirAll(agentDir, 0700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(agentDir, "AGENT.md"), []byte("test agent"), 0600); err != nil {
		t.Fatalf("AGENT.md: %v", err)
	}
	return &ServerDeps{
		AgentsDir: root,
		EventBus:  NewEventBus(),
	}
}

// readAlwaysAllowFromDisk returns the tool names persisted in the agent's
// config.yaml. Returns nil if config.yaml doesn't exist or the field is absent.
func readAlwaysAllowFromDisk(t *testing.T, agentsDir, agentName string) []string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(agentsDir, agentName, "config.yaml"))
	if err != nil {
		return nil
	}
	var raw map[string]interface{}
	if err := yaml.Unmarshal(data, &raw); err != nil {
		t.Fatalf("parse config: %v", err)
	}
	perms, ok := raw["permissions"].(map[string]interface{})
	if !ok {
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

func TestPersistAgentAlwaysAllow_HappyPath(t *testing.T) {
	deps := setupDepsWithAgent(t, "writer")
	ch := deps.EventBus.Subscribe()
	defer deps.EventBus.Unsubscribe(ch)

	ok := PersistAgentAlwaysAllow(deps, "writer", "file_write")
	if !ok {
		t.Fatal("expected true for successful persist")
	}
	got := readAlwaysAllowFromDisk(t, deps.AgentsDir, "writer")
	if len(got) != 1 || got[0] != "file_write" {
		t.Errorf("disk state: %v; want [file_write]", got)
	}
	// No notice on success.
	select {
	case evt := <-ch:
		t.Errorf("unexpected event on success path: %+v", evt)
	case <-time.After(50 * time.Millisecond):
	}
}

// TestPersistAgentAlwaysAllow_FormerlyHighRiskNowPersists pins the 2026-05-18
// policy change. publish_to_web / generate_image / edit_image USED to be
// rejected with a warn notice; they now persist like any other tool. The
// rejection plumbing (EventApprovalNotice + ErrToolNotPersistable) is
// preserved so a future tool that genuinely cannot be persisted can use it
// — but no tool occupies that slot today.
func TestPersistAgentAlwaysAllow_FormerlyHighRiskNowPersists(t *testing.T) {
	deps := setupDepsWithAgent(t, "risky")
	ch := deps.EventBus.Subscribe()
	defer deps.EventBus.Unsubscribe(ch)

	for _, tool := range []string{"publish_to_web", "generate_image", "edit_image"} {
		if ok := PersistAgentAlwaysAllow(deps, "risky", tool); !ok {
			t.Errorf("%s should now persist (deny-list is empty)", tool)
		}
		got := readAlwaysAllowFromDisk(t, deps.AgentsDir, "risky")
		found := false
		for _, t := range got {
			if t == tool {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("%s should be persisted to disk; got %v", tool, got)
		}

		// No warn notice should fire on the success path. Drain anything else
		// that might have accumulated (e.g. cumulative add events from a prior
		// iteration in this loop) by giving a brief window with strict assertion.
		select {
		case evt := <-ch:
			if evt.Type == EventApprovalNotice {
				var payload struct {
					Severity string `json:"severity"`
					Message  string `json:"message"`
				}
				_ = json.Unmarshal(evt.Payload, &payload)
				if payload.Severity == "warn" {
					t.Errorf("%s: unexpected warn notice on success path: %+v", tool, payload)
				}
			}
		case <-time.After(50 * time.Millisecond):
			// Expected: no notice on success.
		}
	}
}

func TestPersistAgentAlwaysAllow_EmptyAgentFallsBack(t *testing.T) {
	deps := setupDepsWithAgent(t, "x")
	ch := deps.EventBus.Subscribe()
	defer deps.EventBus.Unsubscribe(ch)

	if ok := PersistAgentAlwaysAllow(deps, "", "file_write"); ok {
		t.Error("empty agent name should not persist")
	}
	// No notice for the empty-agent fallback (logs only).
	select {
	case evt := <-ch:
		t.Errorf("unexpected event on empty-agent path: %+v", evt)
	case <-time.After(50 * time.Millisecond):
	}
}

func TestPersistAgentAlwaysAllow_WriteFailureDegrades(t *testing.T) {
	// Force a write failure by pointing AgentsDir at a path the agents helper
	// will reject. Easiest reliable trigger: invalid agent name (validation
	// fires inside AppendAlwaysAllowTool before any IO).
	deps := setupDepsWithAgent(t, "valid")
	deps.EventBus = NewEventBus() // fresh
	ch := deps.EventBus.Subscribe()
	defer deps.EventBus.Unsubscribe(ch)

	// Invalid name triggers ValidateAgentName failure → non-Errors.Is path.
	if ok := PersistAgentAlwaysAllow(deps, "Bad Name", "file_write"); ok {
		t.Error("expected false on validation failure")
	}
	// Expect a "could not save" warn notice (generic write-failure path).
	select {
	case evt := <-ch:
		if evt.Type != EventApprovalNotice {
			t.Errorf("expected %s, got %s", EventApprovalNotice, evt.Type)
		}
	case <-time.After(100 * time.Millisecond):
		t.Error("expected approval_notice on write failure")
	}
}

func TestPersistAgentAlwaysAllow_NilDepsSafe(t *testing.T) {
	if ok := PersistAgentAlwaysAllow(nil, "x", "file_write"); ok {
		t.Error("nil deps should return false")
	}
	// Also robust to nil EventBus (helper still returns correctly).
	deps := setupDepsWithAgent(t, "y")
	deps.EventBus = nil
	if ok := PersistAgentAlwaysAllow(deps, "", "file_write"); ok {
		t.Error("empty agent + nil bus should return false")
	}
}

func TestPersistAgentAlwaysAllow_EmptyToolRejected(t *testing.T) {
	deps := setupDepsWithAgent(t, "z")
	if ok := PersistAgentAlwaysAllow(deps, "z", ""); ok {
		t.Error("empty tool name should not persist")
	}
}

// TestPersistAgentAlwaysAllow_Idempotent verifies that two successive
// persists of the same tool do not duplicate the disk entry. This guards the
// "button clicked twice" UX path.
func TestPersistAgentAlwaysAllow_Idempotent(t *testing.T) {
	deps := setupDepsWithAgent(t, "idem")
	if !PersistAgentAlwaysAllow(deps, "idem", "http") {
		t.Fatal("first call should succeed")
	}
	if !PersistAgentAlwaysAllow(deps, "idem", "http") {
		t.Fatal("second call should succeed (idempotent)")
	}
	got := readAlwaysAllowFromDisk(t, deps.AgentsDir, "idem")
	if len(got) != 1 {
		t.Errorf("expected 1 entry after duplicate call, got %d: %v", len(got), got)
	}
}

// TestHandleAlwaysAllowDecision_BashNamedAgent verifies the PR 5 behavior:
// clicking Always Allow on a bash invocation under a named agent writes the
// tool name (not the command) to permissions.always_allow_tools, the broker's
// in-memory set is updated, AND no entry is written to the global
// allowed_commands list (which is the legacy command-string path).
func TestHandleAlwaysAllowDecision_BashNamedAgent(t *testing.T) {
	deps := newDepsWithConfig(t, "writer")
	broker := NewApprovalBroker(func(req ApprovalRequest) error { return nil })

	HandleAlwaysAllowDecision(deps, broker, "writer", "bash",
		`{"command":"find /tmp -name '*.log' | head -5"}`)

	// (a) persisted to per-agent always_allow_tools
	got := readAlwaysAllowFromDisk(t, deps.AgentsDir, "writer")
	if len(got) != 1 || got[0] != "bash" {
		t.Errorf("expected per-agent always_allow_tools=[bash], got %v", got)
	}
	// (b) broker honors this session immediately
	if !broker.IsToolAutoApproved("bash") {
		t.Error("broker should auto-approve bash for the rest of this session")
	}
	// (c) NO entry written to global allowed_commands
	if len(deps.Config.Permissions.AllowedCommands) != 0 {
		t.Errorf("global allowed_commands should be untouched for named-agent bash, got: %v",
			deps.Config.Permissions.AllowedCommands)
	}
}

// TestHandleAlwaysAllowDecision_BashDefaultAgent verifies the legacy
// fallback: default agent (agentName == "") still writes to global
// allowed_commands at command granularity. Future PR will lift this to a
// global tool-level field.
// TestHandleAlwaysAllowDecision_BashDefaultAgent_GlobalToolLevel covers the
// PR 6 behavior: Default agent + bash + safe command now writes "bash" to
// the GLOBAL permissions.always_allow_tools (tool-level), NOT the legacy
// command-level allowed_commands. This is the user-visible fix for the
// "non-technical user on default agent keeps getting prompted because each
// bash command string is different" issue.
func TestHandleAlwaysAllowDecision_BashDefaultAgent_GlobalToolLevel(t *testing.T) {
	deps := newDepsWithConfig(t, "ignored")
	broker := NewApprovalBroker(func(req ApprovalRequest) error { return nil })

	// Genuinely-prompted command (not default-safe), agent name empty
	// (== Default agent path).
	HandleAlwaysAllowDecision(deps, broker, "", "bash",
		`{"command":"find /Users/me -name '*.pdf' -delete"}`)

	// (a) "bash" persisted to GLOBAL always_allow_tools — both in-memory and
	// on disk via config.AppendGlobalAlwaysAllowTool.
	foundInMem := false
	for _, t := range deps.Config.Permissions.AlwaysAllowTools {
		if t == "bash" {
			foundInMem = true
			break
		}
	}
	if !foundInMem {
		t.Errorf("expected 'bash' in deps.Config.Permissions.AlwaysAllowTools, got: %v",
			deps.Config.Permissions.AlwaysAllowTools)
	}
	cfgData, _ := os.ReadFile(filepath.Join(deps.ShannonDir, "config.yaml"))
	if !strings.Contains(string(cfgData), "always_allow_tools") || !strings.Contains(string(cfgData), "bash") {
		t.Errorf("global config.yaml should contain 'bash' under always_allow_tools, got:\n%s", cfgData)
	}

	// (b) Broker honors immediately for this session.
	if !broker.IsToolAutoApproved("bash") {
		t.Error("broker should auto-approve bash for the rest of the session")
	}

	// (c) Legacy allowed_commands is NOT touched — the command-level path
	// is fully retired on PR 6. (Users who previously had allowed_commands
	// entries still benefit from them via permissions.CheckToolCall.)
	if len(deps.Config.Permissions.AllowedCommands) != 0 {
		t.Errorf("PR 6 should write tool-level only; allowed_commands stays empty, got: %v",
			deps.Config.Permissions.AllowedCommands)
	}
}

// TestHandleAlwaysAllowDecision_NonBashDefaultAgent_GlobalToolLevel is the
// regression for the "default agent + non-bash tool, click Always Allow twice
// but every message re-prompts" bug. SSE handler creates a fresh broker per
// request (server.go:1218), so broker.SetToolAutoApprove alone evaporates
// after the message returns. The fix routes default-agent + non-bash through
// the same global persistence path bash already uses (PR 6).
func TestHandleAlwaysAllowDecision_NonBashDefaultAgent_GlobalToolLevel(t *testing.T) {
	deps := newDepsWithConfig(t, "ignored")
	broker := NewApprovalBroker(func(req ApprovalRequest) error { return nil })

	HandleAlwaysAllowDecision(deps, broker, "", "file_write",
		`{"path":"/tmp/x.html","content":"<html></html>","description":"creates test landing page"}`)

	// Disk: global always_allow_tools contains file_write.
	cfgData, _ := os.ReadFile(filepath.Join(deps.ShannonDir, "config.yaml"))
	if !strings.Contains(string(cfgData), "always_allow_tools") ||
		!strings.Contains(string(cfgData), "file_write") {
		t.Errorf("expected global config.yaml to contain file_write under always_allow_tools, got:\n%s", cfgData)
	}
	// In-memory mirror updated.
	foundInMem := false
	for _, tool := range deps.Config.Permissions.AlwaysAllowTools {
		if tool == "file_write" {
			foundInMem = true
			break
		}
	}
	if !foundInMem {
		t.Errorf("in-memory mirror missing file_write, got: %v", deps.Config.Permissions.AlwaysAllowTools)
	}
	// Broker honors immediately (same-message effect).
	if !broker.IsToolAutoApproved("file_write") {
		t.Error("broker should auto-approve file_write for the rest of this session")
	}
}

// TestHandleAlwaysAllowDecision_NonBashDefaultAgent_FormerlyHighRiskPersists
// pins the 2026-05-18 policy change for the global-default path. publish_to_web
// used to be globally non-persistable with a warn notice; it now persists
// like any other tool. (See autoApprovalDenyList trade-off notes.)
func TestHandleAlwaysAllowDecision_NonBashDefaultAgent_FormerlyHighRiskPersists(t *testing.T) {
	deps := newDepsWithConfig(t, "ignored")
	broker := NewApprovalBroker(func(req ApprovalRequest) error { return nil })
	ch := deps.EventBus.Subscribe()
	defer deps.EventBus.Unsubscribe(ch)

	HandleAlwaysAllowDecision(deps, broker, "", "publish_to_web",
		`{"path":"/tmp/x.html","purpose":"share with user"}`)

	found := false
	for _, t := range deps.Config.Permissions.AlwaysAllowTools {
		if t == "publish_to_web" {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("publish_to_web should now persist globally, got: %v",
			deps.Config.Permissions.AlwaysAllowTools)
	}
	if !broker.IsToolAutoApproved("publish_to_web") {
		t.Error("publish_to_web should be in broker auto-approve set after persistence")
	}
	// No warn notice expected on success.
	select {
	case evt := <-ch:
		if evt.Type == EventApprovalNotice {
			var payload struct {
				Severity string `json:"severity"`
			}
			_ = json.Unmarshal(evt.Payload, &payload)
			if payload.Severity == "warn" {
				t.Errorf("unexpected warn notice on success path: %+v", payload)
			}
		}
	case <-time.After(50 * time.Millisecond):
		// Expected: no notice.
	}
}

// TestHandleAlwaysAllowDecision_BashHighRiskNotPersisted covers the always-ask
// gate: pip install / rm -rf / python -c etc. must never persist regardless
// of agent context, and an EventApprovalNotice is emitted.
func TestHandleAlwaysAllowDecision_BashHighRiskNotPersisted(t *testing.T) {
	deps := newDepsWithConfig(t, "writer")
	broker := NewApprovalBroker(func(req ApprovalRequest) error { return nil })
	ch := deps.EventBus.Subscribe()
	defer deps.EventBus.Unsubscribe(ch)

	HandleAlwaysAllowDecision(deps, broker, "writer", "bash",
		`{"command":"pip install requests"}`)

	// Nothing persisted to per-agent file
	if got := readAlwaysAllowFromDisk(t, deps.AgentsDir, "writer"); len(got) != 0 {
		t.Errorf("high-risk bash should not persist to agent config, got: %v", got)
	}
	// Nothing persisted to global allowed_commands
	if len(deps.Config.Permissions.AllowedCommands) != 0 {
		t.Errorf("high-risk bash should not persist to global allowed_commands, got: %v",
			deps.Config.Permissions.AllowedCommands)
	}
	// broker NOT updated (high-risk also blocks session-level auto-approve)
	if broker.IsToolAutoApproved("bash") {
		t.Error("broker should not auto-approve bash after high-risk rejection")
	}
	// Notice emitted
	select {
	case evt := <-ch:
		if evt.Type != EventApprovalNotice {
			t.Errorf("expected %s, got %s", EventApprovalNotice, evt.Type)
		}
	case <-time.After(100 * time.Millisecond):
		t.Error("expected approval_notice on high-risk rejection")
	}
}

// TestAlwaysAllowNotice_I18nStructuredPayload verifies the notice payload
// shape used by UI clients for localization. Daemon emits a stable `code`
// plus `tool` so the UI can render its own localized string; `message` is
// an English fallback for older clients that don't recognize the code.
func TestAlwaysAllowNotice_I18nStructuredPayload(t *testing.T) {
	deps := newDepsWithConfig(t, "ignored")
	broker := NewApprovalBroker(func(req ApprovalRequest) error { return nil })
	ch := deps.EventBus.Subscribe()
	defer deps.EventBus.Unsubscribe(ch)

	type caseT struct {
		name     string
		invoke   func()
		wantCode string
		wantTool string
	}
	// 2026-05-18 update: publish_to_web / generate_image / edit_image used to
	// be on autoApprovalDenyList and produced NoticeCodeHighRiskNotPersistable
	// here. The deny-list is now empty so they persist silently like any
	// other tool; only the bash always-ask path still emits a structured notice.
	// The NoticeCodeHighRiskNotPersistable constant + payload shape stay in
	// place for the next time a tool needs that behavior.
	cases := []caseT{
		{
			name: "bash always-ask command",
			invoke: func() {
				HandleAlwaysAllowDecision(deps, broker, "writer", "bash",
					`{"command":"pip install requests","description":"install requests"}`)
			},
			wantCode: NoticeCodeBashAlwaysAskNotPersisted,
			wantTool: "bash",
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			c.invoke()
			select {
			case evt := <-ch:
				if evt.Type != EventApprovalNotice {
					t.Fatalf("expected %s, got %s", EventApprovalNotice, evt.Type)
				}
				var p AlwaysAllowNoticePayload
				if err := json.Unmarshal(evt.Payload, &p); err != nil {
					t.Fatalf("unmarshal payload: %v", err)
				}
				if p.Code != c.wantCode {
					t.Errorf("code = %q, want %q", p.Code, c.wantCode)
				}
				if p.Tool != c.wantTool {
					t.Errorf("tool = %q, want %q", p.Tool, c.wantTool)
				}
				if p.Severity != "warn" {
					t.Errorf("severity = %q, want warn", p.Severity)
				}
				if p.Message == "" {
					t.Error("message should fall back to English text for old clients")
				}
			case <-time.After(200 * time.Millisecond):
				t.Fatal("timeout waiting for approval notice")
			}
		})
	}
}

// TestPersistAgentAlwaysAllow_RoundTripViaAgentsPackage verifies that what we
// write through the daemon helper is what AgentLoop will read back via the
// agents.LoadAgent path (which PR 2 wires into the loop).
func TestPersistAgentAlwaysAllow_RoundTripViaAgentsPackage(t *testing.T) {
	deps := setupDepsWithAgent(t, "rt")
	if !PersistAgentAlwaysAllow(deps, "rt", "file_write") {
		t.Fatal("persist failed")
	}
	if !PersistAgentAlwaysAllow(deps, "rt", "http") {
		t.Fatal("persist failed")
	}

	loaded, err := agents.LoadAgent(deps.AgentsDir, "rt")
	if err != nil {
		t.Fatalf("LoadAgent: %v", err)
	}
	if loaded.Config == nil || loaded.Config.Permissions == nil {
		t.Fatalf("expected Permissions in loaded config, got: %+v", loaded.Config)
	}
	got := loaded.Config.Permissions.AlwaysAllowTools
	want := map[string]bool{"file_write": true, "http": true}
	for _, tool := range got {
		if !want[tool] {
			t.Errorf("unexpected tool: %s", tool)
		}
		delete(want, tool)
	}
	if len(want) > 0 {
		t.Errorf("missing tools after round-trip: %v", want)
	}
}
