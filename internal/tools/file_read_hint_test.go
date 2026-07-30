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
func TestFileRead_MissingArtifactPathCarriesHint(t *testing.T) {
	ctx := cwdctx.WithSessionCWD(context.Background(), t.TempDir())
	tool := &FileReadTool{}

	// The hint fires only on the lost-MCP-artifact signature: the playwright
	// artifact directory, or artifact-named files (screenshot-/snapshot-/page-).
	for _, path := range []string{
		".playwright-mcp/page-x.png",
		"screenshot-20260730-093831.104.png",
		"sub/snapshot-1.md",
	} {
		result, err := tool.Run(ctx, fmt.Sprintf(`{"path":%q,"description":"read artifact"}`, path))
		if err != nil {
			t.Fatalf("unexpected error for %q: %v", path, err)
		}
		if !result.IsError {
			t.Fatalf("expected error result for missing %q", path)
		}
		if !strings.Contains(result.Content, "[hint]") {
			t.Errorf("missing artifact path %q should carry the hint, got:\n%s", path, result.Content)
		}
		if !strings.Contains(result.Content, "Do NOT scan the filesystem") {
			t.Errorf("hint for %q must warn against whole-disk searches, got:\n%s", path, result.Content)
		}
	}
}

// Ordinary misses keep the plain error — a missing README.md is not an MCP
// artifact, and pointing the model at the daemon's scratch dirs for it would
// be misdirection.
func TestFileRead_MissingOrdinaryPathNoHint(t *testing.T) {
	dir := t.TempDir()
	ctx := cwdctx.WithSessionCWD(context.Background(), dir)
	tool := &FileReadTool{}

	for _, path := range []string{"missing-note.md", "README.md", dir + "/nope.md"} {
		result, err := tool.Run(ctx, fmt.Sprintf(`{"path":%q,"description":"read file"}`, path))
		if err != nil {
			t.Fatalf("unexpected error for %q: %v", path, err)
		}
		if !result.IsError {
			t.Fatalf("expected error result for missing %q", path)
		}
		if strings.Contains(result.Content, "[hint]") {
			t.Errorf("ordinary missing path %q must not carry the artifact hint, got:\n%s", path, result.Content)
		}
	}
}
