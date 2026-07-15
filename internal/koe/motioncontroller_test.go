package koe

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

	"github.com/Kocoro-lab/ShanClaw/internal/koe/reachy"
)

// fakeBridge is a minimal UDS motion bridge for MotionController integration tests:
// it handshakes, advertises a fixed move set, records play_move names, and can emit
// (or withhold) the section-8 status heartbeat.
type fakeBridge struct {
	path    string
	ln      net.Listener
	moves   []string
	emitHB  bool
	hbEvery time.Duration

	mu      sync.Mutex
	writeMu sync.Mutex
	plays   []string
	conns   []net.Conn
}

func newFakeBridge(t *testing.T, moves []string) *fakeBridge {
	t.Helper()
	dir, err := os.MkdirTemp("/tmp", "kmc")
	if err != nil {
		t.Fatalf("mkdtemp: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	path := filepath.Join(dir, "b.sock")
	ln, err := net.Listen("unix", path)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	fb := &fakeBridge{path: path, ln: ln, moves: moves, emitHB: true, hbEvery: 15 * time.Millisecond}
	go fb.serve()
	return fb
}

func (fb *fakeBridge) serve() {
	for {
		conn, err := fb.ln.Accept()
		if err != nil {
			return
		}
		fb.mu.Lock()
		fb.conns = append(fb.conns, conn)
		fb.mu.Unlock()
		go fb.handle(conn)
	}
}

func (fb *fakeBridge) writeFrame(conn net.Conn, ftype string, payload any) error {
	raw, _ := json.Marshal(payload)
	fb.writeMu.Lock()
	defer fb.writeMu.Unlock()
	return reachy.WriteFrame(conn, &reachy.Frame{Type: ftype, Payload: raw})
}

func (fb *fakeBridge) handle(conn net.Conn) {
	defer conn.Close()
	r := bufio.NewReader(conn)
	f, err := reachy.ReadFrame(r)
	if err != nil {
		return
	}
	var req reachy.RPCRequest
	_ = json.Unmarshal(f.Payload, &req)
	hr := reachy.HelloResult{Proto: "1.0", BridgeVersion: "0.1.0", SdkVersion: "1.8.0", Moves: fb.moves, Capabilities: []string{"moves"}}
	raw, _ := json.Marshal(hr)
	_ = fb.writeFrame(conn, reachy.FrameRPCResult, reachy.RPCResult{RequestID: req.RequestID, OK: true, Result: raw})

	if fb.emitHB {
		go func() {
			tk := time.NewTicker(fb.hbEvery)
			defer tk.Stop()
			for range tk.C {
				ev := reachy.Event{Event: reachy.EventStatus, Data: json.RawMessage(`{"motors_ok":true,"daemon_ok":true,"queue_len":0}`), Ts: "t"}
				if fb.writeFrame(conn, reachy.FrameEvent, ev) != nil {
					return
				}
			}
		}()
	}

	for {
		f, err := reachy.ReadFrame(r)
		if err != nil {
			return
		}
		if f.Type != reachy.FrameRPCRequest {
			continue
		}
		var rq reachy.RPCRequest
		_ = json.Unmarshal(f.Payload, &rq)
		if rq.Method == reachy.MethodPlayMove {
			var p struct {
				Name string `json:"name"`
			}
			_ = json.Unmarshal(rq.Params, &p)
			fb.mu.Lock()
			fb.plays = append(fb.plays, p.Name)
			fb.mu.Unlock()
			_ = fb.writeFrame(conn, reachy.FrameRPCResult, reachy.RPCResult{RequestID: rq.RequestID, OK: true, Result: json.RawMessage(`{"move_id":"m-1","queued":1}`)})
		} else {
			_ = fb.writeFrame(conn, reachy.FrameRPCResult, reachy.RPCResult{RequestID: rq.RequestID, OK: true, Result: json.RawMessage(`{}`)})
		}
	}
}

func (fb *fakeBridge) lastPlay() string {
	fb.mu.Lock()
	defer fb.mu.Unlock()
	if len(fb.plays) == 0 {
		return ""
	}
	return fb.plays[len(fb.plays)-1]
}

func (fb *fakeBridge) close() {
	_ = fb.ln.Close()
	fb.mu.Lock()
	cs := fb.conns
	fb.mu.Unlock()
	for _, c := range cs {
		_ = c.Close()
	}
}

func waitForKMC(t *testing.T, d time.Duration, cond func() bool) {
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

func TestMotionControllerExpressReachesBridge(t *testing.T) {
	// Expose only ONE clip from the "happy" pool. Filtering (spec §5) must narrow the
	// pool so express plays exactly that clip — proving the whole path: gate → clip
	// resolve → bridge play_move, end to end over the real reachy client + UDS wire.
	fb := newFakeBridge(t, []string{"laughing1", "curious1", "attentive1"})
	defer fb.close()
	mc := NewMotionController(fb.path, ActivityStandard, nil)
	mc.pollInterval = 15 * time.Millisecond
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go mc.Run(ctx)
	waitForKMC(t, 2*time.Second, mc.IsConnected)
	waitForKMC(t, 2*time.Second, mc.MovesApplied) // clip filter applied from hello.moves

	res := mc.Express(ctx, "happy")
	if !res.Expressed {
		t.Fatalf("express happy should play a gesture, got %+v", res)
	}
	waitForKMC(t, 2*time.Second, func() bool { return fb.lastPlay() != "" })
	if got := fb.lastPlay(); got != "laughing1" {
		t.Errorf("bridge should receive the only exposed happy clip laughing1, got %q", got)
	}
}

func TestMotionControllerDanceRequiresExplicitCurrentTranscript(t *testing.T) {
	fb := newFakeBridge(t, []string{"dance1", "laughing1"})
	defer fb.close()
	mc := NewMotionController(fb.path, ActivityStandard, nil)
	mc.pollInterval = 15 * time.Millisecond
	mc.danceAuthWait = time.Millisecond
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go mc.Run(ctx)
	waitForKMC(t, 2*time.Second, mc.IsConnected)
	waitForKMC(t, 2*time.Second, mc.MovesApplied)

	if res := mc.Express(ctx, "dance"); res.Expressed || res.Reason != "not_explicit" {
		t.Fatalf("dance without a request = %+v, want not_explicit", res)
	}
	mc.ObserveUserTranscript("What is dance?")
	if res := mc.Express(ctx, "dance"); res.Expressed || res.Reason != "not_explicit" {
		t.Fatalf("dance discussion = %+v, want not_explicit", res)
	}

	mc.ObserveUserTranscript("Please dance for me.")
	res := mc.Express(ctx, "dance")
	if !res.Expressed || res.Clip != "dance1" {
		t.Fatalf("explicit dance request = %+v, want dance1", res)
	}
	waitForKMC(t, 2*time.Second, func() bool { return fb.lastPlay() == "dance1" })

	// The grant is single-use even if a later response resets the ordinary express
	// per-response budget.
	mc.NewResponse()
	if res := mc.Express(ctx, "dance"); res.Expressed || res.Reason != "not_explicit" {
		t.Fatalf("reused dance grant = %+v, want not_explicit", res)
	}
}

func TestMotionControllerDanceAuthorizationExpires(t *testing.T) {
	now := time.Unix(1000, 0)
	mc := NewMotionController("/tmp/not-used.sock", ActivityStandard, nil)
	mc.now = func() time.Time { return now }
	mc.danceAuthWait = 0
	mc.ObserveUserTranscript("跳个舞")
	now = now.Add(danceAuthorizationWindow + time.Millisecond)
	if res := mc.Express(context.Background(), "dance"); res.Expressed || res.Reason != "not_explicit" {
		t.Fatalf("expired dance grant = %+v, want not_explicit", res)
	}
}

func TestMotionControllerDanceWaitsForLateTranscript(t *testing.T) {
	fb := newFakeBridge(t, []string{"dance1"})
	defer fb.close()
	mc := NewMotionController(fb.path, ActivityStandard, nil)
	mc.pollInterval = 15 * time.Millisecond
	mc.danceAuthWait = 500 * time.Millisecond
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go mc.Run(ctx)
	waitForKMC(t, 2*time.Second, mc.IsConnected)
	waitForKMC(t, 2*time.Second, mc.MovesApplied)

	result := make(chan ExpressResult, 1)
	go func() { result <- mc.Express(ctx, "dance") }()
	time.Sleep(20 * time.Millisecond)
	mc.ObserveUserTranscript("请给我跳个舞")
	select {
	case res := <-result:
		if !res.Expressed || res.Clip != "dance1" {
			t.Fatalf("late authorized dance = %+v, want dance1", res)
		}
	case <-time.After(time.Second):
		t.Fatal("dance did not resume after late transcript authorization")
	}
}

func TestMotionControllerDrivesBridgeStatusConnected(t *testing.T) {
	fb := newFakeBridge(t, []string{"laughing1"})
	defer fb.close()
	var mu sync.Mutex
	var states []string
	mc := NewMotionController(fb.path, ActivityStandard, func(s string) {
		mu.Lock()
		states = append(states, s)
		mu.Unlock()
	})
	mc.pollInterval = 15 * time.Millisecond
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go mc.Run(ctx)
	waitForKMC(t, 2*time.Second, func() bool {
		mu.Lock()
		defer mu.Unlock()
		for _, s := range states {
			if s == "connected" {
				return true
			}
		}
		return false
	})
	mu.Lock()
	first := states[0]
	mu.Unlock()
	if first != "connecting" {
		t.Errorf("first bridge_status should be connecting, got %q", first)
	}
}

func TestMotionControllerHeartbeatWatchdogDegrades(t *testing.T) {
	// A bridge that connects but emits NO status heartbeats must be flagged degraded
	// after `misses` intervals (spec §10). The socket stays up, so only the watchdog
	// catches it.
	fb := newFakeBridge(t, []string{"laughing1"})
	fb.emitHB = false
	defer fb.close()
	var mu sync.Mutex
	var states []string
	mc := NewMotionController(fb.path, ActivityStandard, func(s string) {
		mu.Lock()
		states = append(states, s)
		mu.Unlock()
	})
	mc.heartbeatInterval = 15 * time.Millisecond
	mc.pollInterval = 15 * time.Millisecond
	mc.misses = 3
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go mc.Run(ctx)
	waitForKMC(t, 2*time.Second, func() bool {
		mu.Lock()
		defer mu.Unlock()
		for _, s := range states {
			if s == "degraded" {
				return true
			}
		}
		return false
	})
}
