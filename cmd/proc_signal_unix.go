//go:build !windows

package cmd

import (
	"os"
	"syscall"
)

// terminateDaemon asks the daemon process to stop. On POSIX we send SIGTERM so
// the daemon runs its graceful shutdown handler. Mirror of the Windows variant.
func terminateDaemon(proc *os.Process) error {
	return proc.Signal(syscall.SIGTERM)
}
