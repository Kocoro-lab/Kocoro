package koe

import (
	"bufio"
	"context"
	"encoding/json"
	"math"
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
	rpcs    []fakeBridgeRPC
	conns   []net.Conn
}

type fakeBridgeRPC struct {
	method string
	params json.RawMessage
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
		fb.mu.Lock()
		fb.rpcs = append(fb.rpcs, fakeBridgeRPC{method: rq.Method, params: append(json.RawMessage(nil), rq.Params...)})
		fb.mu.Unlock()
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

func (fb *fakeBridge) rpcParams(method string) []json.RawMessage {
	fb.mu.Lock()
	defer fb.mu.Unlock()
	var out []json.RawMessage
	for _, rpc := range fb.rpcs {
		if rpc.method == method {
			out = append(out, append(json.RawMessage(nil), rpc.params...))
		}
	}
	return out
}

func (fb *fakeBridge) rpcSequence(methods ...string) []string {
	fb.mu.Lock()
	defer fb.mu.Unlock()
	wanted := make(map[string]bool, len(methods))
	for _, method := range methods {
		wanted[method] = true
	}
	var out []string
	for _, rpc := range fb.rpcs {
		if wanted[rpc.method] {
			out = append(out, rpc.method)
		}
	}
	return out
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

func TestMotionControllerVoiceStateDrivesListeningReflex(t *testing.T) {
	fb := newFakeBridge(t, []string{"success1"})
	defer fb.close()
	mc := NewMotionController(fb.path, ActivityStandard, nil)
	mc.pollInterval = 15 * time.Millisecond
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go mc.Run(ctx)
	waitForKMC(t, 2*time.Second, mc.MovesApplied)

	mc.ObserveVoiceState("listening")
	waitForKMC(t, 2*time.Second, func() bool {
		params := fb.rpcParams(reachy.MethodSetListen)
		if len(params) == 0 {
			return false
		}
		var got struct {
			On bool `json:"on"`
		}
		_ = json.Unmarshal(params[len(params)-1], &got)
		return got.On
	})

	mc.ObserveVoiceState("speaking")
	waitForKMC(t, 2*time.Second, func() bool {
		params := fb.rpcParams(reachy.MethodSetListen)
		if len(params) < 2 {
			return false
		}
		var got struct {
			On bool `json:"on"`
		}
		_ = json.Unmarshal(params[len(params)-1], &got)
		return !got.On
	})
}

func TestMotionControllerDOAReflectionRequiresSustainedSpeechWithoutFace(t *testing.T) {
	fb := newFakeBridge(t, []string{"success1"})
	defer fb.close()
	mc := NewMotionController(fb.path, ActivityStandard, nil)
	mc.pollInterval = 15 * time.Millisecond
	mc.doaHitsRequired = 3
	mc.doaCooldown = 0
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go mc.Run(ctx)
	waitForKMC(t, 2*time.Second, mc.MovesApplied)
	mc.ObserveVoiceState("listening")

	now := time.Unix(100, 0)
	snapshot := PerceptionSnapshot{
		ObservedAt: now,
		Face:       FaceSample{Available: true, Fresh: true, Detected: false},
		DOA:        DOASample{Available: true, Fresh: true, Angle: math.Pi / 4, SpeechDetected: true},
	}
	mc.ObservePerception(snapshot)
	snapshot.ObservedAt = now.Add(100 * time.Millisecond)
	mc.ObservePerception(snapshot)
	if got := len(fb.rpcParams(reachy.MethodLookAt)); got != 0 {
		t.Fatalf("DOA reflection fired before sustained threshold: %d", got)
	}
	snapshot.ObservedAt = now.Add(200 * time.Millisecond)
	mc.ObservePerception(snapshot)
	waitForKMC(t, 2*time.Second, func() bool { return len(fb.rpcParams(reachy.MethodLookAt)) == 1 })

	var params struct {
		World struct {
			X float64 `json:"x"`
			Y float64 `json:"y"`
			Z float64 `json:"z"`
		} `json:"world"`
	}
	_ = json.Unmarshal(fb.rpcParams(reachy.MethodLookAt)[0], &params)
	if math.Abs(params.World.X-math.Sqrt(0.5)) > 1e-6 || math.Abs(params.World.Y-math.Sqrt(0.5)) > 1e-6 || params.World.Z != 0 {
		t.Fatalf("look-at world target = %+v, want front-left unit direction", params.World)
	}
}

func TestMotionControllerIgnoresDOAWhileConversationIsIdle(t *testing.T) {
	fb := newFakeBridge(t, []string{"success1"})
	defer fb.close()
	mc := NewMotionController(fb.path, ActivityStandard, nil)
	mc.pollInterval = 15 * time.Millisecond
	mc.doaHitsRequired = 1
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go mc.Run(ctx)
	waitForKMC(t, 2*time.Second, mc.MovesApplied)

	for i := 0; i < 5; i++ {
		mc.ObservePerception(PerceptionSnapshot{
			ObservedAt: time.Now().Add(time.Duration(i) * 100 * time.Millisecond),
			Face:       FaceSample{Available: true, Fresh: true, Detected: false},
			DOA:        DOASample{Available: true, Fresh: true, Angle: math.Pi / 4, SpeechDetected: true},
		})
	}
	time.Sleep(75 * time.Millisecond)
	if got := len(fb.rpcParams(reachy.MethodLookAt)); got != 0 {
		t.Fatalf("idle room noise drove %d DOA look-at RPC(s)", got)
	}
}

func TestMotionControllerFaceTrackingSuppressesDOAHeadWriter(t *testing.T) {
	fb := newFakeBridge(t, []string{"success1"})
	defer fb.close()
	mc := NewMotionController(fb.path, ActivityStandard, nil)
	mc.pollInterval = 15 * time.Millisecond
	mc.doaHitsRequired = 1
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go mc.Run(ctx)
	waitForKMC(t, 2*time.Second, mc.MovesApplied)
	mc.ObserveVoiceState("listening")

	mc.ObservePerception(PerceptionSnapshot{
		ObservedAt: time.Now(),
		Face:       FaceSample{Available: true, Fresh: true, Detected: true},
		DOA:        DOASample{Available: true, Fresh: true, Angle: 0, SpeechDetected: true},
	})
	time.Sleep(75 * time.Millisecond)
	if got := len(fb.rpcParams(reachy.MethodLookAt)); got != 0 {
		t.Fatalf("tracked face must retain head ownership, got %d DOA look-at RPC(s)", got)
	}
}

func TestMotionControllerTaskCompleteTurnsThenPlaysFixedSuccess(t *testing.T) {
	fb := newFakeBridge(t, []string{"success1", "laughing1"})
	defer fb.close()
	mc := NewMotionController(fb.path, ActivityQuiet, nil) // quiet disables model express, not events
	mc.pollInterval = 15 * time.Millisecond
	now := time.Unix(200, 0)
	mc.now = func() time.Time { return now }
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go mc.Run(ctx)
	waitForKMC(t, 2*time.Second, mc.MovesApplied)
	mc.ObserveVoiceState("listening")

	// A tracked face prevents the immediate DOA reflex but still records who spoke;
	// the event lane uses that angle after the task completes.
	mc.ObservePerception(PerceptionSnapshot{
		ObservedAt: now,
		Face:       FaceSample{Available: true, Fresh: true, Detected: true},
		DOA:        DOASample{Available: true, Fresh: true, Angle: math.Pi, SpeechDetected: true},
	})
	mc.TriggerTaskComplete()
	waitForKMC(t, 2*time.Second, func() bool { return fb.lastPlay() == taskCompleteClip })
	sequence := fb.rpcSequence(reachy.MethodLookAt, reachy.MethodPlayMove)
	if len(sequence) < 2 || sequence[len(sequence)-2] != reachy.MethodLookAt || sequence[len(sequence)-1] != reachy.MethodPlayMove {
		t.Fatalf("task-complete sequence = %v, want look_at then play_move", sequence)
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
