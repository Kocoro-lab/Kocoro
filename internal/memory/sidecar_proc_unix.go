//go:build !windows

package memory

import (
	"os"
	"os/exec"
	"syscall"
	"time"
)

// setProcGroup puts the sidecar into its own process group so terminateProcessTree
// can signal the whole group. Mirror of sidecar_proc_windows.go.
func setProcGroup(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
}

// terminateProcessTree gracefully stops the sidecar and its children: SIGTERM
// the group, wait up to grace, then SIGKILL the group. done closes when the
// child has been reaped (cmd.Wait returned). Falls back to signalling the bare
// PID if the process group can't be resolved.
func terminateProcessTree(proc *os.Process, done <-chan struct{}, grace time.Duration) {
	pgid, err := syscall.Getpgid(proc.Pid)
	if err != nil {
		// Fallback: signal the process directly.
		pgid = proc.Pid
	}
	_ = syscall.Kill(-pgid, syscall.SIGTERM)
	select {
	case <-done:
	case <-time.After(grace):
		_ = syscall.Kill(-pgid, syscall.SIGKILL)
		select {
		case <-done:
		case <-time.After(1 * time.Second):
		}
	}
}
