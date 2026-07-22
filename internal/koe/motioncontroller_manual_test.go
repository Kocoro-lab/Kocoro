package koe

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"
)

func TestMotionControllerManualPlayReachesBridge(t *testing.T) {
	fb := newFakeBridge(t, []string{"happy1", "dance_samba"})
	defer fb.close()
	mc := NewMotionController(fb.path, ActivityStandard, nil)
	mc.pollInterval = 15 * time.Millisecond
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go mc.Run(ctx)
	waitForKMC(t, 2*time.Second, mc.IsConnected)
	waitForKMC(t, 2*time.Second, func() bool { return mc.currentBridgeState() == bridgeStateConnected })

	if err := mc.ManualPlay("dance_samba"); err != nil {
		t.Fatalf("ManualPlay(dance_samba) = %v, want nil", err)
	}
	waitForKMC(t, 2*time.Second, func() bool { return fb.lastPlay() == "dance_samba" })
}

func TestMotionControllerManualPlayUnknownMove(t *testing.T) {
	fb := newFakeBridge(t, []string{"happy1"})
	defer fb.close()
	mc := NewMotionController(fb.path, ActivityStandard, nil)
	mc.pollInterval = 15 * time.Millisecond
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go mc.Run(ctx)
	waitForKMC(t, 2*time.Second, func() bool { return mc.currentBridgeState() == bridgeStateConnected })

	if err := mc.ManualPlay("no_such_move"); !errors.Is(err, ErrUnknownMove) {
		t.Fatalf("ManualPlay(unknown) = %v, want ErrUnknownMove", err)
	}
	if p := fb.lastPlay(); p != "" {
		t.Fatalf("unknown move should not reach the bridge, got play %q", p)
	}
}

func TestMotionControllerManualPlayBridgeNotConnected(t *testing.T) {
	// A controller whose Run never started (or whose bridge is down) is not
	// connected → ErrBridgeUnavailable, and the name is never even checked.
	mc := NewMotionController("/nonexistent/koe-bridge.sock", ActivityStandard, nil)
	if err := mc.ManualPlay("happy1"); !errors.Is(err, ErrBridgeUnavailable) {
		t.Fatalf("ManualPlay while disconnected = %v, want ErrBridgeUnavailable", err)
	}
	if err := mc.ManualStop(); !errors.Is(err, ErrBridgeUnavailable) {
		t.Fatalf("ManualStop while disconnected = %v, want ErrBridgeUnavailable", err)
	}
}

func TestMotionControllerManualPlayBridgeDegraded(t *testing.T) {
	// Socket stays up but the bridge emits no §8 heartbeats → the watchdog marks it
	// degraded. A manual play must then be refused (503), even though IsConnected.
	fb := newFakeBridge(t, []string{"happy1"})
	fb.emitHB = false
	defer fb.close()
	mc := NewMotionController(fb.path, ActivityStandard, nil)
	mc.heartbeatInterval = 15 * time.Millisecond
	mc.pollInterval = 15 * time.Millisecond
	mc.misses = 3
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go mc.Run(ctx)
	waitForKMC(t, 2*time.Second, func() bool { return mc.currentBridgeState() == bridgeStateDegraded })

	if !mc.IsConnected() {
		t.Fatal("precondition: bridge socket should still be connected while degraded")
	}
	if err := mc.ManualPlay("happy1"); !errors.Is(err, ErrBridgeUnavailable) {
		t.Fatalf("ManualPlay while degraded = %v, want ErrBridgeUnavailable", err)
	}
}

func TestMotionControllerStatusSnapshot(t *testing.T) {
	fb := newFakeBridge(t, []string{"happy1", "dance_samba"})
	fb.hbData = json.RawMessage(`{"current_move":"dance_samba","is_listening":true,"breathing_active":true,"queue_len":1}`)
	defer fb.close()
	mc := NewMotionController(fb.path, ActivityStandard, nil)
	mc.pollInterval = 15 * time.Millisecond
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go mc.Run(ctx)
	waitForKMC(t, 2*time.Second, func() bool { return mc.currentBridgeState() == bridgeStateConnected })
	// Wait until a heartbeat with the live fields has been folded into the snapshot.
	waitForKMC(t, 2*time.Second, func() bool { return mc.Status().CurrentMove == "dance_samba" })

	st := mc.Status()
	if len(st.Moves) != 2 || st.Moves[0] != "happy1" || st.Moves[1] != "dance_samba" {
		t.Fatalf("Status().Moves = %v, want the full hello catalog [happy1 dance_samba]", st.Moves)
	}
	if !st.IsListening || !st.BreathingActive {
		t.Fatalf("Status() live fields = %+v, want is_listening & breathing_active true", st)
	}
	if st.BridgeState != bridgeStateConnected {
		t.Fatalf("Status().BridgeState = %q, want connected", st.BridgeState)
	}
}
