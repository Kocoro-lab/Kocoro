package tools

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Kocoro-lab/ShanClaw/internal/mcp"
	mcpgo "github.com/mark3labs/mcp-go/mcp"
)

// staticContentClient returns a fixed text payload from CallTool.
type staticContentClient struct {
	successCallToolClient
	text string
}

func (c *staticContentClient) CallTool(context.Context, mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
	return mcpgo.NewToolResultText(c.text), nil
}

// 2026-07-29 incident: playwright-mcp renders artifact links RELATIVE to the
// client's first advertised root (its response.js `_computRelativeTo`), while
// the model resolves them against the session CWD — a different directory in
// the daemon architecture. The model went looking on the Desktop, missed, and
// ran a 242-second `find /`. The result post-processor must translate such
// links to absolute paths using the root the daemon itself advertised.
func TestMCPTool_Run_AnnotatesPlaywrightRelativeResultPath(t *testing.T) {
	root := t.TempDir()
	artifact := filepath.Join(root, ".playwright-mcp", "page-2026-07-29.png")
	if err := os.MkdirAll(filepath.Dir(artifact), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(artifact, []byte("png"), 0o644); err != nil {
		t.Fatal(err)
	}

	mgr := mcp.NewClientManager()
	mgr.SeedConfig("playwright", mcp.MCPServerConfig{Command: "dummy"})
	mgr.SetRootsHandler(mcp.NewRootsHandler([]string{root}))
	mgr.SeedClient("playwright", &staticContentClient{
		text: "### Result\n- [Screenshot of viewport](.playwright-mcp/page-2026-07-29.png)\n",
	})

	mt := NewMCPTool("playwright", mcpgo.Tool{Name: "browser_take_screenshot"}, mgr)
	result, err := mt.Run(context.Background(), `{"type":"png"}`)
	if err != nil || result.IsError {
		t.Fatalf("unexpected failure: err=%v result=%#v", err, result)
	}
	if !strings.Contains(result.Content, "Saved to: "+artifact) {
		t.Fatalf("expected absolute-path annotation %q, got:\n%s", artifact, result.Content)
	}
}

// A relative link whose file does not exist under the base must NOT be
// annotated — existence is the judgment that separates real artifact paths
// from path-looking strings.
func TestMCPTool_Run_SkipsAnnotationWhenFileMissing(t *testing.T) {
	root := t.TempDir()

	mgr := mcp.NewClientManager()
	mgr.SeedConfig("playwright", mcp.MCPServerConfig{Command: "dummy"})
	mgr.SetRootsHandler(mcp.NewRootsHandler([]string{root}))
	mgr.SeedClient("playwright", &staticContentClient{
		text: "- [Snapshot](.playwright-mcp/never-written.md)",
	})

	mt := NewMCPTool("playwright", mcpgo.Tool{Name: "browser_snapshot"}, mgr)
	result, err := mt.Run(context.Background(), `{}`)
	if err != nil || result.IsError {
		t.Fatalf("unexpected failure: err=%v result=%#v", err, result)
	}
	if strings.Contains(result.Content, "Saved to:") {
		t.Fatalf("must not annotate a non-existent file, got:\n%s", result.Content)
	}
}

// Servers outside the table stay opaque — no rewriting of unknown servers'
// results, however path-like they look (Notion slugs, code snippets).
func TestMCPTool_Run_UnknownServerResultStaysOpaque(t *testing.T) {
	root := t.TempDir()
	// Even with a real matching file on disk, an unknown server's result
	// must not be touched: we don't know its path semantics.
	if err := os.WriteFile(filepath.Join(root, "page.md"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}

	mgr := mcp.NewClientManager()
	mgr.SeedConfig("notion", mcp.MCPServerConfig{Command: "dummy"})
	mgr.SetRootsHandler(mcp.NewRootsHandler([]string{root}))
	mgr.SeedClient("notion", &staticContentClient{text: "- [Doc](page.md)"})

	mt := NewMCPTool("notion", mcpgo.Tool{Name: "search"}, mgr)
	result, err := mt.Run(context.Background(), `{"q":"x"}`)
	if err != nil || result.IsError {
		t.Fatalf("unexpected failure: err=%v result=%#v", err, result)
	}
	if strings.Contains(result.Content, "Saved to:") {
		t.Fatalf("unknown server result must stay opaque, got:\n%s", result.Content)
	}
}

// User-added file-producing MCPs opt in via MCPServerConfig.WorkspaceBase:
// "relative paths in this server's results resolve against this directory".
func TestMCPTool_Run_WorkspaceBaseConfigEnablesTranslation(t *testing.T) {
	base := t.TempDir()
	out := filepath.Join(base, "exports", "report.pdf")
	if err := os.MkdirAll(filepath.Dir(out), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(out, []byte("pdf"), 0o644); err != nil {
		t.Fatal(err)
	}

	mgr := mcp.NewClientManager()
	mgr.SeedConfig("pdfgen", mcp.MCPServerConfig{Command: "dummy", WorkspaceBase: base})
	mgr.SeedClient("pdfgen", &staticContentClient{text: "Done: [report](exports/report.pdf)"})

	mt := NewMCPTool("pdfgen", mcpgo.Tool{Name: "export"}, mgr)
	result, err := mt.Run(context.Background(), `{"doc":"x"}`)
	if err != nil || result.IsError {
		t.Fatalf("unexpected failure: err=%v result=%#v", err, result)
	}
	if !strings.Contains(result.Content, "Saved to: "+out) {
		t.Fatalf("expected workspace_base translation to %q, got:\n%s", out, result.Content)
	}
}

// Candidate extraction: markdown links only; URLs and absolute paths skipped.
func TestMarkdownRelPathCandidates(t *testing.T) {
	content := "- [a](.playwright-mcp/x.png)\n" +
		"- [b](https://example.com/y.png)\n" +
		"- [c](/abs/path.md)\n" +
		"- [d](sub/dir/z.md)\n" +
		"- [e](#anchor)\n" +
		"- [f](mailto:x@y.z)\n"
	got := markdownRelPathCandidates(content)
	want := []string{".playwright-mcp/x.png", "sub/dir/z.md"}
	if len(got) != len(want) {
		t.Fatalf("candidates = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("candidates = %v, want %v", got, want)
		}
	}
}

// Path traversal in a link ("../../etc/passwd") must never resolve outside
// the base directory.
func TestMCPTool_Run_RejectsTraversalOutsideBase(t *testing.T) {
	base := t.TempDir()
	outside := filepath.Join(filepath.Dir(base), "escape.txt")
	if err := os.WriteFile(outside, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Remove(outside) })

	mgr := mcp.NewClientManager()
	mgr.SeedConfig("pdfgen", mcp.MCPServerConfig{Command: "dummy", WorkspaceBase: base})
	mgr.SeedClient("pdfgen", &staticContentClient{text: "[x](../escape.txt)"})

	mt := NewMCPTool("pdfgen", mcpgo.Tool{Name: "export"}, mgr)
	result, err := mt.Run(context.Background(), `{"doc":"x"}`)
	if err != nil || result.IsError {
		t.Fatalf("unexpected failure: err=%v result=%#v", err, result)
	}
	if strings.Contains(result.Content, "Saved to:") {
		t.Fatalf("traversal outside base must not be annotated, got:\n%s", result.Content)
	}
}
