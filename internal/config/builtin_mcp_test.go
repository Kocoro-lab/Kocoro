package config

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/Kocoro-lab/ShanClaw/internal/mcp"
)

func TestMergeBuiltinMCPServers_InjectsFreshEntryDisabled(t *testing.T) {
	cfg := &Config{}
	mergeBuiltinMCPServers(cfg)

	got, ok := cfg.MCPServers["intercom"]
	if !ok {
		t.Fatal("expected intercom built-in to be injected when user config is empty")
	}
	if !got.Builtin {
		t.Error("expected Builtin=true on injected entry")
	}
	if !got.Disabled {
		t.Error("expected Disabled=true on injected entry (built-ins ship off)")
	}
	if got.Command != mcp.BuiltinMCPServers["intercom"].Config.Command {
		t.Errorf("expected command from built-in catalog, got %q", got.Command)
	}
	if len(got.Args) != len(mcp.BuiltinMCPServers["intercom"].Config.Args) {
		t.Errorf("expected args from built-in catalog, got %v", got.Args)
	}
}

func TestMergeBuiltinMCPServers_PreservesUserDisabledFalse(t *testing.T) {
	cfg := &Config{
		MCPServers: map[string]mcp.MCPServerConfig{
			"intercom": {Disabled: false},
		},
	}
	mergeBuiltinMCPServers(cfg)

	got := cfg.MCPServers["intercom"]
	if got.Disabled {
		t.Error("expected user-set Disabled=false to survive the merge")
	}
	if !got.Builtin {
		t.Error("expected Builtin=true after merge")
	}
	if got.Command == "" {
		t.Error("expected command to be filled in from built-in catalog")
	}
}

func TestMergeBuiltinMCPServers_OverridesUserCommand(t *testing.T) {
	cfg := &Config{
		MCPServers: map[string]mcp.MCPServerConfig{
			"intercom": {
				Command:  "rm",
				Args:     []string{"-rf", "/"},
				Disabled: false,
			},
		},
	}
	mergeBuiltinMCPServers(cfg)

	got := cfg.MCPServers["intercom"]
	wantCmd := mcp.BuiltinMCPServers["intercom"].Config.Command
	if got.Command != wantCmd {
		t.Errorf("user command should be overridden by built-in: got %q want %q", got.Command, wantCmd)
	}
	wantArgs := mcp.BuiltinMCPServers["intercom"].Config.Args
	if len(got.Args) != len(wantArgs) || got.Args[0] != wantArgs[0] {
		t.Errorf("user args should be overridden by built-in: got %v want %v", got.Args, wantArgs)
	}
	if got.Disabled {
		t.Error("user-set Disabled=false should still be preserved alongside the override")
	}
}

func TestMergeBuiltinMCPServers_ArgsAreDeepCopied(t *testing.T) {
	// Mutating Args on a merged entry must not reach back into the
	// BuiltinMCPServers global. Without a deep copy in mergeBuiltinMCPServers
	// both slices would share the same backing array.
	cfg := &Config{}
	mergeBuiltinMCPServers(cfg)
	original := append([]string(nil), mcp.BuiltinMCPServers["intercom"].Config.Args...)

	srv := cfg.MCPServers["intercom"]
	if len(srv.Args) > 0 {
		srv.Args[0] = "POISON"
	}

	after := mcp.BuiltinMCPServers["intercom"].Config.Args
	for i := range original {
		if original[i] != after[i] {
			t.Fatalf("BuiltinMCPServers args poisoned by downstream mutation at %d: %v vs %v", i, original, after)
		}
	}
}

func TestMergeBuiltinMCPServers_PreservesUserEnvAndKeepAlive(t *testing.T) {
	cfg := &Config{
		MCPServers: map[string]mcp.MCPServerConfig{
			"intercom": {
				Env:       map[string]string{"INTERCOM_TOKEN": "secret"},
				KeepAlive: true,
			},
		},
	}
	mergeBuiltinMCPServers(cfg)

	got := cfg.MCPServers["intercom"]
	if got.Env["INTERCOM_TOKEN"] != "secret" {
		t.Errorf("user env should be preserved: %v", got.Env)
	}
	if !got.KeepAlive {
		t.Error("user KeepAlive=true should be preserved")
	}
}

func TestLoad_InjectsBuiltinIntercomWithEnvCasingPreserved(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	shannonDir := filepath.Join(home, ".shannon")
	if err := os.MkdirAll(shannonDir, 0700); err != nil {
		t.Fatalf("mkdir shannon dir: %v", err)
	}
	// User opts in by writing a minimal entry; also adds an env var with
	// mixed casing to confirm fixMCPEnvKeyCasing still runs before the
	// built-in merge.
	yaml := "mcp_servers:\n" +
		"  intercom:\n" +
		"    disabled: false\n" +
		"    env:\n" +
		"      INTERCOM_TOKEN: shh\n"
	if err := os.WriteFile(filepath.Join(shannonDir, "config.yaml"), []byte(yaml), 0600); err != nil {
		t.Fatalf("write config: %v", err)
	}

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load error: %v", err)
	}
	srv, ok := cfg.MCPServers["intercom"]
	if !ok {
		t.Fatalf("expected intercom in cfg.MCPServers")
	}
	if !srv.Builtin {
		t.Error("Builtin flag should be set after Load")
	}
	if srv.Disabled {
		t.Error("user override disabled=false should hold after Load")
	}
	if srv.Command != "npx" {
		t.Errorf("expected command from built-in (npx), got %q", srv.Command)
	}
	if srv.Env["INTERCOM_TOKEN"] != "shh" {
		t.Errorf("expected env casing preserved, got map %v", srv.Env)
	}
}
