package agents

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Kocoro-lab/ShanClaw/internal/skills"
	"gopkg.in/yaml.v3"
)

func TestValidateDisplayName_Codes(t *testing.T) {
	var dne *DisplayNameError
	if err := ValidateDisplayName(strings.Repeat("x", 257)); !errors.As(err, &dne) || dne.Code != CodeDisplayNameTooLong {
		t.Errorf("too-long: got %v", err)
	}
	dne = nil
	if err := ValidateDisplayName("a\nb"); !errors.As(err, &dne) || dne.Code != CodeDisplayNameInvalidChars {
		t.Errorf("control-char: got %v", err)
	}
	dne = nil
	if err := (&AgentCreateRequest{Prompt: "p"}).Validate(); !errors.As(err, &dne) || dne.Code != CodeDisplayNameRequired {
		t.Errorf("required: got %v", err)
	}
}

func TestAgentToAPI_Minimal(t *testing.T) {
	a := &Agent{Name: "test", Prompt: "hello"}
	api := a.ToAPI()
	if api.Name != "test" {
		t.Errorf("name = %q", api.Name)
	}
	if api.Memory != nil {
		t.Error("expected nil memory")
	}
	if api.Config != nil {
		t.Error("expected nil config")
	}
	// Without Profile, every new field stays nil — wire null on the JSON side.
	if api.Category != nil {
		t.Errorf("expected nil category, got %+v", api.Category)
	}
	if api.Description != nil {
		t.Errorf("expected nil description, got %v", api.Description)
	}
	if api.GuidePrompts != nil {
		t.Errorf("expected nil guide_prompts, got %v", api.GuidePrompts)
	}
	if api.Examples != nil {
		t.Errorf("expected nil examples, got %v", api.Examples)
	}
}

func TestAgentToAPI_Full(t *testing.T) {
	a := &Agent{
		Name:   "test",
		Prompt: "hello",
		Memory: "some memory",
		Config: &AgentConfig{
			Tools: &AgentToolsFilter{Allow: []string{"bash"}},
		},
		Commands: map[string]string{"review": "do review"},
		Skills:   []*skills.Skill{{Name: "check", Description: "check things", Prompt: "check it"}},
	}
	api := a.ToAPI()
	if api.Memory == nil || *api.Memory != "some memory" {
		t.Error("expected memory")
	}
	if api.Config == nil || api.Config.Tools == nil {
		t.Error("expected config with tools")
	}
	if len(api.Commands) != 1 {
		t.Error("expected 1 command")
	}
	if len(api.Skills) != 1 {
		t.Error("expected 1 skill")
	}
}

func TestAgentToAPI_WithProfile_InlinesCategoryLabel(t *testing.T) {
	a := &Agent{
		Name:   "test",
		Prompt: "hello",
		Profile: &AgentProfile{
			Category:    "coding",
			Description: LocalizedString{"en": "Test agent", "zh-Hans": "测试智能体"},
			GuidePrompts: []GuidePrompt{{
				Title:  LocalizedString{"en": "Hi"},
				Prompt: LocalizedString{"en": "Hello"},
			}},
			Examples: []AgentExample{{
				Turns: []ExampleTurn{
					{Role: "user", Text: LocalizedString{"en": "what?"}},
					{Role: "assistant", Markdown: LocalizedString{"en": "answer"}},
				},
			}},
		},
	}
	api := a.ToAPI()
	if api.Category == nil {
		t.Fatal("expected non-nil category")
	}
	if api.Category.Code != "coding" {
		t.Errorf("category.code=%q, want coding", api.Category.Code)
	}
	if api.Category.Label["en"] != "Coding" {
		t.Errorf("category.label.en=%q, want Coding (resolved from registry)", api.Category.Label["en"])
	}
	if api.Category.Label["zh-Hans"] != "编程" {
		t.Errorf("category.label.zh-Hans=%q, want 编程", api.Category.Label["zh-Hans"])
	}
	if api.Description["en"] != "Test agent" {
		t.Errorf("description.en=%q", api.Description["en"])
	}
	if len(api.GuidePrompts) != 1 || api.GuidePrompts[0].Title["en"] != "Hi" {
		t.Errorf("guide_prompts: %+v", api.GuidePrompts)
	}
	if len(api.Examples) != 1 || len(api.Examples[0].Turns) != 2 {
		t.Errorf("examples: %+v", api.Examples)
	}
}

func TestAgentToAPI_ProfileWithoutCategory_NilCategory(t *testing.T) {
	// Profile present but Category is empty → api.Category stays nil while
	// the other three fields populate. Covers an agent that has guide prompts
	// but the author hasn't picked a category yet.
	a := &Agent{
		Name:   "test",
		Prompt: "hello",
		Profile: &AgentProfile{
			Description: LocalizedString{"en": "x"},
		},
	}
	api := a.ToAPI()
	if api.Category != nil {
		t.Errorf("expected nil category, got %+v", api.Category)
	}
	if api.Description["en"] != "x" {
		t.Errorf("description.en=%q", api.Description["en"])
	}
}

