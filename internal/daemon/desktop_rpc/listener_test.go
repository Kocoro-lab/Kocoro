package desktop_rpc

import (
	"bufio"
	"context"
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// shortTempDir creates a temp dir under /tmp with a short name. macOS limits
// sockaddr_un.sun_path to 104 bytes; the default t.TempDir() roots under
// /var/folders/... which routinely overflows. Cleanup runs via t.Cleanup.
func shortTempDir(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("/tmp", "drpc")
	if err != nil {
		t.Fatalf("MkdirTemp /tmp: %v", err)
	}
	t.Cleanup(func() {
		_ = os.RemoveAll(dir)
	})
	return dir
}

// newListenerTestRig spawns a listener with a temp sock path + pidfile path,
// returns the listener's cancel func + connect helper for fake Desktop clients.
func newListenerTestRig(t *testing.T) (sockPath, pidfilePath string, broker *DesktopRPCBroker, events <-chan *DesktopEvent, cancel context.CancelFunc) {
	t.Helper()
	dir := shortTempDir(t)
	sockPath = filepath.Join(dir, "daemon.sock")
	pidfilePath = filepath.Join(dir, "daemon.pid")
	broker = NewDesktopRPCBroker()
	eventCh := make(chan *DesktopEvent, 16)
	cfg := ListenerConfig{
		SockPath:    sockPath,
		PidfilePath: pidfilePath,
		Platform:    Platform{OS: "macOS", OSVersion: "14.4.1", AppVersion: "0.0.0-test"},
		Broker:      broker,
		EventSink: func(evt *DesktopEvent) {
			eventCh <- evt
		},
	}
	l := NewListener(cfg)
	ctx, cancelFn := context.WithCancel(context.Background())
	runDone := make(chan struct{})
	runErr := make(chan error, 1)
	go func() {
		defer close(runDone)
		runErr <- l.Run(ctx)
	}()
	// Wait for listener to be ready — listen + pidfile-write happen in
	// sequence in Run, so both files must exist before tests can read
	// either. Polling just sockPath has a short race window between sock
	// bind (step 5b) and pidfile rename (step 5d).
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		_, sockErr := os.Stat(sockPath)
		_, pidErr := os.Stat(pidfilePath)
		if sockErr == nil && pidErr == nil {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if _, err := os.Stat(sockPath); err != nil {
		cancelFn()
		<-runDone
		select {
		case rerr := <-runErr:
			t.Fatalf("sock never appeared at %s: %v; Run returned: %v", sockPath, err, rerr)
		default:
			t.Fatalf("sock never appeared at %s: %v", sockPath, err)
		}
	}
	if _, err := os.Stat(pidfilePath); err != nil {
		cancelFn()
		<-runDone
		t.Fatalf("pidfile never appeared at %s: %v", pidfilePath, err)
	}
	t.Cleanup(func() {
		cancelFn()
		<-runDone
	})
	return sockPath, pidfilePath, broker, eventCh, cancelFn
}

func TestListener_SockFilePermissions(t *testing.T) {
	t.Parallel()
	sockPath, pidfilePath, _, _, _ := newListenerTestRig(t)

	// sock file should be 0600
	info, err := os.Stat(sockPath)
	if err != nil {
		t.Fatalf("stat sock: %v", err)
	}
	if mode := info.Mode().Perm(); mode != 0o600 {
		t.Errorf("sock perm: got %o, want 0600", mode)
	}

	// pidfile should exist and contain our PID
	pidBytes, err := os.ReadFile(pidfilePath)
	if err != nil {
		t.Fatalf("read pidfile: %v", err)
	}
	pidStr := strings.TrimSpace(string(pidBytes))
	pid, err := strconv.Atoi(pidStr)
	if err != nil {
		t.Errorf("pidfile content %q not parseable: %v", pidStr, err)
	}
	if pid != os.Getpid() {
		t.Errorf("pidfile content %d != our pid %d", pid, os.Getpid())
	}
}

func TestListener_SystemPingRoundTrip(t *testing.T) {
	t.Parallel()
	sockPath, _, _, _, _ := newListenerTestRig(t)

	conn, err := net.Dial("unix", sockPath)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()

	// Send system.ping REQUEST from "Desktop" side.
	req := &RPCRequest{
		RequestID: "drpc_test-ping-id",
		Method:    MethodSystemPing,
		Params:    json.RawMessage(`{"echo":"hello"}`),
		TimeoutMs: 5000,
		TS:        time.Now().UTC().Format(time.RFC3339),
	}
	reqFrame, err := EncodeRequestFrame(req)
	if err != nil {
		t.Fatalf("encode request: %v", err)
	}
	if err := WriteFrame(conn, reqFrame); err != nil {
		t.Fatalf("write request: %v", err)
	}

	// Read result frame.
	conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	resFrame, err := ReadFrame(bufio.NewReader(conn))
	if err != nil {
		t.Fatalf("read result: %v", err)
	}
	if resFrame.Type != FrameDesktopRPCResult {
		t.Errorf("frame type: got %q, want %q", resFrame.Type, FrameDesktopRPCResult)
	}
	var res RPCResult
	if err := json.Unmarshal(resFrame.Payload, &res); err != nil {
		t.Fatalf("decode result: %v", err)
	}
	if res.RequestID != req.RequestID {
		t.Errorf("request_id mismatch: got %q, want %q", res.RequestID, req.RequestID)
	}
	if !res.OK {
		t.Fatalf("result error: %+v", res.Error)
	}
	var pong SystemPingResult
	if err := json.Unmarshal(res.Result, &pong); err != nil {
		t.Fatalf("decode pong: %v", err)
	}
	if pong.Pong != "hello" {
		t.Errorf("pong.Pong = %q, want %q", pong.Pong, "hello")
	}
	if pong.ServerTime == "" {
		t.Error("pong.ServerTime should be set")
	}
}

func TestListener_SystemCapabilities(t *testing.T) {
	t.Parallel()
	sockPath, _, _, _, _ := newListenerTestRig(t)
	conn, err := net.Dial("unix", sockPath)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()

	req := &RPCRequest{
		RequestID: "drpc_test-caps-id",
		Method:    MethodSystemCapabilities,
		Params:    json.RawMessage(`{}`),
		TimeoutMs: 5000,
	}
	reqFrame, _ := EncodeRequestFrame(req)
	if err := WriteFrame(conn, reqFrame); err != nil {
		t.Fatalf("write: %v", err)
	}
	conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	resFrame, err := ReadFrame(bufio.NewReader(conn))
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	var res RPCResult
	_ = json.Unmarshal(resFrame.Payload, &res)
	if !res.OK {
		t.Fatalf("capabilities error: %+v", res.Error)
	}
	var caps SystemCapabilitiesResult
	if err := json.Unmarshal(res.Result, &caps); err != nil {
		t.Fatalf("decode caps: %v", err)
	}
	if caps.Version != ProtocolVersion {
		t.Errorf("version: got %q, want %q", caps.Version, ProtocolVersion)
	}
	if len(caps.Methods) != len(ProtocolMethods) {
		t.Errorf("methods len: got %d, want %d", len(caps.Methods), len(ProtocolMethods))
	}
	for i, m := range ProtocolMethods {
		if i >= len(caps.Methods) || caps.Methods[i] != m {
			t.Errorf("methods[%d]: got %q, want %q", i, caps.Methods[i], m)
		}
	}
	if caps.Platform.OS != "macOS" || caps.Platform.AppVersion != "0.0.0-test" {
		t.Errorf("platform mismatch: %+v", caps.Platform)
	}
}

func TestListener_UnknownMethod(t *testing.T) {
	t.Parallel()
	sockPath, _, _, _, _ := newListenerTestRig(t)
	conn, _ := net.Dial("unix", sockPath)
	defer conn.Close()
	req := &RPCRequest{
		RequestID: "drpc_test-bad-method",
		Method:    "nonexistent.method",
		Params:    json.RawMessage(`{}`),
	}
	reqFrame, _ := EncodeRequestFrame(req)
	WriteFrame(conn, reqFrame)
	conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	resFrame, _ := ReadFrame(bufio.NewReader(conn))
	var res RPCResult
	json.Unmarshal(resFrame.Payload, &res)
	if res.OK {
		t.Fatal("unknown method should fail")
	}
	if res.Error.Code != ErrCodeInvalidArgument {
		t.Errorf("error code: got %q, want %q", res.Error.Code, ErrCodeInvalidArgument)
	}
	// details.method should carry the offending method name (spec §5.3
	// allows details for structured supplementary info).
	if !strings.Contains(string(res.Error.Details), `"method":"nonexistent.method"`) {
		t.Errorf("details should contain unknown method name; got %s", res.Error.Details)
	}
}

func TestListener_DesktopEventForwarding(t *testing.T) {
	t.Parallel()
	sockPath, _, _, events, _ := newListenerTestRig(t)
	conn, _ := net.Dial("unix", sockPath)
	defer conn.Close()

	evt := &DesktopEvent{
		Event: EventCalendarPermissionChanged,
		Data:  json.RawMessage(`{"status":"granted"}`),
		TS:    "2026-05-26T10:00:00+08:00",
	}
	evtFrame, _ := EncodeEventFrame(evt)
	WriteFrame(conn, evtFrame)

	select {
	case got := <-events:
		if got.Event != EventCalendarPermissionChanged {
			t.Errorf("event: got %q, want %q", got.Event, EventCalendarPermissionChanged)
		}
		if string(got.Data) != `{"status":"granted"}` {
			t.Errorf("event data: got %s", got.Data)
		}
	case <-time.After(1 * time.Second):
		t.Fatal("event not delivered to sink within 1s")
	}
}

// TestListener_DaemonOutgoingRPC verifies the broker → SendFn → conn write
// → fake-Desktop read → fake-Desktop write back result → broker.Resolve
// → Request returns flow. This is the daemon → desktop direction (the
// majority business case for calendar.* methods).
func TestListener_DaemonOutgoingRPC(t *testing.T) {
	t.Parallel()
	sockPath, _, broker, _, _ := newListenerTestRig(t)
	conn, err := net.Dial("unix", sockPath)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()

	// Fake Desktop: read inbound calendar.list_events request, respond with stub result.
	desktopReader := bufio.NewReader(conn)
	var fakeDesktopWg sync.WaitGroup
	fakeDesktopWg.Add(1)
	go func() {
		defer fakeDesktopWg.Done()
		conn.SetReadDeadline(time.Now().Add(3 * time.Second))
		f, err := ReadFrame(desktopReader)
		if err != nil {
			t.Errorf("fake-Desktop read: %v", err)
			return
		}
		if f.Type != FrameDesktopRPCRequest {
			t.Errorf("fake-Desktop got frame type %q", f.Type)
			return
		}
		var req RPCRequest
		json.Unmarshal(f.Payload, &req)
		// Respond with a canned result.
		stubResult := json.RawMessage(`{"events":[],"truncated":false}`)
		res := &RPCResult{RequestID: req.RequestID, OK: true, Result: stubResult}
		resFrame, _ := EncodeResultFrame(res)
		WriteFrame(conn, resFrame)
	}()

	// Wait for handleConn to install SendFn.
	deadline := time.Now().Add(1 * time.Second)
	for time.Now().Before(deadline) && !broker.IsConnected() {
		time.Sleep(5 * time.Millisecond)
	}
	if !broker.IsConnected() {
		t.Fatal("broker never received SendFn")
	}

	// Daemon issues calendar.list_events.
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	res, err := broker.Request(ctx, &RPCRequest{
		Method:    MethodCalendarListEvents,
		Params:    json.RawMessage(`{"start":"2026-05-26T00:00:00+08:00","end":"2026-05-26T23:59:59+08:00"}`),
		TimeoutMs: 2000,
	})
	if err != nil {
		t.Fatalf("Request: %v", err)
	}
	if !res.OK {
		t.Fatalf("Request returned error: %+v", res.Error)
	}
	if string(res.Result) != `{"events":[],"truncated":false}` {
		t.Errorf("result mismatch: %s", res.Result)
	}
	fakeDesktopWg.Wait()
}

// TestListener_DisconnectCancelsPending verifies that closing the client conn
// triggers broker.CancelAll which unblocks an in-flight Request with
// ErrCodeDesktopDisconnected.
func TestListener_DisconnectCancelsPending(t *testing.T) {
	t.Parallel()
	sockPath, _, broker, _, _ := newListenerTestRig(t)
	conn, err := net.Dial("unix", sockPath)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}

	// Wait for SendFn install.
	deadline := time.Now().Add(1 * time.Second)
	for time.Now().Before(deadline) && !broker.IsConnected() {
		time.Sleep(5 * time.Millisecond)
	}
	if !broker.IsConnected() {
		t.Fatal("broker not connected")
	}

	// Issue a request that will never be answered.
	resCh := make(chan *RPCResult, 1)
	errCh := make(chan error, 1)
	go func() {
		res, err := broker.Request(context.Background(), &RPCRequest{
			Method:    MethodSystemPing,
			TimeoutMs: 30000,
		})
		resCh <- res
		errCh <- err
	}()

	// Read the outbound frame on the fake-Desktop side. This is the
	// definitive sync point: when our read completes, daemon has already
	// finished WriteFrame, sendFn has returned nil, and Request is parked
	// on pa.ch. Closing conn now reliably triggers CancelAll, not a
	// transport error mid-send.
	conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	_, err = ReadFrame(bufio.NewReader(conn))
	if err != nil {
		t.Fatalf("fake-Desktop read inbound frame: %v", err)
	}
	// Drop the client conn.
	conn.Close()

	select {
	case res := <-resCh:
		err := <-errCh
		if err != nil {
			t.Fatalf("Request returned err: %v", err)
		}
		if res == nil || res.OK {
			t.Fatalf("expected disconnect result, got %+v", res)
		}
		if res.Error == nil || res.Error.Code != ErrCodeDesktopDisconnected {
			t.Errorf("expected ErrCodeDesktopDisconnected, got %+v", res.Error)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Request did not unblock after client disconnect")
	}
}

// TestListener_SecondConnRejected verifies the single-instance assumption
// (spec §4.1.1): a second concurrent connection is closed immediately.
func TestListener_SecondConnRejected(t *testing.T) {
	t.Parallel()
	sockPath, _, broker, _, _ := newListenerTestRig(t)
	conn1, err := net.Dial("unix", sockPath)
	if err != nil {
		t.Fatalf("dial 1: %v", err)
	}
	defer conn1.Close()

	// Wait for first conn to be the active conn.
	deadline := time.Now().Add(500 * time.Millisecond)
	for time.Now().Before(deadline) && !broker.IsConnected() {
		time.Sleep(5 * time.Millisecond)
	}
	if !broker.IsConnected() {
		t.Fatal("first conn never became active")
	}

	conn2, err := net.Dial("unix", sockPath)
	if err != nil {
		t.Fatalf("dial 2: %v", err)
	}
	defer conn2.Close()

	// Reading from conn2 should immediately see EOF.
	conn2.SetReadDeadline(time.Now().Add(500 * time.Millisecond))
	buf := make([]byte, 4)
	_, err = conn2.Read(buf)
	if err == nil {
		t.Error("second conn should have been closed by listener")
	}
}

// TestListener_ConcurrentConnsSingleSurvivor fires several dials with no gap
// between them, exercising the single-instance guard's race window directly.
// Before the CompareAndSwap fix the slot was stored inside handleConn (async),
// so two near-simultaneous accepts could both observe nil and both run the
// read loop — clobbering Broker.SetSendFn. TestListener_SecondConnRejected
// misses this because it sleeps + polls IsConnected between the two dials,
// serializing past the window. Here we assert exactly one conn survives.
// Run with -race to also surface the SetSendFn data race.
func TestListener_ConcurrentConnsSingleSurvivor(t *testing.T) {
	t.Parallel()
	sockPath, _, broker, _, _ := newListenerTestRig(t)

	const n = 8
	conns := make([]net.Conn, n)
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start // release all dials at once
			c, err := net.Dial("unix", sockPath)
			if err == nil {
				conns[i] = c
			}
		}(i)
	}
	close(start)
	wg.Wait()

	// Let the accept loop process every connection and close the losers.
	deadline := time.Now().Add(time.Second)
	survivors := -1
	for time.Now().Before(deadline) {
		survivors = countOpenConns(conns)
		if survivors == 1 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}

	for _, c := range conns {
		if c != nil {
			defer c.Close()
		}
	}

	if survivors != 1 {
		t.Fatalf("expected exactly 1 surviving connection, got %d", survivors)
	}
	if !broker.IsConnected() {
		t.Error("broker should report a live SendFn for the single survivor")
	}
}

