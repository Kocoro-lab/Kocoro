//go:build windows

package cmd

import (
	"os"
	"os/exec"
	"strconv"
)

// terminateDaemon stops the daemon process on Windows. os.Process.Signal does
// not support SIGTERM on Windows, so we force-terminate the process tree with
// taskkill /T (subtree) /F (force). Graceful HTTP /shutdown remains the primary
// path in stopExistingDaemon; this is the PID-file fallback.
func terminateDaemon(proc *os.Process) error {
	return exec.Command("taskkill", "/T", "/F", "/PID", strconv.Itoa(proc.Pid)).Run()
}
