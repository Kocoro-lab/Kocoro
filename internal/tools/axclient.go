package tools

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
)

// AXRequest is a JSON-RPC request sent to ax_server.
type AXRequest struct {
	ID     int64  `json:"id"`
	Method string `json:"method"`
	Params any    `json:"params"`
}

// AXResponse is a JSON-RPC response from ax_server.
type AXResponse struct {
	ID     int64            `json:"id"`
	Result json.RawMessage  `json:"result,omitempty"`
	Error  *AXError         `json:"error,omitempty"`
}

// AXError is an error returned by ax_server.
type AXError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

// SharedAXClient returns the process-wide singleton AXClient.
// Both the tools (computer_use, accessibility, computer, wait) and daemon permission
// endpoints must use the same instance, because the socket server
// accepts only one client at a time.
func SharedAXClient() *AXClient {
	sharedOnce.Do(func() {
		sharedInstance = &AXClient{}
	})
	return sharedInstance
}

var (
	sharedOnce     sync.Once
	sharedInstance *AXClient
)

// AXClient manages a persistent ax_server process and multiplexes
// requests by ID. Multiple goroutines can call Call() concurrently.
//
// Two transport modes:
// - Bundled: ax_server is inside a .app bundle, launched via LaunchServices
//   (`open -a`), communicates over a Unix domain socket. Required for TCC
//   permission attribution on macOS.
// - Fallback: ax_server is a bare binary, launched via exec.Command,
//   communicates over stdin/stdout pipes. Used for dev, npm, and CLI.
type AXClient struct {
	mu      sync.Mutex // guards process lifecycle (start/restart)
	writeMu sync.Mutex // guards writes to ax_server

	// Transport-agnostic I/O
	writer io.WriteCloser
	nextID atomic.Int64

	// Process management
	cmd        *exec.Cmd // non-nil in fallback mode
	conn       net.Conn  // non-nil in bundled mode
	bundlePID  int       // ax_server PID in bundled mode (for cleanup)
	started    bool

	pendingMu sync.Mutex
	pending   map[int64]chan AXResponse
}

// Ensure starts the ax_server process if not already running.
func (c *AXClient) Ensure(ctx context.Context) error {
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.started {
		return nil
	}

	binPath, bundlePath, err := AXServerPaths()
	if err != nil {
		return err
	}

	c.pending = make(map[int64]chan AXResponse)

	if bundlePath != "" {
		return c.startBundled(ctx, bundlePath)
	}
	return c.startFallback(binPath)
}

// startBundled launches ax_server via LaunchServices and connects over Unix socket.
func (c *AXClient) startBundled(ctx context.Context, bundlePath string) error {
	socketPath := AXSocketPath()

	// Try connecting to an existing socket first — ax_server may already be running
	// (e.g. from a previous open -a that's still alive).
	conn, err := net.Dial("unix", socketPath)
	if err != nil {
		// Not running or stale socket — clean up and launch fresh
		os.Remove(socketPath)

		// Launch via open(1) with -n (new instance) — gives ax_server its own TCC
		// identity and avoids reusing a stale instance with different --args.
		cmd := exec.CommandContext(ctx, "open", "-n", "-a", bundlePath, "--args", "--socket", socketPath)
		cmd.Stderr = os.Stderr
		if err := cmd.Run(); err != nil {
			return fmt.Errorf("ax_server launch: %w", err)
		}

		// Wait for socket to appear. Poll ctx-aware so a cancelled request (e.g. the
		// Desktop capture timeout) does not pin Ensure's lock for the full 10s, which
		// would block both the user's retry and any concurrent AX call.
		// Trade-off: a cancel mid-launch can let a retry spawn a second instance via
		// `open -n` (narrow window; Close() reaps strays) — acceptable vs. a 10s stall.
		deadline := time.Now().Add(10 * time.Second)
		for time.Now().Before(deadline) {
			conn, err = net.Dial("unix", socketPath)
			if err == nil {
				break
			}
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(100 * time.Millisecond):
			}
		}
	}
	if conn == nil {
		return fmt.Errorf("ax_server: socket not available after 10s at %s", socketPath)
	}

	c.conn = conn
	c.writer = conn
	c.started = true

	// Find the ax_server PID for cleanup on Close().
	// pgrep -f matches the socket path in the command line args.
	if out, err := exec.Command("pgrep", "-f", socketPath).Output(); err == nil {
		var pid int
		if _, err := fmt.Sscanf(string(out), "%d", &pid); err == nil {
			c.bundlePID = pid
		}
	}

	// Reader goroutine dispatches responses by ID.
	go c.readLoop(conn)

	return nil
}

