package mcp

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/mark3labs/mcp-go/mcp"
)

func TestNormalizeRoot_TildeExpansion(t *testing.T) {
	home, err := os.UserHomeDir()
	if err != nil {
		t.Skip("no home dir available")
	}
	got := normalizeRoot("~/Downloads")
	want := filepath.Join(home, "Downloads")
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestNormalizeRoot_InvalidInputs(t *testing.T) {
	if got := normalizeRoot(""); got != "" {
		t.Errorf("empty input should return empty, got %q", got)
	}
	if got := normalizeRoot("   "); got != "" {
		t.Errorf("whitespace-only should return empty, got %q", got)
	}
	if got := normalizeRoot("~other"); got != "" {
		t.Errorf("~user form should return empty (unsupported), got %q", got)
	}
}

func TestNewRootsHandler_NormalizeAndDedupe(t *testing.T) {
	tmp := t.TempDir()
	h := NewRootsHandler([]string{
		tmp,
		tmp,            // exact dup
		tmp + "/.",     // resolves to same path after Clean
		"",             // skipped
		"   ",          // skipped
		tmp + "/extra", // distinct
	})
	roots := h.Roots()
	if len(roots) != 2 {
		t.Fatalf("expected 2 deduped roots, got %d: %v", len(roots), roots)
	}
	if roots[0] != tmp {
		t.Errorf("first root = %q, want %q", roots[0], tmp)
	}
	if !strings.HasSuffix(roots[1], "/extra") {
		t.Errorf("second root = %q, want suffix /extra", roots[1])
	}
}

func TestRootsHandler_ListRootsFiltersNonexistent(t *testing.T) {
	tmp := t.TempDir()
	existing := filepath.Join(tmp, "real")
	if err := os.Mkdir(existing, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	missing := filepath.Join(tmp, "never-existed")
	notADir := filepath.Join(tmp, "file.txt")
	if err := os.WriteFile(notADir, []byte("x"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}

	h := NewRootsHandler([]string{existing, missing, notADir})
	result, err := h.ListRoots(context.Background(), mcp.ListRootsRequest{})
	if err != nil {
		t.Fatalf("ListRoots: %v", err)
	}
	if len(result.Roots) != 1 {
		t.Fatalf("expected 1 root (only the real dir survives filter), got %d: %+v", len(result.Roots), result.Roots)
	}
	gotURI := result.Roots[0].URI
	if !strings.HasPrefix(gotURI, "file://") {
		t.Errorf("URI must start with file://, got %q", gotURI)
	}
	if !strings.HasSuffix(gotURI, "/real") {
		t.Errorf("URI should end with /real, got %q", gotURI)
	}
	if result.Roots[0].Name != "real" {
		t.Errorf("Name = %q, want %q", result.Roots[0].Name, "real")
	}
}

func TestRootsHandler_ListRootsRecheckOnCall(t *testing.T) {
	// Proves filtering happens at call time, not at construction — a root
	// created after NewRootsHandler but before ListRoots is advertised, and
	// one removed after NewRootsHandler is filtered out.
	tmp := t.TempDir()
	future := filepath.Join(tmp, "created-later")
	gone := filepath.Join(tmp, "removed-later")
	if err := os.Mkdir(gone, 0o755); err != nil {
		t.Fatalf("mkdir gone: %v", err)
	}
	h := NewRootsHandler([]string{future, gone})

	if err := os.Mkdir(future, 0o755); err != nil {
		t.Fatalf("mkdir future: %v", err)
	}
	if err := os.RemoveAll(gone); err != nil {
		t.Fatalf("rm gone: %v", err)
	}

	result, err := h.ListRoots(context.Background(), mcp.ListRootsRequest{})
	if err != nil {
		t.Fatalf("ListRoots: %v", err)
	}
	if len(result.Roots) != 1 {
		t.Fatalf("expected 1 surviving root, got %d: %+v", len(result.Roots), result.Roots)
	}
	if !strings.HasSuffix(result.Roots[0].URI, "/created-later") {
		t.Errorf("expected /created-later, got %q", result.Roots[0].URI)
	}
}

func TestDefaultWorkspaceRootCandidates_ContainsAttachments(t *testing.T) {
	shannonDir := t.TempDir()
	cands := DefaultWorkspaceRootCandidates(shannonDir)

	expectedAttachments := filepath.Join(shannonDir, "tmp", "attachments")
	found := false
	for _, p := range cands {
		if p == expectedAttachments {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("defaults missing attachments dir %q; got %v", expectedAttachments, cands)
	}

	// Home-relative defaults are only meaningful when a home dir is resolvable.
	if runtime.GOOS == "darwin" {
		if len(cands) < 4 {
			t.Errorf("expected attachments + 3 home dirs on darwin, got %d: %v", len(cands), cands)
		}
	}
}

func TestPathToFileURI(t *testing.T) {
	got := pathToFileURI("/Users/alice/work")
	if got != "file:///Users/alice/work" {
		t.Errorf("got %q, want file:///Users/alice/work", got)
	}
}

func TestRootsHandler_NilClientOptionSafe(t *testing.T) {
	var h *RootsHandler
	if opt := h.clientOption(); opt != nil {
		t.Error("nil handler must return nil client option")
	}
}
