package desktop_rpc

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// MethodHandler is the daemon-side responder for an RPC method invoked by
// the Desktop client. Returns either a serializable result or an *RPCError.
// Implementations should be fast (system.* methods); long-running work
// belongs in the agent loop, not here.
type MethodHandler func(ctx context.Context, params json.RawMessage) (any, *RPCError)

// EventSink is called by the listener when a `desktop_event` frame arrives.
// The daemon's EventBus is the canonical consumer; the sink is a thin
// abstraction so the listener package doesn't import the daemon package
// (avoiding an import cycle).
type EventSink func(evt *DesktopEvent)

// ListenerConfig captures the values cmd/daemon.go assembles before
// constructing a Listener. SockPath / PidfilePath come from CLI flags;
// Platform identifies this daemon for `system.capabilities` responses.
type ListenerConfig struct {
	SockPath    string
	PidfilePath string
	Platform    Platform // populated by caller from daemon.Version + runtime info
	Broker      *DesktopRPCBroker
	EventSink   EventSink // optional; nil-safe (events are dropped if unset)

	// ReadyCh, if non-nil, is closed by Run once the sock is bound, chmod'd,
	// and the pidfile is written — i.e. the listener is fully up and the next
	// accept will be served. Lets callers wait deterministically for readiness
	// instead of racing a fixed sleep. Run never sends on it (only closes), so
	// a select on {ReadyCh, errCh} resolves to exactly one outcome: setup
	// failed (errCh) or setup succeeded (ReadyCh). nil-safe.
	ReadyCh chan<- struct{}
}

// Listener owns the daemon-side Unix domain socket: it listens, accepts
// exactly one Desktop client (v1 single-instance assumption per spec §4.1),
// wires the broker's SendFn to the conn writer, and runs a read loop that
// dispatches incoming frames. On context cancellation it cleans up the sock
// + pidfile (idempotent).
type Listener struct {
	cfg     ListenerConfig
	methods map[string]MethodHandler

	// running flips to 1 the moment Run starts. RegisterMethod panics if
	// the flag is set — the map has no mutex, so post-start registration
	// would race with the read-only dispatch path. v1 only ever registers
	// in NewListener; v1.x dynamic registration would need a sync.RWMutex
	// around methods.
	running atomic.Bool

	// activeConn is set when a Desktop client is accepted and cleared on
	// disconnect. Read with atomic.LoadPointer so concurrent inspectors
	// don't need a mutex; only Run mutates it.
	activeConn atomic.Pointer[net.Conn]

	// writeMu serializes WriteFrame calls on the active conn so concurrent
	// goroutines (broker SendFn for outgoing RPC + read loop's
	// system.capabilities responder) don't interleave bytes at the wire level.
	writeMu sync.Mutex
}

// NewListener constructs a Listener with the standard system.ping and
// system.capabilities responders pre-registered. Additional handlers can
// be added via RegisterMethod before Run is called.
func NewListener(cfg ListenerConfig) *Listener {
	l := &Listener{
		cfg:     cfg,
		methods: make(map[string]MethodHandler),
	}
	l.methods[MethodSystemPing] = l.handleSystemPing
	l.methods[MethodSystemCapabilities] = l.handleSystemCapabilities
	return l
}

// RegisterMethod installs a method handler. Must be called BEFORE Run —
// after Run starts, the methods map is read concurrently from the dispatch
// loop without a mutex (v1 doesn't pay for one because nothing dynamic
// registers). Calling RegisterMethod after Run panics to surface the
// programming error.
func (l *Listener) RegisterMethod(method string, h MethodHandler) {
	if l.running.Load() {
		panic(fmt.Sprintf("desktop_rpc: RegisterMethod(%q) called after Listener.Run started; v1 requires registration in NewListener", method))
	}
	l.methods[method] = h
}

