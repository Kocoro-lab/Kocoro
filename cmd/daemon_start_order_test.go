package cmd

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/Kocoro-lab/ShanClaw/internal/agents"
	daemonruntime "github.com/Kocoro-lab/ShanClaw/internal/daemon"
)

func TestAcquireDaemonPIDFileCreatesStateDirectoryOnFirstStart(t *testing.T) {
	home := t.TempDir()
	shannonDir := filepath.Join(home, ".shannon")
	if _, err := os.Stat(shannonDir); !os.IsNotExist(err) {
		t.Fatalf("test state directory already exists: %v", err)
	}

	pidFile, err := acquireDaemonPIDFile(shannonDir)
	if err != nil {
		t.Fatalf("first daemon ownership acquisition failed: %v", err)
	}
	defer pidFile.Close()
	if info, err := os.Stat(shannonDir); err != nil || !info.IsDir() {
		t.Fatalf("daemon state directory was not created: info=%v err=%v", info, err)
	}
}

func TestDaemonStartDoesNotMutateBeforePIDLock(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	shannonDir := filepath.Join(home, ".shannon")
	agentsDir := filepath.Join(shannonDir, "agents")
	if err := agents.SetAttachedSkills(agentsDir, "analyst", []string{"missing-skill"}); err != nil {
		t.Fatal(err)
	}
	manifestPath := filepath.Join(agentsDir, "analyst", "_attached.yaml")
	before, err := os.ReadFile(manifestPath)
	if err != nil {
		t.Fatal(err)
	}

	pidFile, err := daemonruntime.AcquirePIDFile(filepath.Join(shannonDir, "daemon.pid"))
	if err != nil {
		t.Fatal(err)
	}
	defer pidFile.Close()

	oldForce, _ := daemonStartCmd.Flags().GetBool("force")
	oldDetach, _ := daemonStartCmd.Flags().GetBool("detach")
	if err := daemonStartCmd.Flags().Set("force", "false"); err != nil {
		t.Fatal(err)
	}
	if err := daemonStartCmd.Flags().Set("detach", "false"); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = daemonStartCmd.Flags().Set("force", boolString(oldForce))
		_ = daemonStartCmd.Flags().Set("detach", boolString(oldDetach))
	})

	if err := daemonStartCmd.RunE(daemonStartCmd, nil); err == nil {
		t.Fatal("second daemon start unexpectedly acquired the PID lock")
	}
	after, err := os.ReadFile(manifestPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(after) != string(before) {
		t.Fatalf("second daemon start rewrote agent manifest before acquiring PID lock:\nbefore=%s\nafter=%s", before, after)
	}
	if _, err := os.Stat(filepath.Join(shannonDir, "skills", "kocoro")); !os.IsNotExist(err) {
		t.Fatalf("second daemon start synced builtin skills before acquiring PID lock: %v", err)
	}
}

func boolString(v bool) string {
	if v {
		return "true"
	}
	return "false"
}
