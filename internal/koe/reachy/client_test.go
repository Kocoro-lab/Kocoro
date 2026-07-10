package reachy

import (
	"bufio"
	"context"
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

// mockBridge is a minimal UDS server speaking the motion protocol for tests.
type mockBridge struct {
	path       string
	ln         net.Listener
	helloProto string
	onRequest  func(req *RPCRequest) *RPCResult

	mu      sync.Mutex
	streams []Stream
	accepts int
	conns   []net.Conn
	pushCh  chan *Frame
}

// shortSockPath returns a socket path short enough for the ~104-char sun_path
// limit (t.TempDir() under /var/folders is too long on macOS).
func shortSockPath(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("/tmp", "rx")
	if err != nil {
		t.Fatalf("mkdtemp: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	return filepath.Join(dir, "s.sock")
}

func newMockBridge(t *testing.T) *mockBridge {
	t.Helper()
	path := shortSockPath(t)
	ln, err := net.Listen("unix", path)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	m := &mockBridge{path: path, ln: ln, helloProto: "1.0", pushCh: make(chan *Frame, 8)}
	go m.serve()
	return m
}

func (m *mockBridge) serve() {
	for {
		conn, err := m.ln.Accept()
		if err != nil {
			return
		}
		m.mu.Lock()
		m.accepts++
		m.mu.Unlock()
		go m.handle(conn)
	}
}

func (m *mockBridge) handle(conn net.Conn) {
	defer conn.Close()
	m.mu.Lock()
	m.conns = append(m.conns, conn)
	m.mu.Unlock()
	r := bufio.NewReader(conn)
	// First frame must be hello; respond with a hello result.
	f, err := ReadFrame(r)
	if err != nil {
		return
	}
	var req RPCRequest
	_ = json.Unmarshal(f.Payload, &req)
	hr := HelloResult{Proto: m.helloProto, BridgeVersion: "0.1.0", SdkVersion: "1.8.0", Moves: []string{"happy1"}, Capabilities: []string{"moves"}}
	raw, _ := json.Marshal(hr)
	_ = writePayload(conn, FrameRPCResult, &RPCResult{RequestID: req.RequestID, OK: true, Result: raw})

	// Push events from the test on demand.
	go func() {
		for fr := range m.pushCh {
			_ = WriteFrame(conn, fr)
		}
	}()

	for {
		f, err := ReadFrame(r)
		if err != nil {
			return
		}
		switch f.Type {
		case FrameRPCRequest:
			var rq RPCRequest
			_ = json.Unmarshal(f.Payload, &rq)
			var res *RPCResult
			if m.onRequest != nil {
				res = m.onRequest(&rq)
			} else {
				res = &RPCResult{RequestID: rq.RequestID, OK: true, Result: json.RawMessage(`{}`)}
			}
			res.RequestID = rq.RequestID
			_ = writePayload(conn, FrameRPCResult, res)
		case FrameStream:
			var s Stream
			_ = json.Unmarshal(f.Payload, &s)
			m.mu.Lock()
			m.streams = append(m.streams, s)
			m.mu.Unlock()
		}
	}
}

func (m *mockBridge) pushEvent(event string, data any) {
	d, _ := json.Marshal(data)
	raw, _ := json.Marshal(&Event{Event: event, Data: d, Ts: "2026-07-09T12:00:00Z"})
	m.pushCh <- &Frame{Type: FrameEvent, Payload: raw}
}

func (m *mockBridge) streamCount() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.streams)
}

// close shuts the listener AND drops any accepted connection, simulating a bridge
// crash so the client's read loop sees the drop.
func (m *mockBridge) close() {
	_ = m.ln.Close()
	m.mu.Lock()
	conns := m.conns
	m.conns = nil
	m.mu.Unlock()
	for _, c := range conns {
		_ = c.Close()
	}
}

func writePayload(conn net.Conn, ftype string, payload any) error {
	raw, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	return WriteFrame(conn, &Frame{Type: ftype, Payload: raw})
}

func waitFor(t *testing.T, d time.Duration, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("condition not met before deadline")
}

func startClient(t *testing.T, m *mockBridge) *Client {
	t.Helper()
	c := NewClient(m.path)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go c.Run(ctx)
	return c
}

func TestClientConnectsAndHandshakes(t *testing.T) {
	m := newMockBridge(t)
	defer m.close()
	c := startClient(t, m)
	waitFor(t, 2*time.Second, c.IsConnected)
	if h := c.Hello(); h == nil || h.SdkVersion != "1.8.0" {
		t.Errorf("hello = %+v, want sdk 1.8.0", h)
	}
}

func TestClientCallReturnsResult(t *testing.T) {
	m := newMockBridge(t)
	defer m.close()
	m.onRequest = func(req *RPCRequest) *RPCResult {
		if req.Method == MethodPlayMove {
			return &RPCResult{OK: true, Result: json.RawMessage(`{"move_id":"m-1","queued":1}`)}
		}
		return &RPCResult{OK: true, Result: json.RawMessage(`{}`)}
	}
	c := startClient(t, m)
	waitFor(t, 2*time.Second, c.IsConnected)
	res, err := c.Call(context.Background(), MethodPlayMove, map[string]any{"name": "happy1"})
	if err != nil {
		t.Fatalf("Call: %v", err)
	}
	if string(res) != `{"move_id":"m-1","queued":1}` {
		t.Errorf("result = %s", res)
	}
}

func TestClientCallErrorReturnsRPCError(t *testing.T) {
	m := newMockBridge(t)
	defer m.close()
	m.onRequest = func(req *RPCRequest) *RPCResult {
		return &RPCResult{OK: false, Error: &RPCError{Code: ErrCodeUnknownMove, Message: "no such move", Retriable: false}}
	}
	c := startClient(t, m)
	waitFor(t, 2*time.Second, c.IsConnected)
	_, err := c.Call(context.Background(), MethodPlayMove, map[string]any{"name": "nope"})
	var rpcErr *RPCError
	if err == nil {
		t.Fatal("want error")
	}
	if e, ok := err.(*RPCError); ok {
		rpcErr = e
	}
	if rpcErr == nil || rpcErr.Code != ErrCodeUnknownMove {
		t.Errorf("want RPCError unknown_move, got %v", err)
	}
}

func TestClientSendStreamReachesBridge(t *testing.T) {
	m := newMockBridge(t)
	defer m.close()
	c := startClient(t, m)
	waitFor(t, 2*time.Second, c.IsConnected)
	if err := c.SendStream(StreamSpeechEnvelope, map[string]any{"level": 0.42}); err != nil {
		t.Fatalf("SendStream: %v", err)
	}
	waitFor(t, 2*time.Second, func() bool { return m.streamCount() >= 1 })
}

func TestClientDeliversEvents(t *testing.T) {
	m := newMockBridge(t)
	defer m.close()
	c := startClient(t, m)
	waitFor(t, 2*time.Second, c.IsConnected)
	m.pushEvent(EventMoveFinished, map[string]any{"id": "m-1"})
	select {
	case ev := <-c.Events():
		if ev.Event != EventMoveFinished {
			t.Errorf("event = %s", ev.Event)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("no event delivered")
	}
}

func TestClientVersionMismatchDegrades(t *testing.T) {
	m := newMockBridge(t)
	m.helloProto = "2.0" // major mismatch
	defer m.close()
	c := startClient(t, m)
	// Never becomes connected; a Call fails fast rather than hanging.
	time.Sleep(300 * time.Millisecond)
	if c.IsConnected() {
		t.Error("client must not connect on a proto major mismatch")
	}
	_, err := c.Call(context.Background(), MethodPing, nil)
	if err == nil {
		t.Error("Call should fail while degraded")
	}
}

func TestClientDegradesWithNoBridge(t *testing.T) {
	// A path with no server: Call fails immediately (no block), Run keeps retrying.
	c := NewClient(shortSockPath(t))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go c.Run(ctx)
	time.Sleep(100 * time.Millisecond)
	done := make(chan error, 1)
	go func() { _, err := c.Call(context.Background(), MethodPing, nil); done <- err }()
	select {
	case err := <-done:
		if err == nil {
			t.Error("Call should fail while disconnected")
		}
	case <-time.After(1 * time.Second):
		t.Fatal("Call blocked while degraded (must fail fast)")
	}
}

func TestClientReconnectsAfterDrop(t *testing.T) {
	m := newMockBridge(t)
	c := startClient(t, m)
	waitFor(t, 2*time.Second, c.IsConnected)
	// Drop the server; the client should notice and go disconnected.
	m.close()
	waitFor(t, 3*time.Second, func() bool { return !c.IsConnected() })
	// Bring a new server up on the same path; the client reconnects via backoff.
	_ = os.Remove(m.path)
	ln, err := net.Listen("unix", m.path)
	if err != nil {
		t.Fatalf("relisten: %v", err)
	}
	m2 := &mockBridge{path: m.path, ln: ln, helloProto: "1.0", pushCh: make(chan *Frame, 8)}
	defer m2.close()
	go m2.serve()
	waitFor(t, 15*time.Second, c.IsConnected)
}
