package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Kocoro-lab/ShanClaw/internal/permissions"
	"github.com/spf13/viper"
)

func TestValidateConfig_IdleTimeouts(t *testing.T) {
	mk := func(soft, hard int) *Config {
		c := &Config{}
		c.Agent.IdleSoftTimeoutSecs = soft
		c.Agent.IdleHardTimeoutSecs = hard
		return c
	}
	tests := []struct {
		name    string
		cfg     *Config
		wantErr string
	}{
		{"both zero ok", mk(0, 0), ""},
		{"soft only ok", mk(90, 0), ""},
		{"both positive ordered ok", mk(90, 540), ""},
		{"negative soft", mk(-1, 0), "idle_soft_timeout_secs"},
		{"negative hard", mk(0, -1), "idle_hard_timeout_secs"},
		{"hard too small", mk(0, 10), "too aggressive"},
		{"hard less than soft", mk(90, 60), "must be >=" /* "must be >= agent.idle_soft_timeout_secs" */},
		{"hard = soft ok", mk(90, 90), ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := validateConfig(tt.cfg)
			if tt.wantErr == "" {
				if err != nil {
					t.Fatalf("want no error, got: %v", err)
				}
				return
			}
			if err == nil {
				t.Fatalf("want error containing %q, got nil", tt.wantErr)
			}
			if !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("want error containing %q, got: %v", tt.wantErr, err)
			}
		})
	}
}

