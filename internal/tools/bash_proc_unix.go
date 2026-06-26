//go:build !windows

package tools

import (
	"os"
	"os/exec"
	"syscall"
)

// setBashProcGroup puts sh and any children it spawns into a new process group
// so we can SIGKILL the whole tree on timeout. Without Setpgid, exec's default
// Cancel kills only sh's PID — long-running grandchildren (e.g.
// `python -m http.server` backgrounded from sh) survive as orphans and keep
// ports bound until the user kills them by hand. Mirror of bash_proc_windows.go.
func setBashProcGroup(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		if cmd.Process != nil {
			return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		}
		return os.ErrProcessDone
	}
}
