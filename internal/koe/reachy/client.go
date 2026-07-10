package reachy

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// ErrNotConnected is returned by Call/SendStream while the bridge is unreachable.
// Koe treats it as "no motion" and keeps the conversation going (spec section 10).
var ErrNotConnected = errors.New("reachy: bridge not connected")

// ErrConnLost is returned to an in-flight Call when the connection drops. In-flight
// requests are NOT replayed — a stale motion played late is worse than not at all.
var ErrConnLost = errors.New("reachy: connection lost")

var errVersionMismatch = errors.New("reachy: bridge proto major mismatch")

const (
	defaultDialTimeout = 2 * time.Second
	defaultCallTimeout = 3 * time.Second
	wakeSleepTimeout   = 10 * time.Second
	minBackoff         = 500 * time.Millisecond
	maxBackoff         = 10 * time.Second
	eventBuffer        = 32
)

// Client is Koe's resilient client to the motion bridge. Run() owns the connect →
// hello → read → reconnect lifecycle; Call/SendStream operate on the current
// connection and fail fast (never block) when there is none.
type Client struct {
	socketPath    string
	clientVersion string
	dialTimeout   time.Duration
	callTimeout   time.Duration

	mu        sync.Mutex
	conn      net.Conn
	connDone  chan struct{} // closed when the current connection dies
	pending   map[string]chan *RPCResult
	hello     *HelloResult
	connected atomic.Bool

	writeMu sync.Mutex // serializes frame writes on the shared conn

	events   chan Event
	seq      atomic.Uint64
	stop     chan struct{}
	stopOnce sync.Once
}

// Option configures a Client.
type Option func(*Client)

// WithClientVersion sets the client_version reported in the hello.
func WithClientVersion(v string) Option { return func(c *Client) { c.clientVersion = v } }

// NewClient builds a client for the bridge socket at socketPath. Call Run to
// start the connection lifecycle.
func NewClient(socketPath string, opts ...Option) *Client {
	c := &Client{
		socketPath:    socketPath,
		clientVersion: "0.0.0",
		dialTimeout:   defaultDialTimeout,
		callTimeout:   defaultCallTimeout,
		pending:       make(map[string]chan *RPCResult),
		events:        make(chan Event, eventBuffer),
		stop:          make(chan struct{}),
	}
	for _, o := range opts {
		o(c)
	}
	return c
}

// Run drives the connect/handshake/read/reconnect loop until ctx is cancelled or
// Close is called. Backoff is 0.5s→1→2→4→8, capped at 10s, and resets on a
// successful handshake. A proto major mismatch degrades (retries on the same
// backoff — a redeployed bridge may speak a compatible version).
func (c *Client) Run(ctx context.Context) {
	backoff := minBackoff
	for {
		if c.stopped(ctx) {
			c.teardown()
			return
		}
		conn, reader, hello, err := c.dialAndHandshake(ctx)
		if err != nil {
			if errors.Is(err, errVersionMismatch) {
				log.Printf("koe[reachy]: %v — degrading, will retry", err)
			}
			if c.sleepBackoff(ctx, backoff) {
				c.teardown()
				return
			}
			backoff = nextBackoff(backoff)
			continue
		}
		backoff = minBackoff
		c.setConn(conn, hello)
		c.readLoop(reader) // blocks until the connection dies
		c.clearConn()
	}
}

// Close stops Run and drops the current connection. Idempotent.
func (c *Client) Close() {
	c.stopOnce.Do(func() { close(c.stop) })
	c.mu.Lock()
	if c.conn != nil {
		_ = c.conn.Close()
	}
	c.mu.Unlock()
}

// IsConnected reports whether a handshaked connection is live.
func (c *Client) IsConnected() bool { return c.connected.Load() }

// Hello returns the bridge's hello result (nil while disconnected).
func (c *Client) Hello() *HelloResult {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.hello
}

// Events delivers bridge events (move_started/finished/failed/status). Buffered;
// events are dropped if the consumer falls behind rather than blocking the reader.
func (c *Client) Events() <-chan Event { return c.events }