// countOpenConns probes each conn with a tiny read deadline. A rejected conn
// returns EOF / reset immediately; the live survivor blocks (the daemon sends
// nothing unsolicited) and returns a timeout error. Returns the survivor count.
func countOpenConns(conns []net.Conn) int {
	open := 0
	buf := make([]byte, 1)
	for _, c := range conns {
		if c == nil {
			continue
		}
		_ = c.SetReadDeadline(time.Now().Add(20 * time.Millisecond))
		_, err := c.Read(buf)
		if ne, ok := err.(net.Error); ok && ne.Timeout() {
			open++ // still connected, nothing to read
		}
	}
	return open
}

// TestListener_CleanupOnCancel verifies sock + pidfile are removed when
// Run's ctx is cancelled.
func TestListener_CleanupOnCancel(t *testing.T) {
	t.Parallel()
	dir := shortTempDir(t)
	sockPath := filepath.Join(dir, "daemon.sock")
	pidfilePath := filepath.Join(dir, "daemon.pid")
	cfg := ListenerConfig{
		SockPath:    sockPath,
		PidfilePath: pidfilePath,
		Platform:    Platform{OS: "macOS", AppVersion: "test"},
		Broker:      NewDesktopRPCBroker(),
	}
	l := NewListener(cfg)
	ctx, cancel := context.WithCancel(context.Background())
	runDone := make(chan error, 1)
	go func() { runDone <- l.Run(ctx) }()

	// Wait for files to appear.
	deadline := time.Now().Add(1 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(sockPath); err == nil {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if _, err := os.Stat(sockPath); err != nil {
		t.Fatalf("sock never appeared: %v", err)
	}

	cancel()

	select {
	case err := <-runDone:
		if err != nil {
			t.Errorf("Run returned err: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not return within 2s of cancel")
	}

	if _, err := os.Stat(sockPath); !os.IsNotExist(err) {
		t.Errorf("sock not cleaned up: %v", err)
	}
	if _, err := os.Stat(pidfilePath); !os.IsNotExist(err) {
		t.Errorf("pidfile not cleaned up: %v", err)
	}
}

// TestListener_RegisterMethodPanicsAfterRun confirms the v1 single-mutex-free
// design: registrations are frozen once Run starts.
func TestListener_RegisterMethodPanicsAfterRun(t *testing.T) {
	t.Parallel()
	dir := shortTempDir(t)
	cfg := ListenerConfig{
		SockPath:    dir + "/d.sock",
		PidfilePath: dir + "/d.pid",
		Platform:    Platform{OS: "macOS"},
		Broker:      NewDesktopRPCBroker(),
	}
	l := NewListener(cfg)

	// Pre-Run registration: must not panic.
	l.RegisterMethod("test.before_run", func(_ context.Context, _ json.RawMessage) (any, *RPCError) {
		return nil, nil
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = l.Run(ctx) }()

	// Wait for sock to appear (Run started).
	deadline := time.Now().Add(1 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(cfg.SockPath); err == nil {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}

	defer func() {
		if r := recover(); r == nil {
			t.Error("expected panic when RegisterMethod called after Run")
		}
	}()
	l.RegisterMethod("test.after_run", func(_ context.Context, _ json.RawMessage) (any, *RPCError) {
		return nil, nil
	})
}

// TestListener_StaleSockRemoval verifies a pre-existing sock file at startup
// is removed before Listen (spec §4.1 step 5a).
func TestListener_StaleSockRemoval(t *testing.T) {
	t.Parallel()
	dir := shortTempDir(t)
	sockPath := filepath.Join(dir, "daemon.sock")
	pidfilePath := filepath.Join(dir, "daemon.pid")
	// Pre-create a stale file at sockPath.
	if err := os.WriteFile(sockPath, []byte("stale"), 0o600); err != nil {
		t.Fatalf("create stale: %v", err)
	}
	cfg := ListenerConfig{
		SockPath:    sockPath,
		PidfilePath: pidfilePath,
		Platform:    Platform{OS: "macOS"},
		Broker:      NewDesktopRPCBroker(),
	}
	l := NewListener(cfg)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = l.Run(ctx) }()

	// Wait for sock to be valid (real sock, not the stale file).
	deadline := time.Now().Add(1 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := net.Dial("unix", sockPath); err == nil {
			return // success — listener replaced stale with a real sock
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("listener never accepted on the path that had a stale file")
}
