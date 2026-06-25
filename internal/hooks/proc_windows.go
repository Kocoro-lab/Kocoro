//go:build windows

package hooks

import (
	"os/exec"
	"strconv"
	"syscall"
)

// setProcGroupKill is the Windows counterpart to the POSIX process-group kill.
// Windows has no Setpgid/Kill(-pid); we create a new process group and, on
// cancel, force-kill the whole process tree with taskkill /T (subtree) /F
// (force) — the equivalent of SIGKILL-ing the group.
func setProcGroupKill(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{CreationFlags: syscall.CREATE_NEW_PROCESS_GROUP}
	cmd.Cancel = func() error {
		if cmd.Process == nil {
			return nil
		}
		return exec.Command("taskkill", "/T", "/F", "/PID", strconv.Itoa(cmd.Process.Pid)).Run()
	}
}