// startFallback launches ax_server via exec.Command with stdin/stdout pipes.
func (c *AXClient) startFallback(binPath string) error {
	// Use exec.Command (not CommandContext) — the process lifecycle is managed
	// independently of any single request's context.
	c.cmd = exec.Command(binPath)
	var pipeErr error
	c.writer, pipeErr = c.cmd.StdinPipe()
	if pipeErr != nil {
		return fmt.Errorf("ax_server stdin pipe: %w", pipeErr)
	}
	stdout, pipeErr := c.cmd.StdoutPipe()
	if pipeErr != nil {
		return fmt.Errorf("ax_server stdout pipe: %w", pipeErr)
	}
	c.cmd.Stderr = os.Stderr

	if err := c.cmd.Start(); err != nil {
		return fmt.Errorf("ax_server start: %w", err)
	}
	c.started = true

	// Reader goroutine dispatches responses by ID.
	go func() {
		c.readLoop(stdout)
		// Wait for process exit and mark as dead so next Ensure() restarts it.
		c.cmd.Wait()
		c.mu.Lock()
		c.started = false
		c.mu.Unlock()
	}()

	return nil
}

// axMaxResponseLine bounds a single NDJSON response line from ax_server.
// capture_window returns a base64-encoded PNG of a whole window inline on one
// line; a retina-resolution screenshot's base64 runs several MB and overran the
// old 1 MiB cap, which surfaced as a bogus "unexpected EOF" — the scanner
// stopped with bufio.ErrTooLong and readLoop misreported it as a disconnect.
// 64 MiB fits any single-window capture with headroom; bufio.Scanner only grows
// the buffer toward this on demand.
const axMaxResponseLine = 64 * 1024 * 1024

// readLoop reads NDJSON responses and dispatches them to pending callers.
func (c *AXClient) readLoop(reader io.Reader) {
	scanner := bufio.NewScanner(reader)
	scanner.Buffer(make([]byte, 0, 1024*1024), axMaxResponseLine)
	for scanner.Scan() {
		var resp AXResponse
		if err := json.Unmarshal(scanner.Bytes(), &resp); err != nil {
			continue
		}
		c.pendingMu.Lock()
		ch, ok := c.pending[resp.ID]
		if ok {
			delete(c.pending, resp.ID)
		}
		c.pendingMu.Unlock()
		if ok {
			ch <- resp
		}
	}
	// Loop exited. A clean EOF means ax_server disconnected; a scanner error
	// (e.g. a response line exceeding axMaxResponseLine) means the stream is
	// still alive but unreadable. Report whichever it was instead of always
	// claiming a disconnect — the old hardcoded "unexpected EOF" masked the
	// oversized-capture case and cost real debugging time.
	disconnectMsg := "ax_server: unexpected EOF"
	if err := scanner.Err(); err != nil {
		disconnectMsg = fmt.Sprintf("ax_server: read error: %v", err)
	}
	c.pendingMu.Lock()
	for id, ch := range c.pending {
		ch <- AXResponse{ID: id, Error: &AXError{Code: -1, Message: disconnectMsg}}
		delete(c.pending, id)
	}
	c.pendingMu.Unlock()

	// Mark as not started so next Ensure() reconnects
	c.mu.Lock()
	c.started = false
	c.mu.Unlock()
}

