//go:build windows

package tools

import (
	"os"
	"os/exec"
	"strconv"
	"syscall"
)

// setBashProcGroup is the Windows counterpart: create a new process group and,
// on timeout, force-kill the whole tree with taskkill /T (subtree) /F (force).
// This is the equivalent of SIGKILL-ing the POSIX process group and prevents
// backgrounded grandchildren from outliving the bash command.
func setBashProcGroup(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{CreationFlags: syscall.CREATE_NEW_PROCESS_GROUP}
	cmd.Cancel = func() error {
		if cmd.Process != nil {
			return exec.Command("taskkill", "/T", "/F", "/PID", strconv.Itoa(cmd.Process.Pid)).Run()
		}
		return os.ErrProcessDone
	}
}
