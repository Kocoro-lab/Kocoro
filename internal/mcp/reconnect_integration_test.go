package mcp

import (
	"context"
	"sync"
	"testing"
	"time"
)

// TestReconnectScheduler_DrivesRealManagerUntilAttemptCap exercises the whole
// loop against a real ClientManager and a real (immediately failing) child
// process: connect fails → onResult reports it → the scheduler re-arms →
// StartConnectAll runs again, until the attempt cap stops it.
//
// This is the regression the unit tests cannot cover on their own: they prove
// the scheduler's arithmetic, this proves it is actually wired to something
// that spawns processes, and that the ladder terminates instead of spinning.
func TestReconnectScheduler_DrivesRealManagerUntilAttemptCap(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	mgr := NewClientManager()
	defer mgr.Close()

	const server = "always-fails"
	// A command that cannot be spawned: connect fails fast, with no port to
	// bind and no child to reap, so the test stays hermetic.
	cfg := MCPServerConfig{Command: "/nonexistent/shanclaw-reconnect-test-binary"}
	servers := map[string]MCPServerConfig{server: cfg}
	mgr.RegisterConfigs(servers)

	var (
		mu       sync.Mutex
		attempts int
		done     = make(chan struct{})
	)

	var onResult func(string, error)
	sched := NewReconnectScheduler(func(name string) {
		c, ok := mgr.ConfigFor(name)
		if !ok {
			return
		}
		mgr.StartConnectAll(ctx, map[string]MCPServerConfig{name: c}, 2*time.Second, onResult)
	})
	// Collapse the backoff ladder so the test runs in milliseconds. Still a
	// real timer — fn must not run inline (see ReconnectScheduler.after).
	sched.after = func(_ time.Duration, fn func()) func() {
		timer := time.AfterFunc(time.Millisecond, fn)
		return func() { timer.Stop() }
	}
	defer sched.Stop()

	onResult = func(name string, err error) {
		if err == nil {
			t.Errorf("connect to %s unexpectedly succeeded", name)
			return
		}
		mu.Lock()
		attempts++
		reached := attempts == reconnectMaxAttempts+1 // initial + every retry
		mu.Unlock()
		if reached {
			close(done)
			return
		}
		sched.OnFailure(name)
	}

	mgr.StartConnectAll(ctx, servers, 2*time.Second, onResult)

	select {
	case <-done:
	case <-ctx.Done():
		mu.Lock()
		got := attempts
		mu.Unlock()
		t.Fatalf("timed out after %d attempts, want %d", got, reconnectMaxAttempts+1)
	}

	// The cap must hold: one more failure report schedules nothing, so no
	// further attempt lands.
	sched.OnFailure(server)
	time.Sleep(50 * time.Millisecond)

	mu.Lock()
	got := attempts
	mu.Unlock()
	if got != reconnectMaxAttempts+1 {
		t.Fatalf("scheduler kept retrying past the cap: %d attempts, want %d", got, reconnectMaxAttempts+1)
	}
}

// TestReconnectScheduler_StopHaltsRealRetryLoop proves the shutdown path: once
// stopped, a pending retry never reaches the manager. A resurrected connect on
// a superseded manager is exactly the process-group leak the ownership rules
// exist to prevent.
func TestReconnectScheduler_StopHaltsRealRetryLoop(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	mgr := NewClientManager()
	defer mgr.Close()

	const server = "always-fails"
	cfg := MCPServerConfig{Command: "/nonexistent/shanclaw-reconnect-test-binary"}
	mgr.RegisterConfigs(map[string]MCPServerConfig{server: cfg})

	var mu sync.Mutex
	reconnects := 0

	sched := NewReconnectScheduler(func(name string) {
		mu.Lock()
		reconnects++
		mu.Unlock()
		c, ok := mgr.ConfigFor(name)
		if !ok {
			return
		}
		mgr.StartConnectAll(ctx, map[string]MCPServerConfig{name: c}, 2*time.Second, nil)
	})
	sched.after = func(_ time.Duration, fn func()) func() {
		timer := time.AfterFunc(20*time.Millisecond, fn)
		return func() { timer.Stop() }
	}

	sched.OnFailure(server)
	sched.Stop()

	time.Sleep(100 * time.Millisecond) // well past the pending retry's delay

	mu.Lock()
	got := reconnects
	mu.Unlock()
	if got != 0 {
		t.Fatalf("Stop must cancel the pending retry, but it reconnected %d time(s)", got)
	}
}
