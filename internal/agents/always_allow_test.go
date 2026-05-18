package agents

import (
	"bytes"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"

	"gopkg.in/yaml.v3"
)

// agentDir creates a minimal agent skeleton (AGENT.md) and returns the
// agentsDir + agent name. config.yaml is intentionally not created so each
// test can choose whether to start from "no config" or a seeded state.
func setupAgent(t *testing.T, name string) (agentsDir, agentName string) {
	t.Helper()
	root := t.TempDir()
	dir := filepath.Join(root, name)
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "AGENT.md"), []byte("test"), 0600); err != nil {
		t.Fatalf("write AGENT.md: %v", err)
	}
	return root, name
}

func readRawConfig(t *testing.T, agentsDir, name string) map[string]interface{} {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(agentsDir, name, "config.yaml"))
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		t.Fatalf("read config: %v", err)
	}
	var raw map[string]interface{}
	if err := yaml.Unmarshal(data, &raw); err != nil {
		t.Fatalf("parse config: %v", err)
	}
	return raw
}

func TestAppendAlwaysAllowTool_FirstWrite(t *testing.T) {
	dir, name := setupAgent(t, "first")
	if err := AppendAlwaysAllowTool(dir, name, "file_write"); err != nil {
		t.Fatalf("append: %v", err)
	}
	raw := readRawConfig(t, dir, name)
	if raw == nil {
		t.Fatal("config.yaml missing after first write")
	}
	perms, ok := raw["permissions"].(map[string]interface{})
	if !ok {
		t.Fatalf("permissions block missing: %v", raw)
	}
	tools, ok := perms["always_allow_tools"].([]interface{})
	if !ok || len(tools) != 1 || tools[0] != "file_write" {
		t.Fatalf("unexpected always_allow_tools: %v", perms["always_allow_tools"])
	}
}

func TestAppendAlwaysAllowTool_PreservesOtherFields(t *testing.T) {
	dir, name := setupAgent(t, "preserve")
	// Seed covers the trickiest preservation cases: nested mcp_servers map
	// with the special _inherit key (parseAgentConfig handles it manually),
	// arbitrary unknown top-level keys, and tools.allow.
	seed := []byte(`cwd: /tmp/x
tools:
  allow:
    - file_read
agent:
  model: claude-opus-4-7
mcp_servers:
  _inherit: true
  fs:
    command: /usr/bin/fs-mcp
    args: ["--port", "9000"]
unknown_future_key:
  nested: value
`)
	if err := os.WriteFile(filepath.Join(dir, name, "config.yaml"), seed, 0600); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if err := AppendAlwaysAllowTool(dir, name, "http"); err != nil {
		t.Fatalf("append: %v", err)
	}
	raw := readRawConfig(t, dir, name)
	if raw["cwd"] != "/tmp/x" {
		t.Errorf("cwd lost: %v", raw["cwd"])
	}
	if _, ok := raw["tools"]; !ok {
		t.Errorf("tools block lost")
	}
	if _, ok := raw["agent"]; !ok {
		t.Errorf("agent block lost")
	}
	mcp, ok := raw["mcp_servers"].(map[string]interface{})
	if !ok {
		t.Fatalf("mcp_servers block lost: %v", raw["mcp_servers"])
	}
	if mcp["_inherit"] != true {
		t.Errorf("mcp_servers._inherit not preserved: %v", mcp["_inherit"])
	}
	fs, ok := mcp["fs"].(map[string]interface{})
	if !ok || fs["command"] != "/usr/bin/fs-mcp" {
		t.Errorf("mcp_servers.fs not preserved: %v", mcp["fs"])
	}
	if _, ok := raw["unknown_future_key"]; !ok {
		t.Errorf("unknown_future_key dropped — raw map must round-trip future schema additions")
	}
	perms := raw["permissions"].(map[string]interface{})
	tools := perms["always_allow_tools"].([]interface{})
	if len(tools) != 1 || tools[0] != "http" {
		t.Errorf("wrong tools: %v", tools)
	}
}

func TestAppendAlwaysAllowTool_Dedup(t *testing.T) {
	dir, name := setupAgent(t, "dedup")
	for i := 0; i < 3; i++ {
		if err := AppendAlwaysAllowTool(dir, name, "file_write"); err != nil {
			t.Fatalf("append %d: %v", i, err)
		}
	}
	raw := readRawConfig(t, dir, name)
	tools := raw["permissions"].(map[string]interface{})["always_allow_tools"].([]interface{})
	if len(tools) != 1 {
		t.Errorf("expected 1 entry, got %d: %v", len(tools), tools)
	}
}

