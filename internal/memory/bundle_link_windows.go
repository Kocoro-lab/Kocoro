//go:build windows

package memory

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// swapCurrent points <bundleRoot>/current at finalDir using a directory
// junction instead of a symlink.
//
// Windows reserves os.Symlink for elevated / Developer-Mode processes (it fails
// with ERROR_PRIVILEGE_NOT_HELD otherwise), which would break every bundle
// install on a stock host. A junction is an unprivileged directory reparse
// point: any reader — including the tlm sidecar opening current\<file> —
// traverses it transparently with no symlink awareness, and os.Readlink
// resolves it so currentTs() keeps working.
//
// Junctions cannot be rename-swapped over an existing directory (MoveFileEx
// refuses a directory destination), so unlike the POSIX path we remove then
// recreate. The brief window with no current pointer is harmless: installs are
// rare, the daemon reloads the sidecar only after this returns, and the
// sidecar's own poller/reload retries if it reads mid-swap.
func swapCurrent(bundleRoot, finalDir string) error {
	current := filepath.Join(bundleRoot, "current")
	_ = os.Remove(current) // drop any existing junction; ignore "not found"
	// mklink is a cmd.exe builtin; /J creates a junction without elevation.
	out, err := exec.Command("cmd", "/c", "mklink", "/J", current, finalDir).CombinedOutput()
	if err != nil {
		return fmt.Errorf("mklink /J current: %w: %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}
