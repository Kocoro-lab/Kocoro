package tools

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Kocoro-lab/ShanClaw/internal/cwdctx"
)

func TestMaybeRewriteFileProducingArg_KnownToolRelative(t *testing.T) {
	cwd := t.TempDir()
	ctx := cwdctx.WithSessionCWD(context.Background(), cwd)

	args := map[string]any{"filename": "note_editor.md"}
	got := maybeRewriteFileProducingArg(ctx, "playwright", "browser_snapshot", args)
	if got == "" {
		t.Fatal("expected rewrite, got empty")
	}
	if !strings.HasPrefix(got, cwd) {
		t.Errorf("rewritten path %q should live under cwd %q", got, cwd)
	}
	if args["filename"] != got {
		t.Errorf("args[filename] = %v, want %q", args["filename"], got)
	}
}

func TestMaybeRewriteFileProducingArg_UnknownToolNoop(t *testing.T) {
	ctx := cwdctx.WithSessionCWD(context.Background(), t.TempDir())
	args := map[string]any{"filename": "x.md"}
	if got := maybeRewriteFileProducingArg(ctx, "playwright", "browser_click", args); got != "" {
		t.Errorf("expected no rewrite for unknown tool, got %q", got)
	}
	if args["filename"] != "x.md" {
		t.Errorf("args should be untouched, got %v", args["filename"])
	}
}

func TestMaybeRewriteFileProducingArg_NoCWDNoop(t *testing.T) {
	args := map[string]any{"filename": "x.md"}
	if got := maybeRewriteFileProducingArg(context.Background(), "playwright", "browser_snapshot", args); got != "" {
		t.Errorf("expected no rewrite without session cwd, got %q", got)
	}
}

func TestMaybeRewriteFileProducingArg_AlreadyAbsoluteNoop(t *testing.T) {
	cwd := t.TempDir()
	ctx := cwdctx.WithSessionCWD(context.Background(), cwd)
	abs := "/tmp/somewhere/else.md"
	args := map[string]any{"filename": abs}
	if got := maybeRewriteFileProducingArg(ctx, "playwright", "browser_snapshot", args); got != "" {
		t.Errorf("expected no rewrite for absolute arg, got %q", got)
	}
	if args["filename"] != abs {
		t.Errorf("absolute arg was modified: %v", args["filename"])
	}
}

func TestMaybeRewriteFileProducingArg_TildeExpands(t *testing.T) {
	// Tilde-prefixed paths must be expanded to the user's home BEFORE the
	// MCP call — Node-based MCP servers (playwright-mcp in particular) don't
	// do shell-style tilde expansion, so a literal `~/Desktop/x.md` would
	// be written to `./~/Desktop/x.md` relative to the server process CWD.
	// Expanding here matches the tilde contract in cwdctx and the bash tool.
	home, err := os.UserHomeDir()
	if err != nil {
		t.Skipf("no home dir available: %v", err)
	}
	cwd := t.TempDir()
	ctx := cwdctx.WithSessionCWD(context.Background(), cwd)

	cases := []struct {
		in   string
		want string
	}{
		{"~/Desktop/out.md", filepath.Join(home, "Desktop", "out.md")},
		{"~/notes/x.md", filepath.Join(home, "notes", "x.md")},
		{"~", home},
	}
	for _, tc := range cases {
		args := map[string]any{"filename": tc.in}
		got := maybeRewriteFileProducingArg(ctx, "playwright", "browser_snapshot", args)
		if got != tc.want {
			t.Errorf("filename=%q: got %q, want %q", tc.in, got, tc.want)
		}
		if args["filename"] != tc.want {
			t.Errorf("filename=%q: args[filename] = %v, want %q", tc.in, args["filename"], tc.want)
		}
	}
}

func TestMaybeRewriteFileProducingArg_DotIsSkipped(t *testing.T) {
	// filename="." resolves to the session CWD itself, which is not a valid
	// output filename — the MCP server needs a real name, not a directory.
	// Rewrite must decline so the server sees the original ambiguous value
	// and errors cleanly rather than being pointed at the directory path.
	cwd := t.TempDir()
	ctx := cwdctx.WithSessionCWD(context.Background(), cwd)
	for _, v := range []string{".", "./"} {
		args := map[string]any{"filename": v}
		if got := maybeRewriteFileProducingArg(ctx, "playwright", "browser_snapshot", args); got != "" {
			t.Errorf("filename=%q: expected no rewrite, got %q", v, got)
		}
		if args["filename"] != v {
			t.Errorf("filename=%q: args was mutated to %v", v, args["filename"])
		}
	}
}

func TestMaybeRewriteFileProducingArg_PathTraversalSkipped(t *testing.T) {
	cwd := t.TempDir()
	ctx := cwdctx.WithSessionCWD(context.Background(), cwd)
	args := map[string]any{"filename": "../escape.md"}
	if got := maybeRewriteFileProducingArg(ctx, "playwright", "browser_snapshot", args); got != "" {
		t.Errorf("expected no rewrite for escaping path, got %q", got)
	}
	if args["filename"] != "../escape.md" {
		t.Errorf("args should be untouched, got %v", args["filename"])
	}
}

func TestAnnotateAbsPath(t *testing.T) {
	abs := "/tmp/session/note.md"
	// Appends when absent.
	out := annotateAbsPath("some result", abs)
	if !strings.Contains(out, "Saved to: "+abs) {
		t.Errorf("expected marker in %q", out)
	}
	// Idempotent when path already present.
	if got := annotateAbsPath(out, abs); got != out {
		t.Errorf("second annotate changed content: %q", got)
	}
	// Empty content becomes just the marker line.
	if got := annotateAbsPath("", abs); got != "Saved to: "+abs {
		t.Errorf("empty-content case: %q", got)
	}
	// Empty path no-ops.
	if got := annotateAbsPath("x", ""); got != "x" {
		t.Errorf("empty path should no-op, got %q", got)
	}
}
