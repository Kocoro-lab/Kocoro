package tools

// Decision-support pin for the CrossIterationCacheable question: the
// interface (internal/agent/statecache.go) has no implementor anywhere in the
// repo, so cross-iteration tool-result caching is effectively disabled. The
// practical duplicate-read protection users still get comes from the
// ReadTracker file_read dedup, which is independent of statecache. This test
// pins that independence: with only a bare ReadTracker on the context (no
// state cache anywhere), an identical re-read returns the dedup stub instead
// of a second full read. Deleting the dead interface therefore does not
// regress duplicate-read protection.

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Kocoro-lab/ShanClaw/internal/agent"
)

func TestFileReadDedupHoldsWithoutStateCache(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.txt")
	if err := os.WriteFile(path, []byte("primary endpoint: alpha-7\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	tracker := agent.NewReadTracker()
	tracker.SetCWD(dir)
	ctx := context.WithValue(context.Background(), agent.ReadTrackerKey(), tracker)

	tool := &FileReadTool{}
	args, _ := json.Marshal(map[string]any{"path": path, "description": "read config file"})

	first, err := tool.Run(ctx, string(args))
	if err != nil {
		t.Fatalf("first read: %v", err)
	}
	if !strings.Contains(first.Content, "alpha-7") {
		t.Fatalf("first read should return real content, got: %.120s", first.Content)
	}

	second, err := tool.Run(ctx, string(args))
	if err != nil {
		t.Fatalf("second read: %v", err)
	}
	if !strings.Contains(second.Content, "file unchanged since last read") {
		t.Fatalf("identical re-read should hit the ReadTracker stub, got: %.120s", second.Content)
	}
	if strings.Contains(second.Content, "alpha-7") {
		t.Fatal("stub must not re-deliver file content")
	}
}