// Run opens the sock, writes the pidfile, and runs the accept loop until
// ctx is done. Returns on:
//   - ctx cancellation (clean shutdown, returns nil after cleanup)
//   - permanent listen / setup error (returns the error after cleanup attempt)
//
// Cleanup is idempotent and best-effort: sock file and pidfile are removed
// on exit regardless of how Run terminates. Per spec §7.1, listen failures
// are fatal — callers (cmd/daemon.go) should propagate the error to
// non-zero process exit so DaemonManager surfaces it to the user.
func (l *Listener) Run(ctx context.Context) (retErr error) {
	// Defer cleanup so it fires on every exit path (success, error, panic).
	defer l.cleanupArtifacts()

	// Freeze the methods table — any RegisterMethod call after this point panics.
	l.running.Store(true)
	defer l.running.Store(false)

	if l.cfg.SockPath == "" {
		return errors.New("desktop_rpc: empty sock path")
	}
	if l.cfg.PidfilePath == "" {
		return errors.New("desktop_rpc: empty pidfile path")
	}
	if l.cfg.Broker == nil {
		return errors.New("desktop_rpc: nil broker")
	}

	// Ensure parent dir exists with 0700 (Desktop should have done this
	// before spawn per spec §4.1, but be defensive).
	if err := os.MkdirAll(filepath.Dir(l.cfg.SockPath), 0o700); err != nil {
		return fmt.Errorf("create parent dir: %w", err)
	}
	// Remove any stale sock file (left over from prior crashed daemon).
	if err := os.Remove(l.cfg.SockPath); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("remove stale sock %s: %w", l.cfg.SockPath, err)
	}

	ln, err := net.Listen("unix", l.cfg.SockPath)
	if err != nil {
		return fmt.Errorf("listen unix %s: %w", l.cfg.SockPath, err)
	}
	defer ln.Close()

	// Tighten file permissions to 0600 (macOS default obeys umask, may
	// otherwise be 0755).
	if err := os.Chmod(l.cfg.SockPath, 0o600); err != nil {
		return fmt.Errorf("chmod sock %s: %w", l.cfg.SockPath, err)
	}

	// Atomically write pidfile: write to .tmp then rename. Per-spec §4.1.1
	// step 5d, content is a single line PID with no other fields.
	if err := writePidfileAtomic(l.cfg.PidfilePath, os.Getpid()); err != nil {
		return fmt.Errorf("write pidfile: %w", err)
	}

	log.Printf("desktop_rpc: listening on %s (pidfile %s)", l.cfg.SockPath, l.cfg.PidfilePath)

	// Signal readiness now that every setup step (listen + chmod + pidfile)
	// has succeeded. Closed exactly once; all setup-failure paths returned
	// above without reaching here.
	if l.cfg.ReadyCh != nil {
		close(l.cfg.ReadyCh)
	}

	// Close the listener when ctx is done so Accept unblocks.
	go func() {
		<-ctx.Done()
		ln.Close()
	}()

	// maxAcceptRetries caps consecutive transient accept errors before we
	// bail. Real-world failures (EMFILE / ENFILE / other FD-exhaustion class)
	// are usually persistent; tight-looping on accept just spams logs and
	// pegs CPU. Bail and let Desktop's DaemonManager respawn — clean recovery
	// via process restart is the right shape on macOS.
	const maxAcceptRetries = 10
	consecutiveErrors := 0
	for {
		conn, err := ln.Accept()
		if err != nil {
			// ctx-done path: listener closed deliberately, return cleanly.
			if ctx.Err() != nil {
				return nil
			}
			consecutiveErrors++
			log.Printf("desktop_rpc: accept error (%d/%d): %v", consecutiveErrors, maxAcceptRetries, err)
			if consecutiveErrors >= maxAcceptRetries {
				return fmt.Errorf("desktop_rpc: %d consecutive accept errors, last: %w", consecutiveErrors, err)
			}
			// Brief backoff dampens tight-loop conditions while allowing
			// recovery from genuine transients.
			select {
			case <-time.After(100 * time.Millisecond):
			case <-ctx.Done():
				return nil
			}
			continue
		}
		// Reset on successful accept — only consecutive failures matter.
		consecutiveErrors = 0
		// v1 single-instance: claim the slot synchronously (CAS) here, before
		// spawning handleConn. The store MUST NOT live inside the goroutine —
		// two near-simultaneous accepts would both observe nil in the window
		// between accept and the goroutine's store, both pass the guard, and
		// both run handleConn (clobbering Broker.SetSendFn against each other).
		// CompareAndSwap closes that window: the first conn wins the slot, the
		// second fails the swap and is closed (it sees EOF). handleConn only
		// CLEARS the slot on exit; it never stores.
		if !l.activeConn.CompareAndSwap(nil, &conn) {
			log.Printf("desktop_rpc: rejecting second concurrent client from %v", conn.RemoteAddr())
			conn.Close()
			continue
		}
		go l.handleConn(ctx, conn)
	}
}

