//go:build windows

package mcp

import (
	"context"
	"os"
	"os/exec"
	"strconv"
	"syscall"
	"time"

	"github.com/mark3labs/mcp-go/client/transport"
)

// processGroupCmdFunc returns a transport.CommandFunc that spawns the MCP stdio
// subprocess in a new process group so the daemon can tear down the whole tree
// (not just the direct child) when the context is cancelled.
//
// Windows counterpart of the POSIX Setpgid + kill(-pgid) variant (see
// processgroup_unix.go). There is no pgid signalling here, so we create the
// group with CREATE_NEW_PROCESS_GROUP and, on cancel, force-kill the subtree
// with taskkill /T /F — the same approach the hooks/bash/sidecar Windows paths
// use. This matters for npx-bridged servers (npx → npm exec → node mcp-remote):
// without it the grandchild survives and keeps holding the OAuth callback port,
// so the next toggle hits EADDRINUSE and crashes.
//
// WaitDelay gives exec.Cmd.Wait() a bounded backstop if a straggler lingers.
func processGroupCmdFunc(ctx context.Context, command string, env []string, args []string) (*exec.Cmd, error) {
	cmd := exec.CommandContext(ctx, command, args...)
	cmd.Env = append(os.Environ(), env...)
	cmd.SysProcAttr = &syscall.SysProcAttr{CreationFlags: syscall.CREATE_NEW_PROCESS_GROUP}
	cmd.Cancel = func() error {
		if cmd.Process == nil {
			return nil
		}
		return exec.Command("taskkill", "/T", "/F", "/PID", strconv.Itoa(cmd.Process.Pid)).Run()
	}
	cmd.WaitDelay = 3 * time.Second
	return cmd, nil
}

// withProcessGroup returns the stdio transport option that activates
// processGroupCmdFunc, mirroring the POSIX build.
func withProcessGroup() transport.StdioOption {
	return transport.WithCommandFunc(processGroupCmdFunc)
}
