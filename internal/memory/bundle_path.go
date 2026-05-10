package memory

import (
	"os"
	"path/filepath"
)

// BundleRelativeTLMPath returns the absolute path to the tlm binary embedded
// in Kocoro Desktop's app bundle, relative to the shan executable:
//
//	<shan_exe>/../Helpers/tlm.app/Contents/MacOS/tlm
//
// Returns empty string when shan is not running inside the bundle (dev builds,
// CI, PATH-only installs). Callers treat an empty return as "memory disabled".
func BundleRelativeTLMPath() string {
	exe, err := os.Executable()
	if err != nil {
		return ""
	}
	// EvalSymlinks so code-signed apps accessed via a symlink resolve correctly.
	exe, err = filepath.EvalSymlinks(exe)
	if err != nil {
		return ""
	}
	return resolveFromExe(exe)
}

// resolveFromExe computes the bundle-relative tlm path from exePath and
// checks whether the file exists. Extracted for testability.
func resolveFromExe(exePath string) string {
	candidate := filepath.Clean(
		filepath.Join(filepath.Dir(exePath), "..", "Helpers",
			"tlm.app", "Contents", "MacOS", "tlm"),
	)
	if _, err := os.Stat(candidate); err == nil {
		return candidate
	}
	return ""
}