// handleConn runs the read loop for one client connection. Wires the broker's
// SendFn to write frames on this conn, reads incoming frames, dispatches by
// type. On exit, calls broker.CancelAll() to unblock any in-flight requests.
func (l *Listener) handleConn(ctx context.Context, conn net.Conn) {
	defer conn.Close()
	// The slot was already claimed via CompareAndSwap in Run; we only clear
	// it on exit so the next Desktop client can connect. (No Store here — see
	// the single-instance comment in Run.)
	defer l.activeConn.Store(nil)
	defer l.cfg.Broker.CancelAll()

	log.Printf("desktop_rpc: client connected from %v", conn.RemoteAddr())

	// Wire SendFn so the broker can push frames to this conn.
	l.cfg.Broker.SetSendFn(func(req *RPCRequest) error {
		frame, err := EncodeRequestFrame(req)
		if err != nil {
			return err
		}
		l.writeMu.Lock()
		defer l.writeMu.Unlock()
		return WriteFrame(conn, frame)
	})

	reader := bufio.NewReaderSize(conn, 64*1024)
	for {
		// Honor ctx cancellation by closing conn so ReadFrame errors out.
		// (The Run-side goroutine closes the listener; we additionally
		// close conn here so handleConn unblocks promptly.)
		select {
		case <-ctx.Done():
			return
		default:
		}
		frame, err := ReadFrame(reader)
		if err != nil {
			if errors.Is(err, io.EOF) {
				log.Printf("desktop_rpc: client disconnected (clean EOF)")
			} else {
				log.Printf("desktop_rpc: read error: %v", err)
			}
			return
		}
		l.dispatchFrame(ctx, conn, frame)
	}
}

// dispatchFrame routes an incoming frame to the appropriate handler. Errors
// in handlers are turned into structured RPC error responses (or logged for
// fire-and-forget events).
func (l *Listener) dispatchFrame(ctx context.Context, conn net.Conn, f *Frame) {
	switch f.Type {
	case FrameDesktopRPCRequest:
		l.handleIncomingRequest(ctx, conn, f.Payload)
	case FrameDesktopRPCResult:
		l.handleIncomingResult(f.Payload)
	case FrameDesktopEvent:
		l.handleIncomingEvent(f.Payload)
	default:
		log.Printf("desktop_rpc: unknown frame type %q (dropping)", f.Type)
	}
}

// handleIncomingRequest decodes a Desktop-originated RPC request, dispatches
// to the registered method handler, and writes the result frame back.
// Errors decoding the payload are responded to with an `invalid_argument`
// result whose request_id is best-effort (whatever we could decode).
func (l *Listener) handleIncomingRequest(ctx context.Context, conn net.Conn, payload json.RawMessage) {
	var req RPCRequest
	if err := json.Unmarshal(payload, &req); err != nil {
		log.Printf("desktop_rpc: malformed RPCRequest payload: %v", err)
		// Can't reliably respond — request_id may be lost. Drop silently;
		// Desktop will see request timeout on its side.
		return
	}
	handler, ok := l.methods[req.Method]
	var res RPCResult
	if !ok {
		// Spec §5.5.4 has no `unknown_method` code — we surface this as
		// invalid_argument with details.method so Desktop can still
		// distinguish (it's a structured detail, not a new code).
		details, _ := json.Marshal(map[string]string{"method": req.Method})
		res = RPCResult{
			RequestID: req.RequestID,
			OK:        false,
			Error: &RPCError{
				Code:      ErrCodeInvalidArgument,
				Message:   fmt.Sprintf("daemon does not implement method %q", req.Method),
				Retriable: false,
				Details:   details,
			},
		}
	} else {
		result, rpcErr := handler(ctx, req.Params)
		if rpcErr != nil {
			res = RPCResult{RequestID: req.RequestID, OK: false, Error: rpcErr}
		} else {
			marshaled, err := json.Marshal(result)
			if err != nil {
				res = RPCResult{
					RequestID: req.RequestID,
					OK:        false,
					Error: &RPCError{
						Code:    ErrCodeInternal,
						Message: fmt.Sprintf("marshal result: %v", err),
					},
				}
			} else {
				res = RPCResult{RequestID: req.RequestID, OK: true, Result: marshaled}
			}
		}
	}
	frame, err := EncodeResultFrame(&res)
	if err != nil {
		log.Printf("desktop_rpc: encode result for %s: %v", req.RequestID, err)
		return
	}
	l.writeMu.Lock()
	defer l.writeMu.Unlock()
	if err := WriteFrame(conn, frame); err != nil {
		log.Printf("desktop_rpc: write result for %s: %v", req.RequestID, err)
	}
}

