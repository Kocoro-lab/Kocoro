package agents

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

func TestLoadAgent_ReadsAgentAndMemory(t *testing.T) {
	dir := t.TempDir()
	agentDir := filepath.Join(dir, "ops-bot")
	os.MkdirAll(agentDir, 0700)
	os.WriteFile(filepath.Join(agentDir, "AGENT.md"), []byte("You are ops-bot."), 0600)
	os.WriteFile(filepath.Join(agentDir, "MEMORY.md"), []byte("Last deploy: ok"), 0600)

	a, err := LoadAgent(dir, "ops-bot")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if a.Name != "ops-bot" {
		t.Errorf("name = %q, want %q", a.Name, "ops-bot")
	}
	if a.Prompt != "You are ops-bot." {
		t.Errorf("prompt = %q, want %q", a.Prompt, "You are ops-bot.")
	}
	if a.Memory != "Last deploy: ok" {
		t.Errorf("memory = %q, want %q", a.Memory, "Last deploy: ok")
	}
}

func TestLoadAgent_MissingAgentMD(t *testing.T) {
	dir := t.TempDir()
	os.MkdirAll(filepath.Join(dir, "ops-bot"), 0700)
	_, err := LoadAgent(dir, "ops-bot")
	if err == nil {
		t.Fatal("expected error for missing AGENT.md")
	}
}

func TestLoadAgent_MissingMemoryIsOK(t *testing.T) {
	dir := t.TempDir()
	agentDir := filepath.Join(dir, "ops-bot")
	os.MkdirAll(agentDir, 0700)
	os.WriteFile(filepath.Join(agentDir, "AGENT.md"), []byte("You are ops-bot."), 0600)

	a, err := LoadAgent(dir, "ops-bot")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if a.Memory != "" {
		t.Errorf("memory = %q, want empty", a.Memory)
	}
}

func TestLoadAgent_RejectsInvalidNames(t *testing.T) {
	dir := t.TempDir()
	invalid := []string{"../etc", "a/b", "", ".hidden", "a b", "A_UPPER", "名前"}
	for _, name := range invalid {
		_, err := LoadAgent(dir, name)
		if err == nil {
			t.Errorf("expected error for invalid name %q", name)
		}
	}
}

func TestValidateAgentName(t *testing.T) {
	valid := []string{"ops-bot", "a", "my_agent_123", "x-1"}
	for _, name := range valid {
		if err := ValidateAgentName(name); err != nil {
			t.Errorf("ValidateAgentName(%q) = %v, want nil", name, err)
		}
	}
	invalid := []string{"", "../x", "a/b", ".dot", "UPPER", "a b", "名前"}
	for _, name := range invalid {
		if err := ValidateAgentName(name); err == nil {
			t.Errorf("ValidateAgentName(%q) = nil, want error", name)
		}
	}
}

func TestListAgents(t *testing.T) {
	dir := t.TempDir()
	for _, name := range []string{"alpha", "beta"} {
		agentDir := filepath.Join(dir, name)
		os.MkdirAll(agentDir, 0700)
		os.WriteFile(filepath.Join(agentDir, "AGENT.md"), []byte("test"), 0600)
	}
	os.MkdirAll(filepath.Join(dir, "invalid"), 0700)

	entries, err := ListAgents(dir)
	if err != nil {
		t.Fatalf("ListAgents: %v", err)
	}
	if len(entries) != 2 {
		t.Fatalf("expected 2, got %d", len(entries))
	}
	if entries[0].Name != "alpha" || entries[1].Name != "beta" {
		t.Fatalf("unexpected names: %v", entries)
	}
}

func TestListAgents_WithBuiltins(t *testing.T) {
	dir := t.TempDir()

	// User agent
	userDir := filepath.Join(dir, "myagent")
	os.MkdirAll(userDir, 0700)
	os.WriteFile(filepath.Join(userDir, "AGENT.md"), []byte("user"), 0600)

	// Builtin agents
	for _, name := range []string{"explorer", "reviewer"} {
		d := filepath.Join(dir, "_builtin", name)
		os.MkdirAll(d, 0700)
		os.WriteFile(filepath.Join(d, "AGENT.md"), []byte("builtin"), 0600)
	}

	entries, err := ListAgents(dir)
	if err != nil {
		t.Fatalf("ListAgents: %v", err)
	}
	if len(entries) != 3 {
		t.Fatalf("expected 3 entries, got %d", len(entries))
	}

	byName := make(map[string]AgentEntry)
	for _, e := range entries {
		byName[e.Name] = e
	}
	if e, ok := byName["explorer"]; !ok || !e.Builtin || e.Override {
		t.Fatalf("explorer: expected builtin=true override=false, got %+v", byName["explorer"])
	}
	if e, ok := byName["myagent"]; !ok || e.Builtin || e.Override {
		t.Fatalf("myagent: expected builtin=false override=false, got %+v", byName["myagent"])
	}
}