func TestValidateConfig_AgentModelTierKeyword(t *testing.T) {
	mk := func(model string) *Config {
		c := &Config{}
		c.Agent.Model = model
		return c
	}
	tests := []struct {
		name    string
		cfg     *Config
		wantErr string
	}{
		{"empty ok", mk(""), ""},
		{"specific id ok", mk("claude-opus-4-8"), ""},
		{"tier small rejected", mk("small"), "specific model id"},
		{"tier medium rejected", mk("medium"), "specific model id"},
		{"tier large rejected", mk("large"), "specific model id"},
		{"cased tier rejected", mk("Large"), "specific model id"},
		{"padded tier rejected", mk(" large "), "specific model id"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := validateConfig(tt.cfg)
			if tt.wantErr == "" {
				if err != nil {
					t.Fatalf("want no error, got: %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("want error containing %q, got: %v", tt.wantErr, err)
			}
		})
	}
}

func TestAppendAllowedCommand(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.yaml")
	os.WriteFile(cfgPath, []byte("endpoint: https://example.com\n"), 0644)

	err := AppendAllowedCommand(dir, "git status")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	data, _ := os.ReadFile(cfgPath)
	content := string(data)
	if !strings.Contains(content, "git status") {
		t.Errorf("should contain 'git status', got:\n%s", content)
	}

	// Append another
	err = AppendAllowedCommand(dir, "ls -la")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	data, _ = os.ReadFile(cfgPath)
	content = string(data)
	if !strings.Contains(content, "ls -la") {
		t.Errorf("should contain 'ls -la', got:\n%s", content)
	}
	if !strings.Contains(content, "git status") {
		t.Errorf("should still contain 'git status', got:\n%s", content)
	}
}

func TestAppendAllowedCommand_NoDuplicates(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.yaml")
	os.WriteFile(cfgPath, []byte("permissions:\n  allowed_commands:\n    - \"git status\"\n"), 0644)

	err := AppendAllowedCommand(dir, "git status")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	data, _ := os.ReadFile(cfgPath)
	if strings.Count(string(data), "git status") > 1 {
		t.Error("should not duplicate existing command")
	}
}

func TestAppendGlobalAlwaysAllowTool(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.yaml")
	os.WriteFile(cfgPath, []byte("endpoint: https://example.com\n"), 0644)

	if err := AppendGlobalAlwaysAllowTool(dir, "bash"); err != nil {
		t.Fatalf("append: %v", err)
	}
	data, _ := os.ReadFile(cfgPath)
	if !strings.Contains(string(data), "always_allow_tools") {
		t.Errorf("config should have always_allow_tools block, got:\n%s", data)
	}
	if !strings.Contains(string(data), "bash") {
		t.Errorf("config should contain 'bash', got:\n%s", data)
	}

	// Idempotent
	if err := AppendGlobalAlwaysAllowTool(dir, "bash"); err != nil {
		t.Fatalf("re-append: %v", err)
	}
	data, _ = os.ReadFile(cfgPath)
	if strings.Count(string(data), "- bash") > 1 {
		t.Errorf("duplicate bash entry not deduped, got:\n%s", data)
	}

	// Append a second tool — both must survive
	if err := AppendGlobalAlwaysAllowTool(dir, "file_write"); err != nil {
		t.Fatalf("append file_write: %v", err)
	}
	data, _ = os.ReadFile(cfgPath)
	if !strings.Contains(string(data), "bash") || !strings.Contains(string(data), "file_write") {
		t.Errorf("expected both bash and file_write, got:\n%s", data)
	}
	// Pre-existing config keys must be preserved
	if !strings.Contains(string(data), "endpoint") {
		t.Errorf("endpoint key lost on append:\n%s", data)
	}
}

func TestAppendGlobalAlwaysAllowTool_NoConfigFile(t *testing.T) {
	dir := t.TempDir()
	// No config.yaml exists yet — Append should create one.
	if err := AppendGlobalAlwaysAllowTool(dir, "bash"); err != nil {
		t.Fatalf("append on missing config: %v", err)
	}
	data, _ := os.ReadFile(filepath.Join(dir, "config.yaml"))
	if !strings.Contains(string(data), "bash") {
		t.Errorf("expected bash in config after first-create, got:\n%s", data)
	}
}

func TestRemoveGlobalAlwaysAllowTool(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.yaml")
	if err := AppendGlobalAlwaysAllowTool(dir, "bash"); err != nil {
		t.Fatal(err)
	}
	if err := AppendGlobalAlwaysAllowTool(dir, "file_write"); err != nil {
		t.Fatal(err)
	}
	// Remove one
	if err := RemoveGlobalAlwaysAllowTool(dir, "bash"); err != nil {
		t.Fatalf("remove: %v", err)
	}
	data, _ := os.ReadFile(cfgPath)
	if strings.Contains(string(data), "- bash") {
		t.Errorf("bash should be removed, got:\n%s", data)
	}
	if !strings.Contains(string(data), "file_write") {
		t.Errorf("file_write should remain, got:\n%s", data)
	}

	// Remove the last one — block should be cleaned up
	if err := RemoveGlobalAlwaysAllowTool(dir, "file_write"); err != nil {
		t.Fatalf("remove last: %v", err)
	}
	data, _ = os.ReadFile(cfgPath)
	if strings.Contains(string(data), "always_allow_tools") {
		t.Errorf("empty always_allow_tools key should be dropped, got:\n%s", data)
	}

	// Removing absent tool is a no-op
	if err := RemoveGlobalAlwaysAllowTool(dir, "never_added"); err != nil {
		t.Errorf("removing absent tool should not error: %v", err)
	}

	// Removing from non-existent config is a no-op
	emptyDir := t.TempDir()
	if err := RemoveGlobalAlwaysAllowTool(emptyDir, "bash"); err != nil {
		t.Errorf("removing from non-existent config should be no-op: %v", err)
	}
}

func TestLoad_DoesNotApplyProjectOverlayFromProcessCWD(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	shannonDir := filepath.Join(home, ".shannon")
	if err := os.MkdirAll(shannonDir, 0700); err != nil {
		t.Fatalf("mkdir shannon dir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(shannonDir, "config.yaml"), []byte("model_tier: medium\n"), 0600); err != nil {
		t.Fatalf("write global config: %v", err)
	}

	projectDir := t.TempDir()
	projectConfigDir := filepath.Join(projectDir, ".shannon")
	if err := os.MkdirAll(projectConfigDir, 0755); err != nil {
		t.Fatalf("mkdir project config dir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(projectConfigDir, "config.yaml"), []byte("model_tier: low\n"), 0644); err != nil {
		t.Fatalf("write project config: %v", err)
	}

	oldWD, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	defer func() { _ = os.Chdir(oldWD) }()
	if err := os.Chdir(projectDir); err != nil {
		t.Fatalf("chdir project: %v", err)
	}

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load error: %v", err)
	}
	if cfg.ModelTier != "medium" {
		t.Fatalf("expected global model tier, got %q", cfg.ModelTier)
	}
}

func TestRuntimeConfigForCWD_AppliesOnlySessionSafeProjectOverrides(t *testing.T) {
	projectDir := t.TempDir()
	projectConfigDir := filepath.Join(projectDir, ".shannon")
	if err := os.MkdirAll(projectConfigDir, 0755); err != nil {
		t.Fatalf("mkdir project config dir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(projectConfigDir, "config.yaml"), []byte(strings.Join([]string{
		"endpoint: https://project.example",
		"model_tier: low",
		"tools:",
		"  bash_max_output: 4096",
		"cloud:",
		"  publish_allowed_extensions:",
		"    - .sql",
		"permissions:",
		"  allowed_commands:",
		"    - make test",
		"daemon:",
		"  auto_approve: true",
	}, "\n")), 0644); err != nil {
		t.Fatalf("write project config: %v", err)
	}
	if err := os.WriteFile(filepath.Join(projectConfigDir, "config.local.yaml"), []byte(strings.Join([]string{
		"cloud:",
		"  publish_allowed_extensions:",
		"    - .log",
		"permissions:",
		"  allowed_commands:",
		"    - go test ./...",
	}, "\n")), 0644); err != nil {
		t.Fatalf("write local project config: %v", err)
	}

	base := &Config{
		Endpoint:  "https://global.example",
		ModelTier: "medium",
		Permissions: permissions.PermissionsConfig{
			AllowedCommands: []string{"git status"},
		},
		Tools: ToolsConfig{
			BashMaxOutput: 30000,
		},
		Cloud: CloudConfig{
			Enabled:                  true,
			Timeout:                  3600,
			PublishAllowedExtensions: []string{".md"},
		},
		Sources: buildDefaultSources(),
	}

	cfg, err := RuntimeConfigForCWD(base, projectDir)
	if err != nil {
		t.Fatalf("RuntimeConfigForCWD error: %v", err)
	}

	if cfg.Endpoint != "https://global.example" {
		t.Fatalf("expected endpoint to stay global, got %q", cfg.Endpoint)
	}
	if cfg.ModelTier != "low" {
		t.Fatalf("expected project model tier, got %q", cfg.ModelTier)
	}
	if cfg.Tools.BashMaxOutput != 4096 {
		t.Fatalf("expected project bash_max_output, got %d", cfg.Tools.BashMaxOutput)
	}
	if got := cfg.Permissions.AllowedCommands; len(got) != 3 || got[0] != "git status" || got[1] != "make test" || got[2] != "go test ./..." {
		t.Fatalf("unexpected allowed commands: %#v", got)
	}
	if got := cfg.Cloud.PublishAllowedExtensions; len(got) != 3 || got[0] != ".md" || got[1] != ".sql" || got[2] != ".log" {
		t.Fatalf("unexpected publish extensions: %#v", got)
	}
	if cfg.Daemon.AutoApprove {
		t.Fatal("expected daemon config to remain global")
	}
	if src := cfg.Sources["model_tier"]; src.Level != "project" {
		t.Fatalf("expected project source for model_tier, got %#v", src)
	}
	if src := cfg.Sources["permissions.allowed_commands"]; src.Level != "local" {
		t.Fatalf("expected local source for allowed_commands, got %#v", src)
	}
	if src := cfg.Sources["cloud.publish_allowed_extensions"]; src.Level != "local" {
		t.Fatalf("expected local source for publish extensions, got %#v", src)
	}
}

