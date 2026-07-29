package tools

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/Kocoro-lab/ShanClaw/internal/mcp"
)

// Result-side relative-path translation for file-producing MCP servers.
//
// playwright-mcp renders artifact links RELATIVE to the client's first
// advertised root (its response.js resolves against firstRootPath). In the
// daemon architecture that root (~/.shannon/tmp/attachments) is NOT the
// session CWD, so the model resolves the link against the wrong directory,
// misses, and goes hunting (2026-07-29: a 242-second `find /`). Single-CWD
// agent hosts avoid this structurally — agent CWD, MCP child CWD and
// advertised root coincide — which the long-lived multi-session daemon
// cannot replicate. Translating against the daemon's own advertised root is
// the architectural equivalent.
//
// Scope is deliberately narrow, mirroring fileProducingMCPArgs: only servers
// whose relative-path semantics we KNOW get translated — the built-in table
// below plus any server whose config declares workspace_base. Everything
// else stays opaque (a Notion slug or code snippet may look like a path).
// The final judgment is existence: a candidate only annotates when the
// joined path is a real file under the base.

// resultPathsRelativeToFirstRoot lists servers whose result links resolve
// against the client's first advertised MCP root.
var resultPathsRelativeToFirstRoot = map[string]bool{
	"playwright": true,
}

// mdRelLinkRe matches markdown links `[title](target)`. Targets are filtered
// further in markdownRelPathCandidates.
var mdRelLinkRe = regexp.MustCompile(`\[[^\]\n]*\]\(([^)\s]+)\)`)

// maxResultPathCandidates bounds per-result annotation work. A tool result
// legitimately references a handful of artifacts (screenshot + snapshot);
// beyond that the extra links are page content, not artifacts. When it binds,
// later links are simply not annotated — no override needed.
const maxResultPathCandidates = 4

// markdownRelPathCandidates extracts relative-path-looking markdown link
// targets from content, in order, deduplicated. URLs, anchors, mailto and
// absolute paths are skipped: absolute paths need no translation, the rest
// are not filesystem paths.
func markdownRelPathCandidates(content string) []string {
	matches := mdRelLinkRe.FindAllStringSubmatch(content, -1)
	seen := make(map[string]struct{}, len(matches))
	var out []string
	for _, m := range matches {
		target := m[1]
		if target == "" || filepath.IsAbs(target) {
			continue
		}
		if strings.Contains(target, "://") || strings.HasPrefix(target, "#") || strings.Contains(target, ":") {
			// "scheme://", "#anchor", "mailto:x@y" — not filesystem paths.
			continue
		}
		if _, dup := seen[target]; dup {
			continue
		}
		seen[target] = struct{}{}
		out = append(out, target)
		if len(out) >= maxResultPathCandidates {
			break
		}
	}
	return out
}

// resultPathBase resolves the directory a server's relative result paths are
// rendered against, or "" when unknown (→ no translation).
func resultPathBase(serverName string, manager *mcp.ClientManager) string {
	if cfg, ok := manager.ConfigFor(serverName); ok && strings.TrimSpace(cfg.WorkspaceBase) != "" {
		base := strings.TrimSpace(cfg.WorkspaceBase)
		if strings.HasPrefix(base, "~/") || base == "~" {
			home, err := os.UserHomeDir()
			if err != nil {
				return ""
			}
			if base == "~" {
				base = home
			} else {
				base = filepath.Join(home, base[2:])
			}
		}
		if !filepath.IsAbs(base) {
			return ""
		}
		return filepath.Clean(base)
	}
	if resultPathsRelativeToFirstRoot[serverName] {
		return manager.FirstAdvertisedRoot()
	}
	return ""
}

// maybeAnnotateResultPaths appends "Saved to: <abs>" lines for relative
// artifact links in a successful MCP result, translated against the server's
// known path base. Candidates that do not resolve to an existing regular
// file inside the base are left untouched — existence is what separates real
// artifact paths from path-looking strings, so unknown content can never be
// mis-annotated. Returns content unchanged when the server has no known base.
func maybeAnnotateResultPaths(serverName, content string, manager *mcp.ClientManager) string {
	if manager == nil || content == "" {
		return content
	}
	base := resultPathBase(serverName, manager)
	if base == "" {
		return content
	}
	sep := string(filepath.Separator)
	for _, rel := range markdownRelPathCandidates(content) {
		abs := filepath.Clean(filepath.Join(base, rel))
		// Containment: a traversal target ("../escape") must never resolve
		// outside the base the server was told about.
		if !strings.HasPrefix(abs+sep, base+sep) {
			continue
		}
		info, err := os.Stat(abs)
		if err != nil || info.IsDir() {
			continue
		}
		content = annotateAbsPath(content, abs)
	}
	return content
}
