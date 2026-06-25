//go:build !windows

package hooks

import (
	"os/exec"
	"syscall"
)

// setProcGroupKill puts the hook command into its own process group so that on
// timeout (ctx cancel) we can SIGKILL the entire tree — a hook that spawns
// grandchildren would otherwise leave them orphaned. POSIX path: Setpgid +
// Kill(-pid). Mirror of the Windows variant in proc_windows.go.
func setProcGroupKill(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		// Kill the entire process group (negative PID).
		return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	}
}
