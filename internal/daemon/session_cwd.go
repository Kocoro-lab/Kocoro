package daemon

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// cloudSourceSet enumerates request sources whose final rendering is owned by
// Shannon Cloud (plain-text output profile). These sources never carry an
// effective CWD from the request path — there is no user shell, agent config,
// or prior session CWD to fall back to — so the runner allocates a per-session
// scratch directory under ~/.shannon/tmp/sessions/<id>/ to give filesystem
// tools (file_read, file_write) and file-producing MCP tools (screenshots,
// snapshots) a real working directory to land in.
//
// Keep this list aligned with outputFormatForSource in runner.go; that mapping
// is the authoritative definition of "cloud-distributed source".
var cloudSourceSet = map[string]struct{}{
	"slack":    {},
	"line":     {},
	"feishu":   {},
	"lark":     {},
	"telegram": {},
	"webhook":  {},
}

// isCloudSource reports whether the request source is one ShanClaw Cloud owns
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
