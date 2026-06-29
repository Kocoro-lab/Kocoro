//go:build windows

package memory

import (
	"os"
	"os/exec"
	"strconv"
	"syscall"
	"time"
)

// setProcGroup creates a new process group for the sidecar so terminateProcessTree
// can target the whole tree via taskkill. Windows counterpart of the POSIX
// Setpgid variant.
func setProcGroup(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{CreationFlags: syscall.CREATE_NEW_PROCESS_GROUP}
}

// terminateProcessTree force-terminates the sidecar subtree on Windows.
//
// Unlike the POSIX path (SIGTERM, wait grace, then SIGKILL), there is no usable
// graceful step here: a graceful `taskkill /T` (without /F) cannot deliver a
// close request to a console-less child like the `tlm serve` subprocess — it
// returns "can only be terminated forcefully" immediately, so waiting out the
// grace window before force-killing would just stall every Shutdown by `grace`
// for no benefit. We force-kill the subtree directly and then wait briefly for
// the reaper to observe the exit. grace is accepted for signature parity.
func terminateProcessTree(proc *os.Process, done <-chan struct{}, grace time.Duration) {
	_ = grace // no graceful phase on Windows; see doc comment
	pid := strconv.Itoa(proc.Pid)
	_ = exec.Command("taskkill", "/T", "/F", "/PID", pid).Run()
	select {
	case <-done:
	case <-time.After(1 * time.Second):
	}
}
