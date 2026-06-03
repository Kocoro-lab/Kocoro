package desktop_rpc

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"sync"
	"time"
)

// DefaultTimeoutMs is the timeout applied when a caller passes 0 (or negative)
// for an RPC request's TimeoutMs. Most calendar.* methods are fast so 30s is
// generous; `calendar_request_permission` overrides to 5 minutes because the
// user may take that long to dismiss the TCC dialog (spec §5.2).
const DefaultTimeoutMs = 30000

// SendFn is the transport-level send callback the broker uses to push a
// request frame to the connected Desktop client. Implementations must be
// safe to call from multiple goroutines and must return non-nil on transport
// failure (the broker uses the error to decide whether the request ever
// reached the wire).
//
// The listener installs this when a Desktop client connects; the broker has
// no SendFn before that point and Request will return ErrNotConnected.
type SendFn func(req *RPCRequest) error

// ErrNotConnected is returned by Request when no Desktop client is currently
// attached to the broker. The tools should map this to ErrCodeDesktopDisconnected
// when surfacing to the agent loop.
var ErrNotConnected = errors.New("desktop_rpc: no Desktop client connected")

// pending tracks one in-flight RPC. The result channel is buffered cap-1 so
// Resolve never blocks the sender; emitted records whether the request frame
// actually reached the wire (used by CancelAll to skip cleanup events for
// frames that never went out).
type pending struct {
	ch      chan *RPCResult
	emitted bool
}

// DesktopRPCBroker mediates between agent-loop callers (Request) and the
// transport-level send/recv loop owned by the listener. Lifecycle:
//
//  1. Daemon startup creates one broker via NewDesktopRPCBroker
//  2. Listener accept loop calls SetSendFn when a Desktop conn is established;
//     CancelAll on disconnect
//  3. Tool calls Request → broker generates request_id, calls SendFn, blocks
//     on result chan
//  4. Listener read loop receives desktop_rpc_result frames, calls Resolve
//  5. Result chan unblocks Request, which returns to the tool
//
// The race-safety patterns mirror internal/daemon/approval.go's ApprovalBroker
// (pending-map drain on Resolve, claimForCleanup to prevent double-terminal
// emission). Comments below cite the equivalent ApprovalBroker behavior for
// reference.
type DesktopRPCBroker struct {
	mu      sync.Mutex
	pending map[string]*pending
	sendFn  SendFn
}

// NewDesktopRPCBroker constructs an empty broker. SendFn must be installed
// later via SetSendFn before Request can succeed; without one, Request
// returns ErrNotConnected immediately.
func NewDesktopRPCBroker() *DesktopRPCBroker {
	return &DesktopRPCBroker{
		pending: make(map[string]*pending),
	}
}

// SetSendFn installs (or replaces) the transport send callback. Called by
// the listener on each new accepted connection. Passing nil disconnects the
// broker (subsequent Request calls return ErrNotConnected); existing pending
// requests are NOT cancelled — call CancelAll explicitly for that.
func (b *DesktopRPCBroker) SetSendFn(fn SendFn) {
	b.mu.Lock()
	b.sendFn = fn
	b.mu.Unlock()
}

// IsConnected reports whether a SendFn is currently installed. Used by
// `RegisterCalendarTools` and the `desktop_disconnected` error path to
// short-circuit before allocating pending entries.
func (b *DesktopRPCBroker) IsConnected() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.sendFn != nil
}