// Call sends an RPC and waits for its result. Returns ErrNotConnected immediately
// while degraded, *RPCError on an ok=false result, ErrConnLost if the connection
// drops mid-flight, or a timeout error.
func (c *Client) Call(ctx context.Context, method string, params any) (json.RawMessage, error) {
	c.mu.Lock()
	conn := c.conn
	done := c.connDone
	c.mu.Unlock()
	if conn == nil {
		return nil, ErrNotConnected
	}

	var praw json.RawMessage
	if params != nil {
		b, err := json.Marshal(params)
		if err != nil {
			return nil, err
		}
		praw = b
	}
	rid := c.nextID()
	ch := make(chan *RPCResult, 1)
	c.mu.Lock()
	c.pending[rid] = ch
	c.mu.Unlock()
	defer func() {
		c.mu.Lock()
		delete(c.pending, rid)
		c.mu.Unlock()
	}()

	timeout := c.callTimeout
	if method == MethodWake || method == MethodSleep {
		timeout = wakeSleepTimeout
	}
	req := &RPCRequest{RequestID: rid, Method: method, Params: praw, TimeoutMs: int(timeout / time.Millisecond), Ts: nowRFC3339()}
	if err := c.write(conn, FrameRPCRequest, req); err != nil {
		return nil, err
	}

	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case res := <-ch:
		if !res.OK {
			if res.Error != nil {
				return nil, res.Error
			}
			return nil, errors.New("reachy: rpc failed without an error body")
		}
		return res.Result, nil
	case <-done:
		return nil, ErrConnLost
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-timer.C:
		return nil, &RPCError{Code: ErrCodeTimeout, Message: "request timed out", Retriable: true}
	}
}

// SendStream fires a stream frame (speech_envelope / face_offsets) with no reply.
// Degrades to ErrNotConnected; callers drop it silently.
func (c *Client) SendStream(stream string, data any) error {
	c.mu.Lock()
	conn := c.conn
	c.mu.Unlock()
	if conn == nil {
		return ErrNotConnected
	}
	return c.write(conn, FrameStream, &Stream{Stream: stream, Data: data})
}

// SendSpeechEnvelope / SendFaceOffsets are ergonomic stream wrappers.
func (c *Client) SendSpeechEnvelope(level float64) error {
	return c.SendStream(StreamSpeechEnvelope, map[string]float64{"level": level})
}

func (c *Client) SendFaceOffsets(dx, dy, dz, droll, dpitch, dyaw float64) error {
	return c.SendStream(StreamFaceOffsets, map[string]float64{
		"dx": dx, "dy": dy, "dz": dz, "droll": droll, "dpitch": dpitch, "dyaw": dyaw,
	})
}

// PlayMove / StopMoves / SetListening / Wake / Sleep wrap Call for the common RPCs.
func (c *Client) PlayMove(ctx context.Context, name string, preempt bool) error {
	_, err := c.Call(ctx, MethodPlayMove, map[string]any{"name": name, "preempt": preempt})
	return err
}

func (c *Client) StopMoves(ctx context.Context) error {
	_, err := c.Call(ctx, MethodStopMoves, map[string]any{})
	return err
}

func (c *Client) SetListening(ctx context.Context, on bool) error {
	_, err := c.Call(ctx, MethodSetListen, map[string]any{"on": on})
	return err
}

func (c *Client) Wake(ctx context.Context) error {
	_, err := c.Call(ctx, MethodWake, map[string]any{})
	return err
}

func (c *Client) Sleep(ctx context.Context) error {
	_, err := c.Call(ctx, MethodSleep, map[string]any{})
	return err
}

// ---- lifecycle internals -------------------------------------------------

