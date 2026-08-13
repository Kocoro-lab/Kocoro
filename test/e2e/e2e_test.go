// Package e2e contains end-to-end tests for ShanClaw.
//
// Offline tests (no LLM API needed) run by default.
// Live tests require -tags=live, SHANNON_E2E_LIVE=1, and a configured
// endpoint plus API key.
//
// Tests that exercise the CLI build a fresh shan binary lazily. Compile-only
// runs and focused tests that do not call testBinary avoid that extra build.
package e2e

import (
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"testing"
)

var (
	builtBinary     string
	builtBinaryDir  string
	builtBinaryErr  error
	buildBinaryOnce sync.Once
)

func TestMain(m *testing.M) {
	code := m.Run()
	if builtBinaryDir != "" {
		_ = os.RemoveAll(builtBinaryDir)
	}
	os.Exit(code)
}

func testBinary(t *testing.T) string {
	t.Helper()
	buildBinaryOnce.Do(func() {
		builtBinaryDir, builtBinaryErr = os.MkdirTemp("", "shan-e2e-*")
		if builtBinaryErr != nil {
			return
		}
		builtBinary = filepath.Join(builtBinaryDir, "shan")
		cmd := exec.Command("go", "build", "-o", builtBinary, ".")
		cmd.Dir = repoRoot()
		cmd.Stderr = os.Stderr
		builtBinaryErr = cmd.Run()
	})
	if builtBinaryErr != nil {
		t.Fatalf("build shan binary: %v", builtBinaryErr)
	}
	return builtBinary
}

// neutralTempDir avoids exposing the test function name inside a path that is
// placed in a model-visible prompt. t.TempDir includes that name, which can
// leak the expected behavior into an otherwise natural live probe.
func neutralTempDir(t *testing.T, pattern string) string {
	t.Helper()
	dir, err := os.MkdirTemp("", pattern)
	if err != nil {
		t.Fatalf("create neutral temporary directory: %v", err)
	}
	t.Cleanup(func() {
		if err := os.RemoveAll(dir); err != nil {
			t.Errorf("remove neutral temporary directory: %v", err)
		}
	})
	return dir
}

// skipUnlessLive keeps real-provider calls behind explicit authorization.
// Reachability and local credentials are prerequisites, not consent to spend
// quota or execute tools against external systems.
func skipUnlessLive(t *testing.T) {
	t.Helper()
	if os.Getenv("SHANNON_E2E_LIVE") != "1" {
		t.Skip("live E2E skipped: set SHANNON_E2E_LIVE=1 to authorize real provider calls")
	}
}

func repoRoot() string {
	// test/e2e/ is two levels deep from repo root
	dir, _ := os.Getwd()
	return filepath.Join(dir, "..", "..")
}
