package tools

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Kocoro-lab/ShanClaw/internal/cwdctx"
)

// When the model omits the output filename, playwright writes to its own
// default location and reports a path relative to its own workspace — the
// ambiguity behind the 2026-07-29 `find /` spiral. With an artifact scratch
// dir on ctx, the daemon injects an absolute filename there.
func TestMaybeRewriteFileProducingArg_InjectsDefaultIntoArtifactDir(t *testing.T) {
	scratch := t.TempDir()
	ctx := cwdctx.WithArtifactDir(cwdctx.WithSessionCWD(context.Background(), t.TempDir()), scratch)

	args := map[string]any{"type": "png"}
	got := maybeRewriteFileProducingArg(ctx, "playwright", "browser_take_screenshot", args)
	if got == "" {
		t.Fatal("expected default filename injection, got empty")
	}
	if !strings.HasPrefix(got, scratch+string(filepath.Separator)) {
		t.Errorf("injected path %q should live under artifact dir %q", got, scratch)
	}
	if !strings.HasSuffix(got, ".png") {
		t.Errorf("expected .png default for screenshot, got %q", got)
	}
	if args["filename"] != got {
		t.Errorf("args[filename] = %v, want %q", args["filename"], got)
	}
}

func TestMaybeRewriteFileProducingArg_DefaultExtFollowsJpegType(t *testing.T) {
	ctx := cwdctx.WithArtifactDir(context.Background(), t.TempDir())
	args := map[string]any{"type": "jpeg"}
	got := maybeRewriteFileProducingArg(ctx, "playwright", "browser_take_screenshot", args)
	if !strings.HasSuffix(got, ".jpeg") {
		t.Errorf("expected .jpeg default when type=jpeg, got %q", got)
	}
}

// Relative filenames prefer the artifact scratch dir over the session CWD —
// intermediates stop landing on user-visible folders like ~/Desktop; the
// model addresses user-visible destinations with absolute paths.
func TestMaybeRewriteFileProducingArg_RelativePrefersArtifactDir(t *testing.T) {
	cwd := t.TempDir()
	scratch := t.TempDir()
	ctx := cwdctx.WithArtifactDir(cwdctx.WithSessionCWD(context.Background(), cwd), scratch)

	args := map[string]any{"filename": "sub/dir/snap.md"}
	got := maybeRewriteFileProducingArg(ctx, "playwright", "browser_snapshot", args)
	if !strings.HasPrefix(got, scratch+string(filepath.Separator)) {
		t.Fatalf("rewritten path %q should live under artifact dir %q, not cwd", got, scratch)
	}
	// Parent directory must exist — playwright-mcp does not create it and
	// errors with ENOENT (2026-07-29: .playwright-mcp/ekb-homepage-snapshot.md).
	if _, err := os.Stat(filepath.Dir(got)); err != nil {
		t.Fatalf("parent dir of rewritten path must be created: %v", err)
	}
}

// Without an artifact dir (TUI / one-shot CLI), the session CWD remains the
// target — matching single-CWD host behavior — but the parent dir is now
// created for nested relative names.
func TestMaybeRewriteFileProducingArg_CWDFallbackCreatesParentDir(t *testing.T) {
	cwd := t.TempDir()
	ctx := cwdctx.WithSessionCWD(context.Background(), cwd)

	args := map[string]any{"filename": ".playwright-mcp/snap.md"}
	got := maybeRewriteFileProducingArg(ctx, "playwright", "browser_snapshot", args)
	if !strings.HasPrefix(got, cwd+string(filepath.Separator)) {
		t.Fatalf("rewritten path %q should live under cwd %q", got, cwd)
	}
	if _, err := os.Stat(filepath.Dir(got)); err != nil {
		t.Fatalf("parent dir must be created under cwd fallback: %v", err)
	}
}

// browser_snapshot's filename is a MODE SWITCH, not a location: playwright's
// schema reads "Save snapshot to markdown file instead of returning it in
// the response". Omitted filename = inline accessibility snapshot — the
// model's primary page-reading channel. Injecting a default here would turn
// every daemon-served page read into a file round-trip, so snapshot must
// never receive a default filename (explicit model-supplied filenames are
// still relocated).
func TestMaybeRewriteFileProducingArg_SnapshotNoFilenameStaysInline(t *testing.T) {
	ctx := cwdctx.WithArtifactDir(cwdctx.WithSessionCWD(context.Background(), t.TempDir()), t.TempDir())
	args := map[string]any{}
	if got := maybeRewriteFileProducingArg(ctx, "playwright", "browser_snapshot", args); got != "" {
		t.Fatalf("browser_snapshot without filename must stay inline, got injection %q", got)
	}
	if _, present := args["filename"]; present {
		t.Fatal("browser_snapshot args must stay untouched so the server returns the snapshot inline")
	}
}

// No filename + no artifact dir → unchanged legacy behavior: nothing injected.
func TestMaybeRewriteFileProducingArg_NoDefaultWithoutArtifactDir(t *testing.T) {
	ctx := cwdctx.WithSessionCWD(context.Background(), t.TempDir())
	args := map[string]any{}
	if got := maybeRewriteFileProducingArg(ctx, "playwright", "browser_take_screenshot", args); got != "" {
		t.Errorf("expected no injection without artifact dir, got %q", got)
	}
	if _, present := args["filename"]; present {
		t.Error("args must stay untouched without artifact dir")
	}
}
