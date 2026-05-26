//go:build !windows

package mcp

import (
	"context"
	"os"
	"os/exec"
	"syscall"
	"time"

	"github.com/mark3labs/mcp-go/client/transport"
)

// processGroupCmdFunc returns a transport.CommandFunc that spawns the MCP
// stdio subprocess in its own process group, so the daemon can kill not
// just the direct child but every descendant when the context is cancelled.
//
// Background: orphan-subprocess bug — npx-bridged servers like mcp-remote
// run as a multi-process chain (`npx` → `npm exec` → `node mcp-remote`).
// exec.CommandContext's default Cancel only kills the direct child (`npx`),
// leaving the grandchild `node mcp-remote` holding the OAuth callback
// listen port. Next time the user toggles the server back on, the new
// process hits EADDRINUSE and crashes immediately. The fix is to put the
// chain into a fresh process group at spawn time and signal `-pgid` on
// cancel so every member dies together.
//
// WaitDelay=3s gives the group a SIGTERM grace window; if anything is
// still alive when exec.Cmd.Wait() returns (or hits the delay), Go sends
// SIGKILL to the leader as a backstop.
func processGroupCmdFunc(ctx context.Context, command string, env []string, args []string) (*exec.Cmd, error) {
	cmd := exec.CommandContext(ctx, command, args...)
	cmd.Env = append(os.Environ(), env...)
	cmd.SysProcAttr = &syscall.SysProcAttr{
		Setpgid: true,
	}
	cmd.Cancel = func() error {
		if cmd.Process == nil {
			return nil
		}
		// Negative PID targets the process group whose leader is this PID.
		// Setpgid above guarantees the child IS its own group leader.
		if err := syscall.Kill(-cmd.Process.Pid, syscall.SIGTERM); err != nil {
			// Fallback: direct kill of the leader, matching exec.Cmd's default.
			return cmd.Process.Kill()
		}
		return nil
	}
	cmd.WaitDelay = 3 * time.Second
	return cmd, nil
}

// withProcessGroup returns the stdio transport option that activates
// processGroupCmdFunc. Wrapped in a helper so callers don't have to
// import the mcp-go transport package directly.
func withProcessGroup() transport.StdioOption {
	return transport.WithCommandFunc(processGroupCmdFunc)
}