func TestSkillsConfigDefault(t *testing.T) {
	// Use a scratch HOME so we don't touch the real ~/.shannon/config.yaml.
	tmp := t.TempDir()
	t.Setenv("HOME", tmp)

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	want := "https://raw.githubusercontent.com/Kocoro-lab/shanclaw-skill-registry/main/index.json"
	if cfg.Skills.Marketplace.RegistryURL != want {
		t.Errorf("Skills.Marketplace.RegistryURL = %q, want %q", cfg.Skills.Marketplace.RegistryURL, want)
	}
}

// TestMergeRuntimeOverlayFile_MCPWorkspaceRoots guards the plumbing of
// mcp.workspace_roots from project/local overlay files into the merged
// Config. Before the fix the field was declared on overlayConfig but
// never read in mergeRuntimeOverlayFile, so project-level workspace
// roots were silently dropped.
func TestMergeRuntimeOverlayFile_MCPWorkspaceRoots(t *testing.T) {
	dir := t.TempDir()
	overlayPath := filepath.Join(dir, ".shannon", "config.yaml")
	if err := os.MkdirAll(filepath.Dir(overlayPath), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	overlayYAML := `mcp:
  workspace_roots:
    - /workspace/project-a
    - /workspace/shared
`
	if err := os.WriteFile(overlayPath, []byte(overlayYAML), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}

	// Seed a baseline config with one existing root — overlay should
	// append rather than replace, and dedupe against what's already there.
	cfg := &Config{
		MCP:     MCPConfig{WorkspaceRoots: []string{"/workspace/shared"}},
		Sources: map[string]ConfigSource{},
	}

	mergeRuntimeOverlayFile(cfg, overlayPath, "project")

	got := cfg.MCP.WorkspaceRoots
	if len(got) != 2 {
		t.Fatalf("expected 2 workspace roots after dedup, got %d: %v", len(got), got)
	}
	seen := make(map[string]bool)
	for _, r := range got {
		seen[r] = true
	}
	for _, want := range []string{"/workspace/shared", "/workspace/project-a"} {
		if !seen[want] {
			t.Errorf("missing expected root %q in %v", want, got)
		}
	}
	if src, ok := cfg.Sources["mcp.workspace_roots"]; !ok || src.Level != "project" {
		t.Errorf("expected source to record project overlay, got %+v ok=%v", src, ok)
	}
}

// TestMergeRuntimeOverlayFile_BashConcurrencyEnabled guards the overlay path
// for agent.bash_concurrency_enabled. Before the fix the field existed on
// AgentConfig and viper.SetDefault marked it true (Phase C), but project /
// local overlays could not override it back to false because
// overlayAgentConfig was missing the field.
func TestMergeRuntimeOverlayFile_BashConcurrencyEnabled(t *testing.T) {
	dir := t.TempDir()
	overlayPath := filepath.Join(dir, ".shannon", "config.yaml")
	if err := os.MkdirAll(filepath.Dir(overlayPath), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	overlayYAML := "agent:\n  bash_concurrency_enabled: false\n"
	if err := os.WriteFile(overlayPath, []byte(overlayYAML), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}

	// Seed config with the Phase C default (true).
	cfg := &Config{Sources: map[string]ConfigSource{}}
	cfg.Agent.BashConcurrencyEnabled = true

	mergeRuntimeOverlayFile(cfg, overlayPath, "project")

	if cfg.Agent.BashConcurrencyEnabled {
		t.Errorf("expected overlay to flip BashConcurrencyEnabled to false, still true")
	}
	if src, ok := cfg.Sources["agent.bash_concurrency_enabled"]; !ok || src.Level != "project" {
		t.Errorf("expected source to record project overlay, got %+v ok=%v", src, ok)
	}
}

func TestMemoryDefaults(t *testing.T) {
	// Use a scratch HOME so we don't touch the real ~/.shannon/config.yaml.
	tmp := t.TempDir()
	t.Setenv("HOME", tmp)

	if _, err := Load(); err != nil {
		t.Fatal(err)
	}
	if v := viper.GetString("memory.provider"); v != "disabled" {
		t.Fatalf("memory.provider=%q want disabled (Episodic Memory is opt-in)", v)
	}
	if v := viper.GetInt("memory.sidecar_restart_max"); v != 5 {
		t.Fatalf("sidecar_restart_max=%d want 5", v)
	}
	if v := viper.GetDuration("memory.bundle_pull_interval"); v.Hours() != 24 {
		t.Fatalf("bundle_pull_interval=%v want 24h", v)
	}
}

// Pattern matches existing TestLoad_* tests: redirect HOME → tmp, write
// ~/.shannon/config.yaml, call Load() (no args; returns *Config, error).
func TestPromptSuggestionConfig_Defaults(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	if err := os.MkdirAll(filepath.Join(home, ".shannon"), 0700); err != nil {
		t.Fatal(err)
	}

	cfg, err := Load()
	if err != nil {
		t.Fatalf("load: %v", err)
	}

	if !cfg.Agent.PromptSuggestion.Enabled {
		t.Error("PromptSuggestion.Enabled should default to true")
	}
	if cfg.Agent.PromptSuggestion.CacheColdThresholdTokens != 10000 {
		t.Errorf("CacheColdThresholdTokens default = %d, want 10000",
			cfg.Agent.PromptSuggestion.CacheColdThresholdTokens)
	}
	if cfg.Agent.PromptSuggestion.MinTurns != 2 {
		t.Errorf("MinTurns default = %d, want 2", cfg.Agent.PromptSuggestion.MinTurns)
	}
}

func TestPromptSuggestionConfig_OverlayMerge(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	shannonDir := filepath.Join(home, ".shannon")
	if err := os.MkdirAll(shannonDir, 0700); err != nil {
		t.Fatal(err)
	}
	yaml := `agent:
  prompt_suggestion:
    enabled: true
    cache_cold_threshold_tokens: 20000
    min_turns: 1
`
	if err := os.WriteFile(filepath.Join(shannonDir, "config.yaml"), []byte(yaml), 0600); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load()
	if err != nil {
		t.Fatalf("load: %v", err)
	}

	if !cfg.Agent.PromptSuggestion.Enabled {
		t.Error("expected enabled=true after overlay")
	}
	if cfg.Agent.PromptSuggestion.CacheColdThresholdTokens != 20000 {
		t.Errorf("got %d, want 20000", cfg.Agent.PromptSuggestion.CacheColdThresholdTokens)
	}
	if cfg.Agent.PromptSuggestion.MinTurns != 1 {
		t.Errorf("got %d, want 1", cfg.Agent.PromptSuggestion.MinTurns)
	}
}

// TestConfig_IdleHardTimeoutDefault540 pins the flipped default. The flip is
// the user-visible half of the watchdog feature — regressions here would
// silently re-enable the 600s-HTTP-transport-only fallback.
func TestConfig_IdleHardTimeoutDefault540(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	if err := os.MkdirAll(filepath.Join(home, ".shannon"), 0700); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load()
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if cfg.Agent.IdleHardTimeoutSecs != 540 {
		t.Errorf("IdleHardTimeoutSecs default = %d, want 540", cfg.Agent.IdleHardTimeoutSecs)
	}
}

// TestConfig_StreamIdleTimeoutDefault90 pins the new chunk-gap watchdog
// default (90s).
func TestConfig_StreamIdleTimeoutDefault90(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	if err := os.MkdirAll(filepath.Join(home, ".shannon"), 0700); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load()
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if cfg.Agent.StreamIdleTimeoutSecs != 90 {
		t.Errorf("StreamIdleTimeoutSecs default = %d, want 90", cfg.Agent.StreamIdleTimeoutSecs)
	}
}

// TestConfig_IdleHardTimeoutYamlOverridesDefault verifies a yaml-supplied
// value beats the flipped default — including the opt-out value 0, which the
// daemon converts into a startup WARN at boot.
func TestConfig_IdleHardTimeoutYamlOverridesDefault(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	shannonDir := filepath.Join(home, ".shannon")
	if err := os.MkdirAll(shannonDir, 0700); err != nil {
		t.Fatal(err)
	}
	yaml := "agent:\n  idle_hard_timeout_secs: 180\n  stream_idle_timeout_secs: 30\n"
	if err := os.WriteFile(filepath.Join(shannonDir, "config.yaml"), []byte(yaml), 0600); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load()
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if cfg.Agent.IdleHardTimeoutSecs != 180 {
		t.Errorf("IdleHardTimeoutSecs = %d, want 180 (yaml override)", cfg.Agent.IdleHardTimeoutSecs)
	}
	if cfg.Agent.StreamIdleTimeoutSecs != 30 {
		t.Errorf("StreamIdleTimeoutSecs = %d, want 30 (yaml override)", cfg.Agent.StreamIdleTimeoutSecs)
	}
}

// TestConfig_BrowserTrimmingDefaults pins the browser/GUI context-trimming
// defaults so they stay ON. An absent key must resolve to the default
// (backward-compat: old config files with none of these keys keep parsing and
// pick up the trimming for free).
func TestConfig_BrowserTrimmingDefaults(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	if err := os.MkdirAll(filepath.Join(home, ".shannon"), 0700); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load()
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if cfg.Agent.ObservationWindow != 3 {
		t.Errorf("ObservationWindow default = %d, want 3", cfg.Agent.ObservationWindow)
	}
	if cfg.Agent.MaxRecentImages != 50 {
		t.Errorf("MaxRecentImages default = %d, want 50", cfg.Agent.MaxRecentImages)
	}
	if cfg.Agent.MaxRecentBrowserImages != 1 {
		t.Errorf("MaxRecentBrowserImages default = %d, want 1", cfg.Agent.MaxRecentBrowserImages)
	}
	if cfg.Tools.BrowserResultTruncation != 24000 {
		t.Errorf("BrowserResultTruncation default = %d, want 24000", cfg.Tools.BrowserResultTruncation)
	}
}

// TestConfig_BrowserTrimmingNegativeRejected pins the validateConfig rejection
// of negative values for the new knobs (0 stays valid as the disabled/fallback
// sentinel), matching the idle-timeout sibling precedent.
func TestConfig_BrowserTrimmingNegativeRejected(t *testing.T) {
	cases := []struct {
		name string
		yaml string
		want string
	}{
		{"observation_window", "agent:\n  observation_window: -1\n", "agent.observation_window"},
		{"max_recent_images", "agent:\n  max_recent_images: -1\n", "agent.max_recent_images"},
		{"max_recent_browser_images", "agent:\n  max_recent_browser_images: -1\n", "agent.max_recent_browser_images"},
		{"browser_result_truncation", "tools:\n  browser_result_truncation: -1\n", "tools.browser_result_truncation"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			home := t.TempDir()
			t.Setenv("HOME", home)
			shannonDir := filepath.Join(home, ".shannon")
			if err := os.MkdirAll(shannonDir, 0700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(shannonDir, "config.yaml"), []byte(tc.yaml), 0600); err != nil {
				t.Fatal(err)
			}
			_, err := Load()
			if err == nil {
				t.Fatalf("negative %s accepted, want validation error", tc.name)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error %q does not mention %q", err.Error(), tc.want)
			}
		})
	}

	// 0 must be accepted (disabled/fallback sentinel), not rejected.
	home := t.TempDir()
	t.Setenv("HOME", home)
	shannonDir := filepath.Join(home, ".shannon")
	if err := os.MkdirAll(shannonDir, 0700); err != nil {
		t.Fatal(err)
	}
	yaml := "agent:\n  observation_window: 0\n  max_recent_images: 0\n  max_recent_browser_images: 0\ntools:\n  browser_result_truncation: 0\n"
	if err := os.WriteFile(filepath.Join(shannonDir, "config.yaml"), []byte(yaml), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(); err != nil {
		t.Fatalf("zero values rejected, want accepted (disabled sentinel): %v", err)
	}
}

// TestConfig_BrowserTrimmingYamlOverrides verifies yaml-supplied values beat
// the defaults, including 0 (window disabled / cap falls back to generic).
func TestConfig_BrowserTrimmingYamlOverrides(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	shannonDir := filepath.Join(home, ".shannon")
	if err := os.MkdirAll(shannonDir, 0700); err != nil {
		t.Fatal(err)
	}
	yaml := "agent:\n  observation_window: 5\n  max_recent_images: 2\n  max_recent_browser_images: 3\ntools:\n  browser_result_truncation: 0\n"
	if err := os.WriteFile(filepath.Join(shannonDir, "config.yaml"), []byte(yaml), 0600); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load()
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if cfg.Agent.ObservationWindow != 5 {
		t.Errorf("ObservationWindow = %d, want 5 (yaml override)", cfg.Agent.ObservationWindow)
	}
	if cfg.Agent.MaxRecentImages != 2 {
		t.Errorf("MaxRecentImages = %d, want 2 (yaml override)", cfg.Agent.MaxRecentImages)
	}
	if cfg.Agent.MaxRecentBrowserImages != 3 {
		t.Errorf("MaxRecentBrowserImages = %d, want 3 (yaml override)", cfg.Agent.MaxRecentBrowserImages)
	}
	if cfg.Tools.BrowserResultTruncation != 0 {
		t.Errorf("BrowserResultTruncation = %d, want 0 (yaml override)", cfg.Tools.BrowserResultTruncation)
	}
	// Provenance: values set in the GLOBAL config file must report source
	// "global", not "default" (markGlobalSources must mark the new keys).
	for _, key := range []string{
		"agent.observation_window", "agent.max_recent_images",
		"agent.max_recent_browser_images", "tools.browser_result_truncation",
	} {
		if src := cfg.Sources[key]; src.Level != "global" {
			t.Errorf("Sources[%q].Level = %q, want \"global\"", key, src.Level)
		}
	}
}

// TestConfig_StreamIdleTimeoutNegativeRejected ensures the validator catches
// nonsensical values so a typo can't silently disable the watchdog or wrap
// into a positive duration via int conversion.
func TestConfig_StreamIdleTimeoutNegativeRejected(t *testing.T) {
	cfg := &Config{}
	cfg.Agent.StreamIdleTimeoutSecs = -1
	err := validateConfig(cfg)
	if err == nil {
		t.Fatal("expected validation error for negative stream_idle_timeout_secs, got nil")
	}
	if !strings.Contains(err.Error(), "stream_idle_timeout_secs") {
		t.Errorf("expected error to mention stream_idle_timeout_secs, got %v", err)
	}
}

// TestConfig_CloudStreamIdleTimeoutDefault pins the cloud SSE liveness probe
// default at 45s — load-bearing: if it silently became 0 the per-connection
// idle watchdog would be disabled on every cloud_delegate / research run.
func TestConfig_CloudStreamIdleTimeoutDefault(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	shannonDir := filepath.Join(home, ".shannon")
	if err := os.MkdirAll(shannonDir, 0700); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load()
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if cfg.Cloud.StreamIdleTimeoutSecs != 45 {
		t.Errorf("Cloud.StreamIdleTimeoutSecs default = %d, want 45", cfg.Cloud.StreamIdleTimeoutSecs)
	}
}

// TestConfig_CloudStreamIdleTimeoutNegativeRejected ensures a negative cloud
// value is caught by the validator (a typo can't silently disable or wrap the
// watchdog).
func TestConfig_CloudStreamIdleTimeoutNegativeRejected(t *testing.T) {
	cfg := &Config{}
	cfg.Cloud.StreamIdleTimeoutSecs = -1
	err := validateConfig(cfg)
	if err == nil {
		t.Fatal("expected validation error for negative cloud.stream_idle_timeout_secs, got nil")
	}
	if !strings.Contains(err.Error(), "cloud.stream_idle_timeout_secs") {
		t.Errorf("expected error to mention cloud.stream_idle_timeout_secs, got %v", err)
	}
}