// Call sends a request and waits for the response.
func (c *AXClient) Call(ctx context.Context, method string, params any) (json.RawMessage, error) {
	if runtime.GOOS != "darwin" {
		return nil, fmt.Errorf("ax_server is macOS-only")
	}

	if err := c.Ensure(ctx); err != nil {
		return nil, err
	}

	id := c.nextID.Add(1)
	req := AXRequest{ID: id, Method: method, Params: params}

	// Register pending channel BEFORE writing
	ch := make(chan AXResponse, 1)
	c.pendingMu.Lock()
	c.pending[id] = ch
	c.pendingMu.Unlock()

	data, _ := json.Marshal(req)
	data = append(data, '\n')

	c.writeMu.Lock()
	n, writeErr := c.writer.Write(data)
	if writeErr == nil && n < len(data) {
		writeErr = io.ErrShortWrite
	}
	c.writeMu.Unlock()

	if writeErr != nil {
		c.pendingMu.Lock()
		delete(c.pending, id)
		c.pendingMu.Unlock()
		return nil, fmt.Errorf("ax_server write: %w", writeErr)
	}

	select {
	case resp := <-ch:
		if resp.Error != nil {
			return nil, fmt.Errorf("ax_server: %s", resp.Error.Message)
		}
		return resp.Result, nil
	case <-ctx.Done():
		c.pendingMu.Lock()
		delete(c.pending, id)
		c.pendingMu.Unlock()
		return nil, ctx.Err()
	}
}

// Close terminates the ax_server process and cleans up resources.
func (c *AXClient) Close() {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.conn != nil {
		// Bundled mode: c.writer == c.conn, so only close once.
		c.conn.Close()
		c.conn = nil
		c.writer = nil
		os.Remove(AXSocketPath())
	} else if c.writer != nil {
		// Fallback mode: close stdin pipe.
		c.writer.Close()
	}
	// Bundled mode: kill the LaunchServices-launched ax_server process.
	// Closing the socket causes ax_server to exit on its own (it exits
	// after the sole client disconnects), but SIGTERM is a safety net.
	if c.bundlePID > 0 {
		if proc, err := os.FindProcess(c.bundlePID); err == nil {
			proc.Signal(syscall.SIGTERM)
		}
		c.bundlePID = 0
	}
	// Fallback mode: kill the subprocess
	if c.cmd != nil && c.cmd.Process != nil {
		c.cmd.Process.Kill()
		c.cmd.Wait()
	}
	c.started = false
}

// AXSocketPath returns the Unix socket path for bundled mode.
func AXSocketPath() string {
	tmpDir := os.TempDir()
	return filepath.Join(tmpDir, fmt.Sprintf("run.shannon.shanclaw.ax-server.%d.sock", os.Getpid()))
}

// AXServerPaths returns the binary path and (optionally) the .app bundle path.
// If bundlePath is non-empty, use LaunchServices + socket mode.
// If bundlePath is empty, use exec.Command + stdin/stdout with binPath.
func AXServerPaths() (binPath, bundlePath string, err error) {
	exe, exeErr := os.Executable()
	if exeErr == nil {
		dir := filepath.Dir(exe)

		// Bundled: nested app inside engine helper's Helpers/
		bp := filepath.Join(dir, "..", "Helpers", "Kocoro AX.app")
		bin := filepath.Join(bp, "Contents", "MacOS", "ax_server")
		if _, err := os.Stat(bin); err == nil {
			return bin, bp, nil
		}

		// Flat: same directory as shan binary
		p := filepath.Join(dir, "ax_server")
		if _, err := os.Stat(p); err == nil {
			return p, "", nil
		}

		// npm: bin/ax_server
		p = filepath.Join(dir, "bin", "ax_server")
		if _, err := os.Stat(p); err == nil {
			return p, "", nil
		}
	}

	// Development: relative to working directory
	p := filepath.Join("internal", "tools", "axserver", ".build", "debug", "ax_server")
	if _, err := os.Stat(p); err == nil {
		return p, "", nil
	}

	return "", "", fmt.Errorf("ax_server binary not found")
}