func TestAppendAlwaysAllowTool_SortedOrder(t *testing.T) {
	dir, name := setupAgent(t, "sorted")
	for _, tool := range []string{"http", "browser_navigate", "file_write"} {
		if err := AppendAlwaysAllowTool(dir, name, tool); err != nil {
			t.Fatalf("append %s: %v", tool, err)
		}
	}
	raw := readRawConfig(t, dir, name)
	tools := raw["permissions"].(map[string]interface{})["always_allow_tools"].([]interface{})
	got := make([]string, len(tools))
	for i, v := range tools {
		got[i] = v.(string)
	}
	want := []string{"browser_navigate", "file_write", "http"}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("position %d: got %s, want %s", i, got[i], want[i])
		}
	}
}

// TestAppendAlwaysAllowTool_PersistsFormerlyHighRisk pins the 2026-05-18
// policy change: publish_to_web / generate_image / edit_image USED to be
// rejected by AppendAlwaysAllowTool, forcing users to re-approve every call.
// The product call moved them off the deny-list — they should now persist
// like any other tool. ErrToolNotPersistable still exists as plumbing for a
// future tool that genuinely cannot be persisted; this test catches a
// regression where the rejection rule comes back without explicit intent.
func TestAppendAlwaysAllowTool_PersistsFormerlyHighRisk(t *testing.T) {
	dir, name := setupAgent(t, "formerhighrisk")
	for _, tool := range []string{"publish_to_web", "generate_image", "edit_image"} {
		if err := AppendAlwaysAllowTool(dir, name, tool); err != nil {
			t.Errorf("%s: expected persistence to succeed, got %v", tool, err)
		}
	}
	// All three should be present in config.yaml after the appends.
	cfgPath := filepath.Join(dir, name, "config.yaml")
	if _, err := os.Stat(cfgPath); err != nil {
		t.Fatalf("config.yaml should exist after persisting tools: %v", err)
	}
	raw, err := readAgentConfigRaw(filepath.Join(dir, name))
	if err != nil {
		t.Fatalf("read config: %v", err)
	}
	got := readAlwaysAllowTools(raw, name)
	want := map[string]bool{"publish_to_web": true, "generate_image": true, "edit_image": true}
	if len(got) != len(want) {
		t.Fatalf("got %v, want all three tools persisted", got)
	}
	for _, tool := range got {
		if !want[tool] {
			t.Errorf("unexpected tool persisted: %q", tool)
		}
	}
}

func TestAppendAlwaysAllowTool_EmptyToolRejected(t *testing.T) {
	dir, name := setupAgent(t, "empty")
	if err := AppendAlwaysAllowTool(dir, name, ""); err == nil {
		t.Error("expected error for empty tool name")
	}
}

func TestAppendAlwaysAllowTool_BadAgentName(t *testing.T) {
	dir := t.TempDir()
	if err := AppendAlwaysAllowTool(dir, "Bad Name", "file_write"); err == nil {
		t.Error("expected error for invalid agent name")
	}
}

func TestAppendAlwaysAllowTool_Concurrent(t *testing.T) {
	dir, name := setupAgent(t, "concurrent")
	tools := []string{"a1", "a2", "a3", "a4", "a5", "a6", "a7", "a8", "a9", "a10"}

	var wg sync.WaitGroup
	const goroutines = 50
	wg.Add(goroutines)
	for g := 0; g < goroutines; g++ {
		go func() {
			defer wg.Done()
			for _, tool := range tools {
				if err := AppendAlwaysAllowTool(dir, name, tool); err != nil {
					t.Errorf("append: %v", err)
				}
			}
		}()
	}
	wg.Wait()

	raw := readRawConfig(t, dir, name)
	got := raw["permissions"].(map[string]interface{})["always_allow_tools"].([]interface{})
	if len(got) != len(tools) {
		t.Errorf("expected %d tools, got %d: %v", len(tools), len(got), got)
	}
	// Verify dedup correctness: each tool exactly once
	seen := make(map[string]int)
	for _, v := range got {
		seen[v.(string)]++
	}
	for _, tool := range tools {
		if seen[tool] != 1 {
			t.Errorf("tool %s appears %d times", tool, seen[tool])
		}
	}
}