func TestListAgents_OverrideDeduplication(t *testing.T) {
	dir := t.TempDir()

	builtinDir := filepath.Join(dir, "_builtin", "explorer")
	os.MkdirAll(builtinDir, 0700)
	os.WriteFile(filepath.Join(builtinDir, "AGENT.md"), []byte("builtin"), 0600)

	userDir := filepath.Join(dir, "explorer")
	os.MkdirAll(userDir, 0700)
	os.WriteFile(filepath.Join(userDir, "AGENT.md"), []byte("user"), 0600)

	entries, err := ListAgents(dir)
	if err != nil {
		t.Fatalf("ListAgents: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("expected 1 deduplicated entry, got %d", len(entries))
	}
	if !entries[0].Override {
		t.Fatal("expected Override=true for deduplicated entry")
	}
	if entries[0].Builtin {
		t.Fatal("overridden entry should have Builtin=false")
	}
}

func TestParseAgentMention(t *testing.T) {
	tests := []struct {
		input     string
		wantAgent string
		wantMsg   string
	}{
		{"@ops-bot check prod", "ops-bot", "check prod"},
		{"@OPS-BOT check prod", "ops-bot", "check prod"},
		{"check prod", "", "check prod"},
		{"@ops-bot", "ops-bot", ""},
		{"@ broken", "", "@ broken"},
		{"@invalid/name test", "", "@invalid/name test"},
	}
	for _, tt := range tests {
		agent, msg := ParseAgentMention(tt.input)
		if agent != tt.wantAgent || msg != tt.wantMsg {
			t.Errorf("ParseAgentMention(%q) = (%q, %q), want (%q, %q)",
				tt.input, agent, msg, tt.wantAgent, tt.wantMsg)
		}
	}
}

func TestAgentConfig_ParseWatch(t *testing.T) {
	raw := `
watch:
  - path: ~/Code
    glob: "*.go"
  - path: ~/Downloads
`
	var cfg AgentConfig
	if err := yaml.Unmarshal([]byte(raw), &cfg); err != nil {
		t.Fatal(err)
	}
	if len(cfg.Watch) != 2 {
		t.Fatalf("expected 2 watch entries, got %d", len(cfg.Watch))
	}
	if cfg.Watch[0].Path != "~/Code" {
		t.Errorf("expected ~/Code, got %s", cfg.Watch[0].Path)
	}
	if cfg.Watch[0].Glob != "*.go" {
		t.Errorf("expected *.go, got %s", cfg.Watch[0].Glob)
	}
	if cfg.Watch[1].Glob != "" {
		t.Errorf("expected empty glob, got %s", cfg.Watch[1].Glob)
	}
}

func TestAgentConfig_ParseHeartbeat(t *testing.T) {
	raw := `
heartbeat:
  every: 30m
  active_hours: "09:00-22:00"
  model: small
  isolated_session: true
`
	var cfg AgentConfig
	if err := yaml.Unmarshal([]byte(raw), &cfg); err != nil {
		t.Fatal(err)
	}
	if cfg.Heartbeat == nil {
		t.Fatal("expected heartbeat config")
	}
	if cfg.Heartbeat.Every != "30m" {
		t.Errorf("expected 30m, got %s", cfg.Heartbeat.Every)
	}
	if cfg.Heartbeat.ActiveHours != "09:00-22:00" {
		t.Errorf("expected 09:00-22:00, got %s", cfg.Heartbeat.ActiveHours)
	}
	if cfg.Heartbeat.Model != "small" {
		t.Errorf("expected small, got %s", cfg.Heartbeat.Model)
	}
	if !cfg.Heartbeat.IsIsolatedSession() {
		t.Error("expected isolated_session true")
	}
}

func TestHeartbeatConfig_DefaultIsolatedSession(t *testing.T) {
	raw := `
heartbeat:
  every: 30m
`
	var cfg AgentConfig
	if err := yaml.Unmarshal([]byte(raw), &cfg); err != nil {
		t.Fatal(err)
	}
	if !cfg.Heartbeat.IsIsolatedSession() {
		t.Error("expected default isolated_session to be true (nil pointer)")
	}
}

func TestAgentConfig_CWD(t *testing.T) {
	dir := t.TempDir()
	agentDir := filepath.Join(dir, "test-agent")
	os.MkdirAll(agentDir, 0755)

	projectDir := t.TempDir()
	configYAML := fmt.Sprintf("cwd: %s\nagent:\n  model: medium\n", projectDir)
	os.WriteFile(filepath.Join(agentDir, "config.yaml"), []byte(configYAML), 0644)
	os.WriteFile(filepath.Join(agentDir, "AGENT.md"), []byte("test agent"), 0644)

	agent, err := LoadAgent(dir, "test-agent")
	if err != nil {
		t.Fatalf("LoadAgent failed: %v", err)
	}
	if agent.Config.CWD != projectDir {
		t.Fatalf("expected CWD %q, got %q", projectDir, agent.Config.CWD)
	}
}

func TestLoadAgent_BuiltinFallback(t *testing.T) {
	dir := t.TempDir()
	// Create builtin agent only
	builtinDir := filepath.Join(dir, "_builtin", "explorer")
	os.MkdirAll(builtinDir, 0700)
	os.WriteFile(filepath.Join(builtinDir, "AGENT.md"), []byte("builtin explorer"), 0600)

	ag, err := LoadAgent(dir, "explorer")
	if err != nil {
		t.Fatalf("LoadAgent: %v", err)
	}
	if ag.Prompt != "builtin explorer" {
		t.Fatalf("expected builtin prompt, got %q", ag.Prompt)
	}
}

func TestLoadAgent_UserOverrideWins(t *testing.T) {
	dir := t.TempDir()
	// Create both builtin and user agent
	builtinDir := filepath.Join(dir, "_builtin", "explorer")
	os.MkdirAll(builtinDir, 0700)
	os.WriteFile(filepath.Join(builtinDir, "AGENT.md"), []byte("builtin"), 0600)

	userDir := filepath.Join(dir, "explorer")
	os.MkdirAll(userDir, 0700)
	os.WriteFile(filepath.Join(userDir, "AGENT.md"), []byte("user override"), 0600)

	ag, err := LoadAgent(dir, "explorer")
	if err != nil {
		t.Fatalf("LoadAgent: %v", err)
	}
	if ag.Prompt != "user override" {
		t.Fatalf("expected user override, got %q", ag.Prompt)
	}
}

func TestLoadAgent_MemoryFromRuntimeDir(t *testing.T) {
	dir := t.TempDir()
	// Builtin definition
	builtinDir := filepath.Join(dir, "_builtin", "explorer")
	os.MkdirAll(builtinDir, 0700)
	os.WriteFile(filepath.Join(builtinDir, "AGENT.md"), []byte("explorer"), 0600)

	// Memory in top-level runtime dir (not in _builtin)
	runtimeDir := filepath.Join(dir, "explorer")
	os.MkdirAll(runtimeDir, 0700)
	os.WriteFile(filepath.Join(runtimeDir, "MEMORY.md"), []byte("runtime memory"), 0600)

	ag, err := LoadAgent(dir, "explorer")
	if err != nil {
		t.Fatalf("LoadAgent: %v", err)
	}
	if ag.Memory != "runtime memory" {
		t.Fatalf("expected runtime memory, got %q", ag.Memory)
	}
}

func TestAgentConfig_CWD_RejectsRelativePath(t *testing.T) {
	dir := t.TempDir()
	agentDir := filepath.Join(dir, "test-agent")
	os.MkdirAll(agentDir, 0755)

	configYAML := "cwd: relative/path\nagent:\n  model: medium\n"
	os.WriteFile(filepath.Join(agentDir, "config.yaml"), []byte(configYAML), 0644)
	os.WriteFile(filepath.Join(agentDir, "AGENT.md"), []byte("test agent"), 0644)

	_, err := LoadAgent(dir, "test-agent")
	if err == nil {
		t.Fatal("expected error for relative cwd path")
	}
}

func TestLoadAgent_ParsesModelTier(t *testing.T) {
	dir := t.TempDir()
	agentDir := filepath.Join(dir, "test_opus")
	if err := os.MkdirAll(agentDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(agentDir, "AGENT.md"), []byte("test agent"), 0o644); err != nil {
		t.Fatal(err)
	}
	yaml := `agent:
  model_tier: large
`
	if err := os.WriteFile(filepath.Join(agentDir, "config.yaml"), []byte(yaml), 0o644); err != nil {
		t.Fatal(err)
	}

	a, err := LoadAgent(dir, "test_opus")
	if err != nil {
		t.Fatal(err)
	}
	if a.Config == nil || a.Config.Agent == nil {
		t.Fatal("expected agent config to be loaded")
	}
	if a.Config.Agent.ModelTier == nil {
		t.Fatal("ModelTier expected non-nil")
	}
	if got := *a.Config.Agent.ModelTier; got != "large" {
		t.Errorf("ModelTier = %q, want %q", got, "large")
	}
}

func TestLoadAgent_ModelTierNilWhenAbsent(t *testing.T) {
	dir := t.TempDir()
	agentDir := filepath.Join(dir, "test_default")
	if err := os.MkdirAll(agentDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(agentDir, "AGENT.md"), []byte("test agent"), 0o644); err != nil {
		t.Fatal(err)
	}
	yaml := `agent:
  max_iterations: 10
`
	if err := os.WriteFile(filepath.Join(agentDir, "config.yaml"), []byte(yaml), 0o644); err != nil {
		t.Fatal(err)
	}

	a, err := LoadAgent(dir, "test_default")
	if err != nil {
		t.Fatal(err)
	}
	if a.Config == nil || a.Config.Agent == nil {
		t.Fatal("expected agent config to be loaded")
	}
	if a.Config.Agent.ModelTier != nil {
		t.Errorf("ModelTier expected nil when omitted, got %q", *a.Config.Agent.ModelTier)
	}
}

func writeAgentWithDisplay(t *testing.T, dir, slug, display string) {
	t.Helper()
	ad := filepath.Join(dir, slug)
	os.MkdirAll(ad, 0700)
	os.WriteFile(filepath.Join(ad, "AGENT.md"), []byte("x"), 0600)
	if display != "" {
		os.WriteFile(filepath.Join(ad, "config.yaml"),
			[]byte("display_name: "+display+"\n"), 0600)
	}
}

func TestDisplayNameTaken(t *testing.T) {
	dir := t.TempDir()
	writeAgentWithDisplay(t, dir, "agent-aaa111", "客服助手")
	writeAgentWithDisplay(t, dir, "agent-bbb222", "")

	taken, err := DisplayNameTaken(dir, "客服助手", "")
	if err != nil || !taken {
		t.Errorf("client match: taken=%v err=%v, want true,nil", taken, err)
	}
	if taken, _ := DisplayNameTaken(dir, "  客服助手 ", ""); !taken {
		t.Errorf("normalized match should be taken")
	}
	if taken, _ := DisplayNameTaken(dir, "客服助手", "agent-aaa111"); taken {
		t.Errorf("self-exclude should not be taken")
	}
	if taken, _ := DisplayNameTaken(dir, "新名字", ""); taken {
		t.Errorf("unused name should not be taken")
	}
	if taken, _ := DisplayNameTaken(dir, "  ", ""); taken {
		t.Errorf("empty name should never be taken")
	}
	// A slug with no explicit display_name is reserved under its slug-fallback name.
	if taken, _ := DisplayNameTaken(dir, "agent-bbb222", ""); !taken {
		t.Errorf("slug-fallback display name should be taken")
	}
}

func TestListAgents_DisplayNameFallback(t *testing.T) {
	dir := t.TempDir()
	writeAgentWithDisplay(t, dir, "agent-aaa111", "客服助手")
	writeAgentWithDisplay(t, dir, "agent-bbb222", "")
	entries, err := ListAgents(dir)
	if err != nil {
		t.Fatalf("ListAgents: %v", err)
	}
	got := map[string]string{}
	for _, e := range entries {
		got[e.Name] = e.DisplayName
	}
	if got["agent-aaa111"] != "客服助手" {
		t.Errorf("display = %q, want 客服助手", got["agent-aaa111"])
	}
	if got["agent-bbb222"] != "agent-bbb222" {
		t.Errorf("display = %q, want fallback to slug", got["agent-bbb222"])
	}
}

func TestGenerateAgentSlug_FormatAndUniqueness(t *testing.T) {
	dir := t.TempDir()
	slug, err := GenerateAgentSlug(dir)
	if err != nil {
		t.Fatalf("GenerateAgentSlug: %v", err)
	}
	if !strings.HasPrefix(slug, "agent-") {
		t.Errorf("slug = %q, want prefix agent-", slug)
	}
	if err := ValidateAgentName(slug); err != nil {
		t.Errorf("generated slug %q fails ValidateAgentName: %v", slug, err)
	}
	// Existing dir with that slug must be skipped.
	agentDir := filepath.Join(dir, slug)
	os.MkdirAll(agentDir, 0700)
	os.WriteFile(filepath.Join(agentDir, "AGENT.md"), []byte("x"), 0600)
	slug2, err := GenerateAgentSlug(dir)
	if err != nil {
		t.Fatalf("GenerateAgentSlug 2: %v", err)
	}
	if slug2 == slug {
		t.Errorf("second slug collided with existing: %q", slug2)
	}
}
