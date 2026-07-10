//go:build darwin && cgo

package koe

import (
	"context"
	"os"
	"testing"
	"time"
)

// TestMotionControllerLiveExpress plays a real gesture through a running motion
// bridge + a real Reachy Mini, exercising the Go stack end to end (MotionController
// → reachy.Client → UDS → bridge → robot) — the segment the fake-bridge test mocks.
//
// Opt-in (needs the daemon + bridge up + robot powered):
//
//	KOE_REACHY_LIVE=1 KOE_REACHY_SOCKET=/tmp/kr_motion.sock \
//	  go test ./internal/koe/ -run TestMotionControllerLiveExpress -v
func TestMotionControllerLiveExpress(t *testing.T) {
	if os.Getenv("KOE_REACHY_LIVE") != "1" {
		t.Skip("set KOE_REACHY_LIVE=1 + KOE_REACHY_SOCKET to run against a live bridge")
	}
	sock := os.Getenv("KOE_REACHY_SOCKET")
	if sock == "" {
		t.Fatal("KOE_REACHY_SOCKET is required (e.g. /tmp/kr_motion.sock)")
	}

	mc := NewMotionController(sock, ActivityStandard, func(s string) { t.Logf("bridge_status=%s", s) })
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go mc.Run(ctx)

	waitForKMC(t, 6*time.Second, mc.IsConnected)
	waitForKMC(t, 6*time.Second, mc.MovesApplied)
	if h := mc.client.Hello(); h != nil {
		t.Logf("connected: sdk=%s moves=%d", h.SdkVersion, len(h.Moves))
	}

	res := mc.Express(ctx, "happy")
	t.Logf("express happy -> %+v", res)
	if !res.Expressed {
		t.Fatalf("live express should play a gesture, got %+v", res)
	}
	t.Logf("playing %q on the physical robot…", res.Clip)
	time.Sleep(4 * time.Second) // let the gesture run on the real robot
}