func (c *Client) dialAndHandshake(ctx context.Context) (net.Conn, *bufio.Reader, *HelloResult, error) {
	d := net.Dialer{Timeout: c.dialTimeout}
	conn, err := d.DialContext(ctx, "unix", c.socketPath)
	if err != nil {
		return nil, nil, nil, err
	}
	rid := c.nextID()
	helloParams, _ := json.Marshal(Hello{Proto: ProtoVersion, Client: "koe", ClientVersion: c.clientVersion})
	req := &RPCRequest{RequestID: rid, Method: MethodHello, Params: helloParams, TimeoutMs: 3000, Ts: nowRFC3339()}
	if err := c.write(conn, FrameRPCRequest, req); err != nil {
		_ = conn.Close()
		return nil, nil, nil, err
	}
	reader := bufio.NewReader(conn)
	_ = conn.SetReadDeadline(time.Now().Add(c.dialTimeout))
	f, err := ReadFrame(reader)
	_ = conn.SetReadDeadline(time.Time{})
	if err != nil {
		_ = conn.Close()
		return nil, nil, nil, err
	}
	var res RPCResult
	if err := json.Unmarshal(f.Payload, &res); err != nil {
		_ = conn.Close()
		return nil, nil, nil, err
	}
	if !res.OK {
		_ = conn.Close()
		if res.Error != nil && res.Error.Code == ErrCodeVersionMismatch {
			return nil, nil, nil, errVersionMismatch
		}
		return nil, nil, nil, fmt.Errorf("reachy: hello rejected: %v", res.Error)
	}
	var hr HelloResult
	if err := json.Unmarshal(res.Result, &hr); err != nil {
		_ = conn.Close()
		return nil, nil, nil, err
	}
	if !majorCompatible(ProtoVersion, hr.Proto) {
		_ = conn.Close()
		return nil, nil, nil, errVersionMismatch
	}
	return conn, reader, &hr, nil
}

func (c *Client) readLoop(reader *bufio.Reader) {
	for {
		f, err := ReadFrame(reader)
		if err != nil {
			return // connection died; Run reconnects
		}
		switch f.Type {
		case FrameRPCResult:
			var res RPCResult
			if json.Unmarshal(f.Payload, &res) == nil {
				c.deliverResult(&res)
			}
		case FrameEvent:
			var ev Event
			if json.Unmarshal(f.Payload, &ev) == nil {
				select {
				case c.events <- ev:
				default: // drop rather than block the reader
				}
			}
		}
	}
}

func (c *Client) deliverResult(res *RPCResult) {
	c.mu.Lock()
	ch, ok := c.pending[res.RequestID]
	c.mu.Unlock()
	if ok {
		ch <- res // buffered(1), never blocks
	}
}

func (c *Client) setConn(conn net.Conn, hello *HelloResult) {
	c.mu.Lock()
	c.conn = conn
	c.connDone = make(chan struct{})
	c.hello = hello
	c.mu.Unlock()
	c.connected.Store(true)
	log.Printf("koe[reachy]: bridge connected (proto %s, sdk %s, %d moves)", hello.Proto, hello.SdkVersion, len(hello.Moves))
}

func (c *Client) clearConn() {
	c.mu.Lock()
	c.connected.Store(false)
	if c.connDone != nil {
		close(c.connDone) // unblocks in-flight Calls with ErrConnLost
		c.connDone = nil
	}
	if c.conn != nil {
		_ = c.conn.Close()
		c.conn = nil
	}
	c.hello = nil
	c.mu.Unlock()
}

func (c *Client) write(conn net.Conn, ftype string, payload any) error {
	raw, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	return WriteFrame(conn, &Frame{Type: ftype, Payload: raw})
}

func (c *Client) nextID() string { return fmt.Sprintf("r-%d", c.seq.Add(1)) }

func (c *Client) stopped(ctx context.Context) bool {
	select {
	case <-ctx.Done():
		return true
	case <-c.stop:
		return true
	default:
		return false
	}
}

func (c *Client) sleepBackoff(ctx context.Context, d time.Duration) (stop bool) {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return true
	case <-c.stop:
		return true
	case <-t.C:
		return false
	}
}

func (c *Client) teardown() {
	c.clearConn()
}

func nextBackoff(cur time.Duration) time.Duration {
	next := cur * 2
	if next > maxBackoff {
		return maxBackoff
	}
	return next
}

func majorCompatible(a, b string) bool {
	return strings.SplitN(a, ".", 2)[0] == strings.SplitN(b, ".", 2)[0]
}

func nowRFC3339() string { return time.Now().UTC().Format("2006-01-02T15:04:05Z") }
