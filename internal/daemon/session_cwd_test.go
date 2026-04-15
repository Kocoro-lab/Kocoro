package daemon

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestCloudSourceDefinitionsAgree pins the two places that must agree on
// "what counts as a cloud source" — the allocator (isCloudSource) and the
// output-format profile (outputFormatForSource). Adding a new source to one
// without the other would silently give it a rendered profile but no scratch
// CWD (or vice versa). This test drives both paths with the same inputs and
// asserts identical classification.
func TestCloudSourceDefinitionsAgree(t *testing.T) {
	inputs := []string{
		"slack", "line", "feishu", "lark", "telegram", "webhook",
		"desktop", "cli", "cron", "schedule", "web", "", "unknown",
		"SLACK", " Slack ",
	}
	for _, src := range inputs {
		cloud := isCloudSource(src)
		plain := outputFormatForSource(src) == "plain"
		if cloud != plain {
			t.Errorf("source %q: isCloudSource=%v but outputFormatForSource=%q — definitions drifted",
				src, cloud, outputFormatForSource(src))
		}
	}
}

func TestIsCloudSource(t *testing.T) {
	cases := map[string]bool{
		"slack":    true,
		"SLACK":    true,
		" slack ": true,
		"line":     true,
		"feishu":   true,
		"lark":     true,
		"telegram": true,
		"webhook":  true,
		"desktop":  false,
		"cli":      false,
		"cron":     false,
		"":         false,
	}
	for input, want := range cases {
		if got := isCloudSource(input); got != want {
			t.Errorf("isCloudSource(%q) = %v, want %v", input, got, want)
		}
	}
}

func TestEnsureCloudSessionTmpDir_CloudSourceAllocates(t *testing.T) {
	shannonDir := t.TempDir()
	dir, err := ensureCloudSessionTmpDir(shannonDir, "2026-04-15-abc123", "slack")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if dir == "" {
		t.Fatal("expected non-empty dir for cloud source")
	}
	want := filepath.Join(shannonDir, "tmp", "sessions", "2026-04-15-abc123")
	if dir != want {
		t.Errorf("got %q, want %q", dir, want)
	}
	info, err := os.Stat(dir)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if !info.IsDir() {
		t.Error("expected directory")
	}
	// Idempotent — second call returns same dir.
	dir2, err := ensureCloudSessionTmpDir(shannonDir, "2026-04-15-abc123", "slack")
	if err != nil || dir2 != dir {
		t.Errorf("second call: got (%q, %v), want (%q, nil)", dir2, err, dir)
	}
}

func TestEnsureCloudSessionTmpDir_NonCloudNoop(t *testing.T) {
	shannonDir := t.TempDir()
	for _, src := range []string{"desktop", "cli", "cron", ""} {
		dir, err := ensureCloudSessionTmpDir(shannonDir, "session-id", src)
		if err != nil {
			t.Errorf("src=%q unexpected error: %v", src, err)
		}
		if dir != "" {
			t.Errorf("src=%q expected empty, got %q", src, dir)
		}
	}
}

func TestEnsureCloudSessionTmpDir_EmptyShannonDirOrSessionID(t *testing.T) {
	if dir, err := ensureCloudSessionTmpDir("", "id", "slack"); dir != "" || err != nil {
		t.Errorf("empty shannonDir: got (%q, %v), want (\"\", nil)", dir, err)
	}
	if dir, err := ensureCloudSessionTmpDir(t.TempDir(), "", "slack"); dir != "" || err != nil {
		t.Errorf("empty sessionID: got (%q, %v), want (\"\", nil)", dir, err)
	}
}

func TestEnsureCloudSessionTmpDir_RejectsPathTraversal(t *testing.T) {
	shannonDir := t.TempDir()
	_, err := ensureCloudSessionTmpDir(shannonDir, "../escape", "slack")
	if err == nil {
		t.Fatal("expected error on traversal attempt")
	}
	if !strings.Contains(err.Error(), "escape") {
		t.Errorf("error %q should mention escape", err)
	}
}

// This documents the intended lifecycle contract: a cloud scratch dir is
// ephemeral — it never gets written to sess.CWD, and therefore a resumed
// turn always re-allocates via ensureCloudSessionTmpDir instead of trying
// to reuse a path we just deleted. The test exercises the allocator+cleanup
// pair directly to make the invariant explicit; the runner-side guard that
// skips the sess.CWD write is covered by its own integration test surface.
func TestEnsureCloudSessionTmpDir_ReAllocAfterCleanup(t *testing.T) {
	shannonDir := t.TempDir()
	sessID := "2026-04-15-resume-test"

	first, err := ensureCloudSessionTmpDir(shannonDir, sessID, "slack")
	if err != nil || first == "" {
		t.Fatalf("initial allocation failed: %v %q", err, first)
	}
	cloudSessionTmpCleanup(first)()
	if _, err := os.Stat(first); !os.IsNotExist(err) {
		t.Fatalf("cleanup did not remove dir: %v", err)
	}

	// Resuming the same session must get a fresh (re-created) dir at the same
	// path — not fail, not leak a reference to the deleted one.
	second, err := ensureCloudSessionTmpDir(shannonDir, sessID, "slack")
	if err != nil {
		t.Fatalf("re-allocation failed: %v", err)
	}
	if second != first {
		t.Errorf("expected stable path %q, got %q", first, second)
	}
	if info, err := os.Stat(second); err != nil || !info.IsDir() {
		t.Errorf("re-allocated dir not usable: %v", err)
	}
}

func TestCloudSessionTmpCleanup(t *testing.T) {
	shannonDir := t.TempDir()
	dir, err := ensureCloudSessionTmpDir(shannonDir, "s1", "slack")
	if err != nil {
		t.Fatal(err)
	}
	// Drop a file so we can verify recursive removal.
	if err := os.WriteFile(filepath.Join(dir, "artifact.txt"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	cloudSessionTmpCleanup(dir)()
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Errorf("expected dir removed, stat err = %v", err)
	}
	// Safe to call on already-removed path.
	cloudSessionTmpCleanup(dir)()
	// Safe to call with empty dir.
	cloudSessionTmpCleanup("")()
}
