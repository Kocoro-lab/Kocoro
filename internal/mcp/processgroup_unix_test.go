//go:build !windows

package mcp

import (
	"context"
	"sync"
	"syscall"
	"testing"
	"time"
)

func TestProcessGroupCmdFunc_SetsSetpgidAndCustomCancel(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	cmd, err := processGroupCmdFunc(ctx, "sleep", nil, []string{"30"})
	if err != nil {
		t.Fatalf("processGroupCmdFunc returned error: %v", err)
	}
	if cmd.SysProcAttr == nil || !cmd.SysProcAttr.Setpgid {
		t.Fatal("Setpgid must be true so cancel can SIGTERM the whole process group")
	}
	if cmd.Cancel == nil {
		t.Fatal("Cancel must be set to override the default kill-the-direct-child behavior")
	}
	if cmd.WaitDelay <= 0 {
		t.Fatal("WaitDelay must be set so a process that ignores SIGTERM still gets SIGKILL'd within a bounded time")
	}
}

func TestProcessGroupCmdFunc_KillsSubprocessOnCtxCancel(t *testing.T) {
	// Boot a `sleep 30` under a cancellable ctx; verify cancel reaps it
	// promptly (well under the 30s natural exit). This is the user-facing
	// behavior we depend on for OAuth-abandoned mcp-remote cleanup.
	ctx, cancel := context.WithCancel(context.Background())

	cmd, err := processGroupCmdFunc(ctx, "sleep", nil, []string{"30"})
	if err != nil {
		cancel()
		t.Fatalf("processGroupCmdFunc: %v", err)
	}
	if err := cmd.Start(); err != nil {
		cancel()
		t.Fatalf("cmd.Start: %v", err)
	}
	pid := cmd.Process.Pid

	// Confirm the process actually exists right after Start before we
	// cancel — otherwise a Start failure would let the test pass for the
	// wrong reason.
	if err := syscall.Kill(pid, 0); err != nil {
		cancel()
		t.Fatalf("subprocess %d not running after Start: %v", pid, err)
	}

	// Cancel + Wait in a goroutine so we can bound the test wall-clock.
	cancel()
	waitDone := make(chan error, 1)
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		waitDone <- cmd.Wait()
	}()

	select {
	case <-waitDone:
		// good: cancel killed the process within bounded time.
	case <-time.After(8 * time.Second):
		// 8s leaves room for the 3s WaitDelay backstop + scheduling slack
		// without coming anywhere close to sleep's natural 30s exit.
		_ = syscall.Kill(-pid, syscall.SIGKILL) // best-effort cleanup
		t.Fatal("cmd.Wait did not return within 8s after cancel — process-group kill is broken")
	}
	wg.Wait()

	// Verify the process is gone — Kill(pid, 0) returns ESRCH when no
	// such process exists.
	if err := syscall.Kill(pid, 0); err == nil {
		_ = syscall.Kill(pid, syscall.SIGKILL) // cleanup, just in case
		t.Errorf("subprocess %d still alive after ctx cancel", pid)
	}

}