// handleIncomingResult delivers a Desktop-originated RPC result to the broker.
// If no pending entry matches the request_id, the result is dropped (likely a
// duplicate or late arrival after CancelAll fired).
func (l *Listener) handleIncomingResult(payload json.RawMessage) {
	var res RPCResult
	if err := json.Unmarshal(payload, &res); err != nil {
		log.Printf("desktop_rpc: malformed RPCResult payload: %v", err)
		return
	}
	if !l.cfg.Broker.Resolve(res.RequestID, &res) {
		log.Printf("desktop_rpc: result for unknown request_id %s (dropped)", res.RequestID)
	}
}

// handleIncomingEvent forwards a desktop_event payload to the configured
// sink. Nil-safe — if no sink was provided, the event is logged and dropped.
func (l *Listener) handleIncomingEvent(payload json.RawMessage) {
	var evt DesktopEvent
	if err := json.Unmarshal(payload, &evt); err != nil {
		log.Printf("desktop_rpc: malformed DesktopEvent payload: %v", err)
		return
	}
	if l.cfg.EventSink == nil {
		log.Printf("desktop_rpc: received desktop_event %q (no sink configured, dropping)", evt.Event)
		return
	}
	l.cfg.EventSink(&evt)
}

// handleSystemPing responds to Desktop-initiated system.ping requests.
func (l *Listener) handleSystemPing(_ context.Context, params json.RawMessage) (any, *RPCError) {
	var p SystemPingParams
	if len(params) > 0 {
		if err := json.Unmarshal(params, &p); err != nil {
			return nil, &RPCError{
				Code:      ErrCodeInvalidArgument,
				Message:   "system.ping: malformed params",
				Retriable: false,
			}
		}
	}
	return SystemPingResult{
		Pong:       p.Echo,
		ServerTime: time.Now().UTC().Format(time.RFC3339Nano),
	}, nil
}

// handleSystemCapabilities responds to Desktop-initiated system.capabilities
// requests during reconciliation. Returns the daemon's protocol version +
// ProtocolMethods (byte-identical with Desktop's hardcoded list) + platform.
func (l *Listener) handleSystemCapabilities(_ context.Context, _ json.RawMessage) (any, *RPCError) {
	return SystemCapabilitiesResult{
		Version:  ProtocolVersion,
		Methods:  ProtocolMethods,
		Platform: l.cfg.Platform,
	}, nil
}

// cleanupArtifacts removes sock + pidfile. Idempotent.
func (l *Listener) cleanupArtifacts() {
	if l.cfg.SockPath != "" {
		_ = os.Remove(l.cfg.SockPath)
	}
	if l.cfg.PidfilePath != "" {
		_ = os.Remove(l.cfg.PidfilePath)
	}
}

// writePidfileAtomic writes pid to path via tmp + rename so a partial write
// can never be observed by Desktop's reconciliation read. The file is
// created with 0600 because it sits in a 0700 dir but defense-in-depth.
func writePidfileAtomic(path string, pid int) error {
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, []byte(strconv.Itoa(pid)+"\n"), 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// DefaultPlatform constructs a Platform value suitable for cmd/daemon.go to
// pass into ListenerConfig. The caller provides daemonVersion (typically
// daemon.Version exposed in internal/daemon/client.go); os_version is best-
// effort detected from sw_vers on macOS, empty string elsewhere.
func DefaultPlatform(daemonVersion string) Platform {
	return Platform{
		OS:         mapOS(runtime.GOOS),
		OSVersion:  detectOSVersion(),
		AppVersion: daemonVersion,
	}
}

func mapOS(goos string) string {
	if goos == "darwin" {
		return "macOS"
	}
	return goos
}

func detectOSVersion() string {
	if runtime.GOOS != "darwin" {
		return ""
	}
	out, err := exec.Command("sw_vers", "-productVersion").Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}
