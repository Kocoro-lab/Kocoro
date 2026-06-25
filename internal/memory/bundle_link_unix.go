//go:build !windows

package memory

import (
	"fmt"
	"os"
	"path/filepath"
)

// swapCurrent atomically points <bundleRoot>/current at finalDir.
//
// POSIX: create the link at current.tmp, then rename it over current — both the
// symlink creation and the rename are atomic on the same filesystem, so a
// concurrent reader (the tlm sidecar) never sees a missing or half-written
// pointer. Mirror of the Windows junction variant in bundle_link_windows.go.
func swapCurrent(bundleRoot, finalDir string) error {
	tmpLink := filepath.Join(bundleRoot, "current.tmp")
	_ = os.Remove(tmpLink)
	if err := os.Symlink(finalDir, tmpLink); err != nil {
		return fmt.Errorf("symlink current.tmp: %w", err)
	}
	if err := os.Rename(tmpLink, filepath.Join(bundleRoot, "current")); err != nil {
		return fmt.Errorf("swap current symlink: %w", err)
	}
	return nil
}
