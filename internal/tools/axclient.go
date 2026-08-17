package tools

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
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
	ID     int64           `json:"id"`
	Result json.RawMessage `json:"result,omitempty"`
	Error  *AXError        `json:"error,omitempty"`
}

// AXError is an error returned by ax_server.
type AXError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

const axRPCDisplayTopologyReconfiguringCodeV1 = -32001

var ErrDisplayTopologyReconfiguringV1 = errors.New(
	"display topology is reconfiguring",
)

type AXRPCError struct {
	Code    int
	Message string
}

func (err *AXRPCError) Error() string {
	return fmt.Sprintf("ax_server: %s", err.Message)
}

type displayTopologyRPCCaller interface {
	Call(context.Context, string, any) (json.RawMessage, error)
}

// ReadDisplayTopologyV1 performs the typed, read-only helper RPC and applies
// the strict topology decoder before returning authority to a caller.
func ReadDisplayTopologyV1(ctx context.Context, caller displayTopologyRPCCaller) (DisplayTopologyV1, error) {
	result, err := caller.Call(ctx, "display_topology", map[string]any{})
	if err != nil {
		var rpcErr *AXRPCError
		if errors.As(err, &rpcErr) &&
			rpcErr.Code == axRPCDisplayTopologyReconfiguringCodeV1 {
			return DisplayTopologyV1{}, fmt.Errorf(
				"read display topology v1: %w: %v",
				ErrDisplayTopologyReconfiguringV1,
				err,
			)
		}
		return DisplayTopologyV1{}, fmt.Errorf("read display topology v1: %w", err)
	}
	topology, err := DecodeDisplayTopologyV1(result)
	if err != nil {
		return DisplayTopologyV1{}, fmt.Errorf("read display topology v1 response: %w", err)
	}
	return topology, nil
}