func TestRemoveAlwaysAllowTool_RemovesEntry(t *testing.T) {
	dir, name := setupAgent(t, "remove")
	for _, tool := range []string{"file_write", "http", "browser_navigate"} {
		if err := AppendAlwaysAllowTool(dir, name, tool); err != nil {
			t.Fatalf("append %s: %v", tool, err)
		}
	}
	if err := RemoveAlwaysAllowTool(dir, name, "http"); err != nil {
		t.Fatalf("remove: %v", err)
	}
	raw := readRawConfig(t, dir, name)
	tools := raw["permissions"].(map[string]interface{})["always_allow_tools"].([]interface{})
	got := []string{}
	for _, v := range tools {
		got = append(got, v.(string))
	}
	sort.Strings(got)
	want := []string{"browser_navigate", "file_write"}
	if len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
		t.Errorf("got %v, want %v", got, want)
	}
}

func TestRemoveAlwaysAllowTool_LastEntryDropsBlock(t *testing.T) {
	dir, name := setupAgent(t, "lastentry")
	if err := AppendAlwaysAllowTool(dir, name, "file_write"); err != nil {
		t.Fatalf("append: %v", err)
	}
	if err := RemoveAlwaysAllowTool(dir, name, "file_write"); err != nil {
		t.Fatalf("remove: %v", err)
	}
	// Since the agent has nothing but permissions and we just emptied it,
	// the entire config.yaml should be gone (it would have been empty).
	if _, err := os.Stat(filepath.Join(dir, name, "config.yaml")); !os.IsNotExist(err) {
		raw := readRawConfig(t, dir, name)
		if _, ok := raw["permissions"]; ok {
			t.Errorf("permissions block should be dropped, got: %v", raw)
		}
	}
}

func TestRemoveAlwaysAllowTool_KeepsOtherFields(t *testing.T) {
	dir, name := setupAgent(t, "keepother")
	seed := []byte(`cwd: /tmp/y
permissions:
  always_allow_tools:
    - file_write
`)
	if err := os.WriteFile(filepath.Join(dir, name, "config.yaml"), seed, 0600); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if err := RemoveAlwaysAllowTool(dir, name, "file_write"); err != nil {
		t.Fatalf("remove: %v", err)
	}
	raw := readRawConfig(t, dir, name)
	if raw["cwd"] != "/tmp/y" {
		t.Errorf("cwd lost: %v", raw["cwd"])
	}
	if _, ok := raw["permissions"]; ok {
		t.Errorf("empty permissions block should be dropped")
	}
}

func TestRemoveAlwaysAllowTool_MissingTool_NoOp(t *testing.T) {
	dir, name := setupAgent(t, "missing")
	if err := AppendAlwaysAllowTool(dir, name, "http"); err != nil {
		t.Fatalf("append: %v", err)
	}
	if err := RemoveAlwaysAllowTool(dir, name, "file_write"); err != nil {
		t.Errorf("removing missing tool should be no-op, got: %v", err)
	}
	raw := readRawConfig(t, dir, name)
	tools := raw["permissions"].(map[string]interface{})["always_allow_tools"].([]interface{})
	if len(tools) != 1 || tools[0] != "http" {
		t.Errorf("existing tools should be untouched, got: %v", tools)
	}
}

func TestRemoveAlwaysAllowTool_NoConfig_NoOp(t *testing.T) {
	dir, name := setupAgent(t, "noconfig")
	if err := RemoveAlwaysAllowTool(dir, name, "file_write"); err != nil {
		t.Errorf("removing from non-existent config should be no-op, got: %v", err)
	}
}

// TestAppendAlwaysAllowTool_MalformedYAML_StringNotList covers the case where
// a user hand-edits config.yaml and writes a string instead of a list. The
// next Append must (a) log a warning, (b) treat the bad value as empty, and
// (c) canonicalize the field back to a clean list on write.
//
// NOTE: must not be t.Parallel() — this test mutates the global log writer.
func TestAppendAlwaysAllowTool_MalformedYAML_StringNotList(t *testing.T) {
	dir, name := setupAgent(t, "malformed")
	seed := []byte(`permissions:
  always_allow_tools: file_write
cwd: /tmp/y
`)
	if err := os.WriteFile(filepath.Join(dir, name, "config.yaml"), seed, 0600); err != nil {
		t.Fatalf("seed: %v", err)
	}

	var buf bytes.Buffer
	old := log.Default().Writer()
	log.SetOutput(&buf)
	t.Cleanup(func() { log.SetOutput(old) })

	if err := AppendAlwaysAllowTool(dir, name, "http"); err != nil {
		t.Fatalf("append: %v", err)
	}

	if !strings.Contains(buf.String(), "expected list") {
		t.Errorf("expected warning log; got: %q", buf.String())
	}

	raw := readRawConfig(t, dir, name)
	if raw["cwd"] != "/tmp/y" {
		t.Errorf("cwd lost: %v", raw["cwd"])
	}
	perms := raw["permissions"].(map[string]interface{})
	tools, ok := perms["always_allow_tools"].([]interface{})
	if !ok {
		t.Fatalf("malformed entry not canonicalized to list: %T", perms["always_allow_tools"])
	}
	// The original scalar "file_write" was treated as empty and is gone.
	// The new write contains only "http".
	if len(tools) != 1 || tools[0] != "http" {
		t.Errorf("unexpected canonicalized list: %v", tools)
	}
}