// Request sends an RPC to the connected Desktop and blocks until a result
// arrives, the timeout expires, ctx is cancelled, or the connection drops.
//
// The caller fills in Method / Params / SessionID / Agent / Source / TS;
// RequestID is overwritten with a freshly-generated value and the same ID
// is what the result will carry. TimeoutMs ≤ 0 is replaced with DefaultTimeoutMs.
//
// Returns the result (which may itself carry an RPCError via .OK=false), or
// an error for transport/timeout/cancel paths. The four possible terminal
// states:
//
//	(result, nil)            — RPC completed (success or RPC-level error)
//	(nil, ErrNotConnected)    — no Desktop attached when called
//	(nil, ctx.Err())          — context cancelled before result
//	(nil, ErrCodeTimeout/...) — timeout after TimeoutMs
//
// Race-safety: mirrors ApprovalBroker.Request (internal/daemon/approval.go).
// The pa-still-pending check between SendFn success and bus emission protects
// against concurrent CancelAll erasing the entry.
func (b *DesktopRPCBroker) Request(ctx context.Context, req *RPCRequest) (*RPCResult, error) {
	if req == nil {
		return nil, errors.New("desktop_rpc: nil RPCRequest")
	}
	if req.Method == "" {
		return nil, errors.New("desktop_rpc: empty method")
	}
	if req.TimeoutMs <= 0 {
		req.TimeoutMs = DefaultTimeoutMs
	}
	req.RequestID = generateRequestID()

	b.mu.Lock()
	sendFn := b.sendFn
	if sendFn == nil {
		b.mu.Unlock()
		return nil, ErrNotConnected
	}
	pa := &pending{ch: make(chan *RPCResult, 1)}
	b.pending[req.RequestID] = pa
	b.mu.Unlock()

	defer func() {
		b.mu.Lock()
		delete(b.pending, req.RequestID)
		b.mu.Unlock()
	}()

	if err := sendFn(req); err != nil {
		// Frame never went out — clean up and return as transport error.
		return nil, fmt.Errorf("desktop_rpc: send failed: %w", err)
	}
	// Mark as emitted so CancelAll knows to fire a synthetic cleanup if
	// disconnect races with Resolve.
	b.mu.Lock()
	if _, stillPending := b.pending[req.RequestID]; !stillPending {
		// CancelAll erased us mid-send. Drain any buffered result.
		b.mu.Unlock()
		select {
		case res := <-pa.ch:
			return res, nil
		default:
			return nil, ErrNotConnected
		}
	}
	pa.emitted = true
	b.mu.Unlock()

	timeoutDur := time.Duration(req.TimeoutMs) * time.Millisecond
	select {
	case res := <-pa.ch:
		return res, nil
	case <-time.After(timeoutDur):
		// Best-effort: return a structured RPCResult with timeout error rather
		// than a Go-level error, so the tool can render the code uniformly.
		return &RPCResult{
			RequestID: req.RequestID,
			OK:        false,
			Error: &RPCError{
				Code:      ErrCodeTimeout,
				Message:   fmt.Sprintf("RPC timed out after %dms (daemon side)", req.TimeoutMs),
				Retriable: false,
			},
		}, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// Resolve delivers a result for the matching request. Called from the
// listener's read loop when a `desktop_rpc_result` frame arrives. Returns
// true if a pending entry was found and the result delivered; false if no
// entry existed (already resolved, cancelled, or unknown request_id —
// likely indicates Desktop is buggy or out of sync).
func (b *DesktopRPCBroker) Resolve(requestID string, res *RPCResult) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	pa, ok := b.pending[requestID]
	if !ok {
		return false
	}
	// Non-blocking send: pa.ch is buffered cap-1, and we delete from pending
	// immediately, so only one Resolve / CancelAll path delivers per ID.
	select {
	case pa.ch <- res:
	default:
	}
	delete(b.pending, requestID)
	return true
}

// CancelAll terminates every pending request with ErrCodeDesktopDisconnected.
// Called by the listener when the Desktop sock disconnects (read EOF / EPIPE)
// so the agent loop sees a structured error instead of hanging until each
// request's individual timeout.
//
// Also clears the SendFn so subsequent Request calls return ErrNotConnected
// without trying to write to a dead conn.
func (b *DesktopRPCBroker) CancelAll() {
	b.mu.Lock()
	defer b.mu.Unlock()
	disconnectError := &RPCResult{
		OK: false,
		Error: &RPCError{
			Code:      ErrCodeDesktopDisconnected,
			Message:   "Desktop disconnected",
			Retriable: false,
		},
	}
	for id, pa := range b.pending {
		disconnectError.RequestID = id // shallow-copy field for each
		// Send a per-ID copy: pa.ch is cap-1 so non-blocking
		res := *disconnectError
		select {
		case pa.ch <- &res:
		default:
		}
		delete(b.pending, id)
		if pa.emitted {
			log.Printf("desktop_rpc: cancelled in-flight request %s on disconnect", id)
		}
	}
	b.sendFn = nil
}

// PendingCount returns the current number of in-flight requests, for
// diagnostics and tests.
func (b *DesktopRPCBroker) PendingCount() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return len(b.pending)
}

func generateRequestID() string {
	var b [8]byte
	_, _ = rand.Read(b[:])
	return "drpc_" + hex.EncodeToString(b[:])
}

// MarshalParams is a small helper for callers that want to build an
// RPCRequest from a typed params struct without boilerplate.
func MarshalParams(v any) (json.RawMessage, error) {
	return json.Marshal(v)
}
