package desktop_rpc

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"testing"
	"time"
)

// TestRequest_NotConnected verifies that calling Request without a SendFn
// installed returns ErrNotConnected immediately (not after timeout).
func TestRequest_NotConnected(t *testing.T) {
	t.Parallel()
	b := NewDesktopRPCBroker()
	if b.IsConnected() {
		t.Fatal("fresh broker should not be connected")
	}
	res, err := b.Request(context.Background(), &RPCRequest{Method: MethodSystemPing})
	if !errors.Is(err, ErrNotConnected) {
		t.Errorf("got err %v, want ErrNotConnected", err)
	}
	if res != nil {
		t.Errorf("got res %v, want nil", res)
	}
}

// TestRequest_SuccessRoundTrip wires a fake SendFn that calls Resolve from
// a goroutine, verifies the result arrives at Request.
func TestRequest_SuccessRoundTrip(t *testing.T) {
	t.Parallel()
	b := NewDesktopRPCBroker()
	expectedResult := json.RawMessage(`{"pong":"hi","server_time":"2026-05-26T10:00:00+08:00"}`)
	b.SetSendFn(func(req *RPCRequest) error {
		// Mimic Desktop responding asynchronously.
		go func() {
			time.Sleep(5 * time.Millisecond)
			b.Resolve(req.RequestID, &RPCResult{
				RequestID: req.RequestID,
				OK:        true,
				Result:    expectedResult,
			})
		}()
		return nil
	})
	res, err := b.Request(context.Background(), &RPCRequest{
		Method:    MethodSystemPing,
		Params:    json.RawMessage(`{"echo":"hi"}`),
		TimeoutMs: 1000,
	})
	if err != nil {
		t.Fatalf("Request: %v", err)
	}
	if !res.OK {
		t.Fatalf("expected OK=true, got error: %+v", res.Error)
	}
	if string(res.Result) != string(expectedResult) {
		t.Errorf("Result: got %s, want %s", res.Result, expectedResult)
	}
	if b.PendingCount() != 0 {
		t.Errorf("pending entries after success: %d", b.PendingCount())
	}
}

// TestRequest_SendFnError verifies that a transport error from SendFn
// surfaces immediately as a wrapped Go error, without populating any
// pending entry (must clean up).
func TestRequest_SendFnError(t *testing.T) {
	t.Parallel()
	b := NewDesktopRPCBroker()
	transportErr := errors.New("broken pipe")
	b.SetSendFn(func(req *RPCRequest) error {
		return transportErr
	})
	res, err := b.Request(context.Background(), &RPCRequest{Method: MethodSystemPing})
	if err == nil {
		t.Fatal("expected transport error, got nil")
	}
	if !errors.Is(err, transportErr) {
		t.Errorf("error chain should wrap transportErr; got: %v", err)
	}
	if res != nil {
		t.Errorf("res should be nil on transport error, got %v", res)
	}
	if b.PendingCount() != 0 {
		t.Errorf("send error left pending entries: %d", b.PendingCount())
	}
}

// TestRequest_Timeout verifies that a request whose response never arrives
// completes with a structured timeout RPCResult after TimeoutMs.
func TestRequest_Timeout(t *testing.T) {
	t.Parallel()
	b := NewDesktopRPCBroker()
	b.SetSendFn(func(req *RPCRequest) error {
		// Send succeeds, but no Resolve ever fires.
		return nil
	})
	start := time.Now()
	res, err := b.Request(context.Background(), &RPCRequest{
		Method:    MethodSystemPing,
		TimeoutMs: 50, // 50ms — fast test
	})
	elapsed := time.Since(start)
	if err != nil {
		t.Fatalf("Request returned Go error on timeout: %v", err)
	}
	if res == nil || res.OK {
		t.Fatalf("expected timeout RPCResult with OK=false, got %+v", res)
	}
	if res.Error == nil || res.Error.Code != ErrCodeTimeout {
		t.Errorf("expected ErrCodeTimeout, got %+v", res.Error)
	}
	if elapsed < 50*time.Millisecond || elapsed > 500*time.Millisecond {
		t.Errorf("timeout fired at %v, expected ~50ms", elapsed)
	}
	if b.PendingCount() != 0 {
		t.Errorf("timeout left pending entries: %d", b.PendingCount())
	}
}

// TestRequest_ContextCancel verifies ctx.Done unblocks Request and returns
// ctx.Err() (not a structured timeout result — context cancel is a different
// signal).
func TestRequest_ContextCancel(t *testing.T) {
	t.Parallel()
	b := NewDesktopRPCBroker()
	b.SetSendFn(func(req *RPCRequest) error { return nil })

	ctx, cancel := context.WithCancel(context.Background())
	resultCh := make(chan error, 1)
	go func() {
		_, err := b.Request(ctx, &RPCRequest{Method: MethodSystemPing, TimeoutMs: 10000})
		resultCh <- err
	}()
	// Give Request time to enter the select.
	time.Sleep(20 * time.Millisecond)
	cancel()

	select {
	case err := <-resultCh:
		if !errors.Is(err, context.Canceled) {
			t.Errorf("got %v, want context.Canceled", err)
		}
	case <-time.After(500 * time.Millisecond):
		t.Fatal("Request did not return after ctx cancel within 500ms")
	}
}