func TestWriteAndLoadAgent(t *testing.T) {
	// Layout: shannonDir/agents/<name>/ + shannonDir/skills/<skill>/
	// LoadAgent derives shannonDir from filepath.Dir(agentsDir) and loads
	// skills from shannonDir/skills/, filtered by _attached.yaml manifest.
	shannonDir := t.TempDir()
	agentsDir := filepath.Join(shannonDir, "agents")
	name := "test-agent"

	if err := WriteAgentPrompt(agentsDir, name, "You are test."); err != nil {
		t.Fatalf("WriteAgentPrompt: %v", err)
	}
	if err := WriteAgentCommand(agentsDir, name, "greet", "Say hello"); err != nil {
		t.Fatalf("WriteAgentCommand: %v", err)
	}

	// Write skill to global skills dir (where LoadAgent looks)
	globalSkillDir := filepath.Join(shannonDir, "skills", "check")
	if err := os.MkdirAll(globalSkillDir, 0700); err != nil {
		t.Fatal(err)
	}
	skillContent := "---\nname: check\ndescription: check things\n---\ncheck things\n"
	if err := os.WriteFile(filepath.Join(globalSkillDir, "SKILL.md"), []byte(skillContent), 0600); err != nil {
		t.Fatal(err)
	}

	// Attach the skill via manifest
	if err := WriteAttachedSkills(agentsDir, name, []string{"check"}); err != nil {
		t.Fatalf("WriteAttachedSkills: %v", err)
	}

	a, err := LoadAgent(agentsDir, name)
	if err != nil {
		t.Fatalf("LoadAgent: %v", err)
	}
	if a.Prompt != "You are test." {
		t.Errorf("prompt = %q", a.Prompt)
	}
	if a.Commands["greet"] != "Say hello" {
		t.Errorf("command = %q", a.Commands["greet"])
	}
	found := false
	for _, s := range a.Skills {
		if s.Name == "check" {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("agent skill 'check' not found in skills (got %d skills)", len(a.Skills))
	}
}

func TestDeleteAgentDir(t *testing.T) {
	dir := t.TempDir()
	WriteAgentPrompt(dir, "doomed", "bye")
	if err := DeleteAgentDir(dir, "doomed"); err != nil {
		t.Fatalf("DeleteAgentDir: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "doomed")); !os.IsNotExist(err) {
		t.Error("expected directory removed")
	}
}

func TestAgentCreateRequest_Validate(t *testing.T) {
	// display_name + prompt → valid.
	if err := (&AgentCreateRequest{DisplayName: "客服助手", Prompt: "p"}).Validate(); err != nil {
		t.Errorf("valid request errored: %v", err)
	}
	// Missing display_name → error (slug is server-generated; name is not a client field).
	if err := (&AgentCreateRequest{Prompt: "p"}).Validate(); err == nil {
		t.Errorf("missing display_name should error")
	}
	// Missing prompt → error.
	if err := (&AgentCreateRequest{DisplayName: "x"}).Validate(); err == nil {
		t.Errorf("missing prompt should error")
	}
	// Whitespace-only display_name → error.
	if err := (&AgentCreateRequest{DisplayName: "   ", Prompt: "p"}).Validate(); err == nil {
		t.Errorf("whitespace-only display_name should error")
	}
	// Over-length → error.
	if err := (&AgentCreateRequest{DisplayName: strings.Repeat("x", 257), Prompt: "p"}).Validate(); err == nil {
		t.Errorf("over-length display_name should error")
	}
	// Control char → error.
	if err := (&AgentCreateRequest{DisplayName: "a\nb", Prompt: "p"}).Validate(); err == nil {
		t.Errorf("control-char display_name should error")
	}
	// Exactly 256 runes → valid.
	if err := (&AgentCreateRequest{DisplayName: strings.Repeat("中", 256), Prompt: "p"}).Validate(); err != nil {
		t.Errorf("256-rune display_name should be valid: %v", err)
	}
	// Both allow and deny → error.
	r6 := &AgentCreateRequest{
		DisplayName: "tools-bot", Prompt: "hi",
		Config: &AgentConfigAPI{Tools: &AgentToolsFilter{Allow: []string{"a"}, Deny: []string{"b"}}},
	}
	if err := r6.Validate(); err == nil {
		t.Error("expected error for both allow+deny")
	}
	// Null skill entry → error.
	r7 := &AgentCreateRequest{
		DisplayName: "skill-bot",
		Prompt:      "hi",
		Skills:      []*skills.Skill{nil},
	}
	if err := r7.Validate(); err == nil {
		t.Error("expected error for null skill entry")
	}
}

func TestWriteAgentConfig_PersistsDisplayName(t *testing.T) {
	dir := t.TempDir()
	cfg := &AgentConfigAPI{DisplayName: "客服助手"}
	if err := WriteAgentConfig(dir, "agent-abc123", cfg); err != nil {
		t.Fatalf("WriteAgentConfig: %v", err)
	}
	data, err := os.ReadFile(filepath.Join(dir, "agent-abc123", "config.yaml"))
	if err != nil {
		t.Fatalf("read config: %v", err)
	}
	var parsed AgentConfig
	if err := yaml.Unmarshal(data, &parsed); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if parsed.DisplayName != "客服助手" {
		t.Errorf("display_name = %q, want %q", parsed.DisplayName, "客服助手")
	}
}

func TestAgent_DisplayLabel(t *testing.T) {
	cases := []struct {
		name string
		ag   *Agent
		want string
	}{
		{"display_name set", &Agent{Name: "agent-x", Config: &AgentConfig{DisplayName: "客服"}}, "客服"},
		{"no config", &Agent{Name: "agent-x"}, "agent-x"},
		{"empty display_name", &Agent{Name: "agent-x", Config: &AgentConfig{}}, "agent-x"},
	}
	for _, tc := range cases {
		if got := tc.ag.DisplayLabel(); got != tc.want {
			t.Errorf("%s: DisplayLabel() = %q, want %q", tc.name, got, tc.want)
		}
	}
}

func TestToAPI_DisplayName(t *testing.T) {
	// With display_name set in config.
	withName := &Agent{Name: "agent-aaa111", Prompt: "p",
		Config: &AgentConfig{DisplayName: "客服助手"}}
	if got := withName.ToAPI().DisplayName; got != "客服助手" {
		t.Errorf("DisplayName = %q, want 客服助手", got)
	}
	// No config → fallback to slug.
	noCfg := &Agent{Name: "agent-bbb222", Prompt: "p"}
	if got := noCfg.ToAPI().DisplayName; got != "agent-bbb222" {
		t.Errorf("DisplayName = %q, want fallback agent-bbb222", got)
	}
	// Config present but display_name empty → fallback to slug.
	emptyName := &Agent{Name: "agent-ccc333", Prompt: "p",
		Config: &AgentConfig{CWD: "/tmp"}}
	if got := emptyName.ToAPI().DisplayName; got != "agent-ccc333" {
		t.Errorf("DisplayName = %q, want fallback agent-ccc333", got)
	}
}

func TestSetAgentDisplayName_PreservesOtherFields(t *testing.T) {
	dir := t.TempDir()
	ad := filepath.Join(dir, "agent-keep01")
	os.MkdirAll(ad, 0700)
	os.WriteFile(filepath.Join(ad, "AGENT.md"), []byte("You are test."), 0600)
	os.WriteFile(filepath.Join(ad, "config.yaml"),
		[]byte("auto_approve: true\ncwd: /tmp/work\n"), 0600)

	if err := SetAgentDisplayName(dir, "agent-keep01", "客服助手"); err != nil {
		t.Fatalf("SetAgentDisplayName: %v", err)
	}
	data, err := os.ReadFile(filepath.Join(ad, "config.yaml"))
	if err != nil {
		t.Fatalf("read config: %v", err)
	}
	var m map[string]interface{}
	if err := yaml.Unmarshal(data, &m); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if m["display_name"] != "客服助手" {
		t.Errorf("display_name = %v, want 客服助手", m["display_name"])
	}
	if m["auto_approve"] != true {
		t.Errorf("auto_approve lost: %v", m["auto_approve"])
	}
	if m["cwd"] != "/tmp/work" {
		t.Errorf("cwd lost: %v", m["cwd"])
	}
}

func TestWriteAndLoadAgentProfile_Avatar(t *testing.T) {
	root := t.TempDir()
	if err := WriteAgentProfile(root, "demo", &AgentProfile{
		Category: "coding",
		Avatar:   "https://cdn.example.com/a.png",
	}); err != nil {
		t.Fatalf("WriteAgentProfile: %v", err)
	}
	p, err := LoadAgentProfile(filepath.Join(root, "demo"))
	if err != nil {
		t.Fatalf("LoadAgentProfile: %v", err)
	}
	if p == nil || p.Avatar != "https://cdn.example.com/a.png" {
		t.Fatalf("avatar not round-tripped: %+v", p)
	}
	if p.Category != "coding" {
		t.Fatalf("category not round-tripped: %+v", p)
	}
}

func TestAgentConfigAPI_WatchHeartbeatRoundTrip(t *testing.T) {
	agent := &Agent{
		Name:   "test",
		Prompt: "test prompt",
		Config: &AgentConfig{
			Watch: []WatchEntry{{Path: "~/Code", Glob: "*.go"}},
			Heartbeat: &HeartbeatConfig{
				Every: "30m",
			},
		},
	}
	api := agent.ToAPI()
	if api.Config == nil {
		t.Fatal("expected config")
	}
	if len(api.Config.Watch) != 1 {
		t.Fatalf("expected 1 watch entry, got %d", len(api.Config.Watch))
	}
	if api.Config.Heartbeat == nil {
		t.Fatal("expected heartbeat config")
	}
	if api.Config.Heartbeat.Every != "30m" {
		t.Errorf("expected 30m, got %s", api.Config.Heartbeat.Every)
	}
}