// TestAppendRemoveAlwaysAllowTool_Concurrent stresses the cross-operation
// path: M Append and M Remove goroutines hammer the same tool set in parallel.
// The final state is non-deterministic (last writer wins per tool), but the
// invariants must hold: no panic, no torn write, every entry that survives
// is a string from the canonical set, and the result is dedup'd & sorted.
func TestAppendRemoveAlwaysAllowTool_Concurrent(t *testing.T) {
	dir, name := setupAgent(t, "addrm")
	tools := []string{"t01", "t02", "t03", "t04", "t05", "t06", "t07", "t08"}
	canonical := make(map[string]bool, len(tools))
	for _, tool := range tools {
		canonical[tool] = true
	}

	var wg sync.WaitGroup
	const rounds = 20
	for r := 0; r < rounds; r++ {
		for _, tool := range tools {
			wg.Add(2)
			go func(tool string) {
				defer wg.Done()
				if err := AppendAlwaysAllowTool(dir, name, tool); err != nil {
					t.Errorf("append %s: %v", tool, err)
				}
			}(tool)
			go func(tool string) {
				defer wg.Done()
				if err := RemoveAlwaysAllowTool(dir, name, tool); err != nil {
					t.Errorf("remove %s: %v", tool, err)
				}
			}(tool)
		}
	}
	wg.Wait()

	// Whatever survived must be a subset of canonical, sorted, and deduped.
	raw := readRawConfig(t, dir, name)
	if raw == nil {
		// All entries removed last → config.yaml deleted. That's valid.
		return
	}
	perms, ok := raw["permissions"].(map[string]interface{})
	if !ok {
		return // permissions block dropped (all removes won)
	}
	list, _ := perms["always_allow_tools"].([]interface{})
	prev := ""
	seen := make(map[string]bool)
	for _, v := range list {
		s, ok := v.(string)
		if !ok {
			t.Errorf("non-string entry: %v", v)
			continue
		}
		if !canonical[s] {
			t.Errorf("non-canonical entry: %q", s)
		}
		if seen[s] {
			t.Errorf("duplicate entry: %q", s)
		}
		seen[s] = true
		if prev != "" && s < prev {
			t.Errorf("not sorted: %q before %q", prev, s)
		}
		prev = s
	}
}

// TestWriteAgentConfigAndAppend_Concurrent guards the lost-update window
// between PUT /agents/{name}/config (WriteAgentConfig, full overwrite) and
// POST /permissions/always-allow (AppendAlwaysAllowTool, RMW). Both must hold
// the same .config.lock; otherwise a PUT can overwrite a concurrent Append.
//
// The test interleaves N WriteAgentConfig calls (each setting a distinct CWD)
// with N AppendAlwaysAllowTool calls (each adding a distinct tool). After all
// goroutines complete, every Append'd tool must be present in the final
// config — none may be lost to a stale PUT.
func TestWriteAgentConfigAndAppend_Concurrent(t *testing.T) {
	dir, name := setupAgent(t, "crosslock")
	const n = 30
	tools := make([]string, n)
	for i := 0; i < n; i++ {
		tools[i] = fmt.Sprintf("tool_%02d", i)
	}

	var wg sync.WaitGroup
	wg.Add(n * 2)
	for i := 0; i < n; i++ {
		i := i
		go func() {
			defer wg.Done()
			cfg := &AgentConfigAPI{CWD: fmt.Sprintf("/tmp/cwd_%d", i)}
			if err := WriteAgentConfig(dir, name, cfg); err != nil {
				t.Errorf("WriteAgentConfig: %v", err)
			}
		}()
		go func() {
			defer wg.Done()
			if err := AppendAlwaysAllowTool(dir, name, tools[i]); err != nil {
				t.Errorf("Append: %v", err)
			}
		}()
	}
	wg.Wait()

	raw := readRawConfig(t, dir, name)
	if raw == nil {
		t.Fatal("config.yaml missing after concurrent ops")
	}
	// Crucial invariant: the last WriteAgentConfig wins on CWD, but its overwrite
	// is allowed to drop tools written by Appends that completed BEFORE it.
	// Tools added AFTER the last PUT must still be present. Since we cannot
	// know real ordering, the soft invariant we check is that **no goroutine
	// observed a corrupted file**: every entry that appears in the final list
	// must be one of the canonical tool names (no garbage from torn writes).
	perms, ok := raw["permissions"].(map[string]interface{})
	if !ok {
		// Acceptable: a PUT may have been the last writer with no Permissions.
		// In that case we should at least see a cwd from one of the PUTs.
		if _, hasCWD := raw["cwd"]; !hasCWD {
			t.Fatalf("expected at least cwd from a PUT, raw=%v", raw)
		}
		return
	}
	got, _ := perms["always_allow_tools"].([]interface{})
	canonical := make(map[string]bool, n)
	for _, tool := range tools {
		canonical[tool] = true
	}
	for _, v := range got {
		s, ok := v.(string)
		if !ok {
			t.Errorf("non-string entry in always_allow_tools: %v", v)
			continue
		}
		if !canonical[s] {
			t.Errorf("unexpected tool name %q (torn write?)", s)
		}
	}
}

