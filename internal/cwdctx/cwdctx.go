package cwdctx

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// ErrNoSessionCWD is returned by ResolveFilesystemPath when the caller passes
// a relative path but no session CWD is set in the context. Filesystem tools
// (glob, grep, file_read, directory_list) should treat this as a hard failure
// and ask the model to supply an absolute path.
var ErrNoSessionCWD = errors.New("no session working directory is set; specify an absolute path")

type contextKey struct{}

// WithSessionCWD stores the session CWD in the context.
func WithSessionCWD(ctx context.Context, cwd string) context.Context {
	return context.WithValue(ctx, contextKey{}, cwd)
}

// FromContext retrieves the session CWD from context, returns "" if unset.
func FromContext(ctx context.Context) string {
	v, _ := ctx.Value(contextKey{}).(string)
	return v
}

// expandHome expands a leading ~ to the user's home directory.
func expandHome(path string) string {
	if path == "~" {
		home, err := os.UserHomeDir()
		if err != nil {
			return path
		}
		return home
	}
	if strings.HasPrefix(path, "~/") {
		home, err := os.UserHomeDir()
		if err != nil {
			return path
		}
		return filepath.Join(home, path[2:])
	}
	return path
}

// ResolvePath resolves a path against the session CWD in ctx.
// Absolute paths and ~ paths are returned as-is (after ~ expansion).
// Empty or "." returns the session CWD.
// Falls back to os.Getwd() if no session CWD is set.
func ResolvePath(ctx context.Context, path string) string {
	sessionCWD := FromContext(ctx)

	base := sessionCWD
	if base == "" {
		cwd, err := os.Getwd()
		if err == nil {
			base = cwd
		}
	}

	if path == "" || path == "." {
		return base
	}

	if strings.HasPrefix(path, "~") {
		return filepath.Clean(expandHome(path))
	}

	if filepath.IsAbs(path) {
		return filepath.Clean(path)
	}

	return filepath.Clean(filepath.Join(base, path))
}

// IsUnderSessionCWD checks if the resolved path is under the session CWD.
// Both paths are symlink-resolved to prevent escapes via symlinks pointing
// outside the CWD.
func IsUnderSessionCWD(ctx context.Context, path string) bool {
	sessionCWD := FromContext(ctx)
	if sessionCWD == "" {
		return false
	}
	resolved := ResolvePath(ctx, path)
	// Resolve symlinks on both sides to prevent escape via symlink targets.
	// If EvalSymlinks fails (path doesn't exist yet), try resolving the
	// parent directory and rejoin the filename. This handles the common case
	// of referencing a file that doesn't exist yet in a symlinked dir.
	resolved = evalSymlinksBestEffort(resolved)
	cwdClean := evalSymlinksBestEffort(filepath.Clean(sessionCWD))
	if resolved == cwdClean {
		return true
	}
	return strings.HasPrefix(resolved, cwdClean+string(filepath.Separator))
}

// ResolveFilesystemPath resolves a path intended for filesystem access. Unlike
// ResolvePath, it refuses to silently join against a fallback working directory:
// if the path is relative (or empty/".") and no session CWD is set in the
// context, it returns ErrNoSessionCWD. Absolute and ~ paths always work.
//
// Filesystem tools should use this instead of ResolvePath so that a session
// with no declared working directory cannot accidentally operate on $HOME or
// arbitrary ancestor directories via silent relative-path fallback. Note that
// absolute paths are NOT sandboxed here — that is the job of the permissions
// layer (SafeChecker, allowed/denied_commands). This function only eliminates
// the "relative path joined against the wrong base" class of leak.
func ResolveFilesystemPath(ctx context.Context, path string) (string, error) {
	if strings.HasPrefix(path, "~") {
		return filepath.Clean(expandHome(path)), nil
	}
	if filepath.IsAbs(path) {
		return filepath.Clean(path), nil
	}
	sessionCWD := FromContext(ctx)
	if sessionCWD == "" {
		return "", ErrNoSessionCWD
	}
	if path == "" || path == "." {
		return filepath.Clean(sessionCWD), nil
	}
	return filepath.Clean(filepath.Join(sessionCWD, path)), nil
}

// ResolveEffectiveCWD returns the first non-empty value among requestCWD,
// sessionCWD, agentCWD. When all three are empty it returns the empty string:
// the caller is responsible for deciding what "no working directory" means
// in its context. Daemon-routed runs treat empty as "no filesystem scope";
// local CLIs (one-shot, TUI) fall back to os.Getwd() explicitly.
func ResolveEffectiveCWD(requestCWD, sessionCWD, agentCWD string) string {
	for _, cwd := range []string{requestCWD, sessionCWD, agentCWD} {
		if cwd != "" {
			return cwd
		}
	}
	return ""
}

// ValidateCWD validates that cwd is an absolute path to an existing directory.
// Empty string returns nil (means "use fallback").
func ValidateCWD(cwd string) error {
	if cwd == "" {
		return nil
	}
	if !filepath.IsAbs(cwd) {
		return fmt.Errorf("cwd must be an absolute path, got %q", cwd)
	}
	info, err := os.Stat(cwd)
	if err != nil {
		return fmt.Errorf("cwd %q: %w", cwd, err)
	}
	if !info.IsDir() {
		return fmt.Errorf("cwd %q is not a directory", cwd)
	}
	return nil
}

// evalSymlinksBestEffort resolves symlinks as far up the path as possible.
// If the full path doesn't exist, it walks up the directory tree until it
// finds an existing ancestor, resolves that, and rejoins the tail.
func evalSymlinksBestEffort(path string) string {
	if real, err := filepath.EvalSymlinks(path); err == nil {
		return real
	}
	// Walk up until we find an existing ancestor to resolve.
	clean := filepath.Clean(path)
	for dir := filepath.Dir(clean); dir != clean; clean, dir = dir, filepath.Dir(dir) {
		if real, err := filepath.EvalSymlinks(dir); err == nil {
			tail, _ := filepath.Rel(dir, filepath.Clean(path))
			return filepath.Join(real, tail)
		}
	}
	return path
}