// TestCancelAll verifies that disconnecting (CancelAll) unblocks all pending
// Requests with structured ErrCodeDesktopDisconnected results.
func TestCancelAll(t *testing.T) {
	t.Parallel()
	b := NewDesktopRPCBroker()
	b.SetSendFn(func(req *RPCRequest) error { return nil })

	const n = 5
	var wg sync.WaitGroup
	results := make([]*RPCResult, n)
	errs := make([]error, n)
	wg.Add(n)
	for i := 0; i < n; i++ {
		i := i
		go func() {
			defer wg.Done()
			res, err := b.Request(context.Background(), &RPCRequest{
				Method:    MethodSystemPing,
				TimeoutMs: 10000, // generous — CancelAll should fire first
			})
			results[i] = res
			errs[i] = err
		}()
	}

	// Wait for all to be pending.
	deadline := time.Now().Add(1 * time.Second)
	for time.Now().Before(deadline) {
		if b.PendingCount() == n {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if b.PendingCount() != n {
		t.Fatalf("only %d/%d requests pending before CancelAll", b.PendingCount(), n)
	}

	b.CancelAll()
	wg.Wait()

	for i := 0; i < n; i++ {
		if errs[i] != nil {
			t.Errorf("[%d] Request returned err: %v", i, errs[i])
			continue
		}
		if results[i] == nil || results[i].OK {
			t.Errorf("[%d] expected disconnect error, got %+v", i, results[i])
			continue
		}
		if results[i].Error == nil || results[i].Error.Code != ErrCodeDesktopDisconnected {
			t.Errorf("[%d] expected ErrCodeDesktopDisconnected, got %+v", i, results[i].Error)
		}
	}
	if b.PendingCount() != 0 {
		t.Errorf("pending left after CancelAll: %d", b.PendingCount())
	}
	if b.IsConnected() {
		t.Error("broker still reports connected after CancelAll")
	}
}

// TestResolve_UnknownRequestID returns false when no pending entry matches.
func TestResolve_UnknownRequestID(t *testing.T) {
	t.Parallel()
	b := NewDesktopRPCBroker()
	ok := b.Resolve("drpc_does-not-exist", &RPCResult{RequestID: "drpc_does-not-exist", OK: true})
	if ok {
		t.Error("Resolve returned true for unknown request_id")
	}
}

// TestRequest_ConcurrentResolveAndCancelAll fires both paths under contention
// and asserts that exactly one terminal signal reaches each in-flight request
// (no double-deliver, no leak).
func TestRequest_ConcurrentResolveAndCancelAll(t *testing.T) {
	t.Parallel()
	const iterations = 50
	for iter := 0; iter < iterations; iter++ {
		b := NewDesktopRPCBroker()
		// Use a buffered channel to publish request_id with happens-before
		// vs the goroutine that will read it. A naked `var sentReqID string`
		// trips the race detector — SendFn runs in the Request goroutine
		// but the Resolve/CancelAll goroutines read it without sync.
		sentReqIDCh := make(chan string, 1)
		b.SetSendFn(func(req *RPCRequest) error {
			sentReqIDCh <- req.RequestID
			return nil
		})

		resultCh := make(chan *RPCResult, 1)
		go func() {
			res, _ := b.Request(context.Background(), &RPCRequest{
				Method:    MethodSystemPing,
				TimeoutMs: 5000,
			})
			resultCh <- res
		}()
		// Wait for SendFn to publish the generated request_id.
		var sentReqID string
		select {
		case sentReqID = <-sentReqIDCh:
		case <-time.After(500 * time.Millisecond):
			t.Fatalf("iter %d: SendFn never ran within 500ms", iter)
		}

		// Fire Resolve and CancelAll concurrently.
		var wg sync.WaitGroup
		wg.Add(2)
		go func() {
			defer wg.Done()
			b.Resolve(sentReqID, &RPCResult{RequestID: sentReqID, OK: true})
		}()
		go func() {
			defer wg.Done()
			b.CancelAll()
		}()
		wg.Wait()

		// Whatever wins, exactly one result must reach the caller.
		select {
		case res := <-resultCh:
			if res == nil {
				t.Fatalf("iter %d: nil result delivered", iter)
			}
		case <-time.After(500 * time.Millisecond):
			t.Fatalf("iter %d: caller blocked > 500ms after both terminal paths", iter)
		}
		if b.PendingCount() != 0 {
			t.Errorf("iter %d: pending leaked: %d", iter, b.PendingCount())
		}
	}
}

// TestRequestIDFormat verifies generated IDs match the `drpc_<16hex>` shape
// the spec describes. Important because Desktop side may pattern-match for
// logs.
func TestRequestIDFormat(t *testing.T) {
	t.Parallel()
	for i := 0; i < 100; i++ {
		id := generateRequestID()
		if len(id) != len("drpc_")+16 {
			t.Errorf("ID %q: wrong length %d", id, len(id))
		}
		if id[:5] != "drpc_" {
			t.Errorf("ID %q: wrong prefix", id)
		}
	}
}
