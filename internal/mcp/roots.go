package mcp

import (
	"context"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"

	mcpclient "github.com/mark3labs/mcp-go/client"
	"github.com/mark3labs/mcp-go/mcp"
)

// RootsHandler advertises workspace roots to MCP servers that honor the
// client `roots` capability (e.g. playwright-mcp, whose `browser_file_upload`
// restricts access to directories declared as workspace roots). The handler
// owns a normalized, deduplicated list of candidate roots and, at request
// time, returns only the subset that currently exists on disk — so an
// advertised root cannot point at a stale path the caller wrote earlier.
type RootsHandler struct {
	mu    sync.RWMutex
	roots []string // absolute, cleaned, deduped — order preserved for display
}

// NewRootsHandler builds a handler from the given candidate paths (typically
// defaults plus any user-configured extras). Paths are tilde-expanded,
// converted to absolute form, cleaned, and deduplicated. Invalid entries
// (empty strings, paths that fail to resolve) are dropped at construction.
func NewRootsHandler(paths []string) *RootsHandler {
	seen := make(map[string]struct{}, len(paths))
	resolved := make([]string, 0, len(paths))
	for _, p := range paths {
		abs := normalizeRoot(p)
		if abs == "" {
			continue
		}
		if _, dup := seen[abs]; dup {
			continue
		}
		seen[abs] = struct{}{}
		resolved = append(resolved, abs)
	}
	return &RootsHandler{roots: resolved}
}

// ListRoots implements mcpclient.RootsHandler. Servers call this when they
// need the current workspace-root set (on handshake and, per MCP, after a
// `notifications/roots/list_changed` nudge — we don't emit that notification,
// so this reduces to a one-shot handshake response for most servers).
// Non-existent or non-directory entries are filtered out so we don't lie
// to servers about paths that have since been cleaned up.
func (h *RootsHandler) ListRoots(_ context.Context, _ mcp.ListRootsRequest) (*mcp.ListRootsResult, error) {
	h.mu.RLock()
	candidates := make([]string, len(h.roots))
	copy(candidates, h.roots)
	h.mu.RUnlock()

	roots := make([]mcp.Root, 0, len(candidates))
	for _, abs := range candidates {
		info, err := os.Stat(abs)
		if err != nil || !info.IsDir() {
			continue
		}
		roots = append(roots, mcp.Root{
			URI:  pathToFileURI(abs),
			Name: filepath.Base(abs),
		})
	}
	return &mcp.ListRootsResult{Roots: roots}, nil
}

// Roots returns the normalized candidate list (pre-existence-filter) for
// diagnostics and tests.
func (h *RootsHandler) Roots() []string {
	h.mu.RLock()
	defer h.mu.RUnlock()
	out := make([]string, len(h.roots))
	copy(out, h.roots)
	return out
}

// clientOption returns the mcp-go ClientOption that installs this handler on
// a client, or nil if the handler is nil. Using a typed helper keeps the
// nil-guard out of the caller.
func (h *RootsHandler) clientOption() mcpclient.ClientOption {
	if h == nil {
		return nil
	}
	return mcpclient.WithRootsHandler(h)
}

// DefaultWorkspaceRootCandidates returns the baseline roots ShanClaw
// advertises for any MCP server with roots support: the daemon's attachment
// staging directory (so materialized inline-image attachments are uploadable
// via browser_file_upload) plus the common user-visible directories people
// drop files into. Callers append any `mcp.workspace_roots` config extras
// before handing the slice to NewRootsHandler.
func DefaultWorkspaceRootCandidates(shannonDir string) []string {
	candidates := []string{}
	if shannonDir != "" {
		candidates = append(candidates,
			filepath.Join(shannonDir, "tmp", "attachments"),
			// Per-cloud-session scratch dirs live here. Advertising the parent
			// root lets MCP servers that gate file I/O on declared roots (e.g.
			// playwright-mcp for uploads) accept paths the daemon allocated
			// for browser_snapshot / browser_take_screenshot under
			// ~/.shannon/tmp/sessions/<id>/. ListRoots filters by existence
			// so advertising this before any session has allocated under it
			// is harmless.
			filepath.Join(shannonDir, "tmp", "sessions"),
		)
	}
	if home, err := os.UserHomeDir(); err == nil {
		candidates = append(candidates,
			filepath.Join(home, "Downloads"),
			filepath.Join(home, "Desktop"),
			filepath.Join(home, "Documents"),
		)
	}
	return candidates
}

// normalizeRoot tilde-expands, cleans, and absolutizes a candidate path.
// Returns empty string on failure so callers can drop the entry.
func normalizeRoot(p string) string {
	p = strings.TrimSpace(p)
	if p == "" {
		return ""
	}
	if strings.HasPrefix(p, "~") {
		home, err := os.UserHomeDir()
		if err != nil {
			return ""
		}
		rest := strings.TrimPrefix(p, "~")
		if rest == "" {
			p = home
		} else if rest[0] == '/' || rest[0] == os.PathSeparator {
			p = filepath.Join(home, rest[1:])
		} else {
			// "~user" form — not supported; leave unresolved and fail absolutize.
			return ""
		}
	}
	abs, err := filepath.Abs(p)
	if err != nil {
		return ""
	}
	return filepath.Clean(abs)
}

// pathToFileURI converts an absolute filesystem path to an RFC 8089 file URI.
// Per MCP spec, Root.URI must start with `file://`.
func pathToFileURI(abs string) string {
	u := url.URL{Scheme: "file", Path: abs}
	return u.String()
}
