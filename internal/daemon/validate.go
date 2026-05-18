package daemon

import (
	"fmt"
	"path/filepath"
	"regexp"
	"strings"
)

// safeSessionIDPattern enforces a strict allowlist for session ids that get
// joined onto a filesystem path. Kocoro's production id format is
// "YYYY-MM-DD-<hex12>" (e.g. "2026-05-15-ca10391dad3a"); legacy ids include
// alphanumerics with - and _. The leading-char restriction blocks "." and
// ".." early so they never reach filepath.Base.
var safeSessionIDPattern = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9._-]*$`)

// ValidateSessionID rejects ids that contain path separators, "."/"..", or
// any character outside the allowlist. Returns an error for empty input
// because every external caller (HTTP handlers) is responsible for "empty
// means create new session" upstream — by the time we reach the helper, the
// id should be non-empty.
func ValidateSessionID(id string) error {
	if id == "" {
		return fmt.Errorf("session id is empty")
	}
	if id != filepath.Base(id) || strings.ContainsAny(id, `/\`) {
		return fmt.Errorf("invalid session id: contains path separator")
	}
	if !safeSessionIDPattern.MatchString(id) {
		return fmt.Errorf("invalid session id: contains unsafe characters")
	}
	return nil
}
