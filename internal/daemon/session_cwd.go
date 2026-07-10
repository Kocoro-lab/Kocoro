package daemon

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// cloudSourceSet enumerates request sources whose final rendering is owned by
// Shannon Cloud. These sources never carry an effective CWD from the request
// path — there is no user shell, agent config, or prior session CWD to fall
// back to — so the runner allocates a per-session scratch directory under
// ~/.shannon/tmp/sessions/<id>/ to give filesystem tools (file_read,
// file_write) and file-producing MCP tools (screenshots, snapshots) a real
// working directory to land in.
//
// Most cloud sources use the "plain" output profile, but the format axis is
// decided independently by outputFormatForSource in runner.go — Feishu/Lark/Teams
// are cloud sources that emit "markdown" (see markdownCloudSources). Keep this
// list aligned with that mapping for the CWD axis; the two need not agree on format.
//
// Cross-reference: prompt suggestions are gated to a separate foreground
// allow-list (promptSuggestionSources in runner.go). Sources here are excluded
// from it by construction — IM channels have no /suggestion consumer. The two
// lists are intentionally disjoint, but if you add a NEW foreground source
// (one with a suggestion UI), remember to add it to that allow-list too, or it
// will silently never receive suggestions. The gate is fail-closed.
var cloudSourceSet = map[string]struct{}{
	"slack":    {},
	"line":     {},
	"feishu":   {},
	"lark":     {},
	"wecom":    {},
	"wechat":   {},
	"teams":    {},
	"telegram": {},
	"webhook":  {},
}

// isCloudSource reports whether the request source is one Kocoro Cloud owns
// the final rendering for. Matching is case-insensitive and whitespace-
// trimmed to mirror outputFormatForSource's normalization.
func isCloudSource(source string) bool {
	_, ok := cloudSourceSet[strings.ToLower(strings.TrimSpace(source))]
	return ok
}

// ensureCloudSessionTmpDir creates (or confirms) the per-session scratch
// directory under <shannonDir>/tmp/sessions/<sessionID>/ for cloud sources
// that arrive without any CWD. Returns:
//
//	(path, nil)   — directory exists (newly created or pre-existing)
//	("", nil)     — not applicable: non-cloud source, empty shannonDir, or empty sessionID
//	("", err)     — applicable but mkdir failed
//
// The returned path is always absolute. Callers pass it into
// cwdctx.ResolveEffectiveCWD as the lowest-priority fallback so any real CWD
// (request/resumed/agent) still wins.
//
// sessionID is treated as opaque. Validation happens at session-creation time
// (internal/session), so characters like "/" cannot reach here; we still call
// filepath.Clean defensively to keep any future ID format change from
// escaping the tmp root.
func ensureCloudSessionTmpDir(shannonDir, sessionID, source string) (string, error) {
	if shannonDir == "" || sessionID == "" || !isCloudSource(source) {
		return "", nil
	}
	// filepath.Join+Clean flattens any embedded "../" attempts; combined with
	// the hard check below it guarantees the result stays under shannonDir/tmp/sessions.
	root := filepath.Clean(filepath.Join(shannonDir, "tmp", "sessions"))
	dir := filepath.Clean(filepath.Join(root, sessionID))
	if !strings.HasPrefix(dir+string(filepath.Separator), root+string(filepath.Separator)) {
		return "", fmt.Errorf("session id %q escapes tmp sessions root", sessionID)
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", fmt.Errorf("create cloud session cwd: %w", err)
	}
	return dir, nil
}

// cloudSessionTmpCleanup returns a func that removes the per-session scratch
// directory. Safe to call after ensureCloudSessionTmpDir even when the dir
// has been reused across resumes: os.RemoveAll no-ops on missing paths.
// Registered via sessMgr.OnSessionClose so eviction from the SessionCache
// (inactivity, daemon shutdown) reclaims disk while the session is alive
// through any number of turns.
func cloudSessionTmpCleanup(dir string) func() {
	if dir == "" {
		return func() {}
	}
	return func() {
		_ = os.RemoveAll(dir)
	}
}