func (c *AXClient) DisplayTopologyV1(ctx context.Context) (DisplayTopologyV1, error) {
	return ReadDisplayTopologyV1(ctx, c)
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

const (
	launchServicesRegisterPath = "/System/Library/Frameworks/CoreServices.framework/Frameworks/LaunchServices.framework/Support/lsregister"
	// AXServerBundlePathEnv lets a signed desktop host provide a stable,
	// standalone copy of Kocoro AX.app. macOS TCC cannot resolve the Debug
	// helper when it is nested inside an app running from DerivedData or /tmp,
	// even after an explicit LaunchServices registration.
	AXServerBundlePathEnv = "KOCORO_AX_SERVER_BUNDLE_PATH"
)

type combinedOutputRunner func(string, ...string) ([]byte, error)

func registerBundledApplication(bundlePath string, run combinedOutputRunner) error {
	out, err := run(launchServicesRegisterPath, "-f", "-R", "-trusted", bundlePath)
	if err != nil {
		return fmt.Errorf("register ax_server bundle with LaunchServices: %w: %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}

func runCombinedOutput(name string, args ...string) ([]byte, error) {
	return exec.Command(name, args...).CombinedOutput()
}

// AXClient manages a persistent ax_server process and multiplexes
// requests by ID. Multiple goroutines can call Call() concurrently.
//
// Two transport modes:
//   - Bundled: ax_server is inside a .app bundle, launched via LaunchServices
//     (`open -a`), communicates over a Unix domain socket. Required for TCC
//     permission attribution on macOS.
//   - Fallback: ax_server is a bare binary, launched via exec.Command,
//     communicates over stdin/stdout pipes. Used for dev, npm, and CLI.
type AXClient struct {
	mu      sync.Mutex // guards process lifecycle (start/restart)
	writeMu sync.Mutex // guards writes to ax_server

	// Transport-agnostic I/O
	writer io.WriteCloser
	nextID atomic.Int64

	// Process management
	cmd       *exec.Cmd // non-nil in fallback mode
	conn      net.Conn  // non-nil in bundled mode
	bundlePID int       // ax_server PID in bundled mode (for cleanup)
	started   bool

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
	// Nested helper apps are not registered when LaunchServices registers only
	// the outer Desktop app. TCC cannot resolve a new Debug/release bundle ID to
	// its code requirement until the exact nested bundle has its own record.
	if err := registerBundledApplication(bundlePath, runCombinedOutput); err != nil {
		return err
	}

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
// Coordinate window/display capture returns a base64-encoded PNG inline on one
// line; a Retina-resolution screenshot's base64 runs several MB and overran the
// old 1 MiB cap, which surfaced as a bogus "unexpected EOF" — the scanner
// stopped with bufio.ErrTooLong and readLoop misreported it as a disconnect.
// 64 MiB fits any single-window capture with headroom; bufio.Scanner only grows
// the buffer toward this on demand.
const axMaxResponseLine = 64 * 1024 * 1024

// AXMutationCommitUnknownError means a side-effecting request crossed the
// transport write boundary but the helper did not return a valid synchronous
// acknowledgement. Automatic retry could duplicate a GUI action.
type AXMutationCommitUnknownError struct {
	Method string
	cause  error
}

func (err *AXMutationCommitUnknownError) Error() string {
	return fmt.Sprintf("ax_server %s commit unknown (not retry-safe): %v", err.Method, err.cause)
}

func (err *AXMutationCommitUnknownError) Unwrap() error       { return err.cause }
func (err *AXMutationCommitUnknownError) RetrySafe() bool     { return false }
func (err *AXMutationCommitUnknownError) CommitUnknown() bool { return true }

func newAXMutationCommitUnknown(method string, cause error) error {
	if cause == nil {
		cause = fmt.Errorf("missing valid helper acknowledgement")
	}
	return &AXMutationCommitUnknownError{Method: method, cause: cause}
}

// isAXMutationMethod is deliberately based on an explicit read allow-list.
// Unknown helper RPCs fail closed into post-write acknowledgement semantics so
// a newly added side effect cannot silently regain early context cancellation.
// Dedicated versioned mutation clients (coordinate mouse/drag, semantic
// selection, and press) own stricter typed contracts and do not pass through
// Call.
func isAXMutationMethod(method string) bool {
	switch method {
	case "ping", "display_topology", "capture_coordinate_window", "capture_coordinate_display", "read_tree",
		"get_value", "find", "resolve_pid", "frontmost", "current_context", "list_windows",
		"wait_for", "annotate", "capture_window", "check_permissions":
		return false
	default:
		return true
	}
}

// axMutationAckTimeout bounds how long a cancelled caller keeps the global GUI
// action barrier while the synchronous helper finishes. The bounds cover the
// helper's actual workloads: launch_app polls for at most 10s, focus for 2s,
// semantic_press post-observes for 500ms, and the remaining AX/CGEvent actions
// are single operations. There is intentionally no override path: lowering a
// bound could release the single-operator lease while the helper is still
// mutating the Mac.
func axMutationAckTimeout(method string) time.Duration {
	switch method {
	case "launch_app":
		return 12 * time.Second
	case "focus":
		return 4 * time.Second
	case "semantic_press":
		return 2 * time.Second
	default:
		return 3 * time.Second
	}
}

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
	if isAXMutationMethod(method) {
		return c.callMutationWithAckTimeout(ctx, method, params, axMutationAckTimeout(method))
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
			return nil, &AXRPCError{
				Code:    resp.Error.Code,
				Message: resp.Error.Message,
			}
		}
		return resp.Result, nil
	case <-ctx.Done():
		c.pendingMu.Lock()
		delete(c.pending, id)
		c.pendingMu.Unlock()
		return nil, ctx.Err()
	}
}

// callMutationWithAckTimeout sends a synchronous side-effecting helper RPC.
// Cancellation is honored until the final pre-write check. Once any request
// bytes may have reached the helper, the caller remains blocked until a helper
// acknowledgement or the method's conservative hard bound. This keeps
// guicontrol FinishAction from releasing its quiescence barrier while Swift may
// still be applying the GUI side effect.
func (c *AXClient) callMutationWithAckTimeout(
	ctx context.Context,
	method string,
	params any,
	ackTimeout time.Duration,
) (json.RawMessage, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if ackTimeout <= 0 {
		return nil, fmt.Errorf("ax_server %s mutation acknowledgement timeout must be positive", method)
	}
	if err := c.Ensure(ctx); err != nil {
		return nil, err
	}

	id := c.nextID.Add(1)
	data, err := json.Marshal(AXRequest{ID: id, Method: method, Params: params})
	if err != nil {
		return nil, fmt.Errorf("encode ax_server %s mutation: %w", method, err)
	}
	data = append(data, '\n')

	responses := make(chan AXResponse, 1)
	c.pendingMu.Lock()
	c.pending[id] = responses
	c.pendingMu.Unlock()
	removePending := func() {
		c.pendingMu.Lock()
		delete(c.pending, id)
		c.pendingMu.Unlock()
	}

	c.writeMu.Lock()
	if err := ctx.Err(); err != nil {
		c.writeMu.Unlock()
		removePending()
		return nil, err
	}
	written, writeErr := c.writer.Write(data)
	if writeErr == nil && written < len(data) {
		writeErr = io.ErrShortWrite
	}
	c.writeMu.Unlock()
	if writeErr != nil {
		removePending()
		return nil, newAXMutationCommitUnknown(
			method, fmt.Errorf("ax_server mutation write: %w", writeErr))
	}

	timer := time.NewTimer(ackTimeout)
	defer timer.Stop()
	select {
	case response := <-responses:
		removePending()
		if response.Error != nil {
			return nil, newAXMutationCommitUnknown(method, fmt.Errorf(
				"ax_server RPC error %d: %s", response.Error.Code, response.Error.Message))
		}
		return response.Result, nil
	case <-timer.C:
		removePending()
		return nil, newAXMutationCommitUnknown(
			method, fmt.Errorf("helper acknowledgement timed out after %s", ackTimeout))
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
		// Give the helper's DispatchSourceSignal cleanup a normal Swift context
		// in which to release any journaled key/mouse state. The existing reader
		// goroutine owns Wait/reaping; an uncatchable loss is recovered by the next
		// stable helper start before that helper publishes readiness.
		_ = c.cmd.Process.Signal(syscall.SIGTERM)
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
	if configured := strings.TrimSpace(os.Getenv(AXServerBundlePathEnv)); configured != "" {
		if !filepath.IsAbs(configured) {
			return "", "", fmt.Errorf("%s must be an absolute path", AXServerBundlePathEnv)
		}
		bundlePath = filepath.Clean(configured)
		binPath = filepath.Join(bundlePath, "Contents", "MacOS", "ax_server")
		info, statErr := os.Stat(binPath)
		if statErr != nil {
			return "", "", fmt.Errorf("%s executable: %w", AXServerBundlePathEnv, statErr)
		}
		if info.IsDir() || info.Mode()&0o111 == 0 {
			return "", "", fmt.Errorf("%s executable is not executable: %s", AXServerBundlePathEnv, binPath)
		}
		return binPath, bundlePath, nil
	}

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
