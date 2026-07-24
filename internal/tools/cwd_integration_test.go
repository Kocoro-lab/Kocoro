package tools

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Kocoro-lab/ShanClaw/internal/cwdctx"
)

// TestResumedSession_KeepsStoredCWD verifies that resuming a session from a
// different directory still uses the session's stored project CWD.
func TestResumedSession_KeepsStoredCWD(t *testing.T) {
	projectDir := t.TempDir()
	os.WriteFile(filepath.Join(projectDir, "project_marker.txt"), []byte("here"), 0644)

	// Simulate: resumed session has stored CWD = projectDir, agent has a different default
	agentCWD := t.TempDir()
	sessionCWD := projectDir
	effectiveCWD := cwdctx.ResolveEffectiveCWD("", sessionCWD, agentCWD)

	if effectiveCWD != projectDir {
		t.Fatalf("expected resumed session CWD %q, got %q", projectDir, effectiveCWD)
	}

	// Verify file tool would resolve relative path correctly under this CWD
	ctx := cwdctx.WithSessionCWD(context.Background(), effectiveCWD)
	resolved := cwdctx.ResolvePath(ctx, "project_marker.txt")
	if _, err := os.Stat(resolved); err != nil {
		t.Fatalf("relative path should resolve to file in resumed CWD: %v", err)
	}
}

// TestConcurrentRoutes_DifferentCWDs verifies two concurrent tool runs with
// different session CWDs do not interfere with each other.
func TestConcurrentRoutes_DifferentCWDs(t *testing.T) {
	dir1 := t.TempDir()
	dir2 := t.TempDir()
	os.WriteFile(filepath.Join(dir1, "route1.txt"), []byte("route1"), 0644)
	os.WriteFile(filepath.Join(dir2, "route2.txt"), []byte("route2"), 0644)

	errs := make(chan error, 2)

	// Route 1: session CWD = dir1
	go func() {
		ctx := cwdctx.WithSessionCWD(context.Background(), dir1)
		tool := &FileReadTool{}
		result, err := tool.Run(ctx, `{"path":"route1.txt","description":"test route one"}`)
		if err != nil {
			errs <- fmt.Errorf("route1 err: %w", err)
			return
		}
		if result.IsError || !strings.Contains(result.Content, "route1") {
			errs <- fmt.Errorf("route1 unexpected: error=%v content=%q", result.IsError, result.Content)
			return
		}
		errs <- nil
	}()

	// Route 2: session CWD = dir2
	go func() {
		ctx := cwdctx.WithSessionCWD(context.Background(), dir2)
		tool := &FileReadTool{}
		result, err := tool.Run(ctx, `{"path":"route2.txt","description":"test route two"}`)
		if err != nil {
			errs <- fmt.Errorf("route2 err: %w", err)
			return
		}
		if result.IsError || !strings.Contains(result.Content, "route2") {
			errs <- fmt.Errorf("route2 unexpected: error=%v content=%q", result.IsError, result.Content)
			return
		}
		errs <- nil
	}()

	for i := 0; i < 2; i++ {
		if err := <-errs; err != nil {
			t.Fatal(err)
		}
	}
}

// TestSafeCheckerWithContext_SessionCWD verifies that auto-approval uses
// session CWD while legacy SafeChecker still works.
func TestSafeCheckerWithContext_SessionCWD(t *testing.T) {
	dir := t.TempDir()
	ctx := cwdctx.WithSessionCWD(context.Background(), dir)

	tool := &FileReadTool{}

	// File under session CWD — safe
	safeArgs := fmt.Sprintf(`{"path":"%s/test.txt"}`, dir)
	if !tool.IsSafeArgsWithContext(ctx, safeArgs) {
		t.Error("file under session CWD should be auto-approved")
	}

	// File outside session CWD — not safe
	if tool.IsSafeArgsWithContext(ctx, `{"path":"/etc/passwd"}`) {
		t.Error("file outside session CWD should NOT be auto-approved")
	}

	// Legacy IsSafeArgs still works (uses process CWD)
	cwd, _ := os.Getwd()
	legacyArgs := fmt.Sprintf(`{"path":"%s/anything.txt"}`, cwd)
	if !tool.IsSafeArgs(legacyArgs) {
		t.Error("legacy IsSafeArgs should still work with process CWD")
	}
}
