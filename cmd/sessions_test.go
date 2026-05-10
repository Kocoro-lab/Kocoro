package cmd

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/spf13/viper"

	"github.com/Kocoro-lab/ShanClaw/internal/config"
)

// TestSessionsSync_ReadsConfigViaCobra is the regression guard for the class
// of bug where a cobra subcommand RunE forgets to call config.Load() and
// silently runs on an uninitialized viper. The yaml must flow through so
// `sync.dry_run: true` selects the dry-run codepath and emits a
// `sync: outcome=noop ...` summary instead of trying real Cloud upload config.
func TestSessionsSync_ReadsConfigViaCobra(t *testing.T) {
	home := t.TempDir()
	shannonDir := filepath.Join(home, ".shannon")
	if err := os.MkdirAll(shannonDir, 0700); err != nil {
		t.Fatalf("mkdir shannon dir: %v", err)
	}
	cfgYAML := "sync:\n  enabled: true\n  dry_run: true\n"
	if err := os.WriteFile(filepath.Join(shannonDir, "config.yaml"), []byte(cfgYAML), 0600); err != nil {
		t.Fatalf("write config: %v", err)
	}

	withIsolatedEnv(t, home)
	viper.Reset()

	var stdout, stderr bytes.Buffer
	rootCmd.SetOut(&stdout)
	rootCmd.SetErr(&stderr)
	rootCmd.SetArgs([]string{"sessions", "sync"})
	t.Cleanup(func() {
		rootCmd.SetArgs(nil)
		rootCmd.SetOut(nil)
		rootCmd.SetErr(nil)
	})

	if err := rootCmd.Execute(); err != nil {
		t.Fatalf("rootCmd.Execute: %v (stderr=%q)", err, stderr.String())
	}

	out := stdout.String()
	if strings.Contains(out, "sync is disabled") {
		t.Fatalf("config not loaded — Bug 1 regression; stdout=%q", out)
	}
	if !strings.Contains(out, "sync: outcome=") {
		t.Fatalf("expected `sync: outcome=...` summary on stdout; got %q (stderr=%q)", out, stderr.String())
	}
}

// TestCloudAliasesResolveToTopLevel verifies RegisterAlias wiring: callers
// reading `cloud.api_key` / `cloud.endpoint` get the top-level `api_key` /
// `endpoint` values. This is Bug 2's regression guard.
func TestCloudAliasesResolveToTopLevel(t *testing.T) {
	home := t.TempDir()
	shannonDir := filepath.Join(home, ".shannon")
	if err := os.MkdirAll(shannonDir, 0700); err != nil {
		t.Fatalf("mkdir shannon dir: %v", err)
	}
	cfgYAML := "api_key: top-level-key-value\nendpoint: https://example.test\n"
	if err := os.WriteFile(filepath.Join(shannonDir, "config.yaml"), []byte(cfgYAML), 0600); err != nil {
		t.Fatalf("write config: %v", err)
	}

	withIsolatedEnv(t, home)
	viper.Reset()

	if _, err := config.Load(); err != nil {
		t.Fatalf("config.Load: %v", err)
	}

	if got := viper.GetString("cloud.api_key"); got != "top-level-key-value" {
		t.Fatalf("cloud.api_key alias: got %q, want top-level value", got)
	}
	if got := viper.GetString("cloud.endpoint"); got != "https://example.test" {
		t.Fatalf("cloud.endpoint alias: got %q, want top-level value", got)
	}
}

// withIsolatedEnv redirects HOME (and XDG_CONFIG_HOME for good measure) to
// the supplied tempdir for the duration of the test. Cleanup restores the
// prior values.
func withIsolatedEnv(t *testing.T, home string) {
	t.Helper()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
}