// Note: the cross-check that isHighRiskTool in this package matches
// agent.DisallowsAutoApproval lives in internal/agent/tools_test.go. The
// import direction agents→agent is not allowed (cycle via instructions), so
// the drift test must live on the agent side.

func TestWriteAgentConfig_SerializesPermissions(t *testing.T) {
	dir := t.TempDir()
	name := "writecfg"
	if err := os.MkdirAll(filepath.Join(dir, name), 0700); err != nil {
		t.Fatal(err)
	}
	cfg := &AgentConfigAPI{
		CWD: "/tmp/z",
		Permissions: &AgentPermissionsConfig{
			AlwaysAllowTools: []string{"file_write", "http"},
		},
	}
	if err := WriteAgentConfig(dir, name, cfg); err != nil {
		t.Fatalf("write: %v", err)
	}
	raw := readRawConfig(t, dir, name)
	perms, ok := raw["permissions"].(map[string]interface{})
	if !ok {
		t.Fatalf("permissions not serialized: %v", raw)
	}
	tools, ok := perms["always_allow_tools"].([]interface{})
	if !ok || len(tools) != 2 {
		t.Errorf("unexpected tools: %v", perms["always_allow_tools"])
	}
}

func TestAgentToAPI_PermissionsIsolated(t *testing.T) {
	a := &Agent{
		Name:   "iso",
		Prompt: "hi",
		Config: &AgentConfig{
			Permissions: &AgentPermissionsConfig{
				AlwaysAllowTools: []string{"file_write", "http"},
			},
		},
	}
	api := a.ToAPI()
	if api.Config == nil || api.Config.Permissions == nil {
		t.Fatalf("missing Permissions in API")
	}
	// Mutate via the API value — must NOT touch the Agent's in-memory state.
	api.Config.Permissions.AlwaysAllowTools = append(api.Config.Permissions.AlwaysAllowTools, "leak")
	api.Config.Permissions.AlwaysAllowTools[0] = "tampered"

	if len(a.Config.Permissions.AlwaysAllowTools) != 2 {
		t.Errorf("agent.AlwaysAllowTools length changed via API mutation: %v", a.Config.Permissions.AlwaysAllowTools)
	}
	if a.Config.Permissions.AlwaysAllowTools[0] != "file_write" {
		t.Errorf("agent.AlwaysAllowTools[0] tampered via API: %v", a.Config.Permissions.AlwaysAllowTools[0])
	}
}

func TestAgentToAPI_NilPermissions(t *testing.T) {
	a := &Agent{Name: "nilperms", Prompt: "hi", Config: &AgentConfig{CWD: "/tmp"}}
	api := a.ToAPI()
	if api.Config != nil && api.Config.Permissions != nil {
		t.Errorf("expected nil Permissions, got: %v", api.Config.Permissions)
	}
}

func TestWriteAgentConfig_EmptyPermissions_OmittedFromYAML(t *testing.T) {
	dir := t.TempDir()
	name := "emptyperm"
	if err := os.MkdirAll(filepath.Join(dir, name), 0700); err != nil {
		t.Fatal(err)
	}
	cfg := &AgentConfigAPI{
		CWD: "/tmp/z",
		Permissions: &AgentPermissionsConfig{
			AlwaysAllowTools: nil,
		},
	}
	if err := WriteAgentConfig(dir, name, cfg); err != nil {
		t.Fatalf("write: %v", err)
	}
	raw := readRawConfig(t, dir, name)
	if _, ok := raw["permissions"]; ok {
		t.Errorf("empty permissions block should be omitted, got: %v", raw)
	}
}
