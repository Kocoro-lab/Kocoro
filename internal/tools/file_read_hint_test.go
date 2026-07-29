package tools

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/Kocoro-lab/ShanClaw/internal/cwdctx"
)

// 2026-07-29 incident: the model resolved an MCP server's workspace-relative
// artifact path (".playwright-mcp/page-….png") against the session CWD,
// missed, and ran a 242-second `find /`. When file_read misses on such a
// path, the error must carry a just-in-time hint pointing at the MCP
// workspace roots and warning against filesystem-wide searches.
func TestFileRead_MissingRelativePathCarriesArtifactHint(t *testing.T) {
	ctx := cwdctx.WithSessionCWD(context.Background(), t.TempDir())
	tool := &FileReadTool{}

	for _, path := range []string{".playwright-mcp/page-x.png", "missing-note.md"} {
		result, err := tool.Run(ctx, fmt.Sprintf(`{"path":%q,"description":"read artifact"}`, path))
		if err != nil {
			t.Fatalf("unexpected error for %q: %v", path, err)
		}
		if !result.IsError {
			t.Fatalf("expected error result for missing %q", path)
		}
		if !strings.Contains(result.Content, "[hint]") {
			t.Errorf("missing relative path %q should carry the artifact hint, got:\n%s", path, result.Content)
		}
		if !strings.Contains(result.Content, "Do NOT scan the filesystem") {
			t.Errorf("hint for %q must warn against whole-disk searches, got:\n%s", path, result.Content)
		}
	}
}

// Absolute paths without the artifact marker keep the plain error — the hint
// is scoped to the lost-MCP-artifact pattern, not every missing file.
func TestFileRead_MissingAbsolutePathNoHint(t *testing.T) {
	dir := t.TempDir()
	ctx := cwdctx.WithSessionCWD(context.Background(), dir)
	tool := &FileReadTool{}

	result, err := tool.Run(ctx, fmt.Sprintf(`{"path":%q,"description":"read file"}`, dir+"/nope.md"))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !result.IsError {
		t.Fatal("expected error result for missing absolute path")
	}
	if strings.Contains(result.Content, "[hint]") {
		t.Errorf("absolute non-artifact path must not carry the hint, got:\n%s", result.Content)
	}
}
